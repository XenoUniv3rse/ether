import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import {
  candidateRef,
  createHostedClient,
  createMockHostClient,
  createRemoteHostClient,
  HEADER_VIEWER_TOKEN,
  kindsBrowse,
  kindsVisible,
  MAX_KIND_VISIBLE_IDS,
  parseFeed,
  plugin,
  streamUrl,
  type CatalogProvider,
  type KindItem,
  type KindMatcher,
  type KindResolver,
  type SourceQuery,
} from "./index.ts";

const fixture = JSON.parse(
  readFileSync(new URL("../../conformance/kinds/fixture.json", import.meta.url), "utf8"),
) as {
  kind: Record<string, unknown>;
  items: KindItem[];
  calls: Array<Record<string, any>>;
};

function pageOf(items: KindItem[], req: { cursor?: string; limit?: number }) {
  const sorted = [...items].sort((a, b) => (a.index ?? 0) - (b.index ?? 0));
  const limit = req.limit || 200;
  const off = Number(req.cursor || 0);
  const page: { items: KindItem[]; nextCursor?: string } = { items: sorted.slice(off, off + limit) };
  if (off + limit < sorted.length) page.nextCursor = String(off + limit);
  return page;
}

class Dataset implements CatalogProvider, KindResolver, KindMatcher {
  items: KindItem[];
  constructor(items: KindItem[]) { this.items = items; }
  browse(req: any) {
    return pageOf(this.items.filter((it) => it.level === req.level && (it.parentId ?? "") === (req.parentId ?? "")), req);
  }
  search(req: any) {
    const q = String(req.query ?? "").toLowerCase();
    return pageOf(this.items.filter((it) => it.title.toLowerCase().includes(q)), req);
  }
  detail(_kind: string, id: string) {
    return structuredClone(this.items.find((it) => it.id === id)!);
  }
  changes() {
    return { upserts: structuredClone(this.items), complete: true };
  }
  resolve(req: any) {
    const hints = req.item.hints ?? {};
    const parent = this.items.find((it) => it.id === req.item.parentId);
    const sources: any[] = [];
    if (parent && hints.member) {
      const ph = (parent.hints ?? {}) as Record<string, any>;
      sources.push({
        id: "zip", pattern: "container", mime: "audio/flac",
        container: { format: "zip", volumes: [{ url: ph.zipUrl, size: ph.zipSize }], member: hints.member },
      });
    }
    if (hints.pageUrl) sources.push({ id: "direct", pattern: "range", mime: "audio/flac", url: hints.pageUrl });
    return { sources };
  }
  match(req: any) {
    const matches = (req.candidates ?? []).map((c: any) => {
      const hit = String(c.title ?? "").toLowerCase().includes(req.item.title.toLowerCase());
      return { candidateRef: candidateRef(c), score: hit ? 0.95 : 0.1, reasons: [hit ? "Title matches" : "Title does not match"] };
    });
    return { matches: matches.sort((a: any, b: any) => b.score - a.score) };
  }
}

test("content-kind conformance fixture replay", async () => {
  const inst = plugin({
    manifest: {
      id: "community.example.recordings",
      name: "Field Recordings",
      version: "1.0.0",
      ether: {
        protocol: 1, kind: "content", summary: "Fixture provider",
        runtime: { kind: "remote" },
        capabilities: ["catalog_provider", "kind_resolver", "matcher", "frontend_adapter"],
        contributes: { mediaKinds: [fixture.kind as never] },
      },
    } as never,
    routes: {},
    host: createMockHostClient(),
  });
  const ds = new Dataset(fixture.items);
  inst.onCatalog("recordings", ds);
  inst.onResolve("recordings", ds);
  inst.onMatch("recordings", ds);
  inst.onSetupTest({
    testSetup: (req) =>
      req.fields?.apiKey
        ? { ok: true, detail: "Connected" }
        : { ok: false, detail: "Add your API key first", fields: { apiKey: "Required" } },
  });
  inst.onFrontend("feed", {
    handle: () => ({ status: 200, headers: { "Content-Type": "application/atom+xml" }, body: '<?xml version="1.0"?><feed><title>Field Recordings</title></feed>' }),
  });

  for (const call of fixture.calls) {
    const resp = await inst.dispatch({ method: call.method, path: call.path, body: JSON.stringify(call.body ?? {}) });
    assert.equal(resp.status, call.expectStatus, `${call.name}: ${JSON.stringify(resp.body)}`);
    if (call.expect) {
      assert.deepEqual(resp.body, call.expect, call.name);
    } else if (call.expectIds) {
      const body = resp.body as any;
      assert.deepEqual(body.upserts.map((it: KindItem) => it.id), call.expectIds);
      assert.equal(body.complete, call.expectComplete);
    } else if (call.expectTopRef) {
      assert.equal((resp.body as any).matches[0].candidateRef, call.expectTopRef);
    } else if (call.expectBodyContains) {
      assert.ok(String(resp.body).includes(call.expectBodyContains), call.name);
    }
  }
});

test("frontend dispatch keeps the method and query", async () => {
  const seen: Array<{ method: string; path: string }> = [];
  const inst = plugin({
    manifest: { id: "community.example.dav", name: "DAV", version: "1.0.0" } as never,
    routes: {},
    host: createMockHostClient(),
  });
  inst.onFrontend("dav", {
    handle: (method, path) => {
      seen.push({ method, path });
      return { status: 200, headers: {}, body: `body:${path}` };
    },
  });

  const propfind = await inst.dispatch({ method: "PROPFIND", path: "/ether/v1/frontend/dav/library/" });
  assert.equal(propfind.status, 200);
  assert.deepEqual(seen[0], { method: "PROPFIND", path: "/library/" });

  const ping = await inst.dispatch({ method: "GET", path: "/ether/v1/frontend/dav/rest/ping.view?u=a&t=b" });
  assert.equal(ping.status, 200);
  const parsed = new URL(`http://x${seen[1]!.path}`);
  assert.equal(seen[1]!.method, "GET");
  assert.equal(parsed.pathname, "/rest/ping.view");
  assert.equal(parsed.searchParams.get("u"), "a");
  assert.equal(parsed.searchParams.get("t"), "b");

  const head = await inst.dispatch({ method: "HEAD", path: "/ether/v1/frontend/dav/library/" });
  assert.equal(head.status, 200);
  assert.equal(head.body, "");

  const put = await inst.dispatch({ method: "PUT", path: "/ether/v1/kinds/recordings/browse" });
  assert.equal(put.status, 405);
});

test("mock host kinds, cache and feed kit", async () => {
  const host = createMockHostClient();
  assert.equal(await host.kinds!.upsert("recordings", [{ id: "a", level: "track", title: "A" }]), 1);
  assert.equal((await host.kinds!.items("recordings")).items.length, 1);
  const ref = await host.cache!.put("pk-x-recordings", "a.flac", new TextEncoder().encode("bytes"));
  assert.ok(ref.startsWith("blob:"));
  assert.equal(new TextDecoder().decode(await host.cache!.open(ref)), "bytes");
  await host.kinds!.delete("recordings", ["a"]);
  assert.equal((await host.kinds!.items("recordings")).items.length, 0);
  const feed = parseFeed('<rss><channel><title>T</title><item><title>E</title><guid>g1</guid><enclosure url="https://x/e.mp3"/></item></channel></rss>');
  assert.equal(feed.entries[0]!.audio, "https://x/e.mp3");
});

test("frontend stream url host service", async () => {
  const host = createMockHostClient();
  const ctx = {
    ether: host,
    headers: { "X-Ether-Viewer": "7" } as Record<string, string | string[]>,
  } as unknown as Parameters<typeof streamUrl>[0];
  const url = await streamUrl(ctx, "pk:recordings:a");
  assert.ok(url.startsWith("/ether/v1/kind-media/"));
  assert.deepEqual(host.state.lastStreamUrl, [7, "pk:recordings:a"]);
  assert.equal(host.state.lastStreamUrlPart, undefined);

  const partUrl = await streamUrl(ctx, "pk:recordings:a", 2);
  assert.ok(partUrl.endsWith("?part=2"));
  assert.deepEqual(host.state.lastStreamUrl, [7, "pk:recordings:a"]);
  assert.equal(host.state.lastStreamUrlPart, 2);
  assert.equal(host.state.lastStreamUrlSource, undefined);
  await assert.rejects(() => streamUrl(ctx, "pk:recordings:a", -1));

  const srcUrl = await streamUrl(ctx, "pk:recordings:a", { partIndex: 1, source: "pdf" });
  assert.ok(srcUrl.endsWith("?part=1"));
  assert.equal(host.state.lastStreamUrlPart, 1);
  assert.equal(host.state.lastStreamUrlSource, "pdf");
  await assert.rejects(() => streamUrl(ctx, "pk:recordings:a", { source: "   " }));

  // Hosted and remote clients forward partIndex and options objects when provided.
  const g = globalThis as Record<string, unknown>;
  const prev = g.__ether_host;
  const hostedSeen: Array<{ name: string; args: unknown }> = [];
  g.__ether_host = (name: string, raw: string) => {
    hostedSeen.push({ name, args: JSON.parse(raw) });
    return JSON.stringify({ url: "/ether/v1/kind-media/hosted-part" });
  };
  try {
    const hostedCtx = {
      ether: createHostedClient(),
      headers: { "X-Ether-Viewer": "7" },
    } as unknown as Parameters<typeof streamUrl>[0];
    assert.equal(await streamUrl(hostedCtx, "pk:recordings:a", 1), "/ether/v1/kind-media/hosted-part");
    assert.deepEqual(hostedSeen[0], { name: "frontend.streamURL", args: ["pk:recordings:a", 1] });
    assert.equal(await streamUrl(hostedCtx, "pk:recordings:a", { source: "epub" }), "/ether/v1/kind-media/hosted-part");
    assert.deepEqual(hostedSeen[1], { name: "frontend.streamURL", args: ["pk:recordings:a", { source: "epub" }] });
    assert.equal(
      await streamUrl(hostedCtx, "pk:recordings:a", { partIndex: 2, source: "pdf" }),
      "/ether/v1/kind-media/hosted-part",
    );
    assert.deepEqual(hostedSeen[2], {
      name: "frontend.streamURL",
      args: ["pk:recordings:a", { partIndex: 2, source: "pdf" }],
    });
  } finally {
    if (prev === undefined) delete g.__ether_host;
    else g.__ether_host = prev;
  }

  let remotePayload: unknown;
  const remoteCtx = {
    ether: createRemoteHostClient({
      baseUrl: "http://ether.local",
      credential: "cred-1",
      fetchImpl: (async (_url: string | URL | Request, init?: RequestInit) => {
        remotePayload = JSON.parse(String(init?.body ?? "null"));
        return new Response(JSON.stringify({ url: "/ether/v1/kind-media/remote-part" }), { status: 200 });
      }) as typeof fetch,
    }),
    headers: { "X-Ether-Viewer": "7" },
  } as unknown as Parameters<typeof streamUrl>[0];
  assert.equal(await streamUrl(remoteCtx, "pk:recordings:a", 3), "/ether/v1/kind-media/remote-part");
  assert.deepEqual(remotePayload, { viewerId: 7, itemRef: "pk:recordings:a", partIndex: 3 });
  assert.equal(
    await streamUrl(remoteCtx, "pk:recordings:a", { partIndex: 1, source: "epub" }),
    "/ether/v1/kind-media/remote-part",
  );
  assert.deepEqual(remotePayload, { viewerId: 7, itemRef: "pk:recordings:a", partIndex: 1, source: "epub" });

  const noViewer = { ether: host, headers: {} } as unknown as Parameters<typeof streamUrl>[0];
  await assert.rejects(() => streamUrl(noViewer, "pk:recordings:a"));
});

test("archive.extract wires hosted and remote clients", async () => {
  const calls: Array<{ name: string; args: unknown }> = [];
  const expected = {
    ref: "blob:" + "a".repeat(64),
    size: 4096,
    sha1: "b".repeat(40),
    md5: "c".repeat(32),
    crc32: "12345678",
  };
  const g = globalThis as Record<string, unknown>;
  const prev = g.__ether_host;
  g.__ether_host = (name: string, raw: string) => {
    calls.push({ name, args: JSON.parse(raw) });
    return JSON.stringify(expected);
  };
  try {
    const hosted = createHostedClient();
    const out1 = await hosted.archive!.extract("blob:" + "0".repeat(64), 2);
    assert.deepEqual(out1, expected);
    assert.deepEqual(calls[0], { name: "archive.extract", args: ["blob:" + "0".repeat(64), 2] });
  } finally {
    if (prev === undefined) delete g.__ether_host;
    else g.__ether_host = prev;
  }

  let remoteCall: { url: string; body: unknown } | undefined;
  const remote = createRemoteHostClient({
    baseUrl: "http://ether.local",
    credential: "cred-1",
    fetchImpl: (async (url: string | URL | Request, init?: RequestInit) => {
      remoteCall = { url: String(url), body: JSON.parse(String(init?.body ?? "null")) };
      return new Response(JSON.stringify(expected), { status: 200 });
    }) as typeof fetch,
  });
  const out2 = await remote.archive!.extract("blob:" + "0".repeat(64), 2, { name: "track01.bin" });
  assert.deepEqual(out2, expected);
  assert.deepEqual(remoteCall, {
    url: "http://ether.local/ether/plugin-host/data/archive.extract",
    body: ["blob:" + "0".repeat(64), 2, { name: "track01.bin" }],
  });
});

test("archive.read and runtime.limits wire hosted, remote, and mock clients", async () => {
  const mock = createMockHostClient();
  const mockLimits = await mock.runtime!.limits();
  assert.equal(mockLimits.readBytes, 8 * 1024 * 1024);

  const blob = "blob:" + "1".repeat(64);
  const wireRows = [
    { index: 0, member: 4, size: 5, bodyBase64: Buffer.from("alpha").toString("base64") },
    { index: 1, member: 9, size: 4, bodyBase64: Buffer.from([0, 255, 16, 42]).toString("base64") },
  ];

  const g = globalThis as Record<string, unknown>;
  const prev = g.__ether_host;
  const hostedCalls: Array<{ name: string; args: unknown }> = [];
  g.__ether_host = (name: string, raw: string) => {
    hostedCalls.push({ name, args: JSON.parse(raw) });
    if (name === "runtime.limits") {
      return JSON.stringify({ memoryBytes: 1073741824, readBytes: 268435456 });
    }
    return JSON.stringify(wireRows);
  };
  try {
    const hosted = createHostedClient();
    const lim = await hosted.runtime!.limits();
    assert.equal(lim.readBytes, 268435456);
    const out = await hosted.archive!.read(blob, [
      { member: 4, offset: 0, length: 5 },
      { member: 9, offset: 2, length: 4 },
    ]);
    assert.equal(out.length, 2);
    assert.equal(new TextDecoder().decode(out[0]!.body), "alpha");
    assert.deepEqual([...out[1]!.body], [0, 255, 16, 42]);
    assert.throws(() => hosted.archive!.read(blob, []));
  } finally {
    if (prev === undefined) delete g.__ether_host;
    else g.__ether_host = prev;
  }
  assert.equal(hostedCalls[0]!.name, "runtime.limits");
  assert.equal(hostedCalls[1]!.name, "archive.read");

  const remoteSeen: Array<{ url: string; method: string; body: unknown }> = [];
  const remote = createRemoteHostClient({
    baseUrl: "http://ether.local",
    credential: "cred-1",
    fetchImpl: (async (url: string | URL | Request, init?: RequestInit) => {
      const u = String(url);
      remoteSeen.push({
        url: u,
        method: init?.method ?? "GET",
        body: init?.body ? JSON.parse(String(init.body)) : null,
      });
      if (u.endsWith("/ether/plugin-host/limits")) {
        return new Response(JSON.stringify({ memoryBytes: 33554432, readBytes: 8388608 }), { status: 200 });
      }
      return new Response(JSON.stringify(wireRows), { status: 200 });
    }) as typeof fetch,
  });
  const rLim = await remote.runtime!.limits();
  assert.equal(rLim.readBytes, 8388608);
  const rOut = await remote.archive!.read(blob, [
    { member: 4, offset: 0, length: 5 },
    { member: 9, offset: 2, length: 4 },
  ]);
  assert.equal(rOut.length, 2);
  assert.equal(new TextDecoder().decode(rOut[0]!.body), "alpha");
  assert.deepEqual([...rOut[1]!.body], [0, 255, 16, 42]);
  assert.equal(remoteSeen[0]!.url, "http://ether.local/ether/plugin-host/limits");
  assert.equal(remoteSeen[1]!.url, "http://ether.local/ether/plugin-host/data/archive.read");
});

test("kinds.visible and kinds.browse enforce viewer token, batch cap, and policy across hosted, remote, and mock clients", async () => {
  const mock = createMockHostClient();
  await mock.kinds!.upsert("comic", [
    { id: "a", level: "series", title: "Alpha" },
    { id: "b", level: "series", title: "Beta" },
    { id: "c", level: "series", title: "Gamma" },
  ]);
  mock.state.kindHiddenIds.set("comic", new Set(["b"]));

  // Mock client rejects missing token and oversized batch.
  assert.throws(() => mock.kinds!.visible("comic", ["a", "b"]));
  assert.throws(() => mock.kinds!.browse("comic", {}));
  assert.throws(() =>
    mock.kinds!.visible(
      "comic",
      Array.from({ length: MAX_KIND_VISIBLE_IDS + 1 }, (_, i) => `id-${i}`),
      "tok-1",
    ),
  );
  assert.deepEqual(await mock.kinds!.visible("comic", ["c", "b", "missing", "a", "c"], "tok-1"), ["c", "a"]);
  const mockPage = await mock.kinds!.browse("comic", { query: "a", limit: 10 }, "tok-1");
  assert.deepEqual(mockPage.items.map((it) => it.id), ["a", "c"]);

  // FrontendAdapter receives ctx and kindsVisible/kindsBrowse pick up X-Ether-Viewer-Token.
  const app = plugin({
    manifest: { id: "org.example.comics", name: "Comics", version: "1.0.0", ether: { protocol: 1, kind: "content" } },
    routes: {},
    host: mock,
  });
  app.onFrontend("opds", {
    async handle(_m, _p, _h, _b, ctx) {
      const ids = await kindsVisible(ctx!, "comic", ["a", "b", "c"]);
      const page = await kindsBrowse(ctx!, "comic", { desc: true, limit: 10 });
      return { status: 200, headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ids, browse: page.items.map((it) => it.id) }) };
    },
  });
  const okRes = await app.dispatch({
    method: "GET",
    path: "/ether/v1/frontend/opds/feed",
    headers: { [HEADER_VIEWER_TOKEN]: "tok-live" },
  });
  assert.equal(okRes.status, 200);
  assert.deepEqual(JSON.parse(String(okRes.body)), { ids: ["a", "c"], browse: ["c", "a"] });

  const noTokRes = await app.dispatch({
    method: "GET",
    path: "/ether/v1/frontend/opds/feed",
    headers: { "X-Ether-Viewer": "42" },
  });
  assert.equal(noTokRes.status, 500);

  // Hosted client forwards to __ether_host kinds.visible / kinds.browse.
  const g = globalThis as Record<string, unknown>;
  const prev = g.__ether_host;
  const hostedCalls: Array<{ name: string; args: unknown }> = [];
  g.__ether_host = (name: string, raw: string) => {
    hostedCalls.push({ name, args: JSON.parse(raw) });
    if (name === "kinds.visible") return JSON.stringify(["a"]);
    return JSON.stringify({ items: [{ id: "a", level: "series", title: "Alpha" }] });
  };
  try {
    const hosted = createHostedClient();
    assert.deepEqual(await hosted.kinds!.visible("comic", ["a", "b"]), ["a"]);
    const page = await hosted.kinds!.browse("comic", { query: "alp" });
    assert.deepEqual(page.items.map((it) => it.id), ["a"]);
  } finally {
    if (prev === undefined) delete g.__ether_host;
    else g.__ether_host = prev;
  }
  assert.deepEqual(hostedCalls[0], { name: "kinds.visible", args: ["comic", ["a", "b"]] });
  assert.deepEqual(hostedCalls[1], { name: "kinds.browse", args: ["comic", { query: "alp" }] });

  // Remote client sends X-Ether-Viewer-Token header and never viewerId in body.
  const seenRemote: Array<{ url: string; headers: Record<string, string>; body: Record<string, unknown> }> = [];
  const remote = createRemoteHostClient({
    baseUrl: "http://ether.local",
    credential: "cred-1",
    fetchImpl: (async (url: string | URL | Request, init?: RequestInit) => {
      const hdrs = (init?.headers ?? {}) as Record<string, string>;
      const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : {};
      seenRemote.push({ url: String(url), headers: hdrs, body });
      if (String(url).endsWith("/visible")) {
        return new Response(JSON.stringify({ ids: ["a"] }), { status: 200 });
      }
      return new Response(JSON.stringify({ items: [{ id: "a", level: "series", title: "Alpha" }] }), { status: 200 });
    }) as typeof fetch,
  });
  await assert.rejects(() => Promise.resolve(remote.kinds!.visible("comic", ["a"])));
  await assert.rejects(() => Promise.resolve(remote.kinds!.browse("comic", {})));
  assert.deepEqual(await remote.kinds!.visible("comic", ["a", "b"], "tok-r"), ["a"]);
  const rPage = await remote.kinds!.browse("comic", { query: "alp", desc: true }, "tok-r");
  assert.equal(rPage.items.length, 1);
  assert.equal(seenRemote[0]!.url, "http://ether.local/ether/plugin-host/kinds/comic/visible");
  assert.equal(seenRemote[0]!.headers[HEADER_VIEWER_TOKEN], "tok-r");
  assert.equal("viewerId" in seenRemote[0]!.body, false);
  assert.equal(seenRemote[1]!.url, "http://ether.local/ether/plugin-host/kinds/comic/browse");
  assert.equal(seenRemote[1]!.headers[HEADER_VIEWER_TOKEN], "tok-r");
  assert.equal("viewerId" in seenRemote[1]!.body, false);
});

test("onLive dispatches /live and /live/resolve", async () => {
  const app = plugin({
    manifest: { id: "org.example.games", name: "Games", version: "1.0.0", ether: { protocol: 1, kind: "content" } },
    routes: {},
  });
  let seenReq: unknown;
  let seenResolve: unknown;
  app.onLive("esports", {
    live(req) {
      seenReq = req;
      return {
        items: [{ id: "ch-1", title: "Finals", subtitle: "Main Stage", badge: "LIVE", viewers: 420, tags: ["Esports"] }],
        filters: [{ key: "featured", label: "Featured" }],
        nextCursor: "cur-2",
      };
    },
    resolveLive(req) {
      seenResolve = req;
      return { url: "https://stream.example/live/ch-1/master.m3u8?token=once", mime: "application/vnd.apple.mpegurl", title: "Finals" };
    },
  });

  const liveRes = await app.dispatch({
    method: "POST",
    path: "/ether/v1/kinds/esports/live",
    body: JSON.stringify({ tab: "live", q: "finals", filter: "featured", limit: 10 }),
  });
  assert.equal(liveRes.status, 200);
  assert.deepEqual(seenReq, { kind: "esports", tab: "live", q: "finals", filter: "featured", limit: 10 });
  assert.equal((liveRes.body as { items: unknown[]; nextCursor: string }).items.length, 1);
  assert.equal((liveRes.body as { nextCursor: string }).nextCursor, "cur-2");

  const resolveRes = await app.dispatch({
    method: "POST",
    path: "/ether/v1/kinds/esports/live/resolve",
    body: JSON.stringify({ id: "ch-1" }),
  });
  assert.equal(resolveRes.status, 200);
  assert.deepEqual(seenResolve, { kind: "esports", id: "ch-1" });
  assert.equal((resolveRes.body as { url: string }).url, "https://stream.example/live/ch-1/master.m3u8?token=once");
});

test("resolveSourcesWithResult forwards indexer_search options and decodes indexers", async () => {
  const query = {
    kind: "indexer_search" as const,
    query: "Dune",
    firstResults: true,
    settleMs: 150,
    minResults: 2,
    skipIndexers: ["slow"],
    timeoutMs: 3000,
  };
  const reply = {
    candidates: [{ title: "Fast.Release", indexerId: "fast", indexer: "Fast" }],
    indexers: [
      { indexerId: "fast", indexer: "Fast", status: "answered" as const, results: 1, ms: 18 },
      { indexerId: "slow", indexer: "Slow", status: "skipped" as const, ms: 0 },
    ],
  };

  let seenRemoteBody: unknown;
  const remote = createRemoteHostClient({
    baseUrl: "http://ether.local",
    credential: "cred",
    fetchImpl: (async (_url: string | URL | Request, init?: RequestInit) => {
      seenRemoteBody = JSON.parse(String(init?.body ?? "{}"));
      return new Response(JSON.stringify((seenRemoteBody as SourceQuery).kind === "debrid" ? { enabled: true, cachedOnly: false } : reply), { status: 200 });
    }) as typeof fetch,
  });
  const rRes = await remote.kinds!.resolveSourcesWithResult!(query);
  assert.deepEqual(seenRemoteBody, query);
  assert.deepEqual(rRes, reply);
  assert.deepEqual(await remote.kinds!.resolveSources(query), reply.candidates);
  assert.deepEqual(await remote.kinds!.resolveSourcesWithResult!({ kind: "debrid" }), { candidates: [], enabled: true, cachedOnly: false });

  // Hosted client forwards query via sources.resolve and preserves indexers.
  const g = globalThis as Record<string, unknown>;
  const prevHost = g.__ether_host;
  let seenHostedArgs: unknown;
  g.__ether_host = (name: string, raw: string) => {
    if (name === "sources.resolve") {
      seenHostedArgs = JSON.parse(raw);
      return JSON.stringify(JSON.parse(raw)[0].kind === "debrid_check" ? { candidates: [], cachedOnly: false } : reply);
    }
    throw new Error(`unexpected host call: ${name}`);
  };
  try {
    const hosted = createHostedClient();
    const hRes = hosted.kinds!.resolveSourcesWithResult!(query);
    assert.deepEqual(seenHostedArgs, [query]);
    assert.deepEqual(hRes, reply);
    assert.deepEqual(hosted.kinds!.resolveSources(query), reply.candidates);
  } finally {
    if (prevHost === undefined) delete g.__ether_host;
    else g.__ether_host = prevHost;
  }

  const mock = createMockHostClient({ sources: reply.candidates, sourceIndexers: reply.indexers });
  const mRes = await mock.kinds!.resolveSourcesWithResult!(query);
  assert.deepEqual(mRes, reply);
  assert.deepEqual(await mock.kinds!.resolveSources(query), reply.candidates);

  // Streaming envelope (searchId, pending, done) and kind:"indexers" listing round-trip.
  const streamReply = {
    candidates: [],
    searchId: "srch_1",
    done: false,
    pending: [{ indexerId: "slow", indexer: "Slow" }],
    indexers: [{ indexerId: "slow", indexer: "Slow", status: "pending" as const, ms: 0 }],
  };
  const streamRemote = createRemoteHostClient({
    baseUrl: "http://ether.local",
    credential: "cred",
    fetchImpl: (async () => new Response(JSON.stringify(streamReply), { status: 200 })) as typeof fetch,
  });
  const srRes = await streamRemote.kinds!.resolveSourcesWithResult!({ kind: "indexer_search", query: "Dune", stream: true });
  assert.deepEqual(srRes, streamReply);

  const mockStream = createMockHostClient({
    sourceHandler: (q) => (q.kind === "indexers"
      ? { candidates: [], indexers: [{ indexerId: "a", indexer: "Alpha", kind: "builtin", medianMs: 320, lastMs: 280 }] }
      : streamReply),
  });
  assert.deepEqual(await mockStream.kinds!.resolveSourcesWithResult!({ kind: "indexer_search", query: "Dune", stream: true }), streamReply);
  assert.deepEqual(await mockStream.kinds!.resolveSourcesWithResult!({ kind: "indexers" }), {
    candidates: [],
    indexers: [{ indexerId: "a", indexer: "Alpha", kind: "builtin", medianMs: 320, lastMs: 280 }],
  });
});

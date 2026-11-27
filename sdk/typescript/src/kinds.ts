/**
 * Content-kind handlers (Ether v0.3.8): catalogue providers, resolvers,
 * matchers, frontend adapters and setup tests, plus the dispatch the
 * router calls for /ether/v1/kinds, /ether/v1/setup and
 * /ether/v1/frontend. Wire shapes live in types.ts and are pinned by
 * sdk/conformance/kinds/fixture.json across all three SDKs.
 */
import type { PluginContext } from "./router.ts";
import { HEADER_VIEWER_TOKEN, MAX_KIND_VISIBLE_IDS, decodeBase64, decodeText, encodeText, normalizeStreamUrlOptions } from "./host.ts";
import type {
  FrontendArt,
  FrontendBlob,
  FrontendMedia,
  FrontendUpload,
  KindBrowseQuery,
  KindBrowseRequest,
  KindChangePage,
  KindExpandRequest,
  KindExpandResponse,
  KindLyrics,
  KindLyricsRequest,
  KindRelated,
  KindRelatedRequest,
  KindItem,
  KindLivePage,
  KindLiveRequest,
  KindLiveResolveRequest,
  KindLiveResolveResponse,
  LiveEntitiesRequest,
  LiveEventDetail,
  LiveEventDetailRequest,
  LiveEventEntitiesResponse,
  LiveEventsRequest,
  LiveEventsResponse,
  LiveEventStandingsResponse,
  LiveStandingsRequest,
  PendingReply,
  KindMatchRequest,
  KindMatchScore,
  KindPage,
  KindResolveRequest,
  KindResolveResponse,
  MaybePromise,
  NZBIdentity,
  RawRequest,
  RawResponse,
  SetupTestRequest,
  SetupTestResult,
  SourceCandidate,
  StreamUrlOptions,
} from "./types.ts";

export { HEADER_VIEWER_TOKEN, MAX_KIND_VISIBLE_IDS };

export interface CatalogProvider<S = Record<string, unknown>> {
  browse(req: KindBrowseRequest, ctx: PluginContext<S>): MaybePromise<KindPage>;
  detail(kind: string, id: string, ctx: PluginContext<S>): MaybePromise<KindItem>;
  /** Optional delta feed for pull sync; without it Ether re-walks. */
  changes?(req: { kind: string; cursor?: string; limit?: number }, ctx: PluginContext<S>): MaybePromise<KindChangePage>;
  /** Optional provider search; required when the kind declares browse.liveSearch. */
  search?(req: KindBrowseRequest, ctx: PluginContext<S>): MaybePromise<KindPage>;
  /** Optional on-demand facet/person slice expansion when the kind declares browse.expand. */
  expand?(req: KindExpandRequest, ctx: PluginContext<S>): MaybePromise<KindExpandResponse>;
  /** Optional related items when the kind declares related. */
  related?(req: KindRelatedRequest, ctx: PluginContext<S>): MaybePromise<KindRelated>;
  /** Optional lyrics for a playable item when the kind declares lyrics; null when there are none. */
  lyrics?(req: KindLyricsRequest, ctx: PluginContext<S>): MaybePromise<KindLyrics | null>;
}

export interface KindResolver<S = Record<string, unknown>> {
  resolve(req: KindResolveRequest, ctx: PluginContext<S>): MaybePromise<KindResolveResponse | PendingReply>;
}

export interface KindMatcher<S = Record<string, unknown>> {
  match(req: KindMatchRequest, ctx: PluginContext<S>): MaybePromise<{ matches: KindMatchScore[] } | PendingReply>;
}

/** Spec-name alias. */
export type Matcher<S = Record<string, unknown>> = KindMatcher<S>;

export interface KindLiveProvider<S = Record<string, unknown>> {
  live(req: KindLiveRequest, ctx: PluginContext<S>): MaybePromise<KindLivePage>;
  resolveLive(req: KindLiveResolveRequest, ctx: PluginContext<S>): MaybePromise<KindLiveResolveResponse>;
}

export interface LiveFeedProvider<S = Record<string, unknown>> {
  events(req: LiveEventsRequest, ctx: PluginContext<S>): MaybePromise<LiveEventsResponse>;
  eventDetail(req: LiveEventDetailRequest, ctx: PluginContext<S>): MaybePromise<LiveEventDetail>;
  entities?(req: LiveEntitiesRequest, ctx: PluginContext<S>): MaybePromise<LiveEventEntitiesResponse>;
  standings?(req: LiveStandingsRequest, ctx: PluginContext<S>): MaybePromise<LiveEventStandingsResponse>;
}

export interface FrontendAdapter<S = Record<string, unknown>> {
  handle(
    method: string,
    path: string,
    headers: Record<string, string>,
    body: string,
    ctx?: PluginContext<S>,
    upload?: FrontendUpload,
  ): MaybePromise<{
    status: number;
    headers?: Record<string, string>;
    body?: string;
    /** Ether serves cached bytes, an item's media, or an item's artwork instead of body. */
    blob?: FrontendBlob;
    media?: FrontendMedia;
    art?: FrontendArt;
  }>;
}

export interface SetupTester<S = Record<string, unknown>> {
  testSetup(req: SetupTestRequest, ctx: PluginContext<S>): MaybePromise<SetupTestResult>;
}

/** Request an indexer NZB through Ether and get its file list and vault reference. */
export async function requestNzb<S = Record<string, unknown>>(
  ctx: PluginContext<S>,
  release: string | NZBIdentity,
): Promise<SourceCandidate> {
  if (!ctx.ether?.kinds) throw new Error("NZB request: no host service available");
  const candidates = await ctx.ether.kinds.resolveSources(typeof release === "string"
    ? { kind: "nzb_request", nzbRef: release }
    : { kind: "nzb_request", nzbIdentity: release });
  const result = candidates[0];
  if (!result?.nzbRef) throw new Error("nzb_request returned no vault reference");
  return result;
}

/** The stable ref matchers report for a source candidate. */
export function candidateRef(c: SourceCandidate): string {
  if (c.infoHash) return `infohash:${c.infoHash.toLowerCase()}`;
  if (c.magnet) return `magnet:${c.magnet}`;
  if (c.torrentRef) return `torrentref:${c.torrentRef}`;
  if (c.nzbRef) return `nzbref:${c.nzbRef}`;
  if (c.torrentUrl) return `torrent:${c.torrentUrl}`;
  if (c.nzbUrl) return `nzb:${c.nzbUrl}`;
  return c.title ?? "";
}

/**
 * Mint a scoped, short-lived Ether stream URL for one of this plugin's own
 * kind items (spec §6.5), optionally scoped to a zero-based part (`partIndex`)
 * and/or a specific resolver `PlayableSource.id` (`source`) (#576, #661). The
 * viewer is the one Ether authenticated on this request (X-Ether-Viewer);
 * bytes are served by Ether, never by the plugin.
 */
export async function streamUrl<S = Record<string, unknown>>(
  ctx: PluginContext<S>,
  itemRef: string,
  partIndexOrOpts?: number | StreamUrlOptions,
): Promise<string> {
  if (!ctx.ether?.frontend) throw new Error("stream url: no host service available");
  const opts = normalizeStreamUrlOptions(partIndexOrOpts);
  const raw = ctx.headers["X-Ether-Viewer"] ?? ctx.headers["x-ether-viewer"];
  const value = Array.isArray(raw) ? raw[0] : raw;
  const viewerId = Number.parseInt(String(value ?? ""), 10);
  if (!Number.isFinite(viewerId) || viewerId <= 0) {
    throw new Error("stream url: no authenticated viewer on this request");
  }
  if (opts.source !== undefined || opts.absolute) {
    return ctx.ether.frontend.streamUrl(viewerId, itemRef, opts);
  }
  return opts.partIndex === undefined
    ? ctx.ether.frontend.streamUrl(viewerId, itemRef)
    : ctx.ether.frontend.streamUrl(viewerId, itemRef, opts.partIndex);
}

function readViewerToken<S>(ctx: PluginContext<S>): string {
  for (const [k, v] of Object.entries(ctx.headers ?? {})) {
    if (k.toLowerCase() === HEADER_VIEWER_TOKEN.toLowerCase()) {
      const value = Array.isArray(v) ? v[0] : v;
      return String(value ?? "").trim();
    }
  }
  return "";
}

/**
 * Evaluate the library's Rules and viewer access policy for a bounded batch
 * (at most 500) of external IDs in `kind` (#615). Only frontend adapter
 * requests carrying Ether's authenticated viewer token may call it.
 */
export async function kindsVisible<S = Record<string, unknown>>(
  ctx: PluginContext<S>,
  kind: string,
  ids: string[],
): Promise<string[]> {
  if (!ctx.ether?.kinds?.visible) {
    throw new Error("kinds.visible: no host service available");
  }
  const token = readViewerToken(ctx);
  if (!token) {
    throw new Error("kinds.visible: no authenticated frontend adapter viewer on this request");
  }
  if (ids.length > MAX_KIND_VISIBLE_IDS) {
    throw new Error(`kinds.visible: batch larger than ${MAX_KIND_VISIBLE_IDS} ids`);
  }
  return await ctx.ether.kinds.visible(kind, ids, token);
}

/**
 * Browse or search `kind` with the library's Rules and viewer access policy
 * applied for the requesting frontend-adapter viewer (#615). Only frontend
 * adapter requests carrying Ether's authenticated viewer token may call it.
 */
export async function kindsBrowse<S = Record<string, unknown>>(
  ctx: PluginContext<S>,
  kind: string,
  query: KindBrowseQuery = {},
): Promise<KindPage> {
  if (!ctx.ether?.kinds?.browse) {
    throw new Error("kinds.browse: no host service available");
  }
  const token = readViewerToken(ctx);
  if (!token) {
    throw new Error("kinds.browse: no authenticated frontend adapter viewer on this request");
  }
  return await ctx.ether.kinds.browse(kind, query, token);
}

export interface KindHandlers<S = Record<string, unknown>> {
  catalog: Map<string, CatalogProvider<S>>;
  resolve: Map<string, KindResolver<S>>;
  match: Map<string, KindMatcher<S>>;
  live: Map<string, KindLiveProvider<S>>;
  liveFeeds: Map<string, LiveFeedProvider<S>>;
  frontend: Map<string, FrontendAdapter<S>>;
  /** Screen and surface sources and actions, keyed "<screen>/<name>" (docs/plugin-screens.md). */
  screenSources?: Map<string, ScreenSourceHandler<S>>;
  screenActions?: Map<string, ScreenActionHandler<S>>;
  setup?: SetupTester<S>;
}

/** What Ether sends a screen source: the document's arguments with their holes filled. */
export interface ScreenSourceRequest {
  /** The screen (or surface) id and its page. */
  screen: string;
  route: string;
  page: string;
  args: Record<string, string>;
  locale: string;
  /** The viewer's IANA time zone. */
  tz: string;
}

/** A source answer. Images, links, records, events and entities go through the envelope helpers. */
export interface ScreenSourceAnswer {
  value: unknown;
  records?: ScreenRecord[];
  /** How long Ether may reuse this answer (5..3600 s, default 60; viewer-scoped answers at most 60). */
  maxAgeSec?: number;
  /** While a viewer has the page open, ask again after this many seconds (at least 10). */
  nextRefreshSec?: number;
}

export interface ScreenRecord {
  type: string;
  id: string;
  value: unknown;
}

export interface ScreenActionRequest {
  screen: string;
  action: string;
  page: string;
  args: Record<string, unknown>;
  params: Record<string, string>;
  locale: string;
  tz: string;
}

/** An action answer: patches the client applies at once. */
export interface ScreenActionAnswer {
  records?: ScreenRecord[];
  /** Whole-source replacements, by source id. */
  sources?: Record<string, unknown>;
  /** Sources Ether reads again before answering. */
  refresh?: string[];
  /** Plain text, at most 200 characters. */
  toast?: string;
  /** A Vela navigate action: { action: "navigate", args: { page, ...params } }. */
  navigate?: { action: "navigate"; args: Record<string, string> };
}

export type ScreenSourceHandler<S = Record<string, unknown>> = (
  req: ScreenSourceRequest,
  ctx: PluginContext<S>,
) => MaybePromise<ScreenSourceAnswer>;

export type ScreenActionHandler<S = Record<string, unknown>> = (
  req: ScreenActionRequest,
  ctx: PluginContext<S>,
) => MaybePromise<ScreenActionAnswer | void>;

export function createKindHandlers<S = Record<string, unknown>>(): KindHandlers<S> {
  return { catalog: new Map(), resolve: new Map(), match: new Map(), live: new Map(), liveFeeds: new Map(), frontend: new Map() };
}

function ok(body: unknown): RawResponse {
  return { status: 200, headers: { "Content-Type": "application/json" }, body };
}

function err(status: number, code: string, message: string): RawResponse {
  return { status, headers: { "Content-Type": "application/json" }, body: { error: { code, message } } };
}

function parseBody(body: unknown): Record<string, unknown> {
  if (typeof body === "string" && body.trim()) {
    const parsed = JSON.parse(body) as unknown;
    return parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : {};
  }
  if (body && typeof body === "object") return body as Record<string, unknown>;
  return {};
}

/**
 * The exact bytes of a frontend-adapter request body. Ether sends them in
 * `bodyBase64` on adapter routes — a non-JSON body (multipart, form-encoded,
 * binary) arrives only there, with its Content-Type in `contentType` — so
 * this is the way to read uploads byte-for-byte. Falls back to the string
 * or JSON body for other routes.
 */
export function frontendBodyBytes(req: RawRequest): Uint8Array {
  if (typeof req.bodyBase64 === "string") return decodeBase64(req.bodyBase64);
  if (typeof req.body === "string") return encodeText(req.body);
  if (req.body === undefined || req.body === null) return new Uint8Array(0);
  return encodeText(JSON.stringify(req.body));
}

function frontendBodyString(req: RawRequest): string {
  if (typeof req.body === "string") return req.body;
  if (typeof req.bodyBase64 === "string") {
    // Binary string: one char per byte, so string-based adapters still see
    // the content; use frontendBodyBytes on the raw request for exact bytes.
    const bytes = decodeBase64(req.bodyBase64);
    let out = "";
    for (let i = 0; i < bytes.length; i += 8192) {
      out += String.fromCharCode(...bytes.subarray(i, i + 8192));
    }
    return out;
  }
  return "";
}

/**
 * The original query for a frontend-adapter request, from the flat `query`
 * map and the query string still attached to `path`. Repeated keys arrive
 * flattened to the first value by the server.
 */
function frontendQuery(req: RawRequest): URLSearchParams {
  const query = new URLSearchParams();
  if (req.query) {
    for (const [k, v] of Object.entries(req.query)) {
      if (Array.isArray(v)) {
        if (v.length > 0) query.set(k, String(v[0]));
      } else if (v !== undefined && v !== null) {
        query.set(k, String(v));
      }
    }
  }
  const qIndex = (req.path || "").indexOf("?");
  if (qIndex >= 0) {
    for (const [k, v] of new URLSearchParams((req.path || "").slice(qIndex + 1))) {
      if (!query.has(k)) query.set(k, v);
    }
  }
  return query;
}

/** Dispatch one /ether/v1/* request; returns null when the path is not a kind route. */
export async function dispatchKinds<S = Record<string, unknown>>(
  handlers: KindHandlers<S>,
  req: RawRequest,
  ctx: PluginContext<S>,
): Promise<RawResponse | null> {
  const method = (req.method || "GET").toUpperCase();
  const path = (req.path || "/").split("?")[0] ?? "/";
  if (!path.startsWith("/ether/v1/")) return null;
  const parts = path.slice("/ether/v1/".length).split("/").map((p) => decodeURIComponent(p));

  // Frontend adapters are protocol surfaces (WebDAV, Subsonic, OPDS, …) and
  // see every method token. The kind and setup routes stay JSON GET/POST.
  if (parts.length >= 2 && parts[0] === "frontend") {
    const adapter = handlers.frontend.get(parts[1] ?? "");
    if (!adapter) return err(404, "not_found", `No frontend adapter is registered for "${parts[1]}".`);
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(req.headers ?? {})) headers[k] = Array.isArray(v) ? (v[0] ?? "") : v;
    // Rebuild the adapter URL with the original query. The server flattens
    // repeated keys to the first value, so the query holds one value per key.
    let subPath = `/${parts.slice(2).join("/")}`;
    const query = frontendQuery(req);
    const qs = query.toString();
    if (qs) subPath += `?${qs}`;
    let upload = req.upload;
    const marker = Object.keys(headers).find((key) => key.toLowerCase() === "x-ether-frontend-upload");
    if (marker && headers[marker] === "1") {
      try {
        const metadata = req.bodyBase64 !== undefined
          ? decodeText(decodeBase64(req.bodyBase64))
          : req.body;
        upload = (typeof metadata === "string" ? JSON.parse(metadata) : metadata) as FrontendUpload;
      } catch {
        return err(400, "invalid_upload", "Invalid frontend upload metadata.");
      }
      if (!upload || typeof upload !== "object" || Array.isArray(upload)) {
        return err(400, "invalid_upload", "Invalid frontend upload metadata.");
      }
      delete headers[marker];
    }
    if (upload) {
      const contentTypeKey = Object.keys(headers).find((key) => key.toLowerCase() === "content-type");
      if (contentTypeKey) delete headers[contentTypeKey];
      if (upload.contentType) headers["Content-Type"] = upload.contentType;
    }
    const res = await adapter.handle(method, subPath, headers, upload ? "" : frontendBodyString(req), ctx, upload);
    const body = res.body ?? "";
    if (body.length > 10 * 1024 * 1024) return err(502, "adapter_error", "Frontend adapter response is larger than 10 MiB; serve media through Ether stream URLs.");
    // A HEAD reply carries headers only, exactly as a real server would.
    const out: RawResponse = { status: res.status, headers: res.headers ?? {}, body: method === "HEAD" ? "" : body };
    if (res.blob) out.blob = res.blob;
    if (res.media) out.media = res.media;
    if (res.art) out.art = res.art;
    return out;
  }

  if (method !== "GET" && method !== "POST") {
    return err(405, "method_not_allowed", "Kind routes use GET or POST.");
  }

  let body: Record<string, unknown>;
  try {
    body = parseBody(req.body);
  } catch {
    return err(400, "invalid_json", "Request body must be valid JSON.");
  }

  if (parts.length === 3 && parts[0] === "setup" && parts[2] === "test") {
    if (!handlers.setup) return err(404, "not_found", "Setup test handler is not registered.");
    const input = { step: String(body.step || parts[1]), fields: body.fields as Record<string, unknown> | undefined };
    return ok(await handlers.setup.testSetup(input, ctx));
  }

  if (parts.length === 4 && (parts[0] === "screens" || parts[0] === "surfaces") && (parts[2] === "sources" || parts[2] === "actions")) {
    if (method !== "POST") return err(405, "method_not_allowed", "Screen routes use POST.");
    const screen = parts[1] ?? "";
    const name = parts[3] ?? "";
    const key = `${screen}/${name}`;
    const page = String(body.page ?? "");
    const locale = String(body.locale ?? "en");
    const tz = String(body.tz ?? "UTC");
    if (parts[2] === "sources") {
      const handler = handlers.screenSources?.get(key) ?? handlers.screenSources?.get(`*/${name}`);
      if (!handler) return err(404, "not_found", `No screen source "${name}" is registered for "${screen}".`);
      const args = (body.args && typeof body.args === "object" ? body.args : {}) as Record<string, string>;
      return ok(await handler({ screen, route: name, page, args, locale, tz }, ctx));
    }
    const handler = handlers.screenActions?.get(key) ?? handlers.screenActions?.get(`*/${name}`);
    if (!handler) return err(404, "not_found", `No screen action "${name}" is registered for "${screen}".`);
    const args = (body.args && typeof body.args === "object" ? body.args : {}) as Record<string, unknown>;
    const params = (body.params && typeof body.params === "object" ? body.params : {}) as Record<string, string>;
    return ok((await handler({ screen, action: name, page, args, params, locale, tz }, ctx)) ?? {});
  }

  if (parts.length >= 3 && parts[0] === "live") {
    if (method !== "GET") {
      return err(405, "method_not_allowed", "Live feed routes use GET.");
    }
    const feed = parts[1] ?? "";
    const provider = handlers.liveFeeds.get(feed);
    if (!provider) {
      return err(404, "not_found", `No live feed provider is registered for feed "${feed}".`);
    }
    const tail = parts.slice(2);
    const q = ctx.query ?? {};
    if (tail.length === 1 && tail[0] === "events") {
      const ids = q.ids
        ? q.ids
            .split(",")
            .map((s) => s.trim())
            .filter(Boolean)
        : undefined;
      const res = {
        ...(await provider.events(
          { feed, from: q.from || undefined, to: q.to || undefined, league: q.league || undefined, ids },
          ctx,
        )),
      };
      res.events = res.events ?? [];
      return ok(res);
    }
    if (tail.length === 2 && tail[0] === "events" && tail[1]) {
      return ok(await provider.eventDetail({ feed, id: tail[1] }, ctx));
    }
    if (tail.length === 1 && tail[0] === "entities") {
      if (!provider.entities) {
        return err(501, "not_implemented", "This live feed does not list entities.");
      }
      const res = {
        ...(await provider.entities(
          { feed, kind: q.kind || undefined, league: q.league || undefined, q: q.q || undefined },
          ctx,
        )),
      };
      res.entities = res.entities ?? [];
      return ok(res);
    }
    if (tail.length === 1 && tail[0] === "standings") {
      if (!provider.standings) {
        return err(501, "not_implemented", "This live feed does not serve standings.");
      }
      const res = {
        ...(await provider.standings({ feed, league: q.league || undefined }, ctx)),
      };
      res.tables = res.tables ?? [];
      return ok(res);
    }
    return err(404, "not_found", "No live feed route matched.");
  }

  if (parts.length < 3 || parts[0] !== "kinds") return null;
  const kind = parts[1] ?? "";
  const tail = parts.slice(2);
  const browseReq = { ...body, kind } as unknown as KindBrowseRequest;
  const page = async (p: MaybePromise<KindPage>): Promise<RawResponse> => {
    const result = { ...(await p) };
    const limit = Number(browseReq.limit || 200);
    result.items = (result.items ?? []).slice(0, limit);
    if (!result.nextCursor) delete result.nextCursor;
    return ok(result);
  };

  if (tail.length === 1 && (tail[0] === "browse" || tail[0] === "search")) {
    const provider = handlers.catalog.get(kind);
    if (!provider) return err(404, "not_found", `No catalogue provider is registered for kind "${kind}".`);
    if (tail[0] === "search") {
      if (!provider.search) return err(501, "not_implemented", "This provider does not search.");
      return page(provider.search(browseReq, ctx));
    }
    return page(provider.browse(browseReq, ctx));
  }
  if (tail.length === 1 && tail[0] === "expand") {
    const provider = handlers.catalog.get(kind);
    if (!provider?.expand) return err(501, "not_implemented", "This provider does not expand facets.");
    const result = { ...(await provider.expand({ ...body, kind } as unknown as KindExpandRequest, ctx)) };
    if (!result.job) result.items = result.items ?? [];
    return ok(result);
  }
  if (tail.length === 1 && tail[0] === "related") {
    const provider = handlers.catalog.get(kind);
    if (!provider?.related) return err(501, "not_implemented", "This provider has no related items.");
    const result = { ...(await provider.related({ ...body, kind } as unknown as KindRelatedRequest, ctx)) };
    result.items = result.items ?? [];
    return ok(result);
  }
  if (tail.length === 1 && tail[0] === "lyrics") {
    const provider = handlers.catalog.get(kind);
    if (!provider?.lyrics) return err(501, "not_implemented", "This provider has no lyrics.");
    const result = await provider.lyrics({ ...body, kind } as unknown as KindLyricsRequest, ctx);
    return result ? ok({ ...result }) : err(404, "not_found", "No lyrics for this item.");
  }
  if (tail.length === 2 && tail[0] === "items") {
    const provider = handlers.catalog.get(kind);
    if (!provider) return err(404, "not_found", `No catalogue provider is registered for kind "${kind}".`);
    return ok(await provider.detail(kind, tail[1] ?? "", ctx));
  }
  if (tail.length === 1 && tail[0] === "changes") {
    const provider = handlers.catalog.get(kind);
    if (!provider?.changes) return err(501, "not_implemented", "This provider has no changes feed.");
    const result = { ...(await provider.changes({ kind, cursor: body.cursor as string | undefined, limit: body.limit as number | undefined }, ctx)) };
    result.upserts = result.upserts ?? [];
    return ok(result);
  }
  if (tail.length === 1 && tail[0] === "resolve") {
    const resolver = handlers.resolve.get(kind);
    if (!resolver) return err(404, "not_found", `No resolver is registered for kind "${kind}".`);
    const result = { ...(await resolver.resolve({ ...body, kind } as unknown as KindResolveRequest, ctx)) };
    result.sources = result.sources ?? [];
    return ok(result);
  }
  if (tail.length === 1 && tail[0] === "live") {
    const provider = handlers.live.get(kind);
    if (!provider) return err(404, "not_found", `No live provider is registered for kind "${kind}".`);
    const liveReq = { ...body, kind } as unknown as KindLiveRequest;
    const result = { ...(await provider.live(liveReq, ctx)) };
    const limit = Number(liveReq.limit || 50);
    result.items = (result.items ?? []).slice(0, limit);
    if (!result.nextCursor) delete result.nextCursor;
    return ok(result);
  }
  if (tail.length === 2 && tail[0] === "live" && tail[1] === "resolve") {
    const provider = handlers.live.get(kind);
    if (!provider) return err(404, "not_found", `No live provider is registered for kind "${kind}".`);
    return ok(await provider.resolveLive({ ...body, kind } as unknown as KindLiveResolveRequest, ctx));
  }
  if (tail.length === 1 && tail[0] === "match") {
    const matcher = handlers.match.get(kind);
    if (!matcher) return err(404, "not_found", `No matcher is registered for kind "${kind}".`);
    const result = { ...(await matcher.match({ ...body, kind } as unknown as KindMatchRequest, ctx)) };
    result.matches = result.matches ?? [];
    return ok(result);
  }
  return err(404, "not_found", "No kind route matched.");
}

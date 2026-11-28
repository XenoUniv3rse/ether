import type { CacheSnapshot, ReleaseDetail } from "../pages/cache/Cache";
import type { EtherConfig } from "../lib/types";

/** Offline-only fixtures used by the preview and UI tests. */
export function createCacheMock(
  restartRequired = false,
  titleName?: (key: number) => string | undefined,
) {
  const GiB = 1024 ** 3;
  const files: NonNullable<CacheSnapshot["files"]> = [
    {
      libraryKey: "pk-demo-games",
      library: "Games",
      ref: "blob:" + "a".repeat(64),
      name: "Arcade.iso",
      size: 15 * GiB,
      lastOpen: new Date(Date.now() - 3600000).toISOString(),
      inUse: false,
    },
    {
      libraryKey: "pk-demo-games",
      library: "Games",
      ref: "blob:" + "b".repeat(64),
      name: "Adventure.iso",
      size: 3 * GiB,
      lastOpen: new Date().toISOString(),
      inUse: true,
    },
  ];
  const releases: CacheSnapshot["releases"] = [
    {
      ReleaseID: "demo-arrival-1080",
      Title: "Arrival",
      Bytes: 5.3 * GiB,
      Size: 8 * GiB,
      Segments: 1400,
      Plays: 4,
      LastUsed: new Date(Date.now() - 1200000).toISOString(),
      LastPlayed: new Date().toISOString(),
      Pins: ["owner"],
      reason: {
        causes: [{ code: "playing", firstSeen: new Date(Date.now() - 3600000).toISOString() }],
        retention: { kind: "pinned", pins: ["owner"] },
        file: "Arrival.2016.1080p.mkv",
        source: "Arrival.2016.1080p.BluRay.x264.DTS-HD-SPARKS",
        indexer: "NZBgeek",
        origin: "Usenet",
        ratingKey: 100000,
        cachedAt: new Date(Date.now() - 3600000).toISOString(),
      },
    },
    {
      ReleaseID: "demo-severance-s02e03",
      Title: "Severance · S02E03 · Who Is Alive?",
      Bytes: 2.1 * GiB,
      Size: 3 * GiB,
      Segments: 540,
      Plays: 2,
      LastUsed: new Date(Date.now() - 3600000).toISOString(),
      LastPlayed: new Date().toISOString(),
      Pins: ["playback:inprogress"],
      reason: {
        causes: [
          { code: "unfinished", firstSeen: new Date(Date.now() - 7200000).toISOString() },
          { code: "next-episode", firstSeen: new Date(Date.now() - 7200000).toISOString() },
        ],
        retention: { kind: "pinned", pins: ["playback:inprogress"] },
        file: "Severance.S02E03.1080p.mkv",
        source: "Severance.S02E03.1080p.WEB-DL.DDP5.1.Atmos.H.265-FLUX",
        indexer: "Torrentio",
        origin: "Real-Debrid",
        ratingKey: 400000,
        cachedAt: new Date(Date.now() - 7200000).toISOString(),
      },
    },
    {
      ReleaseID: "demo-dune-2160",
      Title: "Dune: Part Two",
      Bytes: 7.8 * GiB,
      Size: 22 * GiB,
      Segments: 2000,
      Plays: 3,
      LastUsed: new Date(Date.now() - 7200000).toISOString(),
      LastPlayed: new Date(Date.now() - 86400000).toISOString(),
      Pins: ["preloaded"],
      reason: {
        causes: [
          {
            code: "shelf",
            firstSeen: new Date(Date.now() - 86400000).toISOString(),
            shelf: { reasonCodes: ["household-viewing"], score: 85, state: "fetched" },
          },
        ],
        retention: { kind: "pinned", pins: ["preloaded"] },
        file: "Dune.Part.Two.2024.2160p.mkv",
        source: "Dune.Part.Two.2024.2160p.UHD.BluRay.Remux.DV.HDR10.TrueHD.7.1.Atmos-FraMeSToR",
        indexer: "DrunkenSlug",
        origin: "Usenet",
        ratingKey: 100002,
        cachedAt: new Date(Date.now() - 86400000).toISOString(),
      },
    },
    {
      ReleaseID: "demo-interstellar-1080",
      Title: "Interstellar",
      Bytes: 32 * 1024 ** 2,
      Size: 12 * GiB,
      Segments: 8,
      Plays: 0,
      LastUsed: new Date(Date.now() - 100000).toISOString(),
      LastPlayed: "0001-01-01T00:00:00Z",
      Pins: [],
      reason: {
        causes: [{ code: "unrecorded" }],
        retention: {
          kind: "expiry",
          days: 30,
          until: new Date(Date.now() + 86400000).toISOString(),
        },
        file: "Interstellar.2014.1080p.mkv",
        source: "",
        origin: "TorBox",
        indexer: "Prowlarr",
      },
    },
    {
      ReleaseID: "bd959c0598e2818d872b8f0e2a1c9d44",
      Title: "bd959c0598e2818d872b8f0e2a1c9d44",
      Bytes: 10.2 * 1024 ** 2,
      Size: 0,
      Segments: 21,
      Plays: 0,
      LastUsed: new Date(Date.now() - 300000).toISOString(),
      LastPlayed: "0001-01-01T00:00:00Z",
      Pins: [],
      reason: {
        causes: [{ code: "quality-check", firstSeen: new Date(Date.now() - 300000).toISOString() }],
        retention: { kind: "lru" },
        file: "bd959c0598e2818d872b8f0e2a1c9d44.mkv",
        source: "The.Americans.2013.S03E01.1080p.BRRip.x264-HiQVE",
        origin: "Usenet",
        indexer: "NZBgeek",
        cachedAt: new Date(Date.now() - 300000).toISOString(),
      },
    },
  ];

  const details: Record<string, ReleaseDetail> = {
    "demo-arrival-1080": {
      releaseId: "demo-arrival-1080",
      segments: 1400,
      estimatedTotal: 2112,
      spanCount: 2,
      spans: [
        { start: 0, end: 1397 },
        { start: 2110, end: 2111 },
      ],
      coverage: Array.from({ length: 48 }, (_, i) => (i < 32 || i === 47 ? 1 : 0)),
      firstCached: new Date(Date.now() - 3600000).toISOString(),
    },
    "demo-severance-s02e03": {
      releaseId: "demo-severance-s02e03",
      segments: 540,
      estimatedTotal: 772,
      spanCount: 2,
      spans: [
        { start: 0, end: 31 },
        { start: 260, end: 767 },
      ],
      coverage: Array.from({ length: 48 }, (_, i) => (i < 2 || (i >= 16 && i < 47) ? 1 : 0)),
      firstCached: new Date(Date.now() - 7200000).toISOString(),
    },
    "demo-dune-2160": {
      releaseId: "demo-dune-2160",
      segments: 2000,
      estimatedTotal: 5640,
      spanCount: 1,
      spans: [{ start: 0, end: 1999 }],
      coverage: Array.from({ length: 48 }, (_, i) => (i < 17 ? 1 : 0)),
      firstCached: new Date(Date.now() - 86400000).toISOString(),
    },
    "demo-interstellar-1080": {
      releaseId: "demo-interstellar-1080",
      segments: 8,
      estimatedTotal: 3072,
      spanCount: 2,
      spans: [
        { start: 0, end: 5 },
        { start: 3070, end: 3071 },
      ],
      coverage: Array.from({ length: 48 }, (_, i) => (i === 0 || i === 47 ? 0.5 : 0)),
      firstCached: new Date(Date.now() - 100000).toISOString(),
    },
    bd959c0598e2818d872b8f0e2a1c9d44: {
      releaseId: "bd959c0598e2818d872b8f0e2a1c9d44",
      segments: 21,
      estimatedTotal: 21,
      spanCount: 1,
      spans: [{ start: 0, end: 20 }],
      coverage: Array.from({ length: 48 }, () => 1),
      firstCached: new Date(Date.now() - 300000).toISOString(),
    },
  };

  return (
    method: string,
    seg: string[],
    body: unknown,
    cfg: EtherConfig,
    clustered = false,
    includeFiles = false,
  ) => {
    if (seg[0] !== "cache") return undefined;
    if (method === "GET" && seg.length === 1)
      return {
        status: 200,
        body: {
          clustered,
          restartRequired,
          shared: cfg.cache.storage === "shared",
          used:
            releases.reduce((sum, r) => sum + r.Bytes, 0) +
            files.reduce((sum, f) => sum + f.size, 0),
          ...(includeFiles ? { files } : {}),
          budget: cfg.cache.budgetBytes,
          directory: cfg.cache.dir,
          protectedBytes: releases
            .filter((r) => r.Pins?.length)
            .reduce((sum, r) => sum + r.Bytes, 0),
          releases,
          preload: {
            state: "ready",
            message: "Shelf selection is ready; the next pass starts in 15 minutes",
            title: "",
            fetched: 0,
            size: 0,
            allocated: 9 * GiB,
            ready: 1,
            failed: 1,
            paused: false,
            items: [
              {
                key: 400001,
                title: "Severance · S02E04 · Woe's Hollow",
                releaseId: "demo-severance-s02e04",
                size: 3 * GiB,
                complete: true,
                state: "cached",
              },
              {
                key: 100001,
                title: "The Wild Robot",
                releaseId: "demo-wild-robot",
                size: 4 * GiB,
                complete: false,
                state: "queued",
              },
              {
                key: 100002,
                title: "Dune: Part Two",
                releaseId: "demo-dune-2160",
                size: 22 * GiB,
                complete: false,
                state: "failed",
              },
            ],
            viewers: 3,
            selection: [
              {
                key: 400001,
                title: "Severance · S02E04 · Woe's Hollow",
                type: "episode",
                score: 91,
                catalogue: 88,
                household: 96,
                reason:
                  "Recent viewing from 3 household viewers · next in a household viewing sequence",
                reasonCodes: ["household-viewing", "next-in-sequence"],
                reasonArgs: { viewers: 3 },
                state: "cached",
              },
              {
                key: 100001,
                title: "The Wild Robot",
                type: "movie",
                score: 85,
                catalogue: 80,
                household: 88,
                reason: "Recent viewing from 2 household viewers",
                reasonCodes: ["household-viewing"],
                reasonArgs: { viewers: 2 },
                state: "queued",
              },
              {
                key: 100002,
                title: "Dune: Part Two",
                type: "movie",
                score: 72,
                catalogue: 94,
                household: 60,
                reason: "Matches household genre interests",
                reasonCodes: ["household-genres"],
                state: "failed",
              },
            ],
          },
          metrics: { hits: 824, misses: 96, servedBytes: 42 * GiB, evictedBytes: 18 * GiB },
          prefetch: {
            queued: 1,
            running: 1,
            completed: 18,
            failed: 1,
            dropped: 0,
            current: { title: "Severance · S02E04 · Woe's Hollow", cause: "next-episode" },
          },
        } satisfies CacheSnapshot,
      };
    if (method === "GET" && seg[1] === "releases" && seg[2]) {
      const detail = details[decodeURIComponent(seg[2])];
      return detail ? { status: 200, body: detail } : { status: 404, body: null };
    }
    if (seg[1] === "warming") {
      if (method === "GET")
        return {
          status: 200,
          body: {
            titles: (cfg.cache.keepWarmTitles ?? []).map((key) => ({
              key,
              title: titleName?.(key) ?? "Arrival",
            })),
            speculative: cfg.cache.speculativeWarming !== "off",
          },
        };
      if (method === "POST") {
        const b = body as { key?: number; pinned?: boolean; speculative?: boolean };
        if (b.key && b.pinned !== undefined)
          cfg.cache.keepWarmTitles = b.pinned
            ? [...new Set([...(cfg.cache.keepWarmTitles ?? []), b.key])]
            : (cfg.cache.keepWarmTitles ?? []).filter((k) => k !== b.key);
        if (b.speculative !== undefined)
          cfg.cache.speculativeWarming = b.speculative ? "on" : "off";
        return { status: 204, body: null };
      }
    }
    if (method === "POST" && seg[1] === "clear-file") {
      const target = body as { libraryKey: string; ref: string };
      const index = files.findIndex(
        (file) => file.libraryKey === target.libraryKey && file.ref === target.ref,
      );
      if (index < 0) return { status: 404, body: null };
      if (files[index]!.inUse) return { status: 409, body: null };
      files.splice(index, 1);
      return { status: 204, body: null };
    }
    if (method === "POST" && seg[1] === "pin") {
      const b = body as { releaseId: string; pinned: boolean };
      const r = releases.find((r) => r.ReleaseID === b.releaseId);
      if (r)
        r.Pins = b.pinned
          ? [...new Set([...(r.Pins ?? []), "owner"])]
          : (r.Pins?.filter((p) => p !== "owner") ?? []);
      return { status: 204, body: null };
    }
    if (
      (method === "POST" && seg[1] === "clear") ||
      (method === "POST" && seg[1] === "clear-block")
    ) {
      const b = body as { releaseId: string };
      const idx = releases.findIndex((r) => r.ReleaseID === b.releaseId);
      if (idx >= 0) releases.splice(idx, 1);
      return {
        status: 200,
        body: { cleared: true, blocked: seg[1] === "clear-block", message: "Cleared" },
      };
    }
    if (method === "POST" && seg[1] === "preload") return { status: 202, body: null };
    if (method === "POST" && seg[1] === "warm") return { status: 202, body: null };
    return { status: 404, body: null };
  };
}

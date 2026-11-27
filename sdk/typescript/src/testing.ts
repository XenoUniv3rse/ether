import type { FolderListOptions } from "./host";
import type {
  ActionResponse,
  BadgeEntry,
  BadgeTitleRef,
  BadgeUsage,
  CandidateFacts,
  CatalogItem,
  CatalogResponse,
  ConfigureResponse,
  EtherEvent,
  EventCorrelation,
  EventResponse,
  FieldOption,
  FlowActionRequest,
  FlowActionResponse,
  Manifest,
  MaybePromise,
  MarkersResponse,
  MetaItem,
  MetaResponse,
  RawRequest,
  RawResponse,
  StreamResponse,
  SubtitlesResponse,
  TestResponse,
  ToolResponse,
  ViewResponse,
  ViewSpecDocument,
} from "./types.ts";
import {
  encodeText,
  HEADER_IDEMPOTENCY_KEY,
  MAX_KIND_VISIBLE_IDS,
  MaxBadgeEntriesPerCall,
  MaxBadgeLabelLen,
  MaxBadgeLabelsPerTitle,
  MaxFetchManyRequests,
  normalizeStreamUrlOptions,
  PosterBadgeQuotaTitles,
  type CatalogTitleSummary,
  type EtherHost,
  type AICompleteRequest,
  type AICompleteResponse,
  type MediaProbeResult,
  type FolderEntry,
  type HostFetchManyOptions,
  type HostFetchManyResult,
  type HostFetchRequest,
  type HostFetchResponse,
  type KVUsage,
  type ViewerWatchEntry,
} from "./host.ts";
import { plugin, type PluginDefinition, type PluginInstance } from "./router.ts";

export interface RecordedLog {
  level: string;
  message: string;
  attrs?: Record<string, unknown>;
}

const BADGE_SET_KEY_RE = /^[a-z][a-z0-9_]{0,31}$/;
const BADGE_LABEL_RE = /^[A-Z0-9 +&'./-]+$/;
const IMDB_ID_RE = /^tt[0-9]+$/;

function checkBadgeSetKey(setKey: string): void {
  if (typeof setKey !== "string" || !BADGE_SET_KEY_RE.test(setKey)) {
    throw new Error("badge set key must match ^[a-z][a-z0-9_]{0,31}$");
  }
}

function normalizeBadgeLabel(raw: string): string {
  if (typeof raw !== "string") {
    throw new Error("badge label must be a string");
  }
  let upper = "";
  for (let i = 0; i < raw.length; i++) {
    const code = raw.charCodeAt(i);
    upper += code >= 97 && code <= 122 ? String.fromCharCode(code - 32) : raw[i]!;
  }
  if (
    upper.length < 1 ||
    upper.length > MaxBadgeLabelLen ||
    upper.trim().length === 0 ||
    !BADGE_LABEL_RE.test(upper)
  ) {
    throw new Error(`badge label must be 1..${MaxBadgeLabelLen} characters from [A-Z0-9 +&'./-]`);
  }
  return upper;
}

function resolveBadgeTitleKey(
  title: BadgeTitleRef,
  catalog: Map<string, CatalogTitleSummary>,
): string {
  if (!title || (title.type !== "movie" && title.type !== "show")) {
    throw new Error('badge title.type must be "movie" or "show"');
  }
  let tmdb = 0;
  if (title.tmdb !== undefined) {
    if (!Number.isSafeInteger(title.tmdb) || title.tmdb <= 0) {
      throw new Error("badge title.tmdb must be a positive integer");
    }
    tmdb = title.tmdb;
  }
  let imdb = "";
  if (title.imdb !== undefined && title.imdb !== "") {
    const norm = String(title.imdb).trim().toLowerCase();
    if (!IMDB_ID_RE.test(norm)) {
      throw new Error("badge title.imdb must match tt<digits>");
    }
    imdb = norm;
  }
  if (tmdb === 0 && imdb === "") {
    throw new Error("badge title requires tmdb or imdb");
  }
  if (tmdb > 0) {
    for (const item of catalog.values()) {
      if (item.type === title.type && item.tmdbId === tmdb) {
        return String(item.id);
      }
    }
  }
  if (imdb !== "") {
    for (const item of catalog.values()) {
      if (item.type === title.type && item.imdbId?.toLowerCase() === imdb) {
        return String(item.id);
      }
    }
  }
  return tmdb > 0 ? `tmdb:${title.type}:${tmdb}` : `imdb:${title.type}:${imdb}`;
}

function countDistinctBadgeTitles(badges: Map<string, Map<string, string[]>>): number {
  const seen = new Set<string>();
  for (const setMap of badges.values()) {
    for (const titleKey of setMap.keys()) {
      seen.add(titleKey);
    }
  }
  return seen.size;
}

export interface RecordedPublishedEvent {
  key: string;
  payload: Record<string, unknown>;
  correlation?: EventCorrelation;
}

export interface MockHostState {
  kv: Map<string, string>;
  badges: Map<string, Map<string, string[]>>;
  badgeQuotaTitles: number;
  secrets: Map<string, string>;
  catalog: Map<string, CatalogTitleSummary>;
  history: Map<number, ViewerWatchEntry[]>;
  watchState: Map<string, boolean>;
  collections: Record<string, unknown>[];
  kinds: Map<string, import("./types.ts").KindItem[]>;
  /** External IDs hidden by mock library Rules, keyed by kind (#615). */
  kindHiddenIds: Map<string, Set<string>>;
  /** Optional custom predicate simulating library Rules + viewer policy (#615). */
  kindPolicyFilter?: (kind: string, item: import("./types.ts").KindItem) => boolean;
  sources: import("./types.ts").SourceCandidate[];
  /** Canned per-indexer status entries returned by kinds.resolveSourcesWithResult (#733). */
  sourceIndexers?: import("./types.ts").IndexerSearchStatus[];
  /** Optional custom handler for mock kinds.resolveSources / resolveSourcesWithResult (#733). */
  sourceHandler?: (
    q: import("./types.ts").SourceQuery,
  ) => import("./types.ts").SourceResolveResult | Promise<import("./types.ts").SourceResolveResult>;
  blobs: Map<string, Uint8Array>;
  limits: import("./types.ts").RuntimeLimits;
  folderRoots: string[];
  folders: Map<string, FolderEntry[]>;
  publishedEvents: RecordedPublishedEvent[];
  notifications: string[];
  logs: RecordedLog[];
  httpHandler: (req: HostFetchRequest) => HostFetchResponse | Promise<HostFetchResponse>;
  /** Optional handler for mock ai.complete calls; when omitted, throws an Error with code "not_configured". */
  aiHandler?: (req: AICompleteRequest) => AICompleteResponse | Promise<AICompleteResponse>;
  /** Optional handler for mock media.probe calls; when omitted, throws an Error with code "unavailable". */
  mediaProbeHandler?: (
    source: import("./types.ts").PlayableSource,
  ) => MediaProbeResult | Promise<MediaProbeResult>;
  /** The most recent frontend.streamUrl call: [viewerId, itemRef]. */
  lastStreamUrl?: [number, string];
  /** Zero-based part index of the most recent part-scoped frontend.streamUrl call. */
  lastStreamUrlPart?: number;
  /** PlayableSource.id of the most recent source-scoped frontend.streamUrl call (#661). */
  lastStreamUrlSource?: string;
  /** Whether the most recent frontend.streamUrl call requested an absolute URL (#772). */
  lastStreamUrlAbsolute?: boolean;
  lastStreamUrlWait?: import("./types.ts").StreamUrlOptions["wait"];
  /** Optional origin prepended to mock stream URLs when `{ absolute: true }` (defaults to `"http://ether.test"`). */
  streamOrigin?: string;
}

export interface MockHostClient extends EtherHost {
  readonly state: MockHostState;
}

export function createMockHostClient(initial?: Partial<MockHostState>): MockHostClient {
  const state: MockHostState = {
    kv: initial?.kv ?? new Map<string, string>(),
    badges: initial?.badges ?? new Map<string, Map<string, string[]>>(),
    badgeQuotaTitles: initial?.badgeQuotaTitles ?? PosterBadgeQuotaTitles,
    secrets: initial?.secrets ?? new Map<string, string>(),
    catalog: initial?.catalog ?? new Map<string, CatalogTitleSummary>(),
    history: initial?.history ?? new Map<number, ViewerWatchEntry[]>(),
    watchState: initial?.watchState ?? new Map<string, boolean>(),
    collections: initial?.collections ?? [],
    kinds: initial?.kinds ?? new Map(),
    kindHiddenIds: initial?.kindHiddenIds ?? new Map(),
    kindPolicyFilter: initial?.kindPolicyFilter,
    sources: initial?.sources ?? [],
    sourceIndexers: initial?.sourceIndexers,
    sourceHandler: initial?.sourceHandler,
    blobs: initial?.blobs ?? new Map(),
    limits: initial?.limits ?? {
      memoryBytes: 32 * 1024 * 1024,
      readBytes: 8 * 1024 * 1024,
      fetchBytes: 8 * 1024 * 1024 * 1024,
      archiveMemberBytes: 8 * 1024 * 1024 * 1024,
      archiveExpandedBytes: 32 * 1024 * 1024 * 1024,
      archiveMembers: 65535,
      deadline: "12h",
      poolSize: 2,
    },
    folderRoots: [...(initial?.folderRoots ?? [])],
    folders: initial?.folders ?? new Map<string, FolderEntry[]>(),
    publishedEvents: initial?.publishedEvents ?? [],
    notifications: initial?.notifications ?? [],
    logs: initial?.logs ?? [],
    httpHandler:
      initial?.httpHandler ??
      (() => ({
        status: 200,
        headers: { "Content-Type": "application/json" },
        body: "{}",
      })),
    aiHandler: initial?.aiHandler,
    mediaProbeHandler: initial?.mediaProbeHandler,
    streamOrigin: initial?.streamOrigin,
  };

  return {
    state,
    runtime: {
      limits: () => ({ ...state.limits }),
    },
    kv: {
      get: (key: string) => state.kv.get(key) ?? null,
      put: (key: string, value: string) => {
        state.kv.set(key, String(value));
      },
      delete: (key: string) => {
        state.kv.delete(key);
      },
      list: (prefix = "") =>
        Array.from(state.kv.keys())
          .filter((k) => k.startsWith(prefix))
          .sort(),
      usage: (): KVUsage => {
        let used = 0;
        for (const v of state.kv.values()) {
          used += encodeText(v).byteLength;
        }
        const keys = state.kv.size;
        return {
          bytes: used + keys * 256,
          keys,
          quotaBytes: 50 * 1024 * 1024,
          quotaKeys: 10000,
        };
      },
    },
    badges: {
      set: (setKey: string, entries: BadgeEntry[]) => {
        checkBadgeSetKey(setKey);
        if (!Array.isArray(entries)) {
          throw new Error("badges.set entries must be an array");
        }
        if (entries.length > MaxBadgeEntriesPerCall) {
          throw new Error(`badges.set accepts at most ${MaxBadgeEntriesPerCall} entries per call`);
        }
        const nextSet = new Map<string, string[]>(state.badges.get(setKey) ?? []);
        for (const entry of entries) {
          if (!entry || !Array.isArray(entry.labels)) {
            throw new Error("badge entry labels must be an array");
          }
          if (entry.labels.length > MaxBadgeLabelsPerTitle) {
            throw new Error(
              `badge entry may have at most ${MaxBadgeLabelsPerTitle} labels per title`,
            );
          }
          const titleKey = resolveBadgeTitleKey(entry.title, state.catalog);
          if (entry.labels.length === 0) {
            nextSet.delete(titleKey);
            continue;
          }
          const labels = Array.from(new Set(entry.labels.map(normalizeBadgeLabel)));
          nextSet.set(titleKey, labels);
        }
        const preview = new Map(state.badges);
        if (nextSet.size === 0) {
          preview.delete(setKey);
        } else {
          preview.set(setKey, nextSet);
        }
        const totalTitles = countDistinctBadgeTitles(preview);
        if (totalTitles > state.badgeQuotaTitles) {
          throw new Error(
            `poster badge quota exceeded (${totalTitles} > ${state.badgeQuotaTitles} titles)`,
          );
        }
        if (nextSet.size === 0) {
          state.badges.delete(setKey);
        } else {
          state.badges.set(setKey, nextSet);
        }
      },
      clear: (setKey: string) => {
        checkBadgeSetKey(setKey);
        state.badges.delete(setKey);
      },
      usage: (): BadgeUsage => ({
        titles: countDistinctBadgeTitles(state.badges),
        quotaTitles: state.badgeQuotaTitles,
      }),
    },
    secret: {
      get: (name: string) => state.secrets.get(name) ?? "",
      set: (name: string, value: string, _ttlSec?: number) => {
        if (value === "") state.secrets.delete(name);
        else state.secrets.set(name, value);
      },
      delete: (name: string) => {
        state.secrets.delete(name);
      },
    },
    http: {
      fetch: (req: HostFetchRequest) => state.httpHandler(req),
      fetchMany: (requests: HostFetchRequest[], opts?: HostFetchManyOptions) => {
        const list = requests ?? [];
        if (list.length > MaxFetchManyRequests) {
          throw new Error(
            `http.fetchMany accepts at most ${MaxFetchManyRequests} requests per call`,
          );
        }
        if (
          opts?.concurrency !== undefined &&
          (!Number.isInteger(opts.concurrency) || opts.concurrency < 0)
        ) {
          throw new Error("http.fetchMany concurrency must be a non-negative integer");
        }
        const toErr = (err: unknown): HostFetchManyResult => {
          const e = err as { code?: unknown; name?: unknown; message?: unknown } | null;
          const code =
            typeof e?.code === "string" && e.code
              ? e.code
              : e?.name === "TimeoutError" || e?.name === "AbortError"
                ? "timeout"
                : "host_error";
          return { error: { code, message: String(e?.message ?? err ?? "request failed") } };
        };
        const raw: Array<HostFetchManyResult | Promise<HostFetchManyResult>> = [];
        let hasPromise = false;
        for (const req of list) {
          try {
            const res = state.httpHandler(req);
            if (res && typeof (res as Promise<HostFetchResponse>).then === "function") {
              hasPromise = true;
              raw.push(
                (res as Promise<HostFetchResponse>).then(
                  (ok) => ok,
                  (err) => toErr(err),
                ),
              );
            } else {
              raw.push(res as HostFetchResponse);
            }
          } catch (err) {
            raw.push(toErr(err));
          }
        }
        return hasPromise ? Promise.all(raw) : (raw as HostFetchManyResult[]);
      },
    },
    catalog: {
      search: (query: string, limit = 50, offset = 0) => {
        const needle = query.toLowerCase();
        const matches = Array.from(state.catalog.values()).filter((item) =>
          item.title.toLowerCase().includes(needle),
        );
        return { titles: matches.slice(offset, offset + limit) };
      },
      get: (id: string) => state.catalog.get(id) ?? null,
    },
    viewers: {
      granted: () => Array.from(state.history.keys(), (id) => ({ id, name: String(id) })),
      history: (viewerId: number, limit = 50, offset = 0) => {
        const list = state.history.get(viewerId) ?? [];
        return list.slice(offset, offset + limit);
      },
      setWatchState: (viewerId: number, titleId: string, watched: boolean) => {
        state.watchState.set(`${viewerId}:${titleId}`, watched);
      },
    },
    collections: {
      upsert: (collection: Record<string, unknown>) => {
        state.collections.push(collection);
      },
    },
    folders: {
      roots: () => [...state.folderRoots],
      hashStart: () => {
        throw new Error("Set a folders.hashStart mock");
      },
      hashPoll: () => {
        throw new Error("Set a folders.hashPoll mock");
      },
      hashCancel: () => {
        throw new Error("Set a folders.hashCancel mock");
      },
      hash: () => {
        throw new Error("Set a folders.hash mock for file identification tests");
      },
      frame: () => {
        throw new Error("Set a folders.frame mock");
      },
      list: (root: string, path = "", { cursor = "", limit = 1000 }: FolderListOptions = {}) => {
        const key = path ? `${root}/${path}` : root;
        const max = limit > 0 ? Math.min(limit, 1000) : 1000;
        const sorted = (state.folders.get(key) ?? [])
          .filter((entry) => entry.name > cursor)
          .sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
        const entries = sorted.slice(0, max);
        const truncated = sorted.length > max;
        return { entries, truncated, nextCursor: truncated ? (entries.at(-1)?.name ?? "") : "" };
      },
      stat: (root: string, path: string) => {
        const slash = path.lastIndexOf("/");
        const parent = slash > 0 ? `${root}/${path.slice(0, slash)}` : root;
        const list = state.folders.get(parent) ?? state.folders.get(root) ?? [];
        const found = list.find((f) => f.path === path || f.name === path);
        return found ?? { name: path, path, dir: false, size: 0 };
      },
    },
    notify: {
      owner: (text: string) => {
        state.notifications.push(text);
      },
    },
    kinds: {
      upsert: (kind, items) => {
        const kept = new Map((state.kinds.get(kind) ?? []).map((it) => [it.id, it]));
        for (const it of items) kept.set(it.id, it);
        state.kinds.set(kind, [...kept.values()]);
        return items.length;
      },
      delete: (kind, externalIds) => {
        const drop = new Set(externalIds);
        state.kinds.set(
          kind,
          (state.kinds.get(kind) ?? []).filter((it) => !drop.has(it.id)),
        );
      },
      items: (kind, cursor = "", limit = 200) => {
        const all = state.kinds.get(kind) ?? [];
        const start = cursor ? all.findIndex((it) => it.id === cursor) + 1 : 0;
        const pageItems = all.slice(start, start + limit);
        const page: import("./types.ts").KindPage = { items: pageItems };
        if (pageItems.length && start + limit < all.length)
          page.nextCursor = pageItems[pageItems.length - 1]?.id;
        return page;
      },
      visible: (kind, ids, viewerToken) => {
        if (!viewerToken || !viewerToken.trim()) {
          throw new Error(
            "kinds.visible: no authenticated frontend adapter viewer token on this request",
          );
        }
        if (ids.length > MAX_KIND_VISIBLE_IDS) {
          throw new Error(`kinds.visible: batch larger than ${MAX_KIND_VISIBLE_IDS} ids`);
        }
        const byId = new Map<string, import("./types.ts").KindItem>();
        for (const it of state.kinds.get(kind) ?? []) byId.set(it.id, it);
        const hidden = state.kindHiddenIds.get(kind);
        const seen = new Set<string>();
        const out: string[] = [];
        for (const id of ids) {
          if (!id || seen.has(id)) continue;
          seen.add(id);
          const it = byId.get(id);
          if (!it) continue;
          if (hidden?.has(id)) continue;
          if (state.kindPolicyFilter && !state.kindPolicyFilter(kind, it)) continue;
          out.push(id);
        }
        return out;
      },
      browse: (kind, query = {}, viewerToken) => {
        if (!viewerToken || !viewerToken.trim()) {
          throw new Error(
            "kinds.browse: no authenticated frontend adapter viewer token on this request",
          );
        }
        if (
          query.total !== undefined &&
          query.total !== "cheap" &&
          query.total !== "none" &&
          query.total !== "exact"
        ) {
          throw new Error(`kinds.browse: invalid total mode ${String(query.total)}`);
        }
        const hidden = state.kindHiddenIds.get(kind);
        const needle = (query.query ?? "").trim().toLowerCase();
        const excludeNeedle = (query.excludeQuery ?? "").trim().toLowerCase();
        const limit = query.limit && query.limit > 0 ? query.limit : 200;
        let matched = (state.kinds.get(kind) ?? []).filter((it) => {
          if (hidden?.has(it.id)) return false;
          if (state.kindPolicyFilter && !state.kindPolicyFilter(kind, it)) return false;
          if (query.parentId && it.parentId !== query.parentId) return false;
          if (query.level && it.level !== query.level) return false;
          if (needle) {
            const hay = `${it.title ?? ""} ${it.summary ?? ""}`.toLowerCase();
            if (!hay.includes(needle)) return false;
          }
          if (excludeNeedle) {
            const hay = `${it.title ?? ""} ${it.summary ?? ""}`.toLowerCase();
            if (hay.includes(excludeNeedle)) return false;
          }
          const facetHave = (k: string): string[] => {
            const raw = it.fields?.[k];
            return Array.isArray(raw)
              ? raw.map((v) => String(v).toLowerCase())
              : raw != null
                ? [String(raw).toLowerCase()]
                : [];
          };
          if (query.facets) {
            for (const [k, want] of Object.entries(query.facets)) {
              if (!want?.length) continue;
              if (!want.some((w) => facetHave(k).includes(w.toLowerCase()))) return false;
            }
          }
          if (query.allFacets) {
            for (const [k, want] of Object.entries(query.allFacets)) {
              if (!want?.length) continue;
              if (!want.every((w) => facetHave(k).includes(w.toLowerCase()))) return false;
            }
          }
          if (query.excludeFacets) {
            for (const [k, want] of Object.entries(query.excludeFacets)) {
              if (!want?.length) continue;
              if (want.some((w) => facetHave(k).includes(w.toLowerCase()))) return false;
            }
          }
          if (query.anyGroups) {
            for (const g of query.anyGroups) {
              const alts: Array<{ field: string; values: string[] }> = [];
              if (g?.field?.trim() && g.values?.length) {
                alts.push({ field: g.field.trim(), values: g.values });
              }
              if (g?.any?.length) {
                for (const alt of g.any) {
                  if (alt?.field?.trim() && alt.values?.length) {
                    alts.push({ field: alt.field.trim(), values: alt.values });
                  }
                }
              }
              const active = alts
                .map((a) => ({
                  field: a.field,
                  values: a.values.map((w) => w.trim()).filter(Boolean),
                }))
                .filter((a) => a.values.length > 0);
              if (!active.length) continue;
              if (
                !active.some((a) =>
                  a.values.some((w) => facetHave(a.field).includes(w.toLowerCase())),
                )
              ) {
                return false;
              }
            }
          }
          if (query.empty) {
            for (const [k, wantEmpty] of Object.entries(query.empty)) {
              let hasVal = false;
              const raw = it.fields?.[k];
              if (raw !== undefined && raw !== null) {
                if (Array.isArray(raw)) {
                  hasVal = raw.some((v) => v !== null && v !== undefined && v !== "");
                } else if (typeof raw === "string") {
                  hasVal = raw !== "";
                } else {
                  hasVal = true;
                }
              } else {
                switch (k) {
                  case "released":
                    hasVal = Boolean(it.released);
                    break;
                  case "year":
                  case "decade":
                    hasVal = (it.year ?? 0) > 0;
                    break;
                  case "rating":
                    hasVal = (it.rating ?? 0) > 0;
                    break;
                  case "summary":
                    hasVal = Boolean(it.summary);
                    break;
                  case "recording":
                    hasVal = Boolean(it.recording);
                    break;
                  case "parentId":
                    hasVal = Boolean(it.parentId);
                    break;
                  case "index":
                    hasVal = (it.index ?? 0) > 0;
                    break;
                }
              }
              if (wantEmpty && hasVal) return false;
              if (!wantEmpty && !hasVal) return false;
            }
          }
          return true;
        });
        if (query.desc) matched = [...matched].reverse();
        const start = query.cursor ? matched.findIndex((it) => it.id === query.cursor) + 1 : 0;
        const pageItems = matched.slice(start, start + limit);
        const page: import("./types.ts").KindPage = { items: pageItems };
        if (pageItems.length && start + limit < matched.length) {
          page.nextCursor = pageItems[pageItems.length - 1]?.id;
        }
        if (query.total !== "none") {
          page.total = matched.length;
        }
        return page;
      },
      resolveSources: async (q) => {
        if (state.sourceHandler) {
          const r = await state.sourceHandler(q);
          return [...(r?.candidates ?? [])];
        }
        return [...state.sources];
      },
      resolveSourcesWithResult: async (q) => {
        if (state.sourceHandler) {
          const r = await state.sourceHandler(q);
          const out: import("./types.ts").SourceResolveResult = {
            candidates: [...(r?.candidates ?? [])],
          };
          if (r?.indexers !== undefined) out.indexers = [...r.indexers];
          if (r?.pending !== undefined) out.pending = [...r.pending];
          if (r?.searchId !== undefined) out.searchId = r.searchId;
          if (r?.done !== undefined) out.done = r.done;
          if (r?.enabled !== undefined) out.enabled = r.enabled;
          if (r?.cachedOnly !== undefined) out.cachedOnly = r.cachedOnly;
          return out;
        }
        const out: import("./types.ts").SourceResolveResult = { candidates: [...state.sources] };
        if (state.sourceIndexers !== undefined) out.indexers = [...state.sourceIndexers];
        return out;
      },
    },
    cache: {
      put: async (_libraryKey, _name, data) => {
        const digest = await globalThis.crypto.subtle.digest("SHA-256", data as BufferSource);
        const hex = [...new Uint8Array(digest)]
          .map((b) => b.toString(16).padStart(2, "0"))
          .join("");
        const ref = `blob:${hex}`;
        state.blobs.set(ref, new Uint8Array(data));
        return ref;
      },
      open: (blobRef) => {
        const b = state.blobs.get(blobRef);
        if (!b) throw new Error(`blob ${blobRef} not found`);
        return new Uint8Array(b);
      },
      read: (blobRef, range) => {
        const cap = state.limits.readBytes ?? 8 * 1024 * 1024;
        if (range.offset < 0 || range.length <= 0 || range.length > cap) {
          throw new Error(`Data range needs nonnegative offset and length 1..${cap}`);
        }
        const b = state.blobs.get(blobRef);
        if (!b) throw new Error(`blob ${blobRef} not found`);
        return b.slice(range.offset, range.offset + range.length);
      },
      delete: (blobRef) => {
        state.blobs.delete(blobRef);
      },
    },
    frontend: {
      streamUrl: (
        viewerId: number,
        itemRef: string,
        partIndexOrOpts?: number | import("./types.ts").StreamUrlOptions,
      ) => {
        const opts = normalizeStreamUrlOptions(partIndexOrOpts);
        state.lastStreamUrl = [viewerId, itemRef];
        state.lastStreamUrlPart = opts.partIndex;
        state.lastStreamUrlSource = opts.source;
        state.lastStreamUrlAbsolute = opts.absolute;
        state.lastStreamUrlWait = opts.wait;
        const base = `/ether/v1/kind-media/mock-${viewerId}`;
        const rel = opts.partIndex === undefined ? base : `${base}?part=${opts.partIndex}`;
        if (opts.absolute) {
          const origin = (state.streamOrigin ?? "http://ether.test").replace(/\/+$/, "");
          return `${origin}${rel}`;
        }
        return rel;
      },
    },
    events: {
      publish: (key: string, payload?: Record<string, unknown>, correlation?: EventCorrelation) => {
        state.publishedEvents.push({
          key,
          payload: payload ? { ...payload } : {},
          ...(correlation !== undefined ? { correlation: { ...correlation } } : {}),
        });
      },
    },
    ai: {
      complete: <T = unknown>(req: AICompleteRequest) => {
        if (!state.aiHandler) {
          const err = new Error("language model is not configured") as Error & { code?: string };
          err.code = "not_configured";
          throw err;
        }
        return state.aiHandler(req) as MaybePromise<AICompleteResponse<T>>;
      },
    },
    media: {
      probe: (source: import("./types.ts").PlayableSource) => {
        if (!state.mediaProbeHandler) {
          const err = new Error("media.probe is not configured in mock host") as Error & {
            code?: string;
          };
          err.code = "unavailable";
          throw err;
        }
        return state.mediaProbeHandler(source);
      },
    },
    log: (level: string, message: string, attrs?: Record<string, unknown>) => {
      state.logs.push({ level, message, attrs });
    },
    abort: (message = "aborted"): never => {
      throw new Error(
        "__ETHER_FAULT__" + JSON.stringify({ code: "aborted", message: String(message) }),
      );
    },
  };
}

export function assertValidViewSpec(doc: ViewSpecDocument): void {
  if (!doc || typeof doc !== "object") {
    throw new Error("ViewSpecDocument must be an object.");
  }
  if (doc.specVersion !== 1) {
    throw new Error(`ViewSpecDocument specVersion must be 1 (got ${String(doc.specVersion)}).`);
  }
  if (doc.layout !== "list" && doc.layout !== "detail" && doc.layout !== "bare") {
    throw new Error(
      `ViewSpecDocument layout must be list, detail, or bare (got ${String(doc.layout)}).`,
    );
  }
  if (!Array.isArray(doc.header)) {
    throw new Error("ViewSpecDocument header must be an array.");
  }
  if (!Array.isArray(doc.body)) {
    throw new Error("ViewSpecDocument body must be an array.");
  }
  assertNoClassNameFields(doc, "spec");
}

export const assertValidViewPage = assertValidViewSpec;

function assertNoClassNameFields(value: unknown, path: string): void {
  if (!value || typeof value !== "object") {
    return;
  }
  if (Array.isArray(value)) {
    value.forEach((entry, idx) => assertNoClassNameFields(entry, `${path}[${idx}]`));
    return;
  }
  for (const [k, v] of Object.entries(value)) {
    if (k === "className" || k.endsWith("ClassName")) {
      throw new Error(`ViewSpec forbids ${k} at ${path}.${k}.`);
    }
    assertNoClassNameFields(v, `${path}.${k}`);
  }
}

export const fixtures = {
  pickToken(itemId = "ether:movie:1"): string {
    return `pick-v1:${itemId}:sig-test`;
  },
  movie(overrides?: Partial<MetaItem>): MetaItem {
    return {
      id: "tt0133093",
      type: "movie",
      name: "The Matrix",
      releaseInfo: "1999",
      description: "A computer hacker learns about the true nature of reality.",
      genres: ["Action", "Sci-Fi"],
      imdbRating: "8.7",
      ...overrides,
    };
  },
  series(overrides?: Partial<MetaItem>): MetaItem {
    return {
      id: "tt0903747",
      type: "series",
      name: "Breaking Bad",
      releaseInfo: "2008",
      description:
        "A chemistry teacher diagnosed with inoperable lung cancer turns to manufacturing drugs.",
      genres: ["Crime", "Drama", "Thriller"],
      imdbRating: "9.5",
      ...overrides,
    };
  },
  catalogItem(overrides?: Partial<CatalogItem>): CatalogItem {
    return {
      id: "tt0133093",
      type: "movie",
      name: "The Matrix",
      releaseInfo: "1999",
      ...overrides,
    };
  },
  playbackEvent(
    name: EtherEvent["name"] = "playback.completed",
    overrides?: Partial<EtherEvent>,
  ): EtherEvent {
    return {
      id: "evt_01",
      name,
      at: "2026-11-02T12:00:00Z",
      viewerId: 1,
      payload: {
        titleId: "tt0133093",
        title: "The Matrix",
        progress: 0.95,
      },
      ...overrides,
    };
  },
  candidate(overrides?: Partial<CandidateFacts>): CandidateFacts {
    return {
      releaseId: "rel_01",
      name: "The.Matrix.1999.2160p.UHD.BluRay.x265",
      resolution: 2160,
      hdr: ["DV", "HDR10"],
      videoCodec: "hevc",
      cached: true,
      verified: true,
      ...overrides,
    };
  },
};

export interface TestHarness<S = Record<string, unknown>> {
  readonly instance: PluginInstance<S>;
  readonly host: MockHostClient;
  request(req: RawRequest): Promise<RawResponse>;
  requestHosted(req: RawRequest): MaybePromise<RawResponse>;
  manifest(): Promise<Manifest>;
  health(): Promise<{ status: number; body: unknown }>;
  configure(
    settings: Record<string, unknown>,
  ): Promise<{ status: number; body: ConfigureResponse }>;
  test(): Promise<{ status: number; body: TestResponse }>;
  options(field: string): Promise<{ status: number; body: FieldOption[] }>;
  catalog(
    type: string,
    id: string,
    extra?: Record<string, string>,
  ): Promise<{ status: number; body: CatalogResponse }>;
  meta(type: string, id: string): Promise<{ status: number; body: MetaResponse }>;
  stream(
    type: string,
    id: string,
    extra?: Record<string, string>,
  ): Promise<{ status: number; body: StreamResponse }>;
  subtitles(
    type: string,
    id: string,
    pickToken?: string,
    extra?: Record<string, string>,
  ): Promise<{ status: number; body: SubtitlesResponse }>;
  markers(type: string, id: string): Promise<{ status: number; body: MarkersResponse }>;
  event(events: EtherEvent[]): Promise<{ status: number; body: EventResponse }>;
  flowAction(
    key: string,
    req: Omit<FlowActionRequest, "key">,
  ): Promise<{ status: number; body: FlowActionResponse }>;
  view(
    page?: string,
    query?: Record<string, string>,
  ): Promise<{ status: number; body: ViewResponse }>;
  action(
    action: string,
    input?: Record<string, unknown>,
  ): Promise<{ status: number; body: ActionResponse }>;
  tool(
    name: string,
    input?: Record<string, unknown>,
  ): Promise<{ status: number; body: ToolResponse }>;
  rankKey(candidate: CandidateFacts, config?: S): number;
  condMatch(candidate: CandidateFacts, operator: string, value: unknown): boolean;
  file(path: string, range?: string): Promise<RawResponse>;
}

function encodeExtraPath(extra?: Record<string, string>): string {
  if (!extra || Object.keys(extra).length === 0) {
    return "";
  }
  const params = new URLSearchParams(extra);
  return `/${params.toString()}`;
}

export function createTestHarness<S = Record<string, unknown>>(
  defOrInstance: PluginDefinition<S> | PluginInstance<S>,
  mockHost?: MockHostClient,
): TestHarness<S> {
  const host = mockHost ?? createMockHostClient();
  const instance: PluginInstance<S> =
    "dispatch" in defOrInstance && typeof defOrInstance.dispatch === "function"
      ? defOrInstance
      : plugin<S>({
          ...(defOrInstance as PluginDefinition<S>),
          host: (defOrInstance as PluginDefinition<S>).host ?? host,
        });

  return {
    instance,
    host,
    request: (req) => instance.dispatch(req),
    requestHosted: (req) => instance.handle(req),
    async manifest() {
      const res = await instance.dispatch({ method: "GET", path: "/manifest.json" });
      return res.body as Manifest;
    },
    async health() {
      const res = await instance.dispatch({ method: "GET", path: "/ether/health" });
      return { status: res.status, body: res.body };
    },
    async configure(settings) {
      const res = await instance.dispatch({
        method: "POST",
        path: "/ether/configure",
        body: JSON.stringify(settings),
      });
      return { status: res.status, body: res.body as ConfigureResponse };
    },
    async test() {
      const res = await instance.dispatch({ method: "POST", path: "/ether/test" });
      return { status: res.status, body: res.body as TestResponse };
    },
    async options(field) {
      const res = await instance.dispatch({
        method: "GET",
        path: `/ether/options/${encodeURIComponent(field)}`,
      });
      return { status: res.status, body: res.body as FieldOption[] };
    },
    async catalog(type, id, extra) {
      const suffix = encodeExtraPath(extra);
      const res = await instance.dispatch({
        method: "GET",
        path: `/catalog/${encodeURIComponent(type)}/${encodeURIComponent(id)}${suffix}.json`,
      });
      return { status: res.status, body: res.body as CatalogResponse };
    },
    async meta(type, id) {
      const res = await instance.dispatch({
        method: "GET",
        path: `/meta/${encodeURIComponent(type)}/${encodeURIComponent(id)}.json`,
      });
      return { status: res.status, body: res.body as MetaResponse };
    },
    async stream(type, id, extra) {
      const suffix = encodeExtraPath(extra);
      const res = await instance.dispatch({
        method: "GET",
        path: `/stream/${encodeURIComponent(type)}/${encodeURIComponent(id)}${suffix}.json`,
      });
      return { status: res.status, body: res.body as StreamResponse };
    },
    async subtitles(type, id, pickToken, extra) {
      const suffix = encodeExtraPath(extra);
      const headers: Record<string, string> = {};
      if (pickToken !== undefined) {
        headers["X-Ether-Pick"] = pickToken;
      }
      const res = await instance.dispatch({
        method: "GET",
        path: `/subtitles/${encodeURIComponent(type)}/${encodeURIComponent(id)}${suffix}.json`,
        headers,
      });
      return { status: res.status, body: res.body as SubtitlesResponse };
    },
    async markers(type, id) {
      const res = await instance.dispatch({
        method: "GET",
        path: `/ether/markers/${encodeURIComponent(type)}/${encodeURIComponent(id)}.json`,
      });
      return { status: res.status, body: res.body as MarkersResponse };
    },
    async event(events) {
      const res = await instance.dispatch({
        method: "POST",
        path: "/ether/event",
        body: JSON.stringify({ events }),
      });
      return { status: res.status, body: res.body as EventResponse };
    },
    async flowAction(key, req) {
      const decl = instance.manifest.ether?.contributes?.flowActions?.find((a) => a.key === key);
      const path = decl?.route ?? `/ether/v1/flow-actions/${encodeURIComponent(key)}`;
      const headers: Record<string, string> = {};
      if (req.idempotencyKey) {
        headers[HEADER_IDEMPOTENCY_KEY] = req.idempotencyKey;
      }
      const res = await instance.dispatch({
        method: "POST",
        path,
        headers,
        body: JSON.stringify(req),
      });
      return { status: res.status, body: res.body as FlowActionResponse };
    },
    async view(page = "home", query) {
      const cleanPage = page.replace(/^\/+/, "") || "home";
      const res = await instance.dispatch({
        method: "GET",
        path: `/ether/view/${encodeURIComponent(cleanPage)}`,
        query,
      });
      if (res.status === 200) {
        const viewRes = res.body as ViewResponse;
        assertValidViewSpec(viewRes.spec);
      }
      return { status: res.status, body: res.body as ViewResponse };
    },
    async action(action, input = {}) {
      const res = await instance.dispatch({
        method: "POST",
        path: `/ether/action/${encodeURIComponent(action)}`,
        body: JSON.stringify(input),
      });
      return { status: res.status, body: res.body as ActionResponse };
    },
    async tool(name, input = {}) {
      const res = await instance.dispatch({
        method: "POST",
        path: `/ether/tool/${encodeURIComponent(name)}`,
        body: JSON.stringify(input),
      });
      return { status: res.status, body: res.body as ToolResponse };
    },
    rankKey: (candidate, config) => instance.rank_key(candidate, config),
    condMatch: (candidate, operator, value) => instance.cond_match(candidate, operator, value),
    async file(path, range) {
      const headers: Record<string, string> = {};
      if (range) {
        headers.Range = range;
      }
      return instance.dispatch({
        method: "GET",
        path: `/ether/file/${path.replace(/^\/+/, "")}`,
        headers,
      });
    },
  };
}

import { apiFetch } from "../../lib/api";
import type { Rule, TitleCard } from "../../lib/contract";
import type { VisibilityRule } from "./browser/LibraryVisibility";

/** Owner API shapes for /libraries and /plugins-setup (spec §11). */

export type LibraryCardShape = "poster" | "landscape" | "square";

export type MediaClass = "video" | "music" | "audiobook" | "book" | "podcast" | "photo";

export type MediaRole =
  | "item"
  | "artist"
  | "album"
  | "track"
  | "series"
  | "book"
  | "audiobook"
  | "part"
  | "show"
  | "episode"
  | "collection";

export type PresentationAppKey = "vela" | "plex" | "mediaBrowser" | "stremio" | "xtream" | "silo";

export type PresentationSource = "plugin" | "inferred" | "unavailable";

export interface Presentations {
  adapterOnly?: boolean;
  vela?: { type?: string; default?: boolean };
  plex?: { base?: string; default?: boolean };
  mediaBrowser?: {
    collectionType?: string;
    itemTypes?: Record<string, string>;
    default?: boolean;
  };
  stremio?: { type?: string; default?: boolean };
  xtream?: { category?: string; default?: boolean };
  silo?: { type?: string; default?: boolean };
}

export interface ResolvedAppPresentation {
  app: PresentationAppKey;
  available: boolean;
  enabled: boolean;
  defaultOn: boolean;
  overridden?: boolean;
  source: PresentationSource;
  hintValue?: string;
  reason?: string;
}

export interface ResolvedPresentations {
  mediaClass: MediaClass;
  adapterOnly?: boolean;
  effective: Presentations;
  available: Presentations;
  apps: ResolvedAppPresentation[];
}

export interface AppSwitchPatch {
  adapterOnly?: boolean;
  vela?: boolean;
  plex?: boolean;
  mediaBrowser?: boolean;
  stremio?: boolean;
  xtream?: boolean;
  silo?: boolean;
}

export type LibraryTabKind = "recommended" | "library" | "collections" | "people" | "view" | "live";

export type LibraryTabLayout = "grid" | "list" | "timeline";

export type LibraryFacetCategory = "essentials" | "people" | "technical" | "plugin";

export interface LibraryFacetOption {
  id: string;
  label: string;
  category?: LibraryFacetCategory;
}

export interface LibraryAvailableFacets {
  collections: LibraryFacetOption[];
  people: LibraryFacetOption[];
}

export interface LibraryTabFilter {
  match?: "all" | "any";
  rules: Rule[];
}

export interface LibraryTab {
  key: string;
  label: string;
  kind: LibraryTabKind;
  level?: string;
  facet?: string;
  order?: "count" | "name";
  sort?: string;
  direction?: "asc" | "desc";
  card?: LibraryCardShape;
  layout?: LibraryTabLayout;
  search?: boolean;
  searchPlaceholder?: string;
  filters?: LibraryTabFilter;
  hidden?: boolean;
}

export interface LibraryTabsResponse {
  libraryId: number;
  customized: boolean;
  tabs: LibraryTab[];
  effectiveTabs: LibraryTab[];
  defaults: LibraryTab[];
  scopeNote: string;
  availableFacets: LibraryAvailableFacets;
  availableSorts: string[];
}

export interface LibraryTabPreviewItem {
  key?: string | number;
  title: string;
  year?: number;
  poster?: string;
  posterPath?: string;
  art?: { role: string; url: string; width?: number; height?: number }[];
}

export interface LibraryTabPreviewCollection {
  id: string;
  bucketId?: string;
  name: string;
  itemCount: number;
  newestItem?: TitleCard | KindItem;
}

export interface LibraryTabPreviewPerson {
  id: number | string;
  name: string;
  itemCount: number;
  thumb?: string;
}

export type LibraryTabPreviewResponse =
  | {
      kind: "recommended";
      scopeNote: string;
    }
  | {
      kind: "library" | "view";
      sort: string;
      direction: string;
      total: number;
      items: LibraryTabPreviewItem[];
    }
  | {
      kind: "collections";
      facet: string;
      order: string;
      collections: LibraryTabPreviewCollection[];
      more: boolean;
    }
  | {
      kind: "people";
      facet: string;
      order: string;
      people: LibraryTabPreviewPerson[];
      more: boolean;
    }
  | {
      kind: "live";
      card: LibraryCardShape;
      scopeNote: string;
    };

export interface LibraryLiveItem {
  id: string;
  title: string;
  subtitle?: string;
  badge?: string;
  viewers?: number;
  tags?: string[];
  thumb?: string;
  live: boolean;
}

export interface LibraryLiveFilter {
  key: string;
  label: string;
}

export interface LibraryLivePage {
  items: LibraryLiveItem[];
  filters?: LibraryLiveFilter[];
  nextCursor?: string;
}

export interface LibraryLiveSession {
  id: string;
  title?: string;
  playlist: string;
}

export interface KindLevel {
  key: string;
  label: string;
  role?: MediaRole;
  playable?: boolean;
  container?: boolean;
}
export interface KindField {
  key: string;
  type: string;
  label: string;
  indexed?: boolean;
  enum?: string[];
  values?: string[];
}
export interface MediaKind {
  key: string;
  label: string;
  media?: MediaClass;
  library: {
    name: string;
    icon?: string;
    card?: LibraryCardShape;
    dateDisplay?: "year" | "date";
  };
  levels: KindLevel[];
  fields?: KindField[];
  art?: { role: string; label: string }[];
  present?: Presentations;
  browse: { sorts?: string[]; facets?: string[]; search: boolean; searchFields?: string[] };
  sync: { mode: string; refresh?: string };
  cache?: { maxBytes?: number; ttl?: string };
  match?: { thresholdDefault?: number };
}

export interface LibrarySyncDeleteGuard {
  source: "walk" | "changes" | string;
  missing: number;
  total: number;
  protected?: number;
}

export interface LibrarySyncOutcome {
  mode: string;
  upserted?: number;
  deleted?: number;
  skipped?: number;
  protected?: number;
  total: number;
  summary: string;
  finishedAt?: string;
}

export interface LibrarySync {
  pluginId: string;
  kindKey: string;
  generation: number;
  lastStarted?: string;
  lastOk?: string;
  lastError?: string;
  deleteGuard?: LibrarySyncDeleteGuard;
  lastOutcome?: LibrarySyncOutcome;
  items: number;
  running?: boolean;
  phase?: string;
  syncedItems?: number;
  pages?: number;
}

export interface LibrarySyncRequest {
  fullWalk?: boolean;
  force?: boolean;
  keepExisting?: boolean;
}

export interface LibrarySyncResponse {
  started?: boolean;
  alreadyRunning?: boolean;
  running?: boolean;
  kept?: boolean;
  upserted?: number;
  deleted?: number;
  skipped?: number;
  protected?: number;
  sync?: LibrarySync;
}

export interface CatalogueSource {
  logo?: string;
  lastError?: string;
  id: string;
  name: string;
  owner: string;
  group: "builtin" | "plugin" | "stremio";
  mediaTypes: string[];
  health: string;
  enabled: boolean;
  itemCount?: number;
  /** Items the last sync read from the list. */
  listed?: number;
  /** Most items one sync reads (the plugin's "Titles per list"). */
  limit?: number;
  pluginId?: string;
  kindKey?: string;
  libraries: { id: number; name: string }[];
}

/** One catalogue-source sync: titles synced, items read and the most it could read. */
export interface CatalogueSourceSyncResult {
  ok: boolean;
  itemCount: number;
  listed: number;
  limit: number;
}

export interface Library {
  rulesBroken?: boolean;
  id: number;
  key: string;
  name: string;
  sources?: string[];
  source: "builtin" | "plugin" | "catalogue" | "collection";
  mediaType?: string;
  metadataSource?: string;
  pluginId?: string;
  kindKey?: string;
  /** Collection-source libraries: the collection whose titles it shows. */
  collectionId?: number;
  rules?: VisibilityRule[];
  visibilityDefault?: string;
  /** Owner card-shape choice ("poster" | "landscape" | "square"); omitted/"" = default. */
  cardShape?: "" | LibraryCardShape;
  present?: Presentations;
  /** Server-resolved per-app presentations separating HOW (plugin/inferred/unavailable) from WHETHER (owner switches). */
  presentations: ResolvedPresentations;
  hasVideo: boolean;
  logo?: string;
  tabs?: LibraryTab[];
  tabsCustomized?: boolean;
  effectiveTabs?: LibraryTab[];
  sort: number;
  hidden: boolean;
  createdAt: string;
  updatedAt: string;
  itemCount?: number;
  storedItems?: number;
  pluginItems?: number;
  maxPluginItems?: number;
  sync?: LibrarySync;
  kind?: MediaKind;
  pluginName?: string;
}

export interface KindItem {
  unavailable?: { code: string; detail?: string };
  ref: string;
  key: number;
  id: string;
  level: string;
  parentId?: string;
  index?: number;
  title: string;
  sortTitle?: string;
  summary?: string;
  year?: number;
  released?: number;
  rating?: number;
  playable?: boolean;
  childCount?: number;
  leafCount?: number;
  art?: { role: string; url: string; width?: number; height?: number }[];
  fields?: Record<string, unknown>;
  etag?: string;
}

export interface MatchRecord {
  itemRef: string;
  candidateRef: string;
  score: number;
  reasons?: string[];
  source: string;
  chosen: boolean;
  updatedAt: number;
}

export interface LibraryCache {
  libraryId: number;
  used: number;
  blobs: number;
  maxBytes: number;
  ttlSeconds: number;
  sharedUsed?: number;
  sharedBlobs?: number;
  sharedCap?: number;
}

export interface SetupStep {
  key: string;
  label: string;
  hint?: string;
  fields: string[];
  test: string;
  required: boolean;
  status: "pending" | "ok" | "failed" | "skipped";
  detail?: string;
}

export interface PluginSetup {
  pluginId: string;
  pluginName: string;
  steps: SetupStep[];
  blockers: string[];
  settings: Record<string, unknown>;
  schema: unknown;
  kinds: MediaKind[];
  libraries: Library[];
}

/** One collection the "From a collection" picker can bind a library to. */
export interface CollectionSource {
  id: number;
  name: string;
  mediaType: "movie" | "show" | "mixed";
  itemCount: number;
  builtIn?: boolean;
}

export interface PluginLibrarySource {
  pluginId: string;
  pluginName: string;
  summary?: string;
  kinds: MediaKind[];
  libraries: Library[];
}

export const librariesApi = {
  syncCatalogueSource: (id: string) =>
    apiFetch<CatalogueSourceSyncResult>(
      `/libraries/catalogue-sources/${encodeURIComponent(id)}/sync`,
      {
        method: "POST",
      },
    ),
  catalogueSources: (signal?: AbortSignal, options?: { includeDisabled: boolean }) =>
    apiFetch<{ sources: CatalogueSource[] }>(
      options?.includeDisabled
        ? "/libraries/catalogue-sources?includeDisabled=true"
        : "/libraries/catalogue-sources",
      { signal },
    ).then((r) => r.sources),
  list: (signal?: AbortSignal) =>
    apiFetch<{ libraries: Library[] }>("/libraries", { signal }).then((r) => r.libraries),
  get: (id: number, signal?: AbortSignal) => apiFetch<Library>(`/libraries/${id}`, { signal }),
  tabs: (id: number, signal?: AbortSignal) =>
    apiFetch<LibraryTabsResponse>(`/libraries/${id}/tabs`, { signal }),
  putTabs: (id: number, tabs: LibraryTab[]) =>
    apiFetch<LibraryTabsResponse>(`/libraries/${id}/tabs`, {
      method: "PUT",
      body: JSON.stringify({ tabs }),
    }),
  resetTabs: (id: number) =>
    apiFetch<LibraryTabsResponse>(`/libraries/${id}/tabs`, { method: "DELETE" }),
  previewTab: (
    id: number,
    body: { tab: LibraryTab; query?: string; limit?: number },
    signal?: AbortSignal,
  ) =>
    apiFetch<LibraryTabPreviewResponse>(`/libraries/${id}/tabs/preview`, {
      method: "POST",
      body: JSON.stringify(body),
      signal,
    }),
  create: (
    name: string,
    mediaType: string,
    rules: VisibilityRule[],
    visibilityDefault: string,
    present?: Presentations,
    sources?: string[],
    metadataSource?: string,
  ) =>
    apiFetch<Library>("/libraries", {
      method: "POST",
      body: JSON.stringify({
        name,
        mediaType,
        rules,
        visibilityDefault,
        sources,
        metadataSource,
        ...(present ? { present } : {}),
      }),
    }),
  createCollection: (
    name: string,
    collectionId: number,
    mediaType: string,
    present?: Presentations,
    sources?: string[],
    metadataSource?: string,
  ) =>
    apiFetch<Library>("/libraries", {
      method: "POST",
      body: JSON.stringify({
        name,
        collectionId,
        mediaType,
        sources,
        metadataSource,
        ...(present ? { present } : {}),
      }),
    }),
  listCollectionSources: (signal?: AbortSignal) =>
    apiFetch<{ collections: CollectionSource[] }>("/libraries/collection-sources", { signal }).then(
      (r) => r.collections ?? [],
    ),
  patch: (id: number, body: Record<string, unknown>) =>
    apiFetch<Library>(`/libraries/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  remove: (id: number) => apiFetch<void>(`/libraries/${id}`, { method: "DELETE" }),
  items: (id: number, params: Record<string, string>, signal?: AbortSignal) =>
    apiFetch<{ items: KindItem[]; total: number; nextCursor?: string }>(
      `/libraries/${id}/items?${new URLSearchParams(params)}`,
      { signal },
    ),
  live: (id: number, params: Record<string, string>, signal?: AbortSignal) =>
    apiFetch<LibraryLivePage>(`/libraries/${id}/live?${new URLSearchParams(params)}`, { signal }),
  playLive: (id: number, itemId: string, tab?: string, hevc = false, hevc10 = false) => {
    const q = new URLSearchParams();
    if (hevc) q.set("hevc", "1");
    if (hevc10) q.set("hevc10", "1");
    const suffix = q.size ? `?${q}` : "";
    return apiFetch<LibraryLiveSession>(`/libraries/${id}/live/play${suffix}`, {
      method: "POST",
      body: JSON.stringify({ itemId, ...(tab ? { tab } : {}) }),
    });
  },
  stopLive: (id: number, sessionId: string) =>
    apiFetch<{ ok: boolean }>(`/libraries/${id}/live/sessions/${encodeURIComponent(sessionId)}`, {
      method: "DELETE",
    }),
  facets: (id: number, field: string, level = "", signal?: AbortSignal) =>
    apiFetch<{ field: string; values: { value?: string; title?: string; count: number }[] }>(
      `/libraries/${id}/facets/${encodeURIComponent(field)}${level ? `?level=${encodeURIComponent(level)}` : ""}`,
      { signal },
    ),
  fieldValues: (
    id: number,
    field: string,
    opts: {
      q?: string;
      offset?: number;
      limit?: number;
      order?: "count" | "name";
      declared?: boolean;
      level?: string;
    } = {},
    signal?: AbortSignal,
  ) => {
    const p = new URLSearchParams();
    if (opts.q) p.set("q", opts.q);
    if (opts.offset && opts.offset > 0) p.set("offset", String(opts.offset));
    if (opts.limit && opts.limit > 0) p.set("limit", String(opts.limit));
    if (opts.order) p.set("order", opts.order);
    if (opts.declared) p.set("declared", "1");
    if (opts.level) p.set("level", opts.level);
    const qs = p.toString();
    return apiFetch<{
      field: string;
      values: { value: string; count: number }[];
      more?: boolean;
      truncated?: boolean;
    }>(`/libraries/${id}/facets/${encodeURIComponent(field)}${qs ? `?${qs}` : ""}`, { signal });
  },
  item: (id: number, ref: string, signal?: AbortSignal) =>
    apiFetch<{
      item: KindItem;
      children: KindItem[];
      matches: MatchRecord[];
      kind?: MediaKind;
      resolution?: Record<string, unknown>;
    }>(`/libraries/${id}/item/${encodeURIComponent(ref)}`, { signal }),
  sync: (id: number, opts: boolean | LibrarySyncRequest = false) => {
    const body: LibrarySyncRequest = typeof opts === "boolean" ? { fullWalk: opts } : opts;
    return apiFetch<LibrarySyncResponse>(`/libraries/${id}/sync`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  },
  pinMatch: (id: number, ref: string, candidateRef: string) =>
    apiFetch<{ matches: MatchRecord[] }>(`/libraries/${id}/item/${encodeURIComponent(ref)}/match`, {
      method: "PUT",
      body: JSON.stringify({ candidateRef }),
    }),
  clearMatch: (id: number, ref: string) =>
    apiFetch<{ matches: MatchRecord[] }>(`/libraries/${id}/item/${encodeURIComponent(ref)}/match`, {
      method: "DELETE",
    }),
  rematch: (id: number, ref: string) =>
    apiFetch<{ matches: MatchRecord[] }>(`/libraries/${id}/item/${encodeURIComponent(ref)}/match`, {
      method: "POST",
    }),
  cache: (id: number, signal?: AbortSignal) =>
    apiFetch<LibraryCache>(`/libraries/${id}/cache`, { signal }),
  setCache: (id: number, maxBytes: number, ttlSeconds: number) =>
    apiFetch<LibraryCache>(`/libraries/${id}/cache`, {
      method: "PUT",
      body: JSON.stringify({ maxBytes, ttlSeconds }),
    }),
  sweepCache: (id: number) =>
    apiFetch<LibraryCache>(`/libraries/${id}/cache/sweep`, { method: "POST" }),
  purgeCache: (id: number) =>
    apiFetch<LibraryCache>(`/libraries/${id}/cache/purge`, { method: "POST" }),
  listPluginSources: (signal?: AbortSignal) =>
    apiFetch<{ plugins: PluginLibrarySource[] }>("/plugins-setup", { signal }).then(
      (r) => r.plugins ?? [],
    ),
  ensurePluginSetup: (plugin: string, kind?: string) =>
    apiFetch<PluginSetup>(
      `/plugins-setup/${encodeURIComponent(plugin)}/ensure${kind ? "?kind=" + encodeURIComponent(kind) : ""}`,
      {
        method: "POST",
      },
    ),
  setup: (plugin: string, signal?: AbortSignal) =>
    apiFetch<PluginSetup>(`/plugins-setup/${encodeURIComponent(plugin)}`, { signal }),
  librarySetup: (id: number, signal?: AbortSignal) =>
    apiFetch<PluginSetup>(`/libraries/${id}/setup`, { signal }),
  saveSetupSettings: (plugin: string, values: Record<string, unknown>) =>
    apiFetch<{ fieldErrors: Record<string, string> | null }>(
      `/plugins-setup/${encodeURIComponent(plugin)}/settings`,
      { method: "PUT", body: JSON.stringify(values) },
    ),
  testSetupStep: (plugin: string, step: string) =>
    apiFetch<{ ok: boolean; detail?: string; fields?: Record<string, string> }>(
      `/plugins-setup/${encodeURIComponent(plugin)}/${encodeURIComponent(step)}/test`,
      { method: "POST" },
    ),
  skipSetupStep: (plugin: string, step: string) =>
    apiFetch<{ ok: boolean }>(
      `/plugins-setup/${encodeURIComponent(plugin)}/${encodeURIComponent(step)}/skip`,
      { method: "POST" },
    ),
};

/** Protocols a library is published to, for list-card badges. */
export function publishedTo(lib: Library): string[] {
  if (lib.hidden) return [];
  if (lib.source === "builtin")
    return ["vela", "plex", "mediaBrowser", "stremio", "xtream", "silo"];
  const p = lib.present ?? {};
  const out: string[] = [];
  if (p.adapterOnly) out.push("adapter");
  if (p.vela) out.push("vela");
  if (p.plex) out.push("plex");
  if (p.mediaBrowser) out.push("mediaBrowser");
  if (p.stremio) out.push("stremio");
  if (p.xtream) out.push("xtream");
  if (p.silo) out.push("silo");
  return out;
}

export function artUrl(item: KindItem, role = "poster"): string {
  const hit = item.art?.find((a) => a.role === role) ?? item.art?.[0];
  return hit && /^https?:\/\//.test(hit.url) ? hit.url : "";
}

/**
 * Format a plugin item's date according to the kind's dateDisplay preference:
 * full UTC YYYY-MM-DD when dateDisplay === "date" and released is set,
 * otherwise year only.
 */
export function formatItemDate(
  item: Pick<KindItem, "year" | "released">,
  dateDisplay?: "year" | "date",
): string {
  if (dateDisplay === "date" && typeof item.released === "number" && item.released > 0) {
    return new Date(item.released * 1000).toISOString().slice(0, 10);
  }
  return item.year ? String(item.year) : "";
}

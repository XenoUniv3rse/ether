import type { EpisodeOrderDetails, EpisodeOrderProvenance } from "./metadataSources";

/**
 * Shapes of the owner-UI API contract. Kept in one file so the screens, the
 * fake backend and the tests agree on a single definition.
 */

type MediaKind = "movie" | "show" | "season" | "episode";

export interface TitleCard {
  key: string;
  type: MediaKind;
  title: string;
  year?: number;
  summary?: string;
  poster?: string;
  backdrop?: string;
  rating?: number;
  contentRating?: string;
  /** Where contentRating came from: "tmdb:US:6", "tmdb:CA", "omdb", "mdblist". */
  contentRatingSource?: string;
  runtimeMinutes?: number;
  genres?: string[];
  showTitle?: string;
  season?: number;
  episode?: number;
}

export interface ShowOrderCard extends TitleCard {
  episodeOrder?: EpisodeOrderProvenance;
}

export interface TitleDetail extends TitleCard {
  episodeOrder?: EpisodeOrderDetails;
  tmdbId?: number;
  imdbId?: string;
  cast?: { id: number; name: string; role?: string; image?: string }[];
  directors?: { id: number; name: string; image?: string }[];
  keywords?: { id: number; name: string }[] | string[];
  collection?: { id: number; name: string } | null;
  tagline?: string;
  originalTitle?: string;
  language?: string;
  /** TMDB status: "Returning Series", "Ended", "Released", "In Production"… */
  status?: string;
  studios?: string[];
  countries?: string[];
  /** YYYY-MM-DD: first air date or theatrical release. */
  releaseDate?: string;
  homeReleaseDate?: string;
  homeReleaseAgeAdmitted?: boolean;
  lastAirDate?: string;
  seasonCount?: number;
  episodeCount?: number;
}

export interface TitleLibraryReason {
  code: string;
  message: string;
  votes?: number;
  minVotes?: number;
  popularity?: number;
  minPopularity?: number;
  rule?: string;
}

export interface TitleLibraryStatus {
  browsable: boolean;
  included: boolean;
  inLibraryTier: boolean;
  canInclude: boolean;
  releaseGated: boolean;
  reasons: string[];
  items: TitleLibraryReason[];
}

const RULE_FIELDS = [
  "title",
  "genre",
  "keyword",
  "person",
  "studio",
  "network",
  "country",
  "language",
  "year",
  "decade",
  "rating",
  "votes",
  "popularity",
  "runtime",
  "contentRating",
  "released",
  "latestSeasonPremiere",
  "latestEpisode",
  "watchProvider",
  "collection",
  "watched",
  "inProgress",
  "addedAt",
] as const;
export type RuleField = (typeof RULE_FIELDS)[number] | `browse:${string}` | "group" | "viewer";

const RULE_OPS = [
  "is",
  "isNot",
  "in",
  "notIn",
  "gte",
  "lte",
  "between",
  "within",
  "unknown",
  "known",
] as const;
export type RuleOp = (typeof RULE_OPS)[number];

export interface Rule {
  field: RuleField;
  op: RuleOp;
  value: unknown;
  label?: string;
  labels?: string[];
}

type TmdbListName =
  | "trending_day"
  | "trending_week"
  | "popular"
  | "top_rated"
  | "now_playing"
  | "upcoming"
  | "airing_today"
  | "on_the_air";

type StevenLuList =
  | "movies"
  | "movies-imdb-min7"
  | "movies-imdb-min8"
  | "movies-metacritic-min50"
  | "movies-metacritic-min60"
  | "movies-metacritic-min70"
  | "movies-metacritic-min80"
  | "movies-rottentomatoes-min70"
  | "movies-rottentomatoes-min80";

type LookupSourceKind = "tmdbKeyword" | "tmdbCompany" | "tmdbNetwork" | "tmdbPerson";

export type Source =
  | { kind: "catalog" }
  | { kind: "tmdbList"; list: TmdbListName; mediaType: "movie" | "show" }
  | { kind: "tmdbCollection"; id: number; label?: string }
  | { kind: "tmdbDiscover"; params: Record<string, string> }
  | { kind: LookupSourceKind; id: number; label?: string }
  | { kind: "watchProvider"; id: number; region?: string; label?: string }
  | { kind: "tmdbUserList"; url: string }
  | { kind: "traktList"; url: string }
  | { kind: "letterboxdList"; url: string }
  | { kind: "mdblist"; url: string }
  | { kind: "anilistList"; url: string }
  | { kind: "stevenlu"; list: StevenLuList }
  | { kind: "titles"; keys: string[] };

export type CollectionSort =
  | "latestEpisode"
  | "latestSeasonPremiere"
  | "popularity"
  | "rating"
  | "released"
  | "title"
  | "added"
  | "random"
  | "personal"
  | "source";

export type Refresh = "live" | "hourly" | "daily" | "weekly";
export type ArtworkMode = "auto" | "collage" | "backdrop" | "tmdb" | "custom";
export type DeviceClass = "tv" | "mobile" | "web" | "desktop";

export interface CollectionSpec {
  localOnly?: boolean;
  releaseMode?: "released" | "upcoming" | "mixed";
  lookaheadDays?: number;
  id?: string;
  name: string;
  description?: string;
  mediaType: "movie" | "show" | "mixed";
  sources: Source[];
  match: "all" | "any";
  rules: Rule[];
  exclude: Rule[];
  sort: CollectionSort;
  sortDesc: boolean;
  limit: number;
  refresh: Refresh;
  rotate?: { size: number; every: "daily" | "weekly" } | null;
  artwork: { mode: ArtworkMode; url?: string; accent?: string };
  /** Where the collection is browseable. Rows that show it live on Recommendations pages. */
  /** Browse in apps: listed in its library's Collections view. */
  browseInApps: boolean;
  enabled: boolean;
  templateId?: string;
  updatedAt?: string;
  lastBuiltAt?: string;
  itemCount?: number;
  warnings?: string[];
  /** Optional folder name grouping collections in the console and frontends. */
  folder?: string;
  /** Optional 1-based display order within a folder or add-on import. */
  sortOrder?: number;
  /** Present when this collection is kept in sync with a Stremio add-on catalogue. */
  addonSync?: CollectionAddonSync | null;
}

export interface CollectionAddonSync {
  addonId: string;
  addonName?: string;
  catalogType: string;
  catalogId: string;
  catalogKey: string;
  managedName?: string;
  removed?: boolean;
}

export type AddonImportNameMode = "autoType" | "plain" | "appendType" | "prefix" | "suffix";

export interface AddonCatalogExtra {
  name: string;
  label: string;
  required?: boolean;
  options?: string[];
}

export interface AddonCatalogPreviewItem {
  key: string;
  type: "movie" | "series" | string;
  mediaType: "movie" | "show";
  id: string;
  name: string;
  order: number;
  supportsSkip?: boolean;
  standalone: boolean;
  extras?: AddonCatalogExtra[];
  requiredExtras?: string[];
  hasDuplicateName?: boolean;
  imported?: boolean;
  existingCollectionId?: string;
  existingCollectionName?: string;
  existingParams?: Record<string, string>;
  existingMaxItems?: number;
  existingRemoved?: boolean;
}

export interface AddonCatalogRemovedItem {
  id: string;
  name: string;
  catalogType: string;
  catalogId: string;
  catalogKey: string;
}

export interface AddonImportSkippedItem {
  key: string;
  name: string;
  reason: string;
}

export interface AddonImportSummary {
  created: number;
  updated: number;
  removed: number;
  restored: number;
  skipped?: AddonImportSkippedItem[];
}

export interface AddonCatalogSubscription {
  addonId: string;
  addonName?: string;
  addonLogo?: string;
  host?: string;
  addonEnabled: boolean;
  folder: string;
  nameMode: AddonImportNameMode;
  nameAffix?: string;
  followManifest: boolean;
  refresh: Refresh;
  maxItems: number;
  enabled: boolean;
  browseInApps: boolean;
  catalogCount: number;
  managedCount: number;
  removedCount: number;
  lastSyncedAt?: string;
  lastSummary?: AddonImportSummary;
}

export interface AddonCatalogAvailable {
  addonId: string;
  name: string;
  logo?: string;
  host?: string;
  enabled: boolean;
  catalogCount: number;
  hasImport?: boolean;
}

export interface AddonCatalogImportList {
  imports: AddonCatalogSubscription[];
  availableAddons: AddonCatalogAvailable[];
}

export interface AddonCatalogPreview {
  addonId: string;
  installed: boolean;
  addonName: string;
  addonVersion?: string;
  addonDescription?: string;
  addonLogo?: string;
  host?: string;
  configureFirst?: boolean;
  subscription?: AddonCatalogSubscription;
  catalogs: AddonCatalogPreviewItem[];
  removedCollections?: AddonCatalogRemovedItem[];
}

export interface AddonCatalogImportItem {
  key: string;
  name?: string;
  params?: Record<string, string>;
  maxItems?: number;
}

export interface AddonCatalogImportRequest {
  url?: string;
  addonId?: string;
  folder?: string;
  nameMode?: AddonImportNameMode;
  nameAffix?: string;
  followManifest: boolean;
  refresh?: Refresh;
  maxItems?: number;
  enabled: boolean;
  browseInApps: boolean;
  items: AddonCatalogImportItem[];
}

export interface AddonCatalogImportResult {
  addonId: string;
  addonName: string;
  manifestChanged?: boolean;
  created: number;
  updated: number;
  removed: number;
  restored: number;
  skipped?: AddonImportSkippedItem[];
  collectionIds?: string[];
  subscription: AddonCatalogSubscription;
}

/** How a row shows on its page: Pinned to a position, Show, Auto or Hide. */
export type RowPlacementMode = "pinned" | "show" | "auto" | "hide";

/** One row on a Recommendations page that draws from a collection. */
export interface CollectionUse {
  /** The page key: "home" or "library:<id>/<tab>". */
  page: string;
  /** The page in words: "Home", "Movies › Recommendations". */
  pageName: string;
  rowId: string;
  rowName: string;
  placement: RowPlacementMode;
  /** 1-based position when the row is pinned. */
  position?: number;
  /** The row's schedule in plain words, when it has one. */
  schedule?: string;
}

export interface CollectionUsage {
  uses: CollectionUse[];
}

export interface CollectionPreview {
  total: number;
  items: TitleCard[];
  warnings: string[];
  tookMs: number;
}

export interface CollectionTemplate {
  id: string;
  name: string;
  description: string;
  category: string;
  spec: CollectionSpec;
}

export interface CollectionTemplateBundle {
  id: string;
  name: string;
  description: string;
  category?: string;
  templateIds: string[];
}

export interface CollectionTemplateBundlePreviewItem {
  templateId: string;
  name: string;
  description?: string;
  category: string;
  mediaType?: string;
  action: "create" | "skip_existing";
  existingId?: string;
  existingName?: string;
}

export interface CollectionTemplateBundlePreview {
  bundle: CollectionTemplateBundle;
  items: CollectionTemplateBundlePreviewItem[];
  willCreate: number;
  willSkip: number;
}

export interface CollectionTemplateBundleApplyResult {
  bundleId: string;
  bundle?: CollectionTemplateBundle;
  items?: CollectionTemplateBundlePreviewItem[];
  created: CollectionSpec[];
  skipped: CollectionTemplateBundlePreviewItem[];
  createdCount: number;
  skippedCount: number;
}

export type FacetName =
  "tag" | "genre" | "decade" | "contentRating" | "studio" | "country" | "language" | "keyword";
export interface FacetValue {
  key: string;
  title: string;
  count: number;
  /** Equivalent values the key stands for (content-rating levels). */
  labels?: string[];
}

export type LookupKind =
  "person" | "keyword" | "company" | "network" | "collection" | "watchProvider";
export interface LookupResult {
  id: number;
  name: string;
  subtitle?: string;
  image?: string;
}

// ---- Viewers and personalization ------------------------------------------

export type ViewerAccessStatus = "owner" | "active" | "imported" | "inactive";

export type ViewerSource = "owner" | "local" | "invite" | "plex" | "plexHome" | "dlnaGuest";

/** What the owner may do with this viewer, and who manages the account. */
export interface ViewerLifecycle {
  source: ViewerSource;
  /** Access is paused: everything is kept, but they cannot sign in anywhere. */
  paused: boolean;
  /** Plex rows only: plex.tv still shares the server with them. */
  plexShared?: boolean;
  canRemove: boolean;
  canMerge: boolean;
  canPause: boolean;
  /** "plex" when the account is managed on plex.tv (remove and merge are refused in Ether). */
  managedBy: "plex" | "";
}

export interface Viewer {
  id: number;
  plexId: number;
  username: string;
  title: string;
  thumb?: string;
  isOwner: boolean;
  accessStatus?: ViewerAccessStatus;
  lifecycle?: ViewerLifecycle;
  lastActive?: string;
  plays30d: number;
  hoursWatched30d: number;
  /** The viewer's content-rating ceiling ("PG"); absent when there is none. */
  ageLimit?: string;
}

interface ViewerActivity {
  title: TitleCard;
  at: string;
  completed: boolean;
  progress: number;
}

export interface ViewerDetail extends Viewer {
  recent: ViewerActivity[];
}

export type OverrideKind = "genre" | "person" | "keyword";
export interface TasteOverride {
  kind: OverrideKind;
  value: string;
  weight: number;
}

export interface TasteProfile {
  userId: number;
  updatedAt: string;
  confidence: number;
  genres: { name: string; weight: number }[];
  people: { id: number; name: string; role: string; weight: number; image?: string }[];
  keywords: { name: string; weight: number }[];
  decades: { decade: number; weight: number }[];
  languages: { code: string; weight: number }[];
  habits: {
    weeknightMinutes: number;
    weekendMinutes: number;
    preferredDevice: string;
    bingeScore: number;
    rewatchScore: number;
    peakHours: number[];
  };
  overrides: TasteOverride[];
}

export type Device = "tv" | "mobile" | "web" | "desktop";
export type Daypart = "morning" | "afternoon" | "evening" | "late";

interface RankerFeature {
  name: string;
  label: string;
  weight: number;
}

export interface RankerInsight {
  trainedAt?: string;
  examples: number;
  positives: number;
  auc: number;
  baselineAuc: number;
  active: boolean;
  features: RankerFeature[] | null;
}

// ---- Insights ---------------------------------------------------------------

export interface RowInsight {
  hubId: string;
  title: string;
  impressions7d: number;
  plays7d: number;
  playRate: number;
  lowSample: boolean;
}

export interface ActivityDay {
  date: string;
  plays: number;
  hours: number;
  watchedMs?: number;
  uniqueViewers: number;
}

export interface RecommendationCounts {
  impressions: number;
  plays: number;
  measured: number;
  completed: number;
  watchedMs: number;
}

export interface RecommendationRow extends RecommendationCounts {
  kind: string;
  title: string;
  viewers: number;
  rankRate: number;
  low: number;
  high: number;
  lowSample: boolean;
  trend: (number | null)[];
}

export interface RecommendationReport extends RecommendationCounts {
  rows: RecommendationRow[];
  starts: number;
  attributedStarts: number;
}

export interface RecommendationGroup extends RecommendationCounts {
  key: string;
}

export interface RecommendationTitle {
  key: string;
  title: string;
  impressions: number;
  plays: number;
}

export interface RecommendationDetail {
  top: RecommendationTitle[];
  unplayed: RecommendationTitle[];
  viewers: RecommendationGroup[];
  apps: RecommendationGroup[];
  positions: RecommendationGroup[];
}

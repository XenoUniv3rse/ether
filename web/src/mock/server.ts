import {
  recommendationPerformance,
  recommendationPerformanceDetail,
} from "./recommendation-performance";
import type { EpisodeOrder, EpisodeOrderDetails, MetadataSource } from "../lib/metadataSources";
import { interpretDemo } from "./catalogInterpret";
import { indexerUsage, infrastructure, saveCost, sourceHistory } from "./source-stats";
import { matchesCatalogFilter } from "./catalogFilter";
import { createOnAirMock } from "./onair";
import { createVelaSkinsMock } from "./velaSkins";
/**
 * In-memory fake of the Ether admin API. Used by `vite --mode mock` and by the
 * tests; never shipped. It evaluates collection rules against a generated
 * catalogue and builds context-aware Home rows so every screen behaves
 * realistically offline.
 */
import { createPrerollsMock } from "./prerolls";
import { createVelaBrandingMock, VELA_SETTINGS_SCHEMA } from "./velaBranding";
import type {
  ActivityDay,
  AddonCatalogImportRequest,
  AddonCatalogPreview,
  AddonCatalogPreviewItem,
  AddonCatalogSubscription,
  AddonImportNameMode,
  CollectionSpec,
  CollectionUsage,
  CollectionUse,
  Rule,
  Source,
  TasteOverride,
  TasteProfile,
  TitleCard,
  Viewer,
} from "../lib/contract";
import { createLibraryTabsMock } from "./libraryTabs";
import { createPagesMock } from "./pages";
import { normalizeSpec } from "../lib/rules";
import { resolutionGroups } from "../lib/qualityRanking";
import type {
  Bootstrap,
  LLMProvider,
  LoadSample,
  SystemLoad,
  ClusterNode,
  ClusterControl,
  ClusterControlRequest,
  ClusterStatus,
  EtherConfig,
  EtherUser,
  JoinTokenInfo,
  LogEntry,
} from "../lib/types";
import {
  COLLECTIONS,
  GENRES,
  KEYWORDS,
  NETWORKS,
  PEOPLE,
  PROVIDERS,
  STUDIOS,
  avatarFor,
  buildCatalog,
  card,
  hash,
  posterFor,
  type FakeTitle,
} from "./data";
import { createMetadataMock } from "./metadata";
import { createExperienceMock } from "./experience";
import { createMaintenance } from "./maintenance";
import { createOpsMock, UNREACHABLE } from "./ops";
import { createReleaseConsoleMock } from "./releases";
import { createQualityMock } from "./quality";
import { createSitesMock } from "./sites";
import { createTrashMock } from "./trash";
import { createCacheMock } from "./cache";
import { createCalendarMock } from "./calendar";
import { createFlowsMock } from "./flows";
import { createLiveTVMock } from "./livetv";
import { createExtendMock } from "./extend";
import { TEMPLATE_BUNDLES, TEMPLATES } from "./templates";
import { NOT_RATED, RATING_LEVELS, canonicalRating } from "../lib/contentRatings";

/** A few other boards' labels per level, as the server lists them. */
const MOCK_EQUIVALENTS: Record<string, string[]> = {
  G: ["gb/U", "ca/G", "de/0", "br/L"],
  PG: ["gb/PG", "ca/PG", "de/6", "se/7", "br/10"],
  "PG-13": ["gb/12A", "ca/14A", "de/12"],
  R: ["gb/15", "ca/18A", "de/16"],
  "NC-17": ["gb/18", "de/18"],
};

const NOW = () => new Date();

/** "Used on" for the seeded collections: the Recommendations rows that show them. */
const COLLECTION_USAGE: Record<string, CollectionUse[]> = {
  c1: [
    {
      page: "home",
      pageName: "Home",
      rowId: "row-edge-of-your-seat",
      rowName: "Edge of your seat",
      placement: "pinned",
      position: 3,
      schedule: "Fri–Sat evenings",
    },
    {
      page: "library:1/recommended",
      pageName: "Movies › Recommendations",
      rowId: "row-edge-of-your-seat-movies",
      rowName: "Edge of your seat",
      placement: "auto",
    },
  ],
  c4: [
    {
      page: "home",
      pageName: "Home",
      rowId: "row-weeknight-wind-down",
      rowName: "Weeknight wind-down",
      placement: "show",
      schedule: "Mon–Thu from 6 PM",
    },
  ],
};

export interface FakeOptions {
  /** Owner-installed Vela client; absent by default. */
  velaInstalled?: boolean;
  velaDistribution?: "installing" | "rollback";
  liveTVEmpty?: boolean;
  onAirLayout?: string;
  capabilityOverrides?: Array<Record<string, unknown>>;
  /** Override /status/hwaccel and dashboard.sections.hwaccel for OS/GPU tests. */
  hwaccel?: Record<string, unknown>;
  bootstrap?: Partial<Bootstrap>;
  /** Preview the one-time release notes drawer after an update. */
  whatsNew?: boolean;
  /** Artificial latency for realism in the browser; 0 in tests. */
  latencyMs?: number;
  fastUsenetSpeed?: boolean;
  /** Serve as a node of a three-node PostgreSQL cluster instead of a standalone server. */
  cluster?: boolean;
  /** Pin the server in a startup state (see mock/ops.ts), e.g. "migrating" or "failed:database_corrupt". */
  startup?: string;
  /** Milliseconds per step of a simulated restart or restore. */
  opsPace?: number;
  /** Viewers watching, for the restart and restore drawers. */
  viewers?: number;
  /** The cache storage changed and needs a restart. */
  cacheRestart?: boolean;
  /** Report a damaged SQLite database in the health checklist. */
  damagedDatabase?: boolean;
  /** Start with a restore report ("restored", "failed") or a scheduled restore ("pending"). */
  restore?: string;
  /** Household audio and subtitle choices, for the Playback screen in the browser mock. */
  householdAudio?: boolean;
  /** Include a remote stream capped by the shared upload budget (browser mock preview). */
  remoteBudgetDemo?: boolean;
  liveProviderDemo?: boolean;
  /** Seed pre-rolls, their recent plays and a video still converting. */
  prerolls?: boolean;
  /** Simulate a port conflict on Jellyfin (true / "named" / "unknown"). */
  portConflict?: boolean | string;
  /** A reverse proxy forwarding visitor addresses without being trusted. */
  untrustedProxy?: boolean;
}

function json(body: unknown, status = 200): Response {
  return new Response(body === null ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}
function err(status: number, code: string, message: string): Response {
  return json({ error: { code, message } }, status);
}

/** Mirrors internal/config/clientid.go DefaultUserAgent(). */
const DEFAULT_USER_AGENT =
  "Ether/dev (+https://github.com/braedonsaunders/ether/blob/main/docs/indexer-operators.md)";

function defaultConfig(): EtherConfig {
  return {
    liveTV: { latencyProfile: "auto" },
    posters: {
      version: 1,
      assetFolder: "/media/posters",
      enabled: false,
      source: "tmdb",
      baseUrl: "",
      apiKey: "",
      urlTemplate: "",
      accounts: {
        rpdb: {
          enabled: true,
          source: "rpdb",
          baseUrl: "",
          apiKey: "[configured]",
          urlTemplate: "",
        },
      },
    },
    availability: {
      mode: "playable",
      monitor: true,
      verify: false,
      preferVerified: true,
      newlyAvailable: true,
      indexerIds: [],
      debrid: true,
      pollMinutes: 30,
      maxPages: 3,
      feedRequests: 100,
      checkRequests: 100,
      grabs: 20,
      checks: 20,
      pluginUsenetChecks: 20,
      checkBytes: 536870912,
      reservePercent: 80,
    },
    server: {
      startupAnimation: true,
      port: 32400,
      bindAddress: "0.0.0.0",
      friendlyName: "Living Room Ether",
      machineIdentifier: "mock-machine",
      logLevel: "info",
      defaultStreamLimit: 2,
      maxStreams: 0,
    },
    plex: {
      token: "••••",
      clientIdentifier: "mock-client",
      ownerPlexId: 1,
      ownerUsername: "Braedon",
      claimed: true,
      remoteAccess: true,
      useUpnp: true,
      publicPort: 32400,
      publicHost: "",
      customUrls: null,
      certFile: "",
      keyFile: "",
      certHash: "abc",
      secureConnections: "preferred",
    },
    usenet: {
      providers: [
        {
          id: "p1",
          name: "Primary news",
          host: "news.example.com",
          port: 563,
          tls: true,
          username: "me",
          password: "••••",
          connections: 20,
          priority: 0,
          backfill: false,
          enabled: true,
        },
        {
          id: "p2",
          name: "Block account",
          host: "block.example.net",
          port: 563,
          tls: true,
          username: "me",
          password: "••••",
          connections: 8,
          priority: 1,
          backfill: true,
          enabled: true,
        },
      ],
    },
    indexers: [
      {
        id: "i1",
        name: "Indexer One",
        url: "https://indexer-one.example.com",
        apiKey: "••••",
        dailyLimit: 300,
        dailyGrabs: 50,
        priority: 0,
        enabled: true,
        movieCategories: null,
        tvCategories: null,
      },
      {
        id: "i2",
        name: "Indexer Two",
        url: "https://indexer-two.example.com",
        apiKey: "••••",
        dailyLimit: 25,
        dailyGrabs: 10,
        priority: 1,
        enabled: true,
        movieCategories: null,
        tvCategories: null,
      },
      {
        id: "i3",
        name: "Torrent Tracker",
        url: "https://tracker.example.org/api/v2.0/indexers/example/results/torznab",
        apiKey: "••••",
        dailyLimit: 0,
        dailyGrabs: 0,
        priority: 2,
        enabled: true,
        movieCategories: null,
        tvCategories: null,
        protocol: "torrent",
      },
      {
        id: "i4",
        name: "Stream Addon",
        url: "https://addon.example/manifest.json",
        apiKey: "",
        dailyLimit: 0,
        dailyGrabs: 0,
        priority: 3,
        enabled: true,
        movieCategories: null,
        tvCategories: null,
        protocol: "torrent",
        kind: "stremio",
      },
    ],
    clientIdentity: { rules: [] },
    debrid: {
      providers: [
        {
          id: "rd-1",
          name: "Real-Debrid",
          kind: "realdebrid",
          apiKey: "••••",
          priority: 0,
          enabled: true,
        },
        {
          id: "tb-1",
          name: "TorBox backup",
          kind: "torbox",
          apiKey: "••••",
          priority: 1,
          enabled: true,
        },
      ],
      cachedOnly: true,
      removeUncached: true,
    },
    cache: {
      dir: "/var/lib/ether/cache",
      budgetBytes: 200 * 1024 ** 3,
      prefetchNextEpisode: true,
      prefetchBytes: 64 * 1024 ** 2,
      keepInProgressWarm: true,
      nextEpisodeAt: 0.8,
    },
    catalog: {
      tmdbKey: "",
      tvdbKey: "",
      language: "en-US",
      region: "US",
      minPopularity: 5,
      minVotes: 100,
      includeAdult: false,
      refreshInterval: "24h0m0s",
    },
    resolver: {
      healthSamples: 12,
      searchCacheTtl: "6h0m0s",
      unavailableCooloff: "24h0m0s",
      maxCandidates: 3,
      sameQualityAlternatives: 1,
      offerHigherVersions: false,
      blockedGroups: null,
      blockedTerms: null,
    },
    quality: {
      profiles: [
        {
          name: "4K",
          maxHeight: 2160,
          minHeight: 1080,
          allowHdr: true,
          allowDolbyVision: true,
          qualityOrder: resolutionGroups(2160, 1080),
          customFormatScores: {
            "ether:viewer-language": 20000,
            "ether:viewer-subtitles": 15,
            "ether:remux": 6000,
            "ether:bluray": 4000,
            "ether:webdl": 2000,
            "ether:dolby-vision": 500,
            "ether:hdr": 500,
            "ether:codec-hevc": 12,
            "ether:codec-av1": 6,
            "ether:audio-truehd": 8,
            "ether:audio-eac3": 4,
            "ether:atmos": 3,
            "ether:audio-direct": 10,
            "ether:audio-transcode": -15,
            "ether:tone-mapping": -15,
          },
          minSizeMbPerMin: 15,
          maxSizeMbPerMin: 1000,
          episodeMinSizeMbPerMin: 0,
          episodeMaxSizeMbPerMin: 0,
          preferredLanguages: null,
          audioLanguageMode: "preferred",
          allowCam: false,
        },
        {
          name: "1080p",
          maxHeight: 1080,
          minHeight: 720,
          allowHdr: false,
          allowDolbyVision: false,
          qualityOrder: resolutionGroups(1080, 720),
          customFormatScores: {
            "ether:viewer-language": 10000,
            "ether:viewer-subtitles": 15,
            "ether:series-year-differs": -5000,
            "ether:webdl": 2000,
            "ether:bluray": 1000,
            "ether:codec-h264": 12,
            "ether:codec-hevc": 6,
            "ether:audio-eac3": 12,
            "ether:audio-ac3": 8,
            "ether:audio-aac": 4,
            "ether:audio-direct": 10,
            "ether:audio-transcode": -15,
          },
          minSizeMbPerMin: 5,
          maxSizeMbPerMin: 150,
          episodeMinSizeMbPerMin: 0,
          episodeMaxSizeMbPerMin: 0,
          preferredLanguages: null,
          audioLanguageMode: "preferred",
          allowCam: false,
        },
      ],
      rules: [
        {
          id: "r_appletv1",
          name: "Living room Apple TV",
          devices: [{ app: "plex:Plex for Apple TV", label: "Plex for Apple TV" }],
          profile: "4K",
        },
      ],
      default: "1080p",
      defaultAway: "",
      sync: { enabled: false, interval: "24h0m0s" },
    },
    transcode: {
      videoTranscoding: "on",
      ffmpegPath: "",
      ffprobePath: "",
      hwAccel: "auto",
      vaapiDevice: "",
      tempDir: "/tmp/ether-transcode",
      maxTranscodes: 2,
      throttleSeconds: 0,
      remoteMaxBitrateKbps: 8000,
      remoteUploadBudgetKbps: 0,
      rtxVideo: {
        mode: "off",
        executablePath: "",
        sourceMaxHeight: 720,
        targetMaxHeight: 1080,
        quality: 2,
        maxConcurrent: 1,
      },
    },
    trakt: { enabled: false, clientId: "", clientSecret: "" },
    plexServers: { enabled: true },
    openSubtitles: {
      enabled: false,
      apiKey: "",
      username: "",
      password: "",
      automaticPreferred: false,
    },
    subdl: { enabled: false, apiKey: "" },
    subsource: { enabled: false, apiKey: "" },
    llm: { enabled: true, provider: "ollama", baseUrl: "", apiKey: "", model: "llama3" },
    updates: {
      enabled: true,
      channel: "stable",
      autoUpdate: true,
      feedUrl: "",
      maintenanceStart: "03:00",
      maintenanceEnd: "06:00",
    },
    spike: { enabled: false, localFiles: null },
    downloads: {
      enabled: true,
      allowFriends: false,
      maxConcurrent: 2,
      maxConcurrentPerUser: 1,
      diskBudgetBytes: 20 * 1024 ** 3,
      expiry: "24h0m0s",
      maxHeight: 1080,
    },
    personalize: {
      postPlay: true,
      postPlayPrebuffer: true,
      microReasons: true,
      sessionIntent: true,
      clockFormat: "",
    },
    extras: {
      themeMusic: true,
      enabled: true,
      resolverTimeout: "20s",
      prewarmBytes: 48 * 1024 ** 2,
    },
    markers: { theIntroDb: true, apiKey: "" },
    setupComplete: true,
  };
}

const VIEWERS: Viewer[] = [
  {
    id: 1,
    plexId: 1001,
    username: "owner",
    title: "Alex Rivera",
    isOwner: true,
    lastActive: "",
    plays30d: 42,
    hoursWatched30d: 61.5,
    lifecycle: {
      source: "owner",
      paused: false,
      plexShared: false,
      canRemove: false,
      canMerge: false,
      canPause: false,
      managedBy: "",
    },
  },
  {
    id: 2,
    plexId: 1002,
    username: "sam",
    title: "Sam Rivera",
    isOwner: false,
    lastActive: "",
    plays30d: 27,
    hoursWatched30d: 33.2,
    lifecycle: {
      source: "local",
      paused: false,
      plexShared: false,
      canRemove: true,
      canMerge: true,
      canPause: true,
      managedBy: "",
    },
  },
  {
    id: 3,
    plexId: 1003,
    username: "kids",
    title: "The Kids",
    isOwner: false,
    lastActive: "",
    ageLimit: "PG",
    plays30d: 58,
    hoursWatched30d: 24.8,
    lifecycle: {
      source: "plexHome",
      paused: false,
      plexShared: true,
      canRemove: false,
      canMerge: false,
      canPause: true,
      managedBy: "plex",
    },
  },
  {
    id: 4,
    plexId: 1004,
    username: "jordan",
    title: "Jordan Lee",
    isOwner: false,
    lastActive: "",
    plays30d: 6,
    hoursWatched30d: 9.1,
    lifecycle: {
      source: "plex",
      paused: false,
      plexShared: true,
      canRemove: false,
      canMerge: false,
      canPause: true,
      managedBy: "plex",
    },
  },
];

/** Lifecycle as the real API reports it: Plex accounts can be paused, while remove/merge stay on plex.tv. */
function mockLifecycle(
  v: Pick<Viewer, "isOwner" | "plexId" | "title" | "username" | "accessStatus" | "lifecycle">,
) {
  if (v.lifecycle) return v.lifecycle;
  if (v.isOwner) {
    return {
      source: "owner" as const,
      paused: false,
      managedBy: "" as const,
      canPause: false,
      canRemove: false,
      canMerge: false,
    };
  }
  if (v.title === "Living-room guest" && v.plexId === 0 && !v.username) {
    return {
      source: "dlnaGuest" as const,
      paused: false,
      managedBy: "" as const,
      canPause: true,
      canRemove: true,
      canMerge: true,
    };
  }
  if (v.plexId > 0) {
    return {
      source: "plex" as const,
      paused: false,
      plexShared: v.accessStatus !== "inactive",
      managedBy: "plex" as const,
      canPause: true,
      canRemove: false,
      canMerge: false,
    };
  }
  return {
    source: "local" as const,
    paused: v.accessStatus === "inactive",
    managedBy: "" as const,
    canPause: true,
    canRemove: true,
    canMerge: true,
  };
}

const TASTE_SEEDS: Record<number, string[]> = {
  1: ["Science Fiction", "Thriller", "Mystery", "Crime"],
  2: ["Romance", "Comedy", "Drama", "Music"],
  3: ["Animation", "Family", "Adventure", "Fantasy"],
  4: ["Documentary", "History", "War"],
};

function decadeOf(year?: number): string {
  return year ? String(Math.floor(year / 10) * 10) : "";
}

function durationMs(w: string): number {
  const m = /^(\d+)([dwmy])$/.exec(w);
  if (!m) return 0;
  const n = Number(m[1]);
  return n * { d: 1, w: 7, m: 30, y: 365 }[m[2] as "d" | "w" | "m" | "y"] * 86400000;
}

export function createFakeServer(opts: FakeOptions = {}) {
  const velaStatus = {
    installed: opts.velaInstalled === true,
    installedVersion: opts.velaInstalled ? "0.1.41" : "",
    availableVersion: "0.1.42",
    rollbackAvailable: opts.velaDistribution === "rollback",
    busy: opts.velaDistribution === "installing",
    phase: opts.velaDistribution === "installing" ? "downloading" : "",
    downloaded: opts.velaDistribution === "installing" ? 82_000_000 : 0,
    total: opts.velaDistribution === "installing" ? 193_700_293 : 0,
  };
  const velaSkins = createVelaSkinsMock((removedId) => {
    const velaSettings = state.config.frontends?.vela?.settings as
      Record<string, unknown> | undefined;
    if (velaSettings && velaSettings.defaultSkinId === removedId) {
      velaSettings.defaultSkinId = "cinematic";
    }
  });
  // Keep the fixture stable when a setup link changes the page's query string.
  const previewParams = new URL(window.location.href).searchParams;
  const metadataMode = previewParams.get("metadata");
  const bundledTMDB =
    metadataMode !== "none" && metadataMode !== "tvdb" && !previewParams.has("catalogKey");
  function metadataSources(): MetadataSource[] {
    const tmdbKeySource = state.config.catalog.tmdbKey
      ? "owner"
      : bundledTMDB
        ? "built-in"
        : "none";
    const tmdbEnabled = !state.config.catalog.tmdbDisabled;
    const tvdbConfigured = Boolean(state.config.catalog.tvdbKey);
    return [
      {
        id: "tmdb",
        name: "TMDB",
        configured: tmdbEnabled && tmdbKeySource !== "none",
        enabled: tmdbEnabled,
        keySource: tmdbKeySource,
        movies: true,
        shows: true,
      },
      {
        id: "tvdb",
        name: "TheTVDB",
        configured: tvdbConfigured,
        enabled: tvdbConfigured,
        keySource: tvdbConfigured ? "owner" : "none",
        movies: true,
        shows: true,
      },
    ];
  }

  function setupSteps() {
    return [
      {
        id: "catalog",
        title: "Metadata sources",
        complete: metadataSources().some((source) => source.configured),
        required: false,
      },
    ];
  }
  const xtreamGrants = new Map<
    string,
    {
      hash: string;
      surface: string;
      lastUsed: number;
      xtreamUsername?: string;
      revealAvailable?: boolean;
      xtreamStreamLimit?: number;
    }[]
  >();
  const catalog = buildCatalog();
  const byKey = new Map(catalog.map((t) => [t.key, t]));
  const ownerTags = new Map<string, string[]>();
  const ownerIncludes = new Set<string>();
  const watchlistChanges = new Map<string, boolean>();
  const savedForViewer = (viewerId: number, key: string) =>
    watchlistChanges.get(`${viewerId}:${key}`) ?? Number(key.slice(-2)) % 4 === 0;
  const releaseConsole = createReleaseConsoleMock(catalog);
  const trash = createTrashMock();
  const maintenance = createMaintenance();
  const experience = createExperienceMock();
  const sites = createSitesMock();
  const cacheMock = createCacheMock(opts.cacheRestart, (key) => byKey.get(String(key))?.title);
  const liveTV = createLiveTVMock(catalog, opts.liveTVEmpty);
  const onAir = createOnAirMock(liveTV, opts.onAirLayout);
  const calendar = createCalendarMock();
  const flowsMock = createFlowsMock({
    llmEnabled: () => Boolean(state.config.llm.enabled && state.config.llm.model),
    channels: maintenance.channels,
  });
  const qualityMock = createQualityMock();
  const safetyPolicies = new Map<number, Record<string, unknown>>();
  const simklAccounts = new Map<number, { connected: boolean; lastSync?: string }>();
  const mdblistAccounts = new Map<number, { connected: boolean; lastSync?: string }>();
  const traktAccounts = new Map<number, { connected: boolean; username?: string }>();
  const extendMock = createExtendMock();
  const velaBranding = createVelaBrandingMock();
  const prerolls = createPrerollsMock({
    seed: opts.prerolls,
    title: (key) => byKey.get(key),
    viewerName: (id) => VIEWERS.find((v) => v.id === id)?.title ?? `Viewer ${id}`,
  });
  const state = {
    boot: {
      ok: true,
      version: "0.9.0-mock",
      setupComplete: true,
      authenticated: true,
      authRequired: false,
      serverName: "Living Room Ether",
      signInMethods: ["local", "plex"],
      ...opts.bootstrap,
    } as Bootstrap,
    config: {
      ...defaultConfig(),
      ...(opts.capabilityOverrides
        ? {
            clients: {
              overrides: opts.capabilityOverrides as NonNullable<
                ReturnType<typeof defaultConfig>["clients"]
              >["overrides"],
            },
          }
        : {}),
    },
    // Null until a caller opts in (the browser mock does). Viewer tests still
    // overlay these routes on a 404 from the shared fake.
    streamDefaults: (opts.householdAudio
      ? {
          audioLanguages: ["en"],
          subtitleLanguages: ["en"],
          preferOriginal: true,
          subtitleMode: "foreign-only",
          subtitleStyle: "normal",
          loudness: false,
          nightMode: false,
          dialogueBoost: false,
        }
      : null) as null | {
      audioLanguages: string[];
      subtitleLanguages: string[];
      preferOriginal: boolean;
      subtitleMode: string;
      subtitleStyle: string;
      loudness: boolean;
      nightMode: boolean;
      dialogueBoost: boolean;
    },
    pausedViewers: new Set<number>(),
    removedViewers: new Set<number>(),
    users: VIEWERS.map<EtherUser>((v) => ({
      id: v.id,
      plexId: v.plexId,
      username: v.username,
      title: v.title,
      thumb: avatarFor(v.title),
      isOwner: v.isOwner,
      lifecycle: v.lifecycle,
      streamLimit: 0,
      maxContentRating: v.id === 3 ? "PG" : "",
      allowedLibraries: [],
    })),
    favoritesMirror: new Map<number, boolean>(),
    dlnaGuests: [] as Viewer[],
    addedViewers: [] as Viewer[],
    frontendApiKeys: {
      jellyfin: [
        {
          id: 1,
          appName: "Wizarr",
          createdAt: "2026-11-18T14:20:00Z",
          lastUsed: "2026-11-22T17:45:00Z",
        },
        {
          id: 2,
          appName: "Jellyseerr",
          createdAt: "2026-11-20T09:10:00Z",
        },
      ],
      emby: [],
    } as Record<string, { id: number; appName: string; createdAt: string; lastUsed?: string }[]>,
    lockouts: [
      {
        key: "lock-firetv-jordan",
        namespace: "login",
        scope: "identity-ip",
        remoteIp: "192.168.1.48",
        identity: "jordan",
        app: "Jellyfin Android TV",
        attempts: 9,
        limit: 8,
        remainingMs: 742000,
        expiresAt: new Date(Date.now() + 742000).toISOString(),
      },
      {
        key: "lock-ip-203",
        namespace: "login",
        scope: "ip",
        remoteIp: "203.0.113.19",
        app: "Ether viewer portal",
        attempts: 31,
        limit: 30,
        remainingMs: 265000,
        expiresAt: new Date(Date.now() + 265000).toISOString(),
      },
      {
        key: "lock-poll-192",
        namespace: "poll",
        scope: "identity-ip",
        remoteIp: "192.168.1.48",
        app: "Jellyfin Android TV",
        attempts: 61,
        limit: 60,
        remainingMs: 38000,
        expiresAt: new Date(Date.now() + 38000).toISOString(),
      },
    ] as {
      key: string;
      namespace: string;
      scope: string;
      remoteIp?: string;
      identity?: string;
      app?: string;
      attempts: number;
      limit: number;
      remainingMs: number;
      expiresAt: string;
    }[],
    collections: [] as CollectionSpec[],
    addonImports: [] as AddonCatalogSubscription[],
    overrides: new Map<number, TasteOverride[]>(),
    logs: [] as LogEntry[],
    calls: [] as { method: string; path: string; body: unknown }[],
    plexDefaults: {} as Record<number, Record<string, unknown>>,
    clustered: opts.cluster ?? false,
    joinTokens: [] as JoinTokenInfo[],
    backupSettings: {
      intervalHours: 24,
      retain: 7,
      lastRun: 0,
      sharedDir: "",
      skipOnAirMedia: false,
    },
    backups: [] as {
      name: string;
      size: number;
      created: string;
      node?: string;
      nodeId?: string;
      shared?: boolean;
      remote?: boolean;
    }[],
  };
  {
    const ago = (h: number) => new Date(Date.now() - h * 3600_000).toISOString();
    const stamp = (h: number) => `ether-${ago(h).slice(0, 19).replace(/[-:]/g, "")}.tar.gz`;
    state.backups = state.clustered
      ? [
          { name: stamp(2), size: 6_200_000, created: ago(2), node: "den", nodeId: "n-den" },
          {
            name: stamp(26),
            size: 6_100_000,
            created: ago(26),
            node: "attic",
            nodeId: "n-attic",
            remote: true,
          },
        ]
      : [{ name: stamp(2), size: 4_200_000, created: ago(2) }];
    state.joinTokens = state.clustered
      ? [
          {
            id: "t7k2",
            createdAt: ago(30),
            expiresAt: ago(29),
            usedAt: ago(29.5),
            usedBy: "attic",
          },
          {
            id: "expired-example",
            createdAt: ago(2),
            expiresAt: ago(1),
          },
          {
            id: "q9wd",
            createdAt: ago(0.2),
            expiresAt: new Date(Date.now() + 40 * 60_000).toISOString(),
          },
        ]
      : [];
  }
  let seq = 1;

  // Seed collections from templates so the gallery isn't empty.
  for (const id of ["mood-edge-of-your-seat", "recently-added", "family-night", "short-under-90"]) {
    const t = TEMPLATES.find((x) => x.id === id)!;
    const spec = normalizeSpec({
      ...t.spec,
      // One seeded collection is only a row (not browseable in apps), renamed
      // so it does not share a name with the built-in Short on Time source.
      ...(id === "short-under-90" ? { name: "Weeknight wind-down", browseInApps: false } : {}),
      id: `c${seq++}`,
      enabled: id !== "family-night",
      updatedAt: NOW().toISOString(),
      lastBuiltAt: NOW().toISOString(),
    });
    spec.itemCount = evaluate(spec).total;
    Object.assign(spec, { ratingKey: String(20000 + seq) });
    state.collections.push(spec);
  }

  const levels = ["debug", "info", "info", "info", "warn", "error"];
  for (let i = 0; i < 80; i++) {
    const lvl = levels[i % levels.length] as string;
    state.logs.push({
      id: i + 1,
      time: new Date(Date.now() - (80 - i) * 45000).toISOString(),
      level: lvl.toUpperCase(),
      message:
        lvl === "error"
          ? "segment fetch failed"
          : lvl === "warn"
            ? "indexer budget low"
            : "served hubs",
      attrs: { component: i % 2 ? "plex" : "resolver", user: "owner" },
    });
  }

  // ---- Collection evaluation ---------------------------------------------------

  function candidates(spec: CollectionSpec): FakeTitle[] {
    const media = (t: FakeTitle) => spec.mediaType === "mixed" || t.type === spec.mediaType;
    const out = new Map<string, FakeTitle>();
    for (const s of spec.sources) {
      for (const t of sourceTitles(s)) if (media(t) || s.kind === "titles") out.set(t.key, t);
    }
    return [...out.values()];
  }

  function sourceTitles(s: Source): FakeTitle[] {
    switch (s.kind) {
      case "catalog":
        return catalog;
      case "titles":
        return s.keys.map((k) => byKey.get(k)).filter((t): t is FakeTitle => Boolean(t));
      case "tmdbPerson":
        return catalog.filter((t) => t.people.includes(s.id));
      case "tmdbKeyword":
        return catalog.filter((t) => t.keywords.includes(s.id));
      case "tmdbCompany":
        return catalog.filter((t) => t.studio === s.id);
      case "tmdbNetwork":
        return catalog.filter((t) => t.network === s.id);
      case "tmdbCollection":
        return catalog.filter((t) => t.collectionId === s.id);
      case "watchProvider":
        return catalog.filter((t) => t.providers.includes(s.id));
      case "tmdbList":
        return [...catalog]
          .sort((a, b) => (hash(s.list + a.key) % 1000) - (hash(s.list + b.key) % 1000))
          .slice(0, 40);
      default:
        return catalog.filter((t) => hash(JSON.stringify(s) + t.key) % 5 === 0);
    }
  }

  function fieldValues(t: FakeTitle, field: string): (string | number | boolean)[] {
    switch (field) {
      case "title":
        return [t.key];
      case "genre":
        return t.genres ?? [];
      case "keyword":
        return t.keywords;
      case "person":
        return t.people;
      case "studio":
        return [t.studio];
      case "network":
        return t.network ? [t.network] : [];
      case "country":
        return [t.country];
      case "language":
        return [t.language];
      case "year":
        return t.year ? [t.year] : [];
      case "decade":
        return [decadeOf(t.year)];
      case "rating":
        return [t.rating ?? 0];
      case "votes":
        return [t.votes];
      case "popularity":
        return [t.popularity];
      case "runtime":
        return [t.runtimeMinutes ?? 0];
      case "contentRating":
        return [t.contentRating ? canonicalRating(t.contentRating) : NOT_RATED];
      case "released":
        return [t.released];
      case "latestSeasonPremiere":
        return t.type === "show" && t.latestSeasonPremiere ? [t.latestSeasonPremiere] : [];
      case "latestEpisode":
        return t.type === "show" && t.latestEpisode ? [t.latestEpisode] : [];
      case "addedAt":
        return [t.addedAt.slice(0, 10)];
      case "watchProvider":
        return t.providers;
      case "collection":
        return t.collectionId ? [t.collectionId] : [];
      case "watched":
        return [hash("w" + t.key) % 4 === 0];
      case "inProgress":
        return [hash("p" + t.key) % 9 === 0];
      default:
        return [];
    }
  }

  type VisibilityGroup = { action?: string; mediaType: string; match: string; rules: Rule[] };
  function hiddenByRules(title: FakeTitle, groups: VisibilityGroup[], fallback: string) {
    const first = groups.find(
      (g) =>
        (g.mediaType === "mixed" || g.mediaType === title.type) &&
        (g.match === "any"
          ? g.rules.some((r) => matches(title, r))
          : g.rules.every((r) => matches(title, r))),
    );
    return first ? first.action !== "show" : fallback === "hide";
  }

  function matches(t: FakeTitle, r: Rule): boolean {
    if (r.field === "group") {
      const children = r.value as Rule[];
      if (!children.length) return r.op !== "isNot";
      const hit =
        r.op === "in"
          ? children.some((child) => matches(t, child))
          : children.every((child) => matches(t, child));
      return r.op === "isNot" ? !hit : hit;
    }

    if (r.field === "viewer" && r.value && typeof r.value === "object") {
      const v = r.value as Record<string, unknown>;
      if (v.unwatched) return hash("w" + t.key) % 4 !== 0;
      if (v.inProgress) return hash("p" + t.key) % 9 === 0;
      if (v.onWatchlist) return hash("wl" + t.key) % 6 === 0;
      return hash("rt" + t.key) % 5 === 0;
    }
    if (r.field.startsWith("browse:")) {
      const bk = r.field.slice("browse:".length);
      if (bk === "playable") return true;
      if (bk === "debridCached") return hash("dc" + t.key) % 2 === 0;
      if (bk === "minRating") return (t.rating ?? 0) >= Number(r.value);
      if (bk === "maxRating") return (t.rating ?? 0) <= Number(r.value);
      if (bk === "yearFrom") return (t.year ?? 0) >= Number(r.value);
      if (bk === "yearTo") return (t.year ?? 0) <= Number(r.value);
      if (bk === "anime") return Boolean(t.genres?.includes("Animation")) === Boolean(r.value);
      if (bk === "kids") return Boolean(t.genres?.includes("Family")) === Boolean(r.value);
      return hash(bk + t.key) % 3 === 0;
    }
    if (r.field === "contentRating") return ratingMatches(t, r);
    const vals = fieldValues(t, r.field);
    const norm = (v: unknown) =>
      typeof v === "object" && v !== null && "key" in v ? (v as { key: unknown }).key : v;
    const eq = (a: unknown, b: unknown) =>
      String(norm(a)).toLowerCase() === String(norm(b)).toLowerCase();
    switch (r.op) {
      case "in":
        return Array.isArray(r.value) && r.value.some((v) => vals.some((x) => eq(x, v)));
      case "notIn":
        return !(Array.isArray(r.value) && r.value.some((v) => vals.some((x) => eq(x, v))));
      case "is":
        return vals.some((x) => eq(x, r.value));
      case "isNot":
        return !vals.some((x) => eq(x, r.value));
      case "gte":
        return vals.some((x) =>
          typeof x === "number" ? x >= Number(r.value) : String(x) >= String(r.value),
        );
      case "lte":
        return vals.some((x) =>
          typeof x === "number" ? x <= Number(r.value) : String(x) <= String(r.value),
        );
      case "between": {
        const [lo, hi] = Array.isArray(r.value) ? r.value : [];
        return vals.some((x) =>
          typeof x === "number"
            ? x >= Number(lo) && x <= Number(hi)
            : String(x) >= String(lo) && String(x) <= String(hi),
        );
      }
      case "within": {
        const since = new Date(Date.now() - durationMs(String(r.value))).toISOString().slice(0, 10);
        return vals.some((x) => String(x) >= since);
      }
      case "unknown":
        return vals.length === 0 || vals.every((x) => x === "" || x == null);
      case "known":
        return vals.some((x) => x !== "" && x != null);
    }
  }

  /** Content ratings compare by level, as on the server. */
  function ratingMatches(t: FakeTitle, r: Rule): boolean {
    const have = fieldValues(t, "contentRating")[0] as string;
    const want = (Array.isArray(r.value) ? r.value : [r.value]).map((v) =>
      canonicalRating(String(v)),
    );
    const bound = want[0] ?? "";
    const order = RATING_LEVELS.map((l) => l.value);
    switch (r.op) {
      case "in":
      case "is":
        return want.includes(have);
      case "notIn":
      case "isNot":
        return !want.includes(have);
      case "gte":
        return have !== NOT_RATED && order.indexOf(have) >= order.indexOf(bound);
      case "lte":
        return have !== NOT_RATED && order.indexOf(have) <= order.indexOf(bound);
    }
    return false;
  }

  function evaluate(spec: CollectionSpec) {
    const started = performance.now();
    const warnings: string[] = [];
    let items = candidates(spec);
    if (spec.rules.length)
      items = items.filter((t) =>
        spec.match === "any"
          ? spec.rules.some((r) => matches(t, r))
          : spec.rules.every((r) => matches(t, r)),
      );
    if (spec.exclude.length) items = items.filter((t) => !spec.exclude.some((r) => matches(t, r)));
    const dir = spec.sortDesc ? -1 : 1;
    const sorters: Record<string, (a: FakeTitle, b: FakeTitle) => number> = {
      popularity: (a, b) => (a.popularity - b.popularity) * dir,
      rating: (a, b) => ((a.rating ?? 0) - (b.rating ?? 0)) * dir,
      released: (a, b) => a.released.localeCompare(b.released) * dir,
      latestEpisode: (a, b) => {
        if (!a.latestEpisode && !b.latestEpisode) return 0;
        if (!a.latestEpisode) return 1;
        if (!b.latestEpisode) return -1;
        return a.latestEpisode.localeCompare(b.latestEpisode) * dir;
      },
      latestSeasonPremiere: (a, b) => {
        if (!a.latestSeasonPremiere && !b.latestSeasonPremiere) return 0;
        if (!a.latestSeasonPremiere) return 1;
        if (!b.latestSeasonPremiere) return -1;
        return a.latestSeasonPremiere.localeCompare(b.latestSeasonPremiere) * dir;
      },
      added: (a, b) => a.addedAt.localeCompare(b.addedAt) * dir,
      title: (a, b) => a.title.localeCompare(b.title) * dir,
      random: (a, b) =>
        (hash(a.key + new Date().toDateString()) % 997) -
        (hash(b.key + new Date().toDateString()) % 997),
      personal: (a, b) => (hash("me" + b.key) % 100) - (hash("me" + a.key) % 100),
    };
    const sorter = sorters[spec.sort];
    if (sorter) items.sort(sorter);
    if (
      spec.sources.some((s) =>
        [
          "traktList",
          "letterboxdList",
          "mdblist",
          "anilistList",
          "tmdbUserList",
          "stevenlu",
        ].includes(s.kind),
      )
    )
      warnings.push(
        "External lists are refreshed on the schedule; the preview shows the last fetched copy.",
      );
    if (items.length === 0 && spec.rules.length)
      warnings.push("No titles match every rule. Try “any” instead of “all”, or loosen a rule.");
    const total = spec.limit > 0 ? Math.min(spec.limit, items.length) : items.length;
    const limited = spec.limit > 0 ? items.slice(0, spec.limit) : items;
    const shown =
      spec.rotate && spec.rotate.size > 0 ? limited.slice(0, spec.rotate.size) : limited;
    return {
      total,
      items: shown.slice(0, 60).map(card),
      warnings,
      tookMs: Math.max(1, Math.round(performance.now() - started)) + 9,
    };
  }

  // ---- Personalization ---------------------------------------------------------------

  function taste(userId: number): TasteProfile {
    const seeds = TASTE_SEEDS[userId] ?? ["Drama"];
    const overrides = state.overrides.get(userId) ?? [];
    const genres = GENRES.map((g) => {
      const i = seeds.indexOf(g);
      let w = i >= 0 ? 0.9 - i * 0.18 : ((hash(g + userId) % 100) / 100) * 0.6 - 0.35;
      const o = overrides.find((x) => x.kind === "genre" && x.value === g);
      if (o) w = o.weight;
      return { name: g, weight: Math.round(w * 100) / 100 };
    }).sort((a, b) => b.weight - a.weight);
    return {
      userId,
      updatedAt: new Date(Date.now() - 3600_000).toISOString(),
      confidence: userId === 4 ? 0.22 : 0.55 + (userId % 3) * 0.12,
      genres,
      people: PEOPLE.slice(userId, userId + 5).map((p, i) => ({
        id: p.id,
        name: p.name,
        role: p.role,
        weight: Math.round((0.8 - i * 0.15) * 100) / 100,
        image: avatarFor(p.name),
      })),
      keywords: KEYWORDS.slice(0, 6).map((k, i) => ({
        name: k.name,
        weight: Math.round((0.7 - i * 0.22) * 100) / 100,
      })),
      decades: ["1970", "1980", "1990", "2000", "2010", "2020"].map((d, i) => ({
        decade: Number(d),
        weight: Math.round(Math.max(0, Math.sin((i + userId) / 2)) * 100) / 100,
      })),
      languages: [
        { code: "en", weight: 0.92 },
        { code: userId === 1 ? "ko" : "fr", weight: 0.31 },
        { code: "ja", weight: userId === 3 ? 0.4 : -0.1 },
      ],
      habits: {
        weeknightMinutes: userId === 3 ? 45 : 95,
        weekendMinutes: userId === 3 ? 120 : 170,
        preferredDevice: userId === 2 ? "mobile" : userId === 3 ? "tv" : "tv",
        bingeScore: userId === 1 ? 0.72 : 0.38,
        rewatchScore: userId === 3 ? 0.81 : 0.24,
        peakHours: userId === 3 ? [16, 17, 18] : [20, 21, 22],
      },
      overrides,
    };
  }

  function pickTitles(seed: string, n: number, filter?: (t: FakeTitle) => boolean): FakeTitle[] {
    const pool = filter ? catalog.filter(filter) : catalog;
    return [...pool]
      .sort((a, b) => (hash(seed + a.key) % 10007) - (hash(seed + b.key) % 10007))
      .slice(0, n);
  }

  // ---- Homes (layered Home Designer) ---------------------------------------------------

  function activity(): ActivityDay[] {
    const out: ActivityDay[] = [];
    for (let i = 29; i >= 0; i--) {
      const d = new Date(Date.now() - i * 86400000);
      const dow = d.getDay();
      const base = dow === 0 || dow === 6 ? 9 : 5;
      const plays = base + (hash(d.toDateString()) % 6);
      out.push({
        date: d.toISOString().slice(0, 10),
        plays,
        hours: Math.round(plays * 1.4 * 10) / 10,
        uniqueViewers: 1 + (hash("u" + d.toDateString()) % 4),
      });
    }
    return out;
  }

  function debridStatus() {
    const debrid = state.config.debrid;
    const providers = debrid?.providers ?? [];
    return {
      enabled: providers.some((p) => p.enabled && p.apiKey),
      cachedOnly: debrid?.cachedOnly ?? true,
      engine: {
        openFiles: 1,
        blockBytes: 524288,
        memoryHits: 10,
        diskHits: 40,
        blocksFetched: 120,
        bytesFetched: 62914560,
        hedges: 2,
        hedgeWins: 1,
        linkRefreshes: 3,
        errors: 0,
      },
      providers: providers.map((p, i) => {
        const base = {
          id: p.id,
          name: p.name,
          kind: p.kind,
          enabled: p.enabled,
          priority: p.priority,
          accountSlots: 6,
          slots: state.clustered ? 2 : 6,
        };
        const idle = {
          resolveMs: 0,
          linkMs: 0,
          connectMs: 0,
          ttfbMs: 0,
          throughputBps: 0,
          samples: 0,
        };
        if (!p.enabled || !p.apiKey)
          return {
            ...base,
            state: "disabled",
            requests: 0,
            failures: 0,
            cached: 0,
            notCached: 0,
            reused: 0,
            latency: idle,
          };
        if (i === 0)
          return {
            ...base,
            state: "ok",
            lastError: "",
            requests: 48,
            failures: 1,
            cached: 17,
            notCached: 6,
            linksRefreshed: 3,
            reused: 5,
            api: {
              attempts: 156,
              endpoints: { add: 22, list: 9, info: 71, select: 20, link: 24, delete: 10 },
              retries: 2,
              limited: 2,
              http429: 2,
              listHits: 7,
              reuseHits: 5,
              rememberedHits: 18,
              firstPlays: 17,
              firstPlayCalls: 73,
            },
            latency: {
              resolveMs: 812.5,
              linkMs: 140.2,
              connectMs: 35.1,
              ttfbMs: 92.4,
              throughputBps: 48_000_000,
              samples: 12,
            },
            lastSuccess: new Date(Date.now() - 4 * 60_000).toISOString(),
          };
        return {
          ...base,
          state: "backoff",
          lastError: "rate limited by the service; retrying shortly",
          backoffUntil: new Date(Date.now() + 5 * 60_000).toISOString(),
          requests: 9,
          failures: 3,
          cached: 2,
          notCached: 4,
          linksRefreshed: 0,
          reused: 0,
          latency: idle,
          lastSuccess: new Date(Date.now() - 3 * 3600_000).toISOString(),
        };
      }),
    };
  }

  function dashboard() {
    const session = catalog[3]!;
    const remoteTitle = catalog[1] ?? session;
    const overrideKbps = Math.max(0, Number(state.config.transcode.remoteUploadBudgetKbps) || 0);
    const measuredKbps = 42_000;
    const effectiveKbps = overrideKbps > 0 ? overrideKbps : measuredKbps;
    const usableKbps = Math.round(effectiveKbps * 0.85);
    const uploadBudget = {
      enabled: true,
      source: overrideKbps > 0 ? "override" : "measured",
      overrideKbps,
      measuredKbps,
      measuredSamples: 18,
      effectiveKbps,
      usableKbps,
      etherRemoteKbps: 16_000,
      etherRemoteStreams: 2,
      budgetCappedStreams: 1,
      otherPmsKbps: 6_500,
      otherPmsStreams: 1,
      totalUsedKbps: 22_500,
      otherServers: [
        {
          machineId: "pms-home",
          name: "Home Plex Server",
          source: "linked_pms",
          remoteStreams: 1,
          bandwidthKbps: 6_500,
          reachable: true,
          lastChecked: NOW().toISOString(),
        },
      ],
    };
    const remoteSession = {
      ID: "s2",
      UserID: 3,
      Key: remoteTitle.key,
      ratingKey: Number(remoteTitle.key) || 0,
      TitleName: `${remoteTitle.title} (${remoteTitle.year})`,
      Username: "Jordan Lee",
      Client: {
        product: "Plex for iOS",
        deviceName: "iPad Pro",
        platform: "iOS",
        remote: true,
      },
      ip: "198.51.100.24",
      Decision: "transcode",
      decisionReason: "Shared upload budget (8.0 Mbps cap)",
      decisionVideoCopy: false,
      decisionAudioCopy: false,
      bitrateKbps: 8000,
      bitrateCapKbps: 8000,
      bitrateCapSource: "upload_budget",
      sourceVideo: "HEVC 2160p HDR10",
      outputVideo: "H.264 1080p SDR",
      sourceAudio: "TrueHD 7.1",
      outputAudio: "AAC 2.0",
      container: "mpegts",
      BytesServed: 1.4 * 1024 ** 3,
      BufferedBytes: 92 * 1024 ** 2,
      BufferedUntil: 2250e9,
      SpeedBps: 1.05e6,
      StartedAt: new Date(Date.now() - 24 * 60000).toISOString(),
      Info: { size: 22.8 * 1024 ** 3, duration: 132 * 60 * 1e9, bitrateKbps: 23_400 },
      viewOffset: 34 * 60 * 1000,
      origin: "Usenet",
      experience: {
        requested: true,
        pictured: true,
        startTime: 2.3e9,
        bufferEvents: 0,
        buffering: 0,
      },
      Resolution: {
        release: {
          name: `${remoteTitle.title.replace(/ /g, ".")}.${remoteTitle.year}.2160p.REMUX.HEVC.TrueHD`,
        },
      },
    };
    return {
      setupSteps: setupSteps(),
      names: [
        "cache",
        "catalog",
        "database",
        "debrid",
        "hwaccel",
        "indexers",
        "providers",
        "remote",
        "sessions",
        "upload-budget",
      ],
      setupComplete: state.config.setupComplete,
      version: state.boot.version,
      sections: {
        "upload-budget": uploadBudget,
        cache: { used: 87 * 1024 ** 3, budget: state.config.cache.budgetBytes },
        database: state.clustered
          ? { clustered: true, state: "ok", reachable: true, held: 0 }
          : {
              clustered: false,
              state: "ok",
              reachable: true,
              integrity: opts.damagedDatabase
                ? {
                    ok: false,
                    findings: [
                      "*** in database main ***",
                      "Page 4127: btreeInitPage() returns error code 11",
                    ],
                    checked: new Date(Date.now() - 20 * 60_000).toISOString(),
                    tookSeconds: 1.2,
                  }
                : {
                    ok: true,
                    checked: new Date(Date.now() - 3 * 3600_000).toISOString(),
                    tookSeconds: 0.9,
                  },
            },
        catalog: {
          movies: 24929,
          shows: 24298,
          browsableMovies: 24928,
          homeReleaseChecked: 24680,
          homeReleasePending: 248,
          browsableShows: 24298,
          lastSync: new Date(Date.now() - 5 * 3600_000).toISOString(),
          sync: { running: false, titlesSeen: 63500, titlesAdded: 284 },
          trending: ["movie", "show"].flatMap((mediaType) =>
            ["day", "week"].map((window) => ({
              mediaType,
              window,
              source: "TMDB",
              scope: "Global",
              titles: 200,
              state: "fresh",
              fetchedAt: new Date(Date.now() - 1800000).toISOString(),
            })),
          ),
        },
        debrid: debridStatus(),
        sources: {
          startup: [
            { protocol: "usenet", average: 2_140_000_000, last: 1_980_000_000, samples: 37 },
            { protocol: "torrent", average: 410_000_000, last: 380_000_000, samples: 22 },
          ],
        },
        hwaccel: opts.hwaccel ?? {
          os: "darwin",
          arch: "arm64",
          ffmpeg: "8.0.1",
          videoEncoderLimit: state.config.transcode?.maxTranscodes ?? 2,
          filtersProbed: true,
          toneMap: ["videotoolbox"],
          colour: ["zscale", "colorspace", "scale"],
          hwaccel: { name: "videotoolbox", encoders: ["h264_videotoolbox", "hevc_videotoolbox"] },
          enhancement: {
            backend: "nvenc-ngx",
            available: false,
            reason: "An NVIDIA RTX GPU and NVEnc are required on this server.",
            active: 0,
          },
        },
        indexers: (state.config.indexers ?? []).map((ix, i) => ({
          indexer: ix.name,
          day: new Date().toISOString().slice(0, 10),
          hitsUsed: 6 + i * 15,
          hitsLimit: ix.dailyLimit,
          hitsRemaining: Math.max(0, ix.dailyLimit - 6 - i * 15),
          grabsUsed: 3,
          grabsLimit: ix.dailyGrabs,
          grabsRemaining: ix.dailyGrabs - 3,
          enabled: ix.enabled,
          torrent: ix.protocol === "torrent" || ix.kind === "stremio",
        })),
        providers: (state.config.usenet.providers ?? []).map((p, i) => ({
          id: p.id,
          name: p.name,
          host: p.host,
          backfill: p.backfill,
          state: "ok",
          configuredConnections: p.connections,
          activeConnections: i === 0 ? 6 : 0,
          ...(state.clustered ? { nodeShare: Math.ceil(p.connections / 3) } : {}),
        })),
        remote: {
          claimed: true,
          ownerUsername: "Braedon",
          publicAddress: "203.0.113.7",
          remoteAccess: state.config.plex.remoteAccess,
          listed: true,
          presence: { connected: true },
          upnp: { active: true },
          cert: {
            present: true,
            verified: true,
            subject: "*.abc.plex.direct",
            notBefore: new Date(Date.now() - 30 * 86400000).toISOString(),
            notAfter: new Date(Date.now() + 80 * 86400000).toISOString(),
          },
        },
        sessions: opts.liveProviderDemo
          ? [
              {
                ID: "live-sports",
                UserID: 2,
                Key: 0,
                TitleName: "Saturday Football",
                Username: "Sam Rivera",
                Client: {
                  product: "Plex for Apple TV",
                  deviceName: "Living Room",
                  platform: "tvOS",
                },
                Decision: "copy",
                decisionVideoCopy: true,
                decisionAudioCopy: true,
                BytesServed: 54 * 1024 ** 2,
                StartedAt: new Date(Date.now() - 4 * 60_000).toISOString(),
                Info: { bitrateKbps: 6900 },
                Failovers: 1,
                origin: "Live TV",
                liveChannel: {
                  id: "mock-sports",
                  name: "Sports Live",
                  number: "24",
                  provider: {
                    sourceName: "Northstar TV",
                    loginId: "primary",
                    loginLabel: "jo***",
                    loginSlot: 2,
                    loginLimit: 3,
                    localInUse: 2,
                  },
                  previousProvider: { sourceName: "Weekend Sports" },
                },
              },
            ]
          : [
              {
                ID: "s1",
                UserID: 2,
                Key: session.key,
                ratingKey: Number(session.key) || 0,
                TitleName: `${session.title} (${session.year})`,
                Username: "Sam Rivera",
                Client: {
                  product: "Plex for Apple TV",
                  deviceName: "Living Room",
                  platform: "tvOS",
                  remote: false,
                },
                ip: "192.168.1.42",
                Decision: "directplay",
                decisionVideoCopy: true,
                decisionAudioCopy: true,
                bitrateKbps: 6540,
                sourceVideo: "H.264 1080p SDR",
                outputVideo: "H.264 1080p SDR",
                sourceAudio: "EAC3 5.1",
                outputAudio: "EAC3 5.1",
                container: "mkv",
                BytesServed: 2.1 * 1024 ** 3,
                BufferedBytes: 183 * 1024 ** 2,
                BufferedUntil: 3000e9,
                SpeedBps: 3.2e6,
                StartedAt: new Date(Date.now() - 40 * 60000).toISOString(),
                Info: { size: 5.4 * 1024 ** 3, duration: 118 * 60 * 1e9, bitrateKbps: 6540 },
                viewOffset: 46 * 60 * 1000,
                origin: "Real-Debrid",
                experience: {
                  requested: true,
                  pictured: true,
                  startTime: 1.8e9,
                  bufferEvents: 0,
                  buffering: 0,
                },
                Resolution: {
                  release: {
                    name: `${session.title.replace(/ /g, ".")}.${session.year}.1080p.WEB-DL.H264`,
                  },
                },
              },
              ...(opts.remoteBudgetDemo ? [remoteSession] : []),
            ],
      },
    };
  }

  const currentNotes = `## Console
- Release notes open in Ether after an update and can be reopened from Maintenance.
- Updates shows the last and next checks. Use **Check now** whenever you want to check manually.

## Playback
- Continue watching keeps your latest position when switching between apps.
- The activity page shows whether a stream plays directly or uses transcoding.`;
  const availableNotes = `## Libraries
- Library scans show which folder Ether is indexing.
- Search includes season and episode matches.

## Playback
- Subtitle changes keep the current release playing.`;
  let notesUnseen = opts.whatsNew === true;
  const availableVersion = "0.9.1";
  const notesUrl = "https://github.com/braedonsaunders/ether/releases";
  let lastUpdateCheck = Date.now() - 3600_000;
  let updateRequested = false;
  let updateForced = false;
  function updateStatus() {
    return {
      enabled: true,
      current: state.boot.version,
      available: {
        version: availableVersion,
        kind: "binary",
        notes: availableNotes,
        notesUrl,
        staged: true,
      },
      capability: "self-apply",
      state: updateForced
        ? "applying"
        : updateRequested
          ? "waiting-for-idle"
          : "waiting-for-window",
      autoUpdate: state.config.updates.autoUpdate,
      requested: updateRequested,
      force: updateForced,
      lastCheck: new Date(lastUpdateCheck).toISOString(),
      nextCheck: new Date(lastUpdateCheck + 6 * 3600_000).toISOString(),
      window: {
        start: state.config.updates.maintenanceStart ?? "03:00",
        end: state.config.updates.maintenanceEnd ?? "06:00",
        open: false,
      },
      activeSessions: 1,
      quietPeriodSeconds: 600,
      confirming: false,
    };
  }

  // ---- Cluster -----------------------------------------------------------------------

  let clusterSetupState = "idle";
  let nodePaused = false;
  const nodeControls = new Map<
    string,
    { control: ClusterControl; patch?: ClusterControlRequest["patch"]; due?: number }
  >();
  function clusterNodes(): ClusterNode[] {
    const ago = (s: number) => new Date(Date.now() - s * 1000).toISOString();
    const node = (n: Partial<ClusterNode> & { id: string; name: string }): ClusterNode => ({
      incarnation: `boot-${n.id}`,
      version: state.boot.version,
      addrs: [`http://${n.id}.test:32400`],
      caps: { transcode: true, hwAccel: "videotoolbox", egressIp: "203.0.113.7" },
      load: 0,
      startedAt: ago(86400),
      lastSeen: ago(3),
      leader: false,
      live: true,
      ...n,
    });
    if (!state.clustered)
      return [node({ id: "n-solo", name: state.boot.serverName, leader: true, load: 1 })];
    return [
      node({
        id: "n-den",
        name: "den",
        leader: true,
        load: 1,
        draining: nodePaused,
        caps: {
          transcode: true,
          hwAccel: "videotoolbox",
          weight: state.config.server.clusterWeight || 1,
          egressIp: "203.0.113.7",
        },
      }),
      node({
        id: "n-attic",
        name: "attic",
        draining: true,
        load: 2,
        caps: {
          roles: ["transcode", "delivery"],
          transcode: true,
          hwAccel: "nvenc",
          weight: 2,
          egressIp: "198.51.100.20",
        },
      }),
      node({
        id: "n-source",
        name: "source-02",
        load: 3,
        caps: { roles: ["source"], transcode: false, weight: 2 },
      }),
      node({
        id: "n-edge",
        name: "edge-01",
        load: 4,
        caps: { roles: ["delivery"], transcode: false, weight: 1 },
      }),
      node({
        id: "n-garage",
        name: "garage",
        version: "0.8.4",
        live: false,
        lastSeen: ago(3 * 3600),
        caps: { transcode: true, egressIp: "198.51.100.20" },
      }),
    ];
  }

  function clusterStatus(): ClusterStatus {
    const nodes = clusterNodes();
    const self = nodes[0]!;
    // Mirrors internal/app egressWarning: live nodes reaching out from several IPs.
    const byIP = new Map<string, string[]>();
    for (const n of nodes)
      if (n.live && n.caps.egressIp)
        byIP.set(n.caps.egressIp, [...(byIP.get(n.caps.egressIp) ?? []), n.name]);
    const warnings =
      byIP.size > 1
        ? [
            `nodes reach providers from different public addresses (${[...byIP]
              .sort()
              .map(([ip, names]) => `${ip}: ${names.sort().join(", ")}`)
              .join(
                "; ",
              )}); a debrid account can be limited when it is used from more than one address`,
          ]
        : undefined;
    return {
      mockConsole: {
        // Explicit synthetic diagnostic evidence, independent of heartbeat liveness.
        health: Object.fromEntries(
          nodes.map((n) => [
            n.id,
            {
              nodeId: n.id,
              boot: n.incarnation!,
              sampledAt: new Date().toISOString(),
              level: n.id === "n-garage" ? ("error" as const) : ("ok" as const),
            },
          ]),
        ),
        metrics: Object.fromEntries(
          nodes.map((n, i) => [
            n.id,
            { transcodes: i === 1 ? 2 : 0, capacity: i === 1 ? 8 : 4, load: i === 1 ? 63 : 18 },
          ]),
        ),
        events: [
          { node: "attic", kind: "drain", at: new Date(Date.now() - 60000).toISOString() },
          { node: "den", kind: "failover", at: new Date(Date.now() - 180000).toISOString() },
          { node: "source-02", kind: "join", at: new Date(Date.now() - 600000).toISOString() },
        ],
        transition: { node: "n-attic", role: "transcode", phase: "drain" },
      },
      selfRuntime: {
        nodeId: self.id,
        boot: self.incarnation!,
        runtimeSampledAt: new Date().toISOString(),
        protocol: 1,
        started: true,
        readyExisting: true,
        acceptingNew: !nodePaused,
        draining: nodePaused,
      },
      warnings,
      mode: state.clustered ? "postgres" : "solo",
      self,
      leader: self.id,
      epoch: state.clustered ? 4 : 1,
      nodes,
      ...(state.clustered
        ? {
            health: {
              channel: "ether_cluster",
              busConnected: true,
              lastHeartbeat: new Date(Date.now() - 2000).toISOString(),
              clockSkewNs: 12_000_000,
            },
          }
        : {}),
    };
  }

  const metadata = createMetadataMock();
  const plexWatch = new Map<
    number,
    {
      connected: boolean;
      direction: number;
      importInterval: number;
      lastImport: number;
      lastFullImport: number;
      lastSend: number;
      nextImport: number;
      problem: string;
      needsReconnect: boolean;
      pending: number;
      failed: number;
      restricted: boolean;
      pinCode?: string;
      pinUrl?: string;
      accountMatch: boolean;
      username?: string;
      plexId?: number;
    }
  >();

  const ops = createOpsMock({
    startup: opts.startup,
    pace: opts.opsPace,
    viewers: opts.viewers,
    restore: opts.restore,
    clustered: () => state.clustered,
    backups: () => state.backups,
  });

  const viewerStreamRules = new Map<string, unknown>();
  const restoredHidden = new Set<string>();
  let nextMockPlexViewerId = 200;
  const mockPlexCandidateServers = [
    {
      serverId: 1,
      serverName: "Living room PMS",
      plexDirectoryTotal: 42,
      filteredOutCount: 19,
      accounts: [
        {
          serverId: 1,
          serverName: "Living room PMS",
          accountId: 1,
          accountName: "Alex Rivera",
          isOwner: true,
          matchedViewerId: 1,
          matchedViewerName: "Alex Rivera",
          matchedViewerUsername: "owner",
          plexRelation: "owner",
          plexSources: ["owner"],
        },
        {
          serverId: 1,
          serverName: "Living room PMS",
          accountId: 1003,
          accountName: "The Kids",
          isOwner: false,
          matchedViewerId: 3,
          matchedViewerName: "The Kids",
          matchedViewerUsername: "kids",
          plexUsername: "kids",
          plexRelation: "home",
          plexSources: ["home", "ether_share"],
        },
        {
          serverId: 1,
          serverName: "Living room PMS",
          accountId: 10421011,
          accountName: "Morgan Rivera",
          isOwner: false,
          matchedViewerId: 0,
          plexUsername: "morganr",
          plexRelation: "home",
          plexSources: ["home", "pms_share"],
        },
        {
          serverId: 1,
          serverName: "Living room PMS",
          accountId: 1004,
          accountName: "Jordan Lee",
          isOwner: false,
          matchedViewerId: 4,
          matchedViewerName: "Jordan Lee",
          matchedViewerUsername: "jordan",
          plexUsername: "jordan",
          plexRelation: "shared",
          plexSources: ["ether_share", "pms_share"],
        },
        {
          serverId: 1,
          serverName: "Living room PMS",
          accountId: 25815033,
          accountName: "Sam Rivera",
          isOwner: false,
          matchedViewerId: 0,
          suggestedViewerId: 2,
          suggestedViewerName: "Sam Rivera",
          plexUsername: "samr",
          plexRelation: "shared",
          plexSources: ["pms_share", "friend"],
        },
        {
          serverId: 1,
          serverName: "Living room PMS",
          accountId: 703916194,
          accountName: "Alice Chen",
          isOwner: false,
          matchedViewerId: 0,
          plexUsername: "alice99",
          plexRelation: "shared",
          plexSources: ["ether_share", "pms_share"],
        },
        {
          serverId: 1,
          serverName: "Living room PMS",
          accountId: 99002211,
          accountName: "Devon Patel",
          isOwner: false,
          matchedViewerId: 0,
          plexUsername: "dpatel",
          plexRelation: "pending",
          plexSources: ["pending_invite"],
        },
        {
          serverId: 1,
          serverName: "Living room PMS",
          accountId: 88123456,
          accountName: "Taylor Brooks",
          isOwner: false,
          matchedViewerId: 0,
          plexUsername: "tbrooks",
          plexRelation: "friend",
          plexSources: ["community_friend", "friend"],
        },
        {
          serverId: 1,
          serverName: "Living room PMS",
          accountId: 143639010,
          accountName: "Patricson Poliard",
          isOwner: false,
          matchedViewerId: 0,
          plexRelation: "historical",
          plexSources: ["pms_history"],
        },
      ] as Array<{
        serverId: number;
        serverName: string;
        accountId: number;
        accountName: string;
        isOwner: boolean;
        matchedViewerId: number;
        matchedViewerName?: string;
        matchedViewerUsername?: string;
        suggestedViewerId?: number;
        suggestedViewerName?: string;
        plexUsername?: string;
        plexRelation?: string;
        plexSources?: string[];
      }>,
    },
  ];

  // ---- Router ------------------------------------------------------------------------

  const viewerCatalogRules = new Map<number, unknown>();

  const mockLibs: Record<string, unknown>[] = [
    {
      id: 1,
      key: "movies",
      name: "Movies",
      source: "builtin",
      mediaType: "movie",
      sort: 1,
      hidden: false,
      createdAt: "2026-01-01T00:00:00Z",
      updatedAt: "2026-01-01T00:00:00Z",
      itemCount: 1200,
    },
    {
      id: 2,
      key: "shows",
      name: "TV Shows",
      source: "builtin",
      mediaType: "show",
      sort: 2,
      hidden: false,
      createdAt: "2026-01-01T00:00:00Z",
      updatedAt: "2026-01-01T00:00:00Z",
      itemCount: 800,
    },
    {
      id: 3,
      key: "pk-demo-book",
      name: "Books",
      source: "plugin",
      pluginId: "demo",
      kindKey: "book",
      present: {
        vela: { type: "movies" },
        mediaBrowser: { collectionType: "books", itemTypes: { book: "Book" } },
      },
      sort: 3,
      hidden: false,
      createdAt: "2026-01-01T00:00:00Z",
      updatedAt: "2026-01-01T00:00:00Z",
      itemCount: 2,
      pluginName: "Demo books",
      kind: {
        key: "book",
        label: "Books",
        media: "book",
        library: { name: "Books" },
        levels: [{ key: "book", label: "Book", role: "book", playable: true }],
        fields: [
          { key: "author", type: "people", label: "Author", indexed: true },
          {
            key: "subject",
            type: "strings",
            label: "Subject",
            indexed: true,
            values: [
              "Science Fiction",
              "Space Opera",
              "Cyberpunk",
              "Classic Literature",
              "Solarpunk",
              "Historical Fiction",
            ],
          },
          { key: "series", type: "string", label: "Series", indexed: true },
          { key: "publisher", type: "string", label: "Publisher", indexed: true },
          {
            key: "format",
            type: "enum",
            label: "Format",
            indexed: true,
            enum: ["Hardcover", "EPUB", "Audiobook", "Paperback"],
          },
          { key: "pages", type: "int", label: "Page count", indexed: true },
          { key: "duration", type: "duration", label: "Audio length", indexed: true },
          { key: "published", type: "date", label: "Published", indexed: true },
          { key: "abridged", type: "bool", label: "Abridged", indexed: true },
          { key: "language", type: "string", label: "Book language", indexed: true },
        ],
        present: {},
        browse: {
          sorts: ["title", "added", "year", "rating"],
          facets: ["author", "subject", "series"],
          search: true,
        },
        sync: { mode: "push" },
      },
      sync: { pluginId: "demo", kindKey: "book", generation: 1, items: 2 },
    },
  ];
  mockLibs.push({
    id: 4,
    key: "weekend",
    name: "Weekend movies",
    source: "catalogue",
    sources: ["stremio:cinemeta:popular", "stremio:cinemeta:classics"],
    rules: [],
    sort: 4,
    hidden: true,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  });
  mockLibs.push({
    id: 5,
    key: "weekend-collection",
    name: "Weekend collection",
    source: "collection",
    sources: ["tmdb-movies"],
    collectionId: 31,
    mediaType: "movie",
    sort: 5,
    hidden: true,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  });
  mockLibs.push({
    id: 6,
    key: "family-films",
    name: "Family films",
    source: "catalogue",
    sources: ["tmdb-movies"],
    rules: [
      {
        name: "",
        action: "show",
        mediaType: "movie",
        match: "all",
        rules: [{ field: "genre", op: "in", value: ["Family"] }],
      },
    ],
    visibilityDefault: "hide",
    sort: 6,
    hidden: true,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
  });
  mockLibs.push({
    id: 7,
    key: "anime-shows",
    name: "Anime Shows",
    source: "catalogue",
    sources: ["tmdb-tv"],
    mediaType: "show",
    rules: [
      {
        name: "",
        action: "show",
        mediaType: "show",
        match: "all",
        rules: [{ field: "genre", op: "in", value: ["Animation"] }],
      },
    ],
    visibilityDefault: "hide",
    sort: 7,
    hidden: false,
    itemCount: 4500,
  });
  mockLibs.push({
    id: 8,
    key: "pk-demo-clips",
    name: "Studio Clips",
    source: "plugin",
    pluginId: "demo-clips",
    kindKey: "clip",
    present: {
      vela: { type: "movies" },
      plex: { base: "movie" },
      mediaBrowser: { collectionType: "movies", itemTypes: { clip: "Movie" } },
      stremio: { type: "movie" },
      xtream: { category: "movie" },
      silo: { type: "movies" },
    },
    sort: 8,
    hidden: false,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    itemCount: 24,
    pluginName: "Studio Clips",
    kind: {
      key: "clip",
      label: "Clips",
      media: "video",
      library: { name: "Studio Clips", card: "landscape" },
      levels: [{ key: "clip", label: "Clip", role: "item", playable: true }],
      present: { vela: { type: "movies" } },
      browse: { sorts: ["added", "title"], search: true },
      sync: { mode: "push" },
    },
    sync: { pluginId: "demo-clips", kindKey: "clip", generation: 1, items: 24 },
  });
  mockLibs.push({
    id: 9,
    key: "pk-demo-music",
    name: "Field Recordings",
    source: "plugin",
    pluginId: "demo-music",
    kindKey: "recording",
    present: {
      vela: { type: "shows" },
      plex: { base: "artist" },
      mediaBrowser: {
        collectionType: "music",
        itemTypes: { album: "MusicAlbum", track: "Audio" },
      },
    },
    sort: 9,
    hidden: false,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    itemCount: 18,
    pluginName: "Field Recordings",
    kind: {
      key: "recording",
      label: "Field recordings",
      media: "music",
      library: { name: "Field Recordings", card: "square" },
      levels: [
        { key: "album", label: "Album", role: "album", container: true },
        { key: "track", label: "Track", role: "track", playable: true },
      ],
      present: {
        mediaBrowser: {
          collectionType: "music",
          itemTypes: { album: "MusicAlbum", track: "Audio" },
        },
      },
      browse: { sorts: ["title", "added"], search: true },
      sync: { mode: "push" },
    },
    sync: { pluginId: "demo-music", kindKey: "recording", generation: 1, items: 18 },
  });
  const pagesMock = createPagesMock({
    metadataListsAvailable: () =>
      metadataSources().some((source) => source.id === "tmdb" && source.configured),
    catalog,
    card,
    libraries: () =>
      mockLibs
        .filter((l) => !l.hidden)
        .map((l) => {
          const kind = l.kind as { media?: string } | undefined;
          const media = kind?.media;
          const plugin = l.source === "plugin" ? String(l.pluginName ?? l.name) : undefined;
          return {
            id: Number(l.id),
            name: String(l.name),
            experience:
              media === "music" ? "listen" : media === "book" ? "read" : ("watch" as const),
            mediaType:
              l.source === "plugin" ? undefined : l.mediaType === "show" ? "show" : "movie",
            plugin,
          };
        }),
    viewers: () => VIEWERS.map((v) => ({ id: v.id, title: v.title })),
    collections: () => state.collections,
  });
  const libraryTabs = createLibraryTabsMock({
    catalog,
    getCollections: () => state.collections,
    matchesRule: matches,
    toCard: card,
  });
  const mockMatches: Record<string, unknown>[] = [];
  function mockResolvePresentations(lib: Record<string, unknown>) {
    const present = (lib.present && typeof lib.present === "object" ? lib.present : {}) as Record<
      string,
      unknown
    >;
    const adapterOnly = Boolean(present.adapterOnly);
    if (lib.source !== "plugin") {
      const isShow = lib.mediaType === "show";
      const avail: Record<string, unknown> = {
        vela: { type: isShow ? "shows" : "movies" },
        plex: { base: isShow ? "show" : "movie" },
        mediaBrowser: { collectionType: isShow ? "tvshows" : "movies" },
        stremio: { type: isShow ? "series" : "movie" },
        xtream: { category: isShow ? "series" : "movie" },
        silo: { type: isShow ? "tvshows" : "movies" },
      };
      const hints: Record<string, string> = {
        vela: isShow ? "shows" : "movies",
        plex: isShow ? "show" : "movie",
        mediaBrowser: isShow ? "tvshows" : "movies",
        stremio: isShow ? "series" : "movie",
        xtream: isShow ? "series" : "movie",
        silo: isShow ? "tvshows" : "movies",
      };
      const isBuiltin = lib.source === "builtin";
      return {
        mediaClass: "video",
        adapterOnly: !isBuiltin && adapterOnly,
        effective: present,
        available: avail,
        apps: ["vela", "plex", "mediaBrowser", "stremio", "xtream", "silo"].map((app) => ({
          app,
          available: true,
          enabled: isBuiltin ? true : !adapterOnly && Boolean(present[app]),
          defaultOn: isBuiltin,
          source: "inferred",
          hintValue: hints[app],
        })),
      };
    }
    const kind = (lib.kind && typeof lib.kind === "object" ? lib.kind : {}) as {
      media?: string;
      levels?: { key: string; role?: string }[];
      present?: Record<string, Record<string, unknown>>;
    };
    const kp = kind.present ?? {};
    const levels = kind.levels ?? [];
    const multi = levels.length >= 2;
    const mediaClass =
      kind.media ||
      (levels.some((l) => l.key === "book")
        ? "book"
        : levels.some((l) => l.key === "album" || l.key === "track")
          ? "music"
          : "video");
    const avail: Record<string, unknown> = {};
    const apps: Record<string, unknown>[] = [];
    const velaHint = String(kp.vela?.type ?? (multi ? "shows" : "movies"));
    avail.vela = { type: velaHint };
    apps.push({
      app: "vela",
      available: true,
      enabled: !adapterOnly && Boolean(present.vela),
      defaultOn: true,
      source: kp.vela?.type ? "plugin" : "inferred",
      hintValue: velaHint,
    });
    if (
      kp.plex?.base ||
      mediaClass === "video" ||
      mediaClass === "music" ||
      mediaClass === "photo"
    ) {
      const plexBase = String(
        kp.plex?.base ??
          (mediaClass === "music"
            ? "artist"
            : mediaClass === "photo"
              ? "photo"
              : multi
                ? "show"
                : "movie"),
      );
      avail.plex = { base: plexBase };
      apps.push({
        app: "plex",
        available: true,
        enabled: !adapterOnly && Boolean(present.plex),
        defaultOn: true,
        source: kp.plex?.base ? "plugin" : "inferred",
        hintValue: plexBase,
      });
    } else {
      apps.push({
        app: "plex",
        available: false,
        enabled: false,
        defaultOn: false,
        source: "unavailable",
        reason:
          mediaClass === "book"
            ? "Plex does not have a book library type."
            : mediaClass === "audiobook"
              ? "Plex does not have an audiobook library type."
              : "Plex does not have a podcast library type.",
      });
    }
    const mbType = String(
      kp.mediaBrowser?.collectionType ??
        (mediaClass === "music"
          ? "music"
          : mediaClass === "book"
            ? "books"
            : mediaClass === "audiobook"
              ? "audiobooks"
              : mediaClass === "photo"
                ? "photos"
                : multi
                  ? "tvshows"
                  : "movies"),
    );
    avail.mediaBrowser = { collectionType: mbType };
    apps.push({
      app: "mediaBrowser",
      available: true,
      enabled: !adapterOnly && Boolean(present.mediaBrowser),
      defaultOn: true,
      source: kp.mediaBrowser?.collectionType ? "plugin" : "inferred",
      hintValue: mbType,
    });
    for (const [app, field, val, reason] of [
      [
        "stremio",
        "type",
        multi ? "series" : "movie",
        "Stremio only supports movies, shows, and channels.",
      ],
      [
        "xtream",
        "category",
        multi ? "series" : "movie",
        "Xtream only supports movies, series, and live TV.",
      ],
      [
        "silo",
        "type",
        multi ? "tvshows" : "movies",
        "Silo only supports movie and TV show libraries.",
      ],
    ] as const) {
      const declared = kp[app]?.[field];
      if (declared || mediaClass === "video") {
        const hint = String(declared ?? val);
        avail[app] = { [field]: hint };
        apps.push({
          app,
          available: true,
          enabled: !adapterOnly && Boolean(present[app]),
          defaultOn: true,
          source: declared ? "plugin" : "inferred",
          hintValue: hint,
        });
      } else {
        apps.push({
          app,
          available: false,
          enabled: false,
          defaultOn: false,
          source: "unavailable",
          reason,
        });
      }
    }
    return { mediaClass, adapterOnly, effective: present, available: avail, apps };
  }

  if (metadataMode === "both" || metadataMode === "tvdb") state.config.catalog.tvdbKey = "••••";
  if (metadataMode === "both") mockLibs[1]!.metadataSource = "tmdb";

  const episodeOverrides = new Map<string, EpisodeOrder>();
  const tmdbAired = { source: "tmdb", kind: "aired" };
  const tvdbAired = { source: "tvdb", kind: "aired" };
  function episodeOrder(key: string): EpisodeOrderDetails | undefined {
    const orderOptions = metadataSources().filter((source) => source.configured && source.shows);
    const library = mockLibs.find((item) => item.id === 2)!;
    const defaultSource =
      orderOptions.find((source) => source.id === library.metadataSource) ?? orderOptions[0];
    if (!defaultSource) return;
    const libraryDefault = defaultSource.id === "tvdb" ? tvdbAired : tmdbAired;
    const override = episodeOverrides.get(key);
    const effective = override ?? libraryDefault;
    const options = orderOptions.flatMap((source): EpisodeOrder[] =>
      source.id === "tvdb"
        ? [tvdbAired, { source: "tvdb", kind: "dvd" }, { source: "tvdb", kind: "absolute" }]
        : [tmdbAired, { source: "tmdb", kind: "group:mock-story", label: "Story arcs" }],
    );
    return {
      effective,
      override,
      libraryDefault,
      libraryName: String(library.name),
      options,
      gap:
        effective.source === "tmdb" && orderOptions.some((source) => source.id === "tvdb")
          ? { order: tvdbAired, count: 4120 }
          : undefined,
    };
  }

  function matchProviderId(key: string, source: string) {
    return String((Number(key) % 1e15) + (source === "tvdb" ? 100000 : 0));
  }

  function hydrateLib(lib: Record<string, unknown>) {
    if (!lib.sources)
      lib.sources = [
        lib.source === "plugin"
          ? `plugin:${lib.pluginId}:${lib.kindKey}`
          : lib.mediaType === "show"
            ? metadataSources().find((source) => source.configured && source.shows)?.id === "tvdb"
              ? "tvdb"
              : "tmdb-tv"
            : "tmdb-movies",
      ];
    if (lib.source !== "plugin" && lib.rules === undefined) {
      const cat = state.config.catalog as { exclusions?: unknown[]; visibilityDefault?: string };
      lib.rules = structuredClone(cat.exclusions ?? []);
      lib.visibilityDefault = cat.visibilityDefault ?? "show";
    }
    const tabsDto = libraryTabs.getTabs(lib);
    lib.tabsCustomized = tabsDto.customized;
    lib.tabs = tabsDto.tabs;
    lib.effectiveTabs = tabsDto.effectiveTabs;
    lib.presentations = mockResolvePresentations(lib);
    return lib;
  }
  function mockLibraries(method: string, seg: string[], url: URL, body: unknown): Response {
    const b = (body && typeof body === "object" ? body : {}) as Record<string, unknown>;
    if (seg[0] === "plugins-setup") {
      if (!seg[1] && method === "GET") {
        const demoLibs = mockLibs.filter((l) => l.pluginId === "demo");
        return json({
          plugins: [
            {
              pluginId: "demo",
              pluginName: "Demo books",
              summary: "EPUB and audiobook library",
              kinds: demoLibs.map((l) => l.kind).filter(Boolean),
              libraries: demoLibs,
            },
          ],
        });
      }
      if (method === "GET" || (method === "POST" && seg[2] === "ensure"))
        return json({
          pluginId: seg[1],
          pluginName: "Demo books",
          steps: [],
          blockers: [],
          settings: {},
          schema: {},
          kinds: mockLibs
            .filter((l) => l.pluginId === seg[1])
            .map((l) => l.kind)
            .filter(Boolean),
          libraries: mockLibs.filter((l) => l.pluginId === seg[1]),
        });
      if (seg[2] === "settings") return json({ fieldErrors: null });
      return json({ ok: true });
    }
    if (seg[1] === "collection-sources") {
      // The "From a collection" picker: the owner's collections, never their
      // members.
      return json({
        collections: state.collections
          .filter((c) => typeof c.id === "string")
          .map((c, i) => ({
            // Seeded collections are addressed by slug (c1, c2, builtin-
            // recent); the owner one the picker offers is a number.
            id: /^\d+$/.test(c.id ?? "") ? Number(c.id) : 31 + i,
            name: c.name,
            mediaType: c.mediaType,
            itemCount: c.itemCount ?? 0,
          })),
      });
    }
    if (seg[1] === "catalogue-sources")
      return json({
        sources: [
          {
            id: "tmdb-movies",
            name: "TMDB Movies",
            owner: "Built in",
            group: "builtin",
            mediaTypes: ["movie"],
            health: "ok",
            itemCount: 48260,
            libraries: [{ id: 1, name: "Movies" }],
          },
          {
            id: "tmdb-tv",
            name: "TMDB TV shows",
            owner: "Built in",
            group: "builtin",
            mediaTypes: ["show"],
            health: "ok",
            itemCount: 12480,
            libraries: [{ id: 2, name: "TV Shows" }],
          },
          {
            id: "tvdb",
            name: "TheTVDB",
            owner: "TheTVDB",
            group: "builtin",
            mediaTypes: ["show"],
            health: "unknown",
            libraries: [],
          },
          {
            id: "plugin:demo:book",
            name: "Books",
            owner: "Open Library",
            group: "plugin",
            mediaTypes: ["book"],
            health: "ok",
            pluginId: "demo",
            kindKey: "book",
            libraries: [],
          },
          {
            id: "plugin:demo-clips:clip",
            name: "Clips",
            owner: "Studio Clips",
            group: "plugin",
            mediaTypes: ["movie"],
            health: "ok",
            pluginId: "demo-clips",
            kindKey: "clip",
            libraries: [],
          },
          {
            id: "stremio:cinemeta:popular",
            name: "Popular",
            owner: "Cinemeta",
            group: "stremio",
            mediaTypes: ["movie"],
            health: "ok",
            // Cut off at the default 500 titles per list.
            itemCount: 468,
            listed: 500,
            limit: 500,
            pluginId: "cinemeta",
            libraries: [],
          },
          {
            id: "stremio:cinemeta:classics",
            name: "Classics",
            owner: "Cinemeta",
            group: "stremio",
            mediaTypes: ["movie"],
            health: "error",
            itemCount: 0,
            pluginId: "cinemeta",
            libraries: [{ id: 4, name: "Weekend movies" }],
          },
          ...[
            {
              id: "popular-series",
              name: "Popular",
              type: "series",
              extraRequired: [] as string[],
              extra: [] as { isRequired: boolean }[],
            },
            {
              id: "last-videos",
              name: "Last videos",
              type: "movie",
              extraRequired: ["ids"],
              extra: [],
            },
            {
              id: "calendar-videos",
              name: "Calendar videos",
              type: "series",
              extraRequired: [],
              extra: [{ isRequired: true }],
            },
          ]
            .filter(
              (cat) => !cat.extraRequired.length && !cat.extra.some((extra) => extra.isRequired),
            )
            .map((cat) => ({
              id: `stremio:cinemeta:${cat.id}`,
              name: cat.name,
              owner: "Cinemeta",
              group: "stremio",
              pluginId: "cinemeta",
              mediaTypes: [cat.type === "series" ? "show" : "movie"],
              health: "unknown",
              libraries: [],
            })),
        ]
          .filter((source) => {
            if (source.pluginId) {
              const enabled = extendMock.pluginEnabled(source.pluginId);
              return (
                enabled === true ||
                (enabled === false && url.searchParams.get("includeDisabled") === "true")
              );
            }
            if (source.id === "tvdb")
              return metadataSources().some((entry) => entry.id === "tvdb" && entry.configured);
            if (source.id.startsWith("tmdb-"))
              return metadataSources().some((entry) => entry.id === "tmdb" && entry.configured);
            return true;
          })
          .map((source) => ({
            ...source,
            enabled: source.pluginId ? extendMock.pluginEnabled(source.pluginId) === true : true,
            libraries: mockLibs
              .filter(
                (lib) =>
                  (source.group === "plugin" &&
                    "kindKey" in source &&
                    lib.pluginId === source.pluginId &&
                    lib.kindKey === source.kindKey) ||
                  ((hydrateLib(lib).sources ?? []) as string[]).includes(source.id),
              )
              .map((lib) => ({ id: lib.id, name: lib.name })),
          })),
      });
    if (!seg[1]) {
      if (method === "POST") {
        const id = Math.max(0, ...mockLibs.map((l) => Number(l.id))) + 1;
        const lib = {
          id,
          key: `lib-${id}`,
          name: String(b.name ?? "Library"),
          sources: b.sources,
          metadataSource: b.metadataSource,
          source: b.collectionId ? "collection" : "catalogue",
          collectionId: b.collectionId,
          mediaType: b.mediaType,
          rules: Array.isArray(b.rules) ? b.rules : [],
          visibilityDefault: String(b.visibilityDefault ?? "show"),
          present: b.present ?? {},
          sort: id,
          hidden: false,
          createdAt: "2026-01-01T00:00:00Z",
          updatedAt: "2026-01-01T00:00:00Z",
        };
        mockLibs.push(lib);
        return json(lib, 201);
      }
      return json({
        libraries: [...mockLibs].map(hydrateLib).sort((a, z) => Number(a.sort) - Number(z.sort)),
      });
    }
    const lib = mockLibs.find((l) => l.id === Number(seg[1]));
    if (!lib) return err(404, "not_found", "Library not found");
    hydrateLib(lib);
    if (!seg[2]) {
      if (method === "PATCH") {
        if (b.presentApps && typeof b.presentApps === "object") {
          const resolved = mockResolvePresentations(lib);
          const patch = b.presentApps as Record<string, boolean | undefined>;
          const curPresent = (
            lib.present && typeof lib.present === "object"
              ? { ...(lib.present as Record<string, unknown>) }
              : {}
          ) as Record<string, unknown>;
          const wasAdapter = Boolean(curPresent.adapterOnly);
          const nextAdapter = patch.adapterOnly !== undefined ? patch.adapterOnly : wasAdapter;
          if (nextAdapter) {
            lib.present = { adapterOnly: true };
          } else {
            delete curPresent.adapterOnly;
            for (const app of resolved.apps as {
              app: string;
              available: boolean;
              defaultOn: boolean;
              reason?: string;
            }[]) {
              const val = patch[app.app];
              if (val === undefined) {
                if (wasAdapter && app.available && app.defaultOn) {
                  curPresent[app.app] = resolved.available[app.app];
                }
                continue;
              }
              if (!app.available) {
                if (val) return err(400, "validation", app.reason ?? "Unavailable");
                continue;
              }
              if (val) curPresent[app.app] = resolved.available[app.app];
              else delete curPresent[app.app];
            }
            lib.present = curPresent;
          }
          const rest = { ...b };
          delete rest.presentApps;
          Object.assign(lib, rest);
        } else {
          Object.assign(lib, b);
        }
        if (b.cardShape === "") delete lib.cardShape;
        hydrateLib(lib);
        return json(lib);
      }
      if (method === "DELETE") {
        if (lib.source === "builtin" || lib.source === "plugin")
          return err(409, "conflict", "Cannot delete");
        mockLibs.splice(mockLibs.indexOf(lib), 1);
        return new Response(null, { status: 204 });
      }
      return json(lib);
    }
    if (seg[2] === "items") {
      const items =
        lib.source === "plugin"
          ? [
              {
                ref: "pk:demo:book:b1",
                key: 1,
                id: "b1",
                level: "book",
                title: "Dune",
                playable: true,
                fields: { author: "Herbert" },
              },
            ]
          : [];
      const q = (url.searchParams.get("q") || "").toLowerCase();
      const shown = q ? items.filter((i) => i.title.toLowerCase().includes(q)) : items;
      return json({ items: shown, total: shown.length });
    }
    if (seg[2] === "tabs") {
      if (seg[3] === "preview" && method === "POST") {
        const res = libraryTabs.previewTab(lib, body);
        return res.ok ? json(res.data) : err(400, "validation", res.message);
      }
      if (!seg[3] && method === "GET") return json(libraryTabs.getTabs(lib));
      if (!seg[3] && method === "PUT") {
        const res = libraryTabs.putTabs(lib, body);
        return res.ok ? json(res.data) : err(400, "validation", res.message);
      }
      if (!seg[3] && method === "DELETE") return json(libraryTabs.resetTabs(lib));
      return err(405, "method_not_allowed", "method not allowed");
    }
    if (seg[2] === "facets") {
      const q = url.searchParams.get("q") ?? undefined;
      const offsetRaw = url.searchParams.get("offset");
      const limitRaw = url.searchParams.get("limit");
      const orderRaw = url.searchParams.get("order");
      const declaredRaw = url.searchParams.get("declared");
      const hasOpts =
        q !== undefined ||
        offsetRaw !== null ||
        limitRaw !== null ||
        orderRaw !== null ||
        declaredRaw !== null;
      return json(
        libraryTabs.facetValuesFor(
          lib,
          decodeURIComponent(seg[3] ?? ""),
          hasOpts
            ? {
                q,
                offset: offsetRaw ? Number(offsetRaw) : undefined,
                limit: limitRaw ? Number(limitRaw) : undefined,
                order: orderRaw === "name" ? "name" : "count",
                declared: declaredRaw === "1" || declaredRaw === "true",
              }
            : undefined,
        ),
      );
    }
    if (seg[2] === "sync") return json({ upserted: 1, deleted: 0, skipped: 0 });
    if (seg[2] === "cache") {
      if (seg[3])
        return json({ libraryId: lib.id, used: 0, blobs: 0, maxBytes: 2147483648, ttlSeconds: 0 });
      return json({
        libraryId: lib.id,
        used: 0,
        blobs: 0,
        maxBytes: 2147483648,
        ttlSeconds: 0,
        sharedUsed: 0,
        sharedBlobs: 0,
        sharedCap: 10737418240,
      });
    }
    if (seg[2] === "setup")
      return json({
        pluginId: lib.pluginId,
        pluginName: lib.pluginName ?? "",
        steps: [],
        blockers: [],
        settings: {},
        schema: {},
        kinds: [],
        libraries: [lib],
      });
    if (seg[2] === "item") {
      const ref = decodeURIComponent(seg[3] ?? "");
      if (seg[4] === "match") {
        if (method === "PUT") {
          mockMatches.length = 0;
          mockMatches.push({
            itemRef: ref,
            candidateRef: String(b.candidateRef ?? ""),
            score: 1,
            source: "manual",
            chosen: true,
            updatedAt: 1,
          });
        }
        if (method === "DELETE") mockMatches.length = 0;
        return json({ matches: mockMatches });
      }
      return json({
        item: {
          ref,
          key: 1,
          id: "b1",
          level: "book",
          title: "Dune",
          playable: true,
          fields: { author: "Herbert" },
        },
        children: [],
        matches: mockMatches,
        kind: lib.kind,
      });
    }
    return err(404, "not_found", "not found");
  }

  let uploadedPosters = 24;
  async function handle(method: string, url: URL, body: unknown): Promise<Response> {
    const p = url.pathname.replace(/^\/ether\/api/, "");
    if (p === "/vela/status" && method === "GET") return json(velaStatus);
    if (p === "/vela/check" && method === "POST") return json(velaStatus);
    if (p === "/vela/install" && method === "POST") {
      velaStatus.busy = true;
      velaStatus.phase = "downloading";
      velaStatus.downloaded = 82_000_000;
      velaStatus.total = 193_700_293;
      await new Promise((resolve) => setTimeout(resolve, 3000));
      velaStatus.busy = false;
      velaStatus.phase = "";
      velaStatus.rollbackAvailable = velaStatus.installed;
      velaStatus.installed = true;
      opts.velaInstalled = true;
      velaStatus.installedVersion = "0.1.42";
      velaStatus.availableVersion = "";
      return json(velaStatus);
    }
    if (p === "/vela/rollback" && method === "POST") {
      velaStatus.installedVersion = "0.1.41";
      velaStatus.availableVersion = "0.1.42";
      return json(velaStatus);
    }
    const vs = velaSkins.handle(method, p, body);
    if (vs) return json(vs.body, vs.status);
    if (method === "GET" && p.startsWith("/viewer-plex-defaults/")) {
      const id = Number(p.split("/").pop());
      return json({
        hasProfile: true,
        hasSettings: false,
        audioLanguage: "en",
        autoSelectAudio: true,
        subtitleLanguage: "en",
        subtitleMode: "off",
        filterMovies: "",
        filterTelevision: "",
        allowSync: null,
        ...(state.plexDefaults[id] ?? {}),
      });
    }
    if (
      state.streamDefaults &&
      p === "/stream-preferences/0" &&
      (method === "GET" || method === "PUT")
    ) {
      if (method === "PUT" && body && typeof body === "object") {
        state.streamDefaults = {
          ...state.streamDefaults,
          ...(body as NonNullable<typeof state.streamDefaults>),
        };
      }
      return json(state.streamDefaults);
    }
    const q = url.searchParams;
    const seg = p.split("/").filter(Boolean);
    if (seg[0] === "posters" && seg[1] === "status")
      return json({
        uploaded: uploadedPosters,
        lastUpload: uploadedPosters ? "2026-11-12T20:00:00Z" : null,
        limited: false,
        folder: state.config.posters?.assetFolder ?? "",
        found: state.config.posters?.assetFolder ? 12 : 0,
        folderLimited: false,
      });
    if (seg[0] === "posters" && seg[1] === "clear" && method === "POST") {
      uploadedPosters = 0;
      return json({ ok: true });
    }
    if (seg[0] === "poster-folders") {
      const path = url.searchParams.get("path") || "/media/posters";
      return json({
        path,
        parent: path === "/" ? "/" : path.slice(0, path.lastIndexOf("/")) || "/",
        breadcrumbs: [
          { name: "/", path: "/" },
          ...path
            .split("/")
            .filter(Boolean)
            .map((name, i, parts) => ({ name, path: "/" + parts.slice(0, i + 1).join("/") })),
        ],
        directoryPosters: { "Dune (2021)": 1, "Arrival (2016)": 1, Movies: 0, TV: 0 },
        directories:
          path === "/media/posters" ? ["Dune (2021)", "Arrival (2016)", "Movies", "TV"] : [],
        posters: 12,
        limited: false,
      });
    }

    const route = `${method} /${seg.join("/")}`;
    if ((seg[0] === "libraries" && seg[1] !== "preview") || seg[0] === "plugins-setup") {
      return mockLibraries(method, seg, url, body);
    }
    if (seg[0] === "cluster" && seg[1] === "nodes" && seg[3] === "role-preflight") {
      const node = clusterNodes().find((n) => n.id === seg[2]);
      if (!node || q.get("boot") !== node.incarnation)
        return err(409, "stale_boot", "Refresh the node");
      return json({
        nodeId: node.id,
        boot: node.incarnation,
        observedAt: new Date().toISOString(),
        membershipSampledAt: node.lastSeen,
        eligible: false,
        executor: "none",
        reasons: [
          "executor_unavailable",
          "runtime_guard_not_verified",
          "ownership_retirement_unknown",
          "active_sessions_unknown",
          ...(node.leader ? ["last_full_server", "leader_handoff_unverified"] : []),
        ],
        safety: {
          membership: "observed",
          ownershipRetirement: "unknown",
          activeSessions: "unknown",
          restart: "unsupported",
        },
        guidance: {
          scope: "deployment_pool",
          deploymentOwnership: "unknown",
          verifiedAgainst: ["packaging/helm/ether/values.yaml"],
          exampleValues: "pools:\n  full:\n    replicas: 2",
          impact: "",
          execution: "",
        },
      });
    }
    if (seg[0] === "cluster" && seg[1] === "nodes" && seg[3] === "control") {
      const node = clusterNodes().find((n) => n.id === seg[2]);
      if (!node) return err(404, "unavailable", "Unknown node");
      let saved = nodeControls.get(node.id);
      if (!saved) {
        saved = {
          control: {
            nodeId: node.id,
            boot: node.incarnation!,
            requestedVersion: 0,
            appliedVersion: 0,
            state: "idle",
            updatedAt: new Date().toISOString(),
            observed: { weight: node.caps.weight || 1, draining: !!node.draining },
            capabilities: [
              { key: "weight", scope: "node", supported: true, reason: "" },
              ...["draining", "pause", "roles"].map((key) => ({
                key,
                scope: "node" as const,
                supported: false,
                reason: "ownership_interlock_required",
              })),
              ...["maxStreams", "defaultStreamLimit"].map((key) => ({
                key,
                scope: "fleet" as const,
                supported: false,
                reason: "fleet_setting",
              })),
              ...["videoTranscoding"].map((key) => ({
                key,
                scope: "node" as const,
                supported: false,
                reason: "not_allowlisted",
              })),
              ...["maxTranscodes", "hwAccel"].map((key) => ({
                key,
                scope: "node" as const,
                supported: false,
                reason: "restart_required",
              })),
            ],
          },
        };
        nodeControls.set(node.id, saved);
      }
      if (saved.due && Date.now() >= saved.due && node.live) {
        Object.assign(saved.control.observed, saved.patch);
        saved.control.appliedVersion = saved.control.requestedVersion;
        saved.control.state = "applied";
        saved.due = undefined;
      }
      if (node.live) saved.control.updatedAt = new Date().toISOString();
      else saved.control.updatedAt = node.lastSeen;
      if (method === "GET") {
        if (q.get("boot") !== node.incarnation) return err(409, "stale_boot", "Stale boot");
        return json(saved.control);
      }
      if (method === "POST") {
        const request = body as ClusterControlRequest;
        if (
          request.boot !== node.incarnation ||
          request.expectedVersion !== saved.control.requestedVersion ||
          saved.control.state === "pending"
        )
          return err(409, "conflict", "Refresh the node");
        if (request.patch.draining !== undefined) return err(422, "unsupported", "No safe setter");
        saved.control.requestedVersion++;
        saved.control.state = "pending";
        saved.control.requestId = request.requestId;
        saved.patch = request.patch;
        saved.due = Date.now() + 5000;
        return json(saved.control, 202);
      }
    }
    if (seg[0] === "stream-preferences") {
      if (seg[2] === "shows") return json([]);
      const id = seg[1] ?? "0";
      if (method === "PUT") viewerStreamRules.set(id, body);
      if (method === "DELETE") viewerStreamRules.delete(id);
      return json(
        viewerStreamRules.get(id) ??
          state.streamDefaults ?? {
            audioLanguages: ["en"],
            subtitleLanguages: ["en"],
            preferOriginal: true,
            subtitleMode: "foreign-only",
            subtitleStyle: "normal",
            loudness: false,
            nightMode: false,
            dialogueBoost: false,
          },
      );
    }
    if (seg[0] === "plex-servers") {
      // Demo: one owner-run Plex Media Server with the owner mapped.
      const servers = (state as { plexServers?: unknown[] }).plexServers ?? [
        {
          id: 1,
          name: "Living room PMS",
          url: "http://192.168.1.10:32400",
          machineId: "demo-pms",
          version: "1.43.4.10903",
          owner: "owner",
          stepAside: false,
          checkedAt: Math.floor(Date.now() / 1000),
          problem: "",
          viewers: [
            {
              accountId: 1,
              accountName: "owner",
              owner: true,
              userId: 1,
              direction: 3,
              interval: 10,
              canSend: true,
              lastImport: Math.floor(Date.now() / 1000) - 240,
              lastSend: Math.floor(Date.now() / 1000) - 240,
              nextSync: Math.floor(Date.now() / 1000) + 360,
              problem: "",
              unmatched: 1,
            },
          ],
        },
      ];
      (state as { plexServers?: unknown[] }).plexServers = servers;
      if (seg[2] === "accounts" && !seg[3] && method === "GET")
        return json([
          { id: 1, name: "owner", owner: true, userId: 1 },
          { id: 2, name: "family", owner: false, userId: 0 },
        ]);
      if (seg[2] === "accounts" && seg[4] === "backfill" && method === "GET")
        return json({ remoteViews: 128, remoteKnown: true, localWatched: 64, localInProgress: 4 });
      if (
        seg[2] === "accounts" &&
        (seg[4] === "backfill-in" || seg[4] === "backfill-out") &&
        method === "POST"
      ) {
        const srv = servers[0] as { viewers?: Array<Record<string, unknown>> };
        const acctId = Number(seg[3]);
        const nowSec = Math.floor(Date.now() / 1000);
        srv.viewers = (srv.viewers ?? []).map((m) =>
          m.accountId === acctId
            ? {
                ...m,
                ...(seg[4] === "backfill-in"
                  ? { inBackfill: { running: false, processed: 128, total: 128, doneAt: nowSec } }
                  : { outBackfill: { running: false, processed: 68, total: 68, doneAt: nowSec } }),
              }
            : m,
        );
        return json(srv);
      }
      if (seg[2] === "unmatched")
        return json([
          { direction: "in", ref: "9001", title: "Home Video", reason: "no_match", seenAt: 0 },
        ]);
      if (seg[1] && seg[2] === "step-aside" && method === "PUT") {
        const next = Boolean((body as { stepAside?: boolean }).stepAside);
        const srv = servers.find((x) => (x as { id?: number }).id === Number(seg[1])) as
          { stepAside?: boolean } | undefined;
        if (srv) srv.stepAside = next;
        return json(srv ?? servers[0]);
      }
      if (seg[1] && method === "DELETE") {
        (state as { plexServers?: unknown[] }).plexServers = [];
        return new Response(null, { status: 204 });
      }
      if (seg[1]) return json(servers[0]);
      return json(servers);
    }
    if (p === "/discovery/settings") return json({ enabled: true });
    if (seg[0] === "discovery" && seg[1] === "users") {
      const viewer = Number(seg[2]);
      if (seg[3] === "sync" && seg[4] === "jobs") {
        return json({ id: 1, state: "completed", plex: "synced", count: 0 });
      }
      if (seg[3] === "check") {
        return json({
          state: "completed",
          done: 0,
          total: 0,
          enabled: true,
          nextCheck: new Date(Date.now() + 30 * 60_000).toISOString(),
        });
      }
      if (seg[3] === "membership" && method === "POST") {
        const keys = (body as { keys?: string[] }).keys || [];
        return json(Object.fromEntries(keys.map((key) => [key, savedForViewer(viewer, key)])));
      }
      if (seg[3] === "catalog" && seg[4] && (method === "PUT" || method === "DELETE")) {
        watchlistChanges.set(`${viewer}:${decodeURIComponent(seg[4])}`, method === "PUT");
        return json({ traktFailed: false });
      }
      return json([]);
    }
    if (seg[0] === "viewers" && seg[2] === "favorite-episode-alert") return json({ enabled: true });

    if (p === "/users/local" && method === "POST") {
      const b = body as { username?: string; password?: string };
      if (!b.username || (b.password ?? "").length < 12)
        return err(
          400,
          "invalid",
          "a username and a password of at least 12 characters are required",
        );
      const id = 200 + state.addedViewers.length;
      const viewer: Viewer = {
        id,
        plexId: 0,
        username: b.username,
        title: b.username,
        isOwner: false,
        accessStatus: "active",
        plays30d: 0,
        hoursWatched30d: 0,
        lifecycle: {
          source: "local",
          paused: false,
          managedBy: "",
          canPause: true,
          canRemove: true,
          canMerge: true,
        },
      };
      state.addedViewers.push(viewer);
      state.users.push({
        id,
        plexId: 0,
        username: viewer.username,
        title: viewer.title,
        thumb: avatarFor(viewer.title),
        isOwner: false,
        lifecycle: viewer.lifecycle,
        streamLimit: 0,
        maxContentRating: "",
        allowedLibraries: [],
      });
      return json({ id }, 201);
    }

    if (p === "/users/invite" && method === "POST") {
      const b = body as { display?: string };
      const id = 200 + state.addedViewers.length;
      const viewer: Viewer = {
        id,
        plexId: 0,
        username: "",
        title: b.display ?? "",
        isOwner: false,
        accessStatus: "active",
        plays30d: 0,
        hoursWatched30d: 0,
        lifecycle: {
          source: "invite",
          paused: false,
          managedBy: "",
          canPause: true,
          canRemove: true,
          canMerge: true,
        },
      };
      state.addedViewers.push(viewer);
      state.users.push({
        id,
        plexId: 0,
        username: viewer.username,
        title: viewer.title,
        thumb: avatarFor(viewer.title),
        isOwner: false,
        lifecycle: viewer.lifecycle,
        streamLimit: 0,
        maxContentRating: "",
        allowedLibraries: [],
      });
      return json({ id, url: "/ether/me#invite=mock-secret" }, 201);
    }

    if (p === "/frontends/dlna/guest" && method === "POST") {
      const viewer: Viewer = {
        id: 100 + state.dlnaGuests.length,
        plexId: 0,
        username: "",
        title: "Living-room guest",
        isOwner: false,
        accessStatus: "active",
        plays30d: 0,
        hoursWatched30d: 0,
        lifecycle: {
          source: "dlnaGuest",
          paused: false,
          managedBy: "",
          canPause: true,
          canRemove: true,
          canMerge: true,
        },
      };
      state.dlnaGuests.push(viewer);
      return json(viewer, 201);
    }

    if (p === "/me/favorites-mirror") {
      if (method === "POST") state.favoritesMirror.set(1, (body as { enabled: boolean }).enabled);
      return json({ enabled: state.favoritesMirror.get(1) ?? true });
    }

    const appsOffset = seg[0] === "me" ? 1 : seg[0] === "viewers" && seg[1] ? 2 : -1;
    if (appsOffset >= 0 && seg[appsOffset] === "apps") {
      const enabled = [
        "plex",
        "jellyfin",
        "emby",
        "silo",
        "stremio",
        "webdav",
        "m3u",
        "xtream",
      ].filter((id) => state.config.frontends?.[id]?.enabled);
      const adapters: Record<
        string,
        { name: string; logo?: string; auth?: string; protocolHint?: string }
      > = {};
      for (const [id, entry] of Object.entries(state.config.frontends ?? {})) {
        if (!id.startsWith("plugin:") || !entry?.enabled) continue;
        enabled.push(id);
        adapters[id] = {
          name: id.split(":").at(-1) || id,
          auth: "ether_token",
          protocolHint: id.endsWith(":subsonic") ? "subsonic" : undefined,
        };
      }
      if (method === "GET" && seg.length === appsOffset + 1)
        return json(Object.keys(adapters).length ? { apps: enabled, adapters } : { apps: enabled });
      const surface = seg[appsOffset + 1] ? decodeURIComponent(seg[appsOffset + 1]!) : undefined;
      if (method === "POST" && surface && enabled.includes(surface)) {
        const qr =
          "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=";
        const id = seg[0] === "viewers" ? seg[1] : "1";
        if (surface === "xtream") {
          const grants = xtreamGrants.get(id ?? "1") ?? [];
          grants.push({
            hash: `mock-xtream-${grants.length + 1}`,
            surface: "xtream",
            xtreamUsername: "alice42",
            revealAvailable: true,
            xtreamStreamLimit: 0,
            lastUsed: Date.now(),
          });
          xtreamGrants.set(id ?? "1", grants);
          return json({
            surface,
            url: "http://ether.local/xtream",
            username: "alice42",
            password: "k7m2-9qxr-4t",
            qr: "",
          });
        }
        if (surface === "webdav")
          return json({
            surface,
            url: "http://ether.local/webdav/",
            username: id,
            password: "mock-app-password-7f3a9c2e8b1d6f4a0c9e2b7d5a1f8c3e",
            // Scannable fixture for the modal preview; encodes the mock WebDAV URL.
            qr: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAMAAAADAAQMAAABoEv5EAAAABlBMVEX///8AAABVwtN+AAABJklEQVR42uzWMa6rMBCF4R+5oPQSvBR2BsnOvJRZAiWFlfPkASV5UaorBbvgNAh9zQgP4+HKlR9FNUGSLTJ/uXUABUgwSbfR/OVMSNoSUBhkJar0A0GZdO8PFGzpCYAUVoAvZ94K9m5fp2Dz+OU3+C0AEJQd+EwrKAxbUgVIkt4+YkOAQUaUdPdnMDqAIEkFqF1fmAo9gPTwLpfN3vWyHgDmUVoJNni5b7/B70E2bJS4X2zE/DzBpgBeJn7XhvV1gm3BLxSmYMtGWCdZDyBbtkTMskE6Gr89AHiZteuJ+VQ4VrVcUt2WougCCpD2Lh/3oXnrAY69BBglvT5ieyCqpEedW9P/5TaGLJvBpyk9wHG0viLso/Q8OHa4ZzWfy10buHLlj/k3ACp8cxu0+Cr1AAAAAElFTkSuQmCC",
          });
        if (surface === "jellyfin" || surface === "emby" || surface === "silo") {
          const viewer = state.users.find((u) => String(u.id) === id) ?? state.users[0];
          const published = state.config.frontends?.[surface]?.publishedAddresses?.[0];
          return json({
            surface,
            url: published || `http://ether.local/${surface}`,
            username: viewer?.username || id,
            password: "mock-app-password",
            qr,
          });
        }
        if (surface.startsWith("plugin:")) {
          const key = surface.split(":").at(-1) || "adapter";
          return json({ surface, url: `http://ether.local/${key}`, token: "mock-grant", qr });
        }
        const published = state.config.frontends?.[surface]?.publishedAddresses?.[0];
        if (published) {
          const trimmed = published.replace(/\/+$/, "");
          const suffix =
            surface === "m3u" ? "/mock-grant/playlist.m3u" : "/mock-grant/manifest.json";
          return json({ surface, url: `${trimmed}${suffix}`, qr });
        }
        const path =
          surface === "m3u" ? "m3u/mock-grant/playlist.m3u" : "stremio/mock-grant/manifest.json";
        return json({ surface, url: `http://ether.local/${path}`, qr });
      }
      return err(404, "not_found", "app unavailable");
    }

    const cached = cacheMock(method, seg, body, state.config, state.clustered);
    if (cached) return json(cached.body, cached.status);

    const site = sites.handle(method, seg, body, state.config);
    if (site) return json(site.body, site.status);

    const ext = await extendMock.handle(method, seg, q, body);
    if (ext) return ext;

    const cal = calendar.handle(method, seg, q);
    if (cal) return json(cal.body, cal.status);

    const flw = flowsMock.handle(method, seg, q, body);
    if (flw) return json(flw.body, flw.status);

    const oar = onAir.handle(method, seg, q, body);
    if (oar) return json(oar.body, oar.status);
    const tvr = liveTV.handle(method, seg, q, body);
    if (tvr) return json(tvr.body, tvr.status);

    const tr = trash.handle(method, seg, body, state.config, q);
    if (tr) return json(tr.body, tr.status);

    const qm = qualityMock.handle(
      method,
      seg,
      q,
      body,
      state.config,
      mockLibs.map((l) => ({
        id: Number(l.id),
        name: String(l.name ?? ""),
        source: String(l.source ?? "builtin"),
        mediaType: l.mediaType ? String(l.mediaType) : undefined,
        pluginId: l.pluginId ? String(l.pluginId) : undefined,
        pluginName: l.pluginName ? String(l.pluginName) : undefined,
        sort: Number(l.sort ?? 0),
        hidden: Boolean(l.hidden),
      })),
      (id) => safetyPolicies.get(id),
      (key) => {
        const hit = byKey.get(key);
        if (!hit) return null;
        const isEpisode = hit.type === "episode" || hit.type === "show";
        return {
          kind: isEpisode ? "episode" : "movie",
          libraryIds: isEpisode ? [2] : [1, 4],
        };
      },
    );
    if (qm) return json(qm.body, qm.status);

    const rc = releaseConsole.handle(method, seg, q, body);
    if (rc)
      return rc.status === 204 ? new Response(null, { status: 204 }) : json(rc.body, rc.status);

    const md = metadata(method, p, body, q);
    if (md) return md;

    const mr = maintenance.handle(method, seg, body);
    if (mr) return json(mr.body, mr.status);

    if (
      method === "GET" &&
      seg[0] === "stats" &&
      seg[1] === "plays" &&
      seg[2] &&
      seg.length === 3
    ) {
      const targetId = decodeURIComponent(seg[2]);
      const exPlay = experience.handle(method, seg, q);
      if (exPlay && exPlay.status === 200) return json(exPlay.body, exPlay.status);
      const liveSession = (dashboard().sections.sessions as Array<Record<string, unknown>>).find(
        (s) => s.ID === targetId || s.id === targetId,
      );
      if (liveSession) {
        const started = liveSession.StartedAt
          ? Math.floor(new Date(String(liveSession.StartedAt)).getTime() / 1000)
          : Math.floor(Date.now() / 1000);
        const nowSec = Math.floor(Date.now() / 1000);
        const info = (liveSession.Info ?? {}) as Record<string, unknown>;
        const client = (liveSession.Client ?? {}) as Record<string, unknown>;
        const exp = (liveSession.experience ?? {}) as Record<string, unknown>;
        const res = (liveSession.Resolution ?? {}) as Record<string, unknown>;
        const rel = (res.release ?? {}) as Record<string, unknown>;
        const durationMs = Math.round(Number(info.duration || 0) / 1e6);
        const watchedMs = Math.max(0, (nowSec - started) * 1000);
        const bytes = Number(liveSession.BytesServed || 0);
        return json({
          id: targetId,
          userId: Number(liveSession.UserID || 0),
          username: String(liveSession.Username || ""),
          ratingKey: Number(liveSession.ratingKey || liveSession.Key || 0),
          title: String(liveSession.TitleName || ""),
          platform: String(client.platform || client.product || ""),
          device: String(client.deviceName || client.product || ""),
          decision: String(liveSession.Decision || ""),
          started,
          updated: nowSec,
          ended: 0,
          watchedMs,
          durationMs,
          bytes,
          completed: false,
          failovers: Number(liveSession.Failovers || 0),
          switches: Number(liveSession.Failovers || 0),
          origin: String(liveSession.origin || ""),
          release: String(rel.name || ""),
          ip: String(liveSession.ip || ""),
          remote: Boolean(client.remote),
          measured: Boolean(exp.requested),
          startMs: exp.pictured ? Math.round(Number(exp.startTime || 0) / 1e6) : null,
          bufferEvents: exp.requested ? Number(exp.bufferEvents || 0) : null,
          bufferMs: exp.requested ? Math.round(Number(exp.buffering || 0) / 1e6) : null,
          failed: false,
          error: "",
          reasonKind: "",
          reasonDetail: "",
          sourceKbps: Number(info.bitrateKbps || liveSession.bitrateKbps || 0),
          avgBitrateKbps: watchedMs > 0 && bytes > 0 ? Math.round((bytes * 8) / watchedMs) : 0,
          uhd: /2160p|4k/i.test(String(liveSession.sourceVideo || "")),
          hdr: /hdr|dolby vision|hlg/i.test(String(liveSession.sourceVideo || "")),
        });
      }
      if (exPlay) return json(exPlay.body, exPlay.status);
    }
    const ex = experience.handle(method, seg, q);
    if (ex) return json(ex.body, ex.status);

    if (seg[0] === "frontends" && seg[1] === "qr" && seg.length === 2 && method === "GET") {
      const target = q.get("url") ?? "";
      if (!target || /:\/\/[^/]*@/.test(target)) {
        return err(400, "invalid_url", "Invalid URL");
      }
      const b64 =
        "iVBORw0KGgoAAAANSUhEUgAAAMAAAADAAQMAAABoEv5EAAAABlBMVEX///8AAABVwtN+AAABJklEQVR42uzWMa6rMBCF4R+5oPQSvBR2BsnOvJRZAiWFlfPkASV5UaorBbvgNAh9zQgP4+HKlR9FNUGSLTJ/uXUABUgwSbfR/OVMSNoSUBhkJar0A0GZdO8PFGzpCYAUVoAvZ94K9m5fp2Dz+OU3+C0AEJQd+EwrKAxbUgVIkt4+YkOAQUaUdPdnMDqAIEkFqF1fmAo9gPTwLpfN3vWyHgDmUVoJNni5b7/B70E2bJS4X2zE/DzBpgBeJn7XhvV1gm3BLxSmYMtGWCdZDyBbtkTMskE6Gr89AHiZteuJ+VQ4VrVcUt2WougCCpD2Lh/3oXnrAY69BBglvT5ieyCqpEedW9P/5TaGLJvBpyk9wHG0viLso/Q8OHa4ZzWfy10buHLlj/k3ACp8cxu0+Cr1AAAAAElFTkSuQmCC";
      const bin = atob(b64);
      const bytes = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      return new Response(bytes, { status: 200, headers: { "Content-Type": "image/png" } });
    }
    const mockRuntimePort = (id: string, configuredPort?: number) => {
      if (
        opts.portConflict &&
        id === "jellyfin" &&
        state.config.frontendPorts?.jellyfin == null &&
        configuredPort
      ) {
        return {
          actualPort: 8098,
          busyPort: configuredPort,
          busyPortHolder: opts.portConflict === "unknown" ? undefined : "Jellyfin Server",
        };
      }
      return { actualPort: configuredPort, busyPort: undefined, busyPortHolder: undefined };
    };
    const mockConnectUrls = (id: string, port?: number) => {
      if (id === "plex" && port) {
        return [
          `http://192.168.1.20:${port}`,
          `http://100.64.0.20:${port}`,
          `https://203-0-113-7.abc.plex.direct:${port}`,
        ];
      }
      if ((id === "jellyfin" || id === "emby" || id === "silo") && port) {
        return [
          `http://192.168.1.20:${port}`,
          `http://100.64.0.20:${port}`,
          `http://[fd00::20]:${port}`,
          `http://192.168.1.20:${state.config.server.port}/${id}`,
        ];
      }
      if (id === "vela") {
        if (opts.velaInstalled !== true) return [];
        const serveMain = state.config.frontends?.vela?.settings?.serveOnMainPort !== false;
        const urls: string[] = [];
        if (port) {
          urls.push(`http://192.168.1.20:${port}`, `http://100.64.0.20:${port}`);
        }
        if (!port || serveMain) {
          urls.push(
            `http://192.168.1.20:${state.config.server.port}/vela`,
            `http://100.64.0.20:${state.config.server.port}/vela`,
          );
        }
        return urls;
      }
      if (id === "stremio" || id === "webdav" || id === "dlna" || id === "m3u" || id === "xtream") {
        return [`http://192.168.1.20:${state.config.server.port}/${id}`];
      }
      return [];
    };
    const mockObservedAddresses = (id: string, enabled: boolean): string[] => {
      if (!enabled) return [];
      if (id === "jellyfin") {
        return ["https://jellyfin.example.com", "https://media.example.com/jellyfin"];
      }
      return [];
    };
    const mockPublishedAddresses = (id: string): string[] => {
      if (id === "plex") {
        return [...(state.config.plex?.customUrls ?? [])];
      }
      const entry = state.config.frontends?.[id];
      if (entry?.publishedAddresses?.length) return [...entry.publishedAddresses];
      if (entry?.publicUrl) return [entry.publicUrl];
      return [];
    };
    // Settings schemas mirror internal/frontend (WS1d): keep beside the
    // descriptors so the Apps page renders from the same shape.
    const mediaBrowserSchema = (quickConnect: boolean) => ({
      signInHelp: true,
      sections: [
        {
          key: "home",
          fields: [{ key: "homeRowCount", kind: "number", min: 0, max: 12 }],
        },
        {
          key: "find",
          fields: [
            { key: "serverName", kind: "serverName" },
            { key: "port", kind: "port", min: 1, max: 65535 },
            { key: "discovery", kind: "bool", more: true },
            ...(quickConnect ? [{ key: "quickConnect", kind: "bool", more: true }] : []),
            { key: "publicViewerPicker", kind: "bool", more: true },
          ],
        },
      ],
    });
    const accountSchema = (note: string) => ({
      accountConnect: true,
      sections: [{ key: "connect", fields: [{ key: note, kind: "note" }] }],
    });
    const protocolDefs = [
      { id: "vela", name: "Vela", defaultPort: 8098, settingsSchema: VELA_SETTINGS_SCHEMA },
      {
        id: "plex",
        name: "Plex",
        defaultPort: 32400,
        settingsSchema: {
          sections: [{ key: "find", fields: [{ key: "port", kind: "port", min: 1, max: 65535 }] }],
        },
      },
      {
        id: "jellyfin",
        name: "Jellyfin",
        defaultPort: 8096,
        settingsSchema: mediaBrowserSchema(true),
      },
      { id: "emby", name: "Emby", defaultPort: 8097, settingsSchema: mediaBrowserSchema(false) },
      { id: "stremio", name: "Stremio", settingsSchema: accountSchema("addon") },
      {
        id: "silo",
        name: "Silo",
        defaultPort: 8090,
        settingsSchema: {
          signInHelp: true,
          sections: [
            {
              key: "find",
              fields: [
                { key: "serverName", kind: "serverName" },
                { key: "port", kind: "port", min: 1, max: 65535 },
                { key: "discovery", kind: "bool", more: true },
              ],
            },
          ],
        },
      },
      {
        id: "dlna",
        name: "DLNA",
        settingsSchema: {
          sections: [
            {
              key: "tv",
              fields: [
                { key: "serverName", kind: "serverName" },
                { key: "viewerId", kind: "viewer" },
                { key: "discovery", kind: "bool", more: true },
              ],
            },
          ],
        },
      },
      { id: "webdav", name: "Files (WebDAV)", settingsSchema: accountSchema("files") },
      { id: "m3u", name: "M3U & XMLTV", settingsSchema: accountSchema("playlist") },
      { id: "xtream", name: "IPTV apps (Xtream)", settingsSchema: accountSchema("xtream") },
      // A plugin adapter (ether.music) that has not shipped its own icon yet.
      // Plugins install after first-run setup, so the setup chooser never lists it.
      ...(state.config.setupComplete
        ? [
            {
              id: "plugin:ether.music:subsonic",
              name: "Subsonic",
              auth: "query_credentials",
              protocolHint: "subsonic",
              pathPrefix: "/subsonic",
            },
          ]
        : []),
    ];
    if (seg[0] === "frontends" && seg.length === 1 && method === "GET") {
      return json(
        protocolDefs.map((descriptor) => {
          const entry = state.config.frontends?.[descriptor.id];
          const enabled =
            entry?.enabled ??
            (descriptor.id === "vela" ||
              (state.config.frontends == null && descriptor.id === "plex"));
          const configuredPort =
            descriptor.id === "plex"
              ? (state.config.frontendPorts?.plex ??
                state.config.server.port ??
                descriptor.defaultPort)
              : descriptor.id === "vela"
                ? state.config.frontendPorts?.vela
                : (state.config.frontendPorts?.[descriptor.id] ?? descriptor.defaultPort);
          const runtime = mockRuntimePort(descriptor.id, configuredPort);
          const port = runtime.actualPort;
          const observed = mockObservedAddresses(descriptor.id, enabled);
          const published = mockPublishedAddresses(descriptor.id);
          return {
            descriptor,
            enabled,
            status: {
              webPlayerReady:
                descriptor.id === "vela"
                  ? opts.velaInstalled === true
                  : descriptor.id === "jellyfin",
              addresses: enabled && port ? [`127.0.0.1:${port}`] : [],
              connectUrls: enabled ? mockConnectUrls(descriptor.id, port) : [],
              ...(observed.length ? { observedAddresses: observed } : {}),
              ...(published.length ? { publishedAddresses: published } : {}),
              ...(enabled && runtime.busyPort
                ? {
                    busyPort: runtime.busyPort,
                    actualPort: runtime.actualPort,
                    ...(runtime.busyPortHolder ? { busyPortHolder: runtime.busyPortHolder } : {}),
                    warnings: [
                      `Another server on this computer is using port ${runtime.busyPort}; Ether still works at 127.0.0.1:${runtime.actualPort}`,
                    ],
                  }
                : {}),
              lan: "unknown",
              wan: "unknown",
              clients24h: 0,
              clients24hKnown: true,
              liveStreams: 0,
              liveStreamsKnown: true,
              targetVersion: "",
              conformance: "unknown",
            },
          };
        }),
      );
    }
    if (seg[0] === "frontends" && seg.length === 3) {
      const [, id, action] = seg;
      const descriptor = protocolDefs.find((item) => item.id === id);
      if (!descriptor) return err(404, "not_found", "Front end not found");
      if (action === "status" && method === "GET") {
        const enabled =
          state.config.frontends?.[id!]?.enabled ??
          (id === "vela" || (state.config.frontends == null && id === "plex"));
        const configuredPort =
          id === "plex"
            ? (state.config.frontendPorts?.plex ??
              state.config.server.port ??
              descriptor.defaultPort)
            : id === "vela"
              ? state.config.frontendPorts?.vela
              : (state.config.frontendPorts?.[id!] ?? descriptor.defaultPort);
        const runtime = mockRuntimePort(id ?? "", configuredPort);
        const port = runtime.actualPort;
        const observed = mockObservedAddresses(id ?? "", enabled);
        const published = mockPublishedAddresses(id ?? "");
        return json({
          webPlayerReady: id === "vela" ? opts.velaInstalled === true : id === "jellyfin",
          addresses: enabled && port ? [`127.0.0.1:${port}`] : [],
          connectUrls: enabled ? mockConnectUrls(id ?? "", port) : [],
          ...(observed.length ? { observedAddresses: observed } : {}),
          ...(published.length ? { publishedAddresses: published } : {}),
          ...(enabled && runtime.busyPort
            ? {
                busyPort: runtime.busyPort,
                actualPort: runtime.actualPort,
                ...(runtime.busyPortHolder ? { busyPortHolder: runtime.busyPortHolder } : {}),
                warnings: [
                  `Another server on this computer is using port ${runtime.busyPort}; Ether still works at 127.0.0.1:${runtime.actualPort}`,
                ],
              }
            : {}),
          lan: "unknown",
          wan: "unknown",
          clients24h: 0,
          clients24hKnown: true,
          liveStreams: 0,
          liveStreamsKnown: true,
          targetVersion: "",
          conformance: "unknown",
        });
      }
      if (action === "settings" && method === "GET") {
        const published = mockPublishedAddresses(id!);
        const storedPort =
          id === "vela"
            ? state.config.frontendPorts?.vela
            : (state.config.frontendPorts?.[id!] ?? descriptor.defaultPort);
        return json({
          ...state.config.frontends?.[id!]?.settings,
          ...(id !== "plex" && published.length ? { publishedAddresses: published } : {}),
          ...(storedPort ? { port: storedPort } : {}),
        });
      }
      const entries =
        state.config.frontends ?? (state.config.frontends = { plex: { enabled: true } });
      const entry = entries[id!] ?? (entries[id!] = { enabled: id === "vela" });
      if (id === "vela" && action === "wordmark" && method === "POST") {
        const uploaded = await velaBranding.upload(body);
        return json(uploaded.body, uploaded.status);
      }
      if (id === "vela" && action === "icon" && method === "POST") {
        const uploaded = await velaBranding.uploadIcon(body);
        return json(uploaded.body, uploaded.status);
      }
      if (action === "settings" && method === "PUT") {
        const { port, publishedAddresses, publicUrl, ...settings } = body as Record<
          string,
          unknown
        >;
        if (publishedAddresses !== undefined) {
          if (id === "plex") {
            return err(400, "invalid", "configure Plex published addresses under Plex settings");
          }
          if (!Array.isArray(publishedAddresses)) {
            return err(400, "invalid", "publishedAddresses must be a list of URLs");
          }
          const clean = publishedAddresses
            .map((v) => String(v).trim().replace(/\/+$/, ""))
            .filter(Boolean);
          if (clean.length) entry.publishedAddresses = clean;
          else delete entry.publishedAddresses;
          delete entry.publicUrl;
        } else if (typeof publicUrl === "string") {
          const clean = publicUrl.trim().replace(/\/+$/, "");
          if (clean) entry.publishedAddresses = [clean];
          else delete entry.publishedAddresses;
          delete entry.publicUrl;
        }
        if (id === "vela") {
          if (typeof settings.appName === "string") {
            const trimmed = settings.appName.trim();
            if (trimmed) settings.appName = trimmed;
            else delete settings.appName;
          }
          if (settings.wordmark == null) delete settings.wordmark;
          if (
            settings.trailerPlayback !== undefined &&
            settings.trailerPlayback !== "server" &&
            settings.trailerPlayback !== "clientWhenPossible"
          ) {
            return json({ error: { message: "invalid Vela setting" } }, 400);
          }
          if (
            settings.livePreview !== undefined &&
            settings.livePreview !== "auto" &&
            settings.livePreview !== "on-click" &&
            settings.livePreview !== "off"
          ) {
            return json({ error: { message: "invalid livePreview" } }, 400);
          }
          if (settings.appIcon == null) delete settings.appIcon;
        }
        if (id === "vela" && typeof port !== "number") {
          delete settings.serveOnMainPort;
        }
        entry.settings = settings;
        if (typeof port === "number") {
          (state.config.frontendPorts ??= {})[id!] = port;
          if (id === "plex") state.config.server.port = port;
        } else if (state.config.frontendPorts) {
          delete state.config.frontendPorts[id!];
        }
        return json({ ok: true });
      }
      if (
        action === "enabled" &&
        method === "PUT" &&
        typeof (body as { enabled?: unknown }).enabled === "boolean"
      ) {
        entry.enabled = (body as { enabled: boolean }).enabled;
        return json({ ok: true });
      }
      if (action === "api-keys" && method === "GET" && (id === "jellyfin" || id === "emby")) {
        return json(state.frontendApiKeys[id] ?? []);
      }
    }
    if (
      seg[0] === "frontends" &&
      seg.length === 4 &&
      seg[2] === "api-keys" &&
      method === "DELETE" &&
      (seg[1] === "jellyfin" || seg[1] === "emby")
    ) {
      const keyId = Number(seg[3]);
      const list = state.frontendApiKeys[seg[1]] ?? [];
      const idx = list.findIndex((k) => k.id === keyId);
      if (idx < 0) return err(404, "not_found", "API key not found");
      list.splice(idx, 1);
      return json({ ok: true });
    }

    if (
      method === "POST" &&
      seg.length === 4 &&
      seg[0] === "test" &&
      seg[3] === "speed" &&
      (seg[1] === "usenet" || seg[1] === "debrid")
    ) {
      const usenet = seg[1] === "usenet";
      const providers = usenet ? state.config.usenet.providers : state.config.debrid?.providers;
      if (
        !providers?.some(
          (provider) => provider.id === decodeURIComponent(seg[2]!) && provider.enabled,
        )
      ) {
        return json({ status: "unavailable" });
      }
      const slow = usenet && !opts.fastUsenetSpeed;
      const mbps = slow ? 0.65 : 6.4;
      const connections = usenet ? 4 : 1;
      return json({
        status: "ok",
        at: new Date().toISOString(),
        seconds: 10,
        mbps,
        mbpsPerConnection: mbps / connections,
        streamMbps: mbps * 8 * 0.8,
        typicalMbps: 10.16,
        connections,
        opened: usenet ? 2 : 1,
        refused: 0,
        misses: slow ? 3 : 0,
        spareOnly: slow,
        streamsPlaying: slow ? 2 : 0,
      });
    }

    // Static routes
    switch (route) {
      case "GET /availability/titles":
        return json([
          {
            key: "1",
            title: "Ready Movie",
            year: 2026,
            theatrical: 1_760_000_000,
            digital: 1_762_000_000,
            code: "playable",
            source: "Indexer One",
          },
          {
            key: "2",
            title: "Waiting Movie",
            year: 2026,
            theatrical: 1_761_000_000,
            code: "searching",
            nextCheck: Math.floor(Date.now() / 1000) + 7200,
          },
          {
            key: "3",
            title: "Theatre Movie",
            year: 2026,
            theatrical: 1_762_500_000,
            code: "theatrical",
          },
          {
            key: "4",
            title: "Quality Movie",
            year: 2026,
            theatrical: 1_760_500_000,
            code: "quality",
            reason: "cam",
          },
        ]);
      case "GET /availability/title/1":
        return json({ code: "playable" });
      case "GET /availability/summary":
        return json({
          ready: 12,
          waiting: 34,
          lastChecked: Math.floor(Date.now() / 1000) - 300,
        });
      case "GET /availability":
        return json({
          mode: state.config.availability?.verify
            ? "checking"
            : state.config.availability?.monitor
              ? "monitoring"
              : "off",
          message: "Search results remain independent of source evidence.",
          feeds: (state.config.indexers ?? [])
            .filter((x) => x.enabled)
            .map((x) => ({
              id: x.id,
              name: x.name,
              protocol: x.protocol,
              supported: true,
              selected: true,
              lastSync: 0,
              gap: false,
              message: "Ready to monitor",
              matched: 0,
            })),
          usage: { feedRequests: 12 },
          counts: {},
          recent: [],
          day: new Date().toISOString().slice(0, 10),
          effort: state.config.availability?.effort || "normal",
          effectiveFeedRequests: state.config.availability?.feedRequests ?? 100,
          effectivePollMinutes: state.config.availability?.pollMinutes ?? 30,
          effectiveMaxPages: state.config.availability?.maxPages ?? 3,
        });
      case "POST /availability/sync":
        return state.config.availability?.monitor
          ? json(null, 202)
          : err(409, "disabled", "Enable release monitoring first");
      case "GET /web-player":
      case "POST /web-player": {
        const players = [
          ...(opts.velaInstalled ? [{ id: "vela", url: `${window.location.origin}/vela/` }] : []),
          { id: "jellyfin", url: `${window.location.origin}/jellyfin/web/ether-signin` },
        ];
        const target =
          players.find((player) => player.id === q.get("app")) ??
          players.find((player) => player.id === "jellyfin")!;
        return json({ available: true, players, url: target.url });
      }
      case "GET /bootstrap":
      case "GET /health":
        return json(state.boot);
      case "GET /server/proxy-hint": {
        const trusted = state.config.server.trustedProxyCIDRs ?? [];
        const proxies =
          opts.untrustedProxy && !trusted.includes("172.18.0.2") && !trusted.includes("npm")
            ? [
                {
                  address: "172.18.0.2",
                  name: "npm",
                  requests: 42,
                  lastSeen: new Date().toISOString(),
                },
              ]
            : [];
        return json({ proxies });
      }
      case "GET /server/console-listener": {
        const mainPort = state.config.server.port ?? 32400;
        const configuredPort = state.config.server.consolePort ?? 0;
        const disableOnMain = Boolean(state.config.server.disableConsoleOnMainPort);
        return json({
          configuredPort,
          activePort: configuredPort > 0 && configuredPort !== mainPort ? configuredPort : 0,
          mainPort,
          onMainPort: configuredPort <= 0 || !disableOnMain,
          fallback: false,
        });
      }
      case "GET /dashboard":
        return json(dashboard());
      case "GET /indexer-usage":
        return json(indexerUsage());
      case "GET /stats/sources":
        return json(sourceHistory(Math.min(90, Math.max(1, Number(q.get("days")) || 30))));
      case "GET /stats/infrastructure":
        return json(infrastructure());
      case "PUT /stats/costs":
        saveCost(body as { kind: string; id: string; monthly: number; currency: string });
        return json({ ok: true });
      case "GET /config":
        return json({ config: state.config });
      case "PUT /config":
        state.config = body as EtherConfig;
        for (const key of ["tmdbKey", "tvdbKey"] as const) {
          if (state.config.catalog[key]) state.config.catalog[key] = "••••";
        }
        return json({ config: state.config });
      case "GET /metadata/sources":
        return json({ sources: metadataSources() });
      case "GET /setup/status":
        return json({
          setupComplete: state.config.setupComplete,
          requiredComplete: true,
          steps: setupSteps(),
        });
      case "POST /setup/complete":
        state.config.setupComplete = true;
        state.boot.setupComplete = true;
        return json({ setupComplete: true, requiredComplete: true, steps: setupSteps() });
      case "POST /setup/reopen":
        return json({
          setupComplete: state.config.setupComplete,
          requiredComplete: true,
          steps: [],
        });
      case "POST /plex/pin":
        return json({ id: "pin1", code: "WXYZ", authUrl: "https://app.plex.tv/auth#?code=WXYZ" });
      case "POST /plex/claim":
        state.config.plex.claimed = true;
        if (!state.config.plex.token) state.config.plex.token = "••••";
        if (!state.config.plex.ownerUsername) state.config.plex.ownerUsername = "Braedon";
        return json({ ok: true });
      case "POST /plex/friends/sync":
        return json({ ok: true });
      case "POST /auth/pin":
        return json({ handle: "h1", code: "ABCD", authUrl: "https://app.plex.tv/auth#?code=ABCD" });
      case "POST /auth/session":
        return json({ done: true, token: "ether-admin-mock", expiresAt: 0 });
      case "DELETE /auth/session":
        return json({ ok: true });
      case "GET /auth/lockouts":
        return json({ lockouts: state.lockouts });
      case "DELETE /auth/lockouts": {
        const cleared = state.lockouts.length;
        state.lockouts = [];
        return json({ ok: true, cleared });
      }
      case "GET /llm/providers":
        return json({ providers: MOCK_LLM_PROVIDERS });
      case "POST /llm/models": {
        const req = body as { provider?: string; apiKey?: string };
        const p = MOCK_LLM_PROVIDERS.find((x) => x.id === (req.provider || "custom"));
        if (p?.needsKey && req.apiKey === "bad")
          return json({ models: [], error: "401: invalid key" });
        return json({
          models: MOCK_LLM_MODELS[p?.id ?? ""] ?? [{ id: "llama3" }, { id: "qwen3:8b" }],
        });
      }
      case "GET /system/load":
        return json(mockSystemLoad(Date.now()));
      case "POST /outbound/check": {
        const posted = body as { enabled?: boolean; url?: string };
        if (posted?.enabled && posted.url === "http://down") {
          return json({
            sourceError: "proxy did not answer",
            publishedIp: "203.0.113.7",
            remoteAccess: true,
            proxyEnabled: true,
          });
        }
        if (posted?.enabled) {
          return json({
            sourceIp: "198.51.100.20",
            publishedIp: "203.0.113.7",
            remoteAccess: true,
            proxyEnabled: true,
          });
        }
        return json({
          sourceIp: "203.0.113.7",
          publishedIp: "203.0.113.7",
          remoteAccess: true,
          proxyEnabled: false,
        });
      }
      case "POST /indexer/preview": {
        type MockKV = { name: string; value?: string; remove?: boolean };
        type MockRule = {
          id?: string;
          target: string;
          indexerId?: string;
          host?: string;
          scope?: string;
          mediaType?: string;
          profile?: string;
          headers?: MockKV[] | null;
          params?: MockKV[] | null;
        };
        const req = (body ?? {}) as {
          rule?: MockRule;
          rules?: MockRule[];
          indexer?: { id?: string; url?: string; protocol?: string };
          url?: string;
        };
        const arrAccept = "application/rss+xml, text/rss+xml, application/xml, text/xml";
        const profiles: Record<string, { name: string; value: string }[]> = {
          ether: [{ name: "User-Agent", value: DEFAULT_USER_AGENT }],
          sonarr: [
            { name: "User-Agent", value: "Sonarr/4.0.20.3014 (alpine 3.24.4)" },
            { name: "Accept", value: arrAccept },
            { name: "Accept-Encoding", value: "gzip" },
          ],
          radarr: [
            { name: "User-Agent", value: "Radarr/7.0.3.10918 (alpine 3.24.4)" },
            { name: "Accept", value: arrAccept },
            { name: "Accept-Encoding", value: "gzip" },
          ],
          prowlarr: [
            { name: "User-Agent", value: "Prowlarr/2.7.0.5723 (alpine 3.24.4)" },
            { name: "Accept", value: arrAccept },
            { name: "Accept-Encoding", value: "gzip" },
          ],
          nzbhydra2: [
            { name: "User-Agent", value: "NZBHydra2 9.3.1" },
            { name: "Accept", value: "application/xml" },
            { name: "Accept-Encoding", value: "gzip" },
          ],
          sabnzbd: [
            { name: "User-Agent", value: "SABnzbd/5.1.3" },
            { name: "Accept-Encoding", value: "gzip" },
            { name: "Connection", value: "close" },
          ],
          nzbget: [
            { name: "User-Agent", value: "nzbget/26.3" },
            { name: "Accept", value: "*/*" },
            { name: "Accept-Encoding", value: "gzip" },
            { name: "Connection", value: "close" },
          ],
          browser: [
            {
              name: "User-Agent",
              value:
                "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/158.0.0.0 Safari/537.36",
            },
            {
              name: "Accept",
              value:
                "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8",
            },
            { name: "Accept-Encoding", value: "gzip, deflate" },
          ],
          custom: [{ name: "User-Agent", value: DEFAULT_USER_AGENT }],
        };
        const sensitiveHeader = (name: string) => {
          const n = name.toLowerCase();
          return (
            n === "authorization" ||
            n === "proxy-authorization" ||
            n === "cookie" ||
            n === "x-api-key" ||
            n.includes("token") ||
            n.includes("secret") ||
            n.includes("auth") ||
            n.includes("key")
          );
        };
        const sensitiveParam = (name: string) => {
          const n = name.toLowerCase();
          return (
            n === "apikey" ||
            n === "api_key" ||
            n === "key" ||
            n === "token" ||
            n === "passkey" ||
            n === "password" ||
            n === "secret" ||
            n === "auth" ||
            n.includes("token") ||
            n.includes("secret") ||
            n.includes("key")
          );
        };
        const applyHeaders = (profileId: string, overrides?: MockKV[] | null) => {
          const base = (profiles[profileId] ?? profiles.ether!).map((h) => ({ ...h }));
          for (const ov of overrides ?? []) {
            const name = ov.name.trim();
            if (!name) continue;
            const idx = base.findIndex((h) => h.name.toLowerCase() === name.toLowerCase());
            if (ov.remove) {
              if (idx >= 0) base.splice(idx, 1);
              continue;
            }
            const val = sensitiveHeader(name) && ov.value ? "••••" : (ov.value ?? "");
            if (idx >= 0) base[idx] = { name: base[idx]!.name, value: val };
            else base.push({ name, value: val });
          }
          return base;
        };
        const applyParams = (base: [string, string][], overrides?: MockKV[] | null) => {
          const map = new Map<string, string>(base);
          for (const ov of overrides ?? []) {
            const name = ov.name.trim();
            if (!name) continue;
            if (ov.remove) {
              if (name !== "apikey" && name !== "t") map.delete(name);
              continue;
            }
            map.set(name, sensitiveParam(name) ? "••••" : (ov.value ?? ""));
          }
          const keys = [...map.keys()].sort();
          return keys
            .map((k) => {
              const v = map.get(k) ?? "";
              return `${encodeURIComponent(k)}=${v === "••••" ? "••••" : encodeURIComponent(v)}`;
            })
            .join("&");
        };
        const matchHost = (pattern: string, host: string) => {
          const p = pattern.trim().toLowerCase();
          const h = host.trim().toLowerCase().replace(/:\d+$/, "");
          if (!p || !h) return false;
          if (p.startsWith("*.")) {
            const suf = p.slice(1);
            return h.length > suf.length && h.endsWith(suf);
          }
          return h === p;
        };
        const activeRules: MockRule[] =
          req.rules ?? (state.config.clientIdentity?.rules as MockRule[] | null | undefined) ?? [];
        const resolve = (
          rules: MockRule[],
          kind: "search" | "nzb_download" | "torrent_download",
          indexerId: string,
          host: string,
          mediaType = "",
        ): { profile: string; ruleId: string; headers: MockKV[]; params: MockKV[] } => {
          let bestSpec = 0;
          let best: MockRule | null = null;
          const isDownload = kind !== "search";
          const reqMedia = mediaType.trim().toLowerCase();
          for (const r of rules) {
            const ruleMedia = (r.mediaType ?? "").trim().toLowerCase();
            if (ruleMedia && ruleMedia !== reqMedia) continue;
            let targetRank = 0;
            if (r.target === "indexer") {
              if (r.indexerId !== indexerId) continue;
              if (r.scope === "search" && isDownload) continue;
              if (r.scope === "download" && !isDownload) continue;
              targetRank = 4;
            } else if (r.target === "host") {
              if (!matchHost(r.host ?? "", host)) continue;
              if (r.scope === "search" && isDownload) continue;
              if (r.scope === "download" && !isDownload) continue;
              targetRank = (r.host ?? "").trim().startsWith("*.") ? 2 : 3;
            } else if (r.target === "nzb_downloads") {
              if (kind !== "nzb_download") continue;
              targetRank = 1;
            } else if (r.target === "torrent_downloads") {
              if (kind !== "torrent_download") continue;
              targetRank = 1;
            } else if (r.target === "all_searches") {
              if (kind !== "search") continue;
              targetRank = 1;
            }
            if (targetRank === 0) continue;
            const spec = targetRank * 2 + (ruleMedia ? 1 : 0);
            if (spec > bestSpec) {
              bestSpec = spec;
              best = r;
            }
          }
          if (!best) return { profile: "ether", ruleId: "", headers: [], params: [] };
          return {
            profile: best.profile || "ether",
            ruleId: best.id || "",
            headers: best.headers ?? [],
            params: best.params ?? [],
          };
        };

        const indexers = state.config.indexers ?? [];
        const rule = req.rule;
        const previewMedia = (rule?.mediaType ?? "").trim().toLowerCase();
        let sampleHost = "indexer.example.com";
        let sampleIndexerId = "";
        let downloadKind: "nzb" | "torrent" = "nzb";
        const isShadowed = (id: string) =>
          activeRules.some((r) => {
            if (r.target !== "indexer" || r.indexerId !== id) return false;
            const rm = (r.mediaType ?? "").trim().toLowerCase();
            return !rm || rm === previewMedia;
          });
        const pickUnshadowed = (wantTorrent: boolean) =>
          indexers.find(
            (x) =>
              (!x.kind || x.kind === "newznab") &&
              (x.protocol === "torrent") === wantTorrent &&
              x.url &&
              !isShadowed(x.id),
          );
        if (rule) {
          if (rule.target === "indexer" && rule.indexerId) {
            sampleIndexerId = rule.indexerId;
            const ix = indexers.find((x) => x.id === rule.indexerId);
            if (ix?.url) {
              try {
                sampleHost = new URL(ix.url).host || sampleHost;
              } catch {
                /* keep default */
              }
            }
            if (ix?.protocol === "torrent") downloadKind = "torrent";
          } else if (rule.target === "host" && rule.host?.trim()) {
            const h = rule.host.trim();
            sampleHost = h.startsWith("*.") ? `api.${h.slice(2)}` : h;
          } else if (rule.target === "torrent_downloads") {
            downloadKind = "torrent";
            const ix = pickUnshadowed(true);
            if (ix) {
              sampleIndexerId = ix.id;
              try {
                sampleHost = new URL(ix.url).host || "tracker.example.org";
              } catch {
                sampleHost = "tracker.example.org";
              }
            } else {
              sampleHost = "tracker.example.org";
            }
          } else if (rule.target === "nzb_downloads" || rule.target === "all_searches") {
            const ix = pickUnshadowed(false);
            if (ix) {
              sampleIndexerId = ix.id;
              try {
                sampleHost = new URL(ix.url).host || sampleHost;
              } catch {
                /* keep default */
              }
            }
          }
        } else {
          const ix = req.indexer ?? (req.url ? { url: req.url } : undefined);
          if (ix?.id) sampleIndexerId = ix.id;
          if (ix?.url) {
            try {
              sampleHost = new URL(ix.url).host || sampleHost;
            } catch {
              /* keep default */
            }
          }
          if (ix?.protocol === "torrent") downloadKind = "torrent";
        }
        const evalRules = rule ? (req.rules ? req.rules : [rule]) : activeRules;
        let searchId = resolve(evalRules, "search", sampleIndexerId, sampleHost, previewMedia);
        const dlKind = downloadKind === "torrent" ? "torrent_download" : "nzb_download";
        let dlId = resolve(evalRules, dlKind, sampleIndexerId, sampleHost, previewMedia);
        if (rule) {
          const direct = {
            profile: rule.profile || "ether",
            ruleId: rule.id || "",
            headers: rule.headers ?? [],
            params: rule.params ?? [],
          };
          if (rule.target === "all_searches") {
            searchId = direct;
          } else if (rule.target === "nzb_downloads" || rule.target === "torrent_downloads") {
            dlId = direct;
          } else if (rule.target === "indexer" || rule.target === "host") {
            if (rule.scope !== "download") searchId = direct;
            if (rule.scope !== "search") dlId = direct;
          }
        }
        const sampleSearchBase: [string, string][] =
          previewMedia === "movie"
            ? [
                ["apikey", "••••"],
                ["cat", "2000"],
                ["extended", "1"],
                ["imdbid", "0133093"],
                ["limit", "100"],
                ["t", "movie"],
              ]
            : previewMedia === "music"
              ? [
                  ["apikey", "••••"],
                  ["cat", "3000"],
                  ["extended", "1"],
                  ["limit", "100"],
                  ["q", "Example Album"],
                  ["t", "search"],
                ]
              : previewMedia === "books"
                ? [
                    ["apikey", "••••"],
                    ["cat", "7000"],
                    ["extended", "1"],
                    ["limit", "100"],
                    ["q", "Example Book"],
                    ["t", "search"],
                  ]
                : [
                    ["apikey", "••••"],
                    ["cat", "5000"],
                    ["ep", "1"],
                    ["extended", "1"],
                    ["limit", "100"],
                    ["q", "Example Show"],
                    ["season", "1"],
                    ["t", "tvsearch"],
                  ];
        const searchQuery = applyParams(sampleSearchBase, searchId.params);
        const downloadPath =
          downloadKind === "torrent"
            ? `/download/example.torrent${
                dlId.params.length ? `?${applyParams([], dlId.params)}` : ""
              }`
            : `/api?${applyParams(
                [
                  ["apikey", "••••"],
                  ["id", "example-guid"],
                  ["t", "get"],
                ],
                dlId.params,
              )}`;
        const primary =
          rule &&
          (rule.target === "nzb_downloads" ||
            rule.target === "torrent_downloads" ||
            rule.scope === "download")
            ? dlId
            : searchId;
        return json({
          profile: primary.profile,
          matchedRuleId: primary.ruleId || undefined,
          downloadKind,
          search: {
            requestLine: `GET /api?${searchQuery} HTTP/1.1\nHost: ${sampleHost}`,
            headers: applyHeaders(searchId.profile, searchId.headers),
            profile: searchId.profile,
            matchedRuleId: searchId.ruleId || undefined,
          },
          download: {
            requestLine: `GET ${downloadPath} HTTP/1.1\nHost: ${sampleHost}`,
            headers: applyHeaders(dlId.profile, dlId.headers),
            profile: dlId.profile,
            matchedRuleId: dlId.ruleId || undefined,
          },
          profiles: Object.entries(profiles).map(([id, hs]) => ({
            id,
            userAgent: hs[0]?.value ?? DEFAULT_USER_AGENT,
          })),
        });
      }
      case "POST /test/indexer-identity": {
        const payload = (body ?? {}) as {
          indexerId?: string;
          rules?: Array<{
            id?: string;
            target: string;
            indexerId?: string;
            host?: string;
            scope?: string;
            mediaType?: string;
            profile?: string;
            headers?: Array<{ name: string; value?: string; remove?: boolean }> | null;
          }>;
        };
        const ix = (state.config.indexers ?? []).find((x) => x.id === payload.indexerId);
        if (!ix) return err(404, "not_found", "indexer not found");
        const rules = payload.rules ?? state.config.clientIdentity?.rules ?? [];
        let host = "";
        try {
          host = new URL(ix.url).hostname.toLowerCase();
        } catch {
          host = "";
        }
        let bestSpec = 0;
        let matched: (typeof rules)[number] | null = null;
        for (const r of rules) {
          if ((r.mediaType ?? "").trim()) continue;
          let spec = 0;
          if (r.target === "indexer" && r.indexerId === ix.id && r.scope !== "download") spec = 4;
          else if (r.target === "host" && r.scope !== "download") {
            const p = (r.host ?? "").trim().toLowerCase();
            if (p.startsWith("*.")) {
              const suf = p.slice(1);
              if (host.length > suf.length && host.endsWith(suf)) spec = 2;
            } else if (p && host === p) spec = 3;
          } else if (r.target === "all_searches") spec = 1;
          if (spec > bestSpec) {
            bestSpec = spec;
            matched = r;
          }
        }
        const profile = matched?.profile || "ether";
        return json({
          ok: true,
          durationMs: 48 + (hash(ix.id) % 120),
          profile,
          ruleId: matched?.id || undefined,
          userAgent:
            matched?.headers?.find((h) => h.name.toLowerCase() === "user-agent" && !h.remove)
              ?.value || DEFAULT_USER_AGENT,
        });
      }
      case "POST /test/tvdb":
      case "POST /test/tmdb": {
        const key = (body as { key?: string } | null)?.key?.trim();
        const source = route.endsWith("/tmdb") ? "tmdb" : "tvdb";
        const configured = metadataSources().some(
          (entry) => entry.id === source && entry.configured,
        );
        return json({ ok: Boolean(key && key !== "••••") || configured, durationMs: 73 });
      }
      case "POST /test/usenet":
      case "POST /test/indexer":
      case "POST /test/debrid":
      case "POST /test/introdb":
      case "POST /test/llm":
      case "POST /test/trakt":
      case "POST /test/opensubtitles":
      case "POST /test/subdl":
      case "POST /test/subsource":
        return json({ ok: true, durationMs: 40 + (hash(route) % 200) });
      case "GET /playback/policy": {
        const activeOverrides =
          (
            (state.config as unknown as Record<string, unknown>).clients as
              { overrides?: Array<Record<string, unknown>> } | undefined
          )?.overrides ??
          opts.capabilityOverrides ??
          [];
        return json({
          overrides: activeOverrides,
          apps: [
            {
              protocol: "plex",
              name: "Plezy",
              devices: ["Home theater PC", "Apple TV", "iOS", "Android TV", "Android"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc", "vp9", "av1", "mpeg2video", "vc1", "mpeg4"],
              audioCodecs: [
                "aac",
                "ac3",
                "eac3",
                "truehd",
                "dts",
                "dtshd",
                "mp3",
                "flac",
                "opus",
                "vorbis",
                "pcm",
              ],
              containers: ["mp4", "mkv", "mov", "avi", "mpegts", "webm"],
              subtitles: ["srt", "subrip", "webvtt", "vtt", "ass", "ssa", "pgs", "vobsub"],
              dolbyVisionProfiles: [5, 7, 8],
              hdr10: true,
              dolbyVision: false,
              directPlay: "builtin",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "plex",
              name: "Plex for Apple TV",
              devices: ["Apple TV"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc"],
              audioCodecs: ["aac", "ac3", "eac3", "mp3", "flac", "alac", "opus"],
              containers: ["mp4", "mov", "mkv", "mpegts"],
              subtitles: ["srt", "ass", "vtt"],
              dolbyVisionProfiles: [5, 8],
              hdr10: true,
              dolbyVision: true,
              directPlay: "reported",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "plex",
              name: "Plex for iOS",
              devices: ["iOS"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc"],
              audioCodecs: ["aac", "ac3", "eac3", "mp3", "flac", "alac", "opus"],
              containers: ["mp4", "mov", "mkv", "mpegts"],
              subtitles: ["srt", "ass", "vtt"],
              dolbyVisionProfiles: [5, 8],
              hdr10: true,
              dolbyVision: true,
              directPlay: "reported",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "plex",
              name: "Plex for Android phones",
              product: "Plex for Android (Mobile)",
              devices: ["Android"],
              maxHeight: 1080,
              videoCodecs: ["h264", "hevc", "vp9"],
              audioCodecs: ["aac", "ac3", "eac3", "mp3", "flac", "opus", "vorbis"],
              containers: ["mp4", "mkv", "mpegts", "webm"],
              subtitles: ["srt", "ass", "vtt"],
              directPlay: "reported",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "plex",
              name: "Plex for Android TV",
              product: "Plex for Android (TV)",
              devices: ["Android TV", "Google TV", "NVIDIA Shield"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc", "vp9", "av1"],
              audioCodecs: ["aac", "ac3", "eac3", "mp3", "flac", "opus", "vorbis"],
              containers: ["mp4", "mkv", "mpegts", "webm"],
              subtitles: ["srt", "ass", "vtt"],
              hdr10: true,
              directPlay: "reported",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "plex",
              name: "Plex HTPC",
              devices: ["Home theater PC"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc", "vp9", "av1", "mpeg2video", "vc1", "mpeg4"],
              audioCodecs: ["aac", "ac3", "eac3", "truehd", "dts", "dtshd", "mp3", "flac", "opus"],
              containers: ["mp4", "mkv", "mov", "avi", "mpegts", "webm"],
              subtitles: ["srt", "ass", "pgs", "vobsub"],
              dolbyVisionProfiles: [5, 7, 8],
              hdr10: true,
              directPlay: "reported",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "plex",
              name: "Plex for Roku",
              devices: ["Roku"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc"],
              audioCodecs: ["aac", "ac3", "eac3", "mp3", "flac"],
              containers: ["mp4", "mkv", "mpegts"],
              subtitles: ["srt", "vtt"],
              hdr10: true,
              directPlay: "reported",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "plex",
              name: "Plex Web",
              devices: ["Browser"],
              maxHeight: 2160,
              videoCodecs: ["h264"],
              subtitles: ["srt", "vtt", "ass"],
              directPlay: "browser",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "jellyfin",
              name: "Swiftfin",
              devices: ["Apple TV", "iOS"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc"],
              audioCodecs: ["aac", "ac3", "eac3", "mp3", "flac", "alac", "opus"],
              containers: ["mp4", "mov", "mkv", "mpegts"],
              subtitles: ["srt", "ass", "vtt"],
              dolbyVisionProfiles: [5, 8],
              hdr10: true,
              dolbyVision: true,
              directPlay: "reported",
              note: "",
              certification: "pending",
            },
            {
              protocol: "jellyfin",
              name: "Infuse",
              devices: ["Apple TV", "iOS"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc", "vp9", "mpeg2video", "vc1", "mpeg4"],
              subtitles: ["srt", "ass", "pgs", "vobsub"],
              dolbyVisionProfiles: [5, 7, 8],
              hdr10: true,
              directPlay: "reported",
              note: "capture",
              certification: "pending",
            },
            {
              protocol: "jellyfin",
              name: "Streamyfin",
              devices: ["Android"],
              videoCodecs: ["h264", "hevc", "vp8", "vp9", "av1"],
              subtitles: ["srt", "vtt", "ttml"],
              directPlay: "listed",
              note: "no-dv5",
              certification: "pending",
            },
            {
              protocol: "jellyfin",
              name: "Findroid",
              devices: ["Android"],
              videoCodecs: [],
              subtitles: ["srt", "ass"],
              directPlay: "findroid",
              note: "",
              certification: "pending",
            },
            {
              protocol: "jellyfin",
              name: "Jellyfin TV",
              devices: ["Android TV"],
              videoCodecs: [],
              subtitles: ["srt"],
              directPlay: "reported",
              note: "",
              certification: "pending",
            },
            {
              protocol: "webdav",
              name: "Kodi",
              devices: ["Kodi", "Home theater PC", "Android TV"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc", "vp9", "av1", "mpeg2video", "vc1", "mpeg4"],
              subtitles: ["srt", "ass", "pgs", "vobsub"],
              dolbyVisionProfiles: [5, 7, 8],
              hdr10: true,
              directPlay: "player",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "webdav",
              name: "VLC",
              devices: ["Home theater PC", "Mac", "Apple TV", "iOS", "Android"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc", "vp9", "av1", "mpeg2video", "vc1", "mpeg4"],
              subtitles: ["srt", "ass", "pgs", "vobsub"],
              dolbyVisionProfiles: [5, 7, 8],
              hdr10: true,
              directPlay: "player",
              note: "",
              certification: "builtin",
            },
            {
              protocol: "stremio",
              name: "Stremio TV",
              devices: ["Android TV"],
              maxHeight: 2160,
              videoCodecs: ["h264", "hevc", "vp9", "av1"],
              subtitles: ["srt", "ass", "pgs"],
              dolbyVisionProfiles: [8],
              hdr10: true,
              directPlay: "stream",
              note: "",
              certification: "builtin",
            },
          ],
          devices: [
            {
              name: "Apple TV",
              id: "apple-tv",
              dolbyVisionProfiles: [5, 8],
              subtitleCodecs: ["srt"],
              videoCodecs: ["h264", "hevc"],
              maxHeight: 2160,
              hdr10: true,
              dolbyVision: true,
              builtinAudioCodecs: ["aac", "ac3", "eac3", "mp3", "flac", "alac", "opus"],
              builtinContainers: ["mp4", "mov", "mkv", "mpegts"],
              builtinMaxHeight: 2160,
              builtinHdr10: true,
              builtinDolbyVision: true,
              notes:
                "Dolby Vision profile 7 uses a compatible HDR base layer; TrueHD requires an audio transcode unless declared supported.",
            },
            {
              name: "NVIDIA Shield",
              id: "shield",
              dolbyVisionProfiles: [5, 8],
              subtitleCodecs: ["srt", "ass", "pgs", "vobsub"],
              videoCodecs: ["h264", "hevc", "vp9"],
              maxHeight: 2160,
              hdr10: true,
              dolbyVision: false,
              builtinAudioCodecs: ["aac", "ac3", "eac3", "mp3", "flac", "opus", "vorbis"],
              builtinContainers: ["mp4", "mkv", "mpegts", "webm"],
              builtinMaxHeight: 2160,
              builtinHdr10: true,
              builtinDolbyVision: false,
              notes: "Text is served separately; image subtitles require explicit client support.",
            },
            {
              name: "Android TV",
              id: "android-tv",
              dolbyVisionProfiles: [],
              subtitleCodecs: ["srt"],
              videoCodecs: ["h264", "hevc", "vp9", "av1"],
              maxHeight: 2160,
              hdr10: true,
              dolbyVision: false,
              builtinMaxHeight: 2160,
              builtinHdr10: true,
              builtinDolbyVision: false,
              notes: "Text is served separately; image subtitles require explicit client support.",
            },
            {
              name: "iOS",
              id: "ios",
              dolbyVisionProfiles: [5, 8],
              subtitleCodecs: ["srt", "ass", "vtt"],
              videoCodecs: ["h264", "hevc"],
              maxHeight: 2160,
              hdr10: true,
              dolbyVision: true,
              builtinMaxHeight: 2160,
              builtinHdr10: true,
              builtinDolbyVision: true,
              notes: "Text is served separately; image subtitles require explicit client support.",
            },
            {
              name: "Home theater PC",
              id: "htpc",
              dolbyVisionProfiles: [5, 7, 8],
              subtitleCodecs: ["srt", "ass", "pgs", "vobsub"],
              videoCodecs: ["h264", "hevc", "vp9", "av1", "mpeg2video", "vc1", "mpeg4"],
              maxHeight: 2160,
              hdr10: true,
              dolbyVision: false,
              builtinMaxHeight: 2160,
              builtinHdr10: true,
              builtinDolbyVision: false,
              notes: "Text is served separately; image subtitles require explicit client support.",
            },
            {
              name: "Roku",
              id: "roku",
              dolbyVisionProfiles: [],
              subtitleCodecs: ["srt", "vtt"],
              videoCodecs: ["h264", "hevc"],
              maxHeight: 2160,
              hdr10: true,
              dolbyVision: false,
              builtinMaxHeight: 2160,
              builtinHdr10: true,
              builtinDolbyVision: false,
              notes: "Text is served separately; image subtitles require explicit client support.",
            },
            {
              name: "LG webOS",
              id: "webos",
              dolbyVisionProfiles: [],
              subtitleCodecs: ["srt"],
              videoCodecs: ["h264", "hevc"],
              maxHeight: 2160,
              hdr10: true,
              dolbyVision: false,
              builtinMaxHeight: 2160,
              builtinHdr10: true,
              builtinDolbyVision: false,
              notes: "Text is served separately; image subtitles require explicit client support.",
            },
            {
              name: "Samsung Tizen",
              id: "tizen",
              dolbyVisionProfiles: [],
              subtitleCodecs: ["srt"],
              videoCodecs: ["h264", "hevc"],
              maxHeight: 2160,
              hdr10: true,
              dolbyVision: false,
              builtinMaxHeight: 2160,
              builtinHdr10: true,
              builtinDolbyVision: false,
              notes: "Text is served separately; image subtitles require explicit client support.",
            },
            {
              name: "Browser",
              id: "browser",
              dolbyVisionProfiles: [],
              subtitleCodecs: ["srt"],
              videoCodecs: ["h264"],
              maxHeight: 2160,
              hdr10: false,
              dolbyVision: false,
              builtinMaxHeight: 2160,
              builtinHdr10: false,
              builtinDolbyVision: false,
              notes: "Browser codec declarations govern playback; text subtitles remain sidecars.",
            },
            {
              name: "DLNA TV or receiver",
              dolbyVisionProfiles: [],
              subtitleCodecs: ["srt", "subrip"],
              notes: "The TV or receiver advertises what it can play.",
            },
          ],
          seenDevices: [
            {
              clientIdentifier: "mock-tv",
              name: "Living room TV",
              platform: "Android",
              device: "Xiaomi TV Box",
              app: "Plex for Android (TV)",
              viewer: "Alex",
              lastSeen: new Date().toISOString(),
            },
            {
              clientIdentifier: "mock-plezy",
              name: "Desk Linux PC",
              platform: "Linux",
              device: "Arch Workstation",
              app: "Plezy",
              viewer: "You",
              lastSeen: new Date(Date.now() - 1200000).toISOString(),
            },
            {
              clientIdentifier: "mock-appletv",
              name: "Bedroom Apple TV",
              platform: "tvOS",
              device: "Apple TV",
              app: "Swiftfin",
              viewer: "You",
              lastSeen: new Date(Date.now() - 2400000).toISOString(),
            },
            {
              clientIdentifier: "mock-phone",
              name: "Alex’s phone",
              platform: "iOS",
              device: "iPhone",
              app: "Plex for iOS",
              viewer: "Alex",
              lastSeen: new Date(Date.now() - 3600000).toISOString(),
            },
          ].map((row) => {
            const index = activeOverrides.findLastIndex(
              (o) =>
                o.device &&
                (row.device.toLowerCase().includes(String(o.device).toLowerCase()) ||
                  row.name.toLowerCase().includes(String(o.device).toLowerCase())) &&
                (!o.platform ||
                  row.platform.toLowerCase().includes(String(o.platform).toLowerCase())) &&
                (!o.product || row.app.toLowerCase().includes(String(o.product).toLowerCase())),
            );
            return { ...row, ...(index >= 0 ? { overrideIndex: index } : {}) };
          }),
          observed: [
            {
              protocol: "plex",
              name: "Plezy",
              lastSeen: new Date(Date.now() - 1200000).toISOString(),
            },
            {
              protocol: "jellyfin",
              name: "Swiftfin",
              lastSeen: new Date(Date.now() - 2400000).toISOString(),
            },
            { protocol: "jellyfin", name: "Jellyfin TV", lastSeen: "2026-10-31T12:00:00Z" },
          ],
        });
      }
      case "GET /playback/prowlarr":
        return json({ config: {}, status: "" });
      case "POST /playback/prowlarr/tags":
        return json({
          tags: [
            { id: 1, label: "ether" },
            { id: 2, label: "anime" },
          ],
        });
      case "POST /playback/prowlarr": {
        const req = body as {
          config?: { dailyLimit?: number; dailyGrabs?: number };
          import?: boolean;
        };
        const found = [
          { id: "prowlarr-1", name: "Prowlarr Usenet", protocol: "usenet" as const },
          { id: "prowlarr-2", name: "Prowlarr Torrents", protocol: "torrent" as const },
        ].map((ix, i) => ({
          ...ix,
          url: `http://localhost:9696/${i + 1}/`,
          apiKey: "••••",
          dailyLimit: req.config?.dailyLimit ?? 0,
          dailyGrabs: req.config?.dailyGrabs ?? 0,
          priority: (state.config.indexers ?? []).length + i,
          enabled: true,
          movieCategories: null,
          tvCategories: null,
        }));
        if (req.import) {
          const have = new Set((state.config.indexers ?? []).map((ix) => ix.id));
          state.config.indexers = [
            ...(state.config.indexers ?? []),
            ...found.filter((ix) => !have.has(ix.id)),
          ];
        }
        return json({ indexers: found, imported: Boolean(req.import) });
      }
      case "GET /subtitle-sync":
        return json([
          {
            id: "abc123",
            language: "pt-br",
            release: "Some.Show.S01E02.720p.WEB-DL-GROUP",
            fetchedAt: new Date(Date.now() - 3 * 3600_000).toISOString(),
            title: "The Episode",
            showTitle: "Some Show",
            season: 1,
            episode: 2,
          },
          {
            id: "orphan99",
            language: "en",
            release: "Other.Movie.2023.720p",
            fetchedAt: new Date(Date.now() - 26 * 3600_000).toISOString(),
            title: "",
          },
        ]);
      case "GET /disk":
        return json({
          path: q.get("path") ?? "",
          probedPath: "/",
          totalBytes: 2e12,
          freeBytes: 1.2e12,
          usedBytes: 0.8e12,
          cacheBudgetBytes: state.config.cache.budgetBytes,
        });
      case "GET /updates":
        return json(updateStatus());
      case "GET /updates/notes": {
        const version = q.get("version");
        if (version === availableVersion)
          return json({ version, markdown: availableNotes, notesUrl });
        if (version && version !== state.boot.version)
          return err(404, "release_not_found", "Release notes not found for this version.");
        return json({ version: state.boot.version, markdown: currentNotes, unseen: notesUnseen });
      }
      case "POST /updates/notes/dismiss":
        notesUnseen = false;
        return new Response(null, { status: 204 });
      case "POST /updates/check":
        lastUpdateCheck = Date.now();
        return json(updateStatus(), 202);
      case "POST /updates/apply":
        updateRequested = true;
        updateForced = Boolean((body as { force?: boolean } | null)?.force);
        return json(updateStatus(), 202);
      case "GET /users":
        return json({
          users: state.users
            .filter((u) => !state.removedViewers.has(u.id))
            .map((u) => {
              const lifecycle = u.lifecycle ?? mockLifecycle(u);
              return { ...u, lifecycle: { ...lifecycle, paused: state.pausedViewers.has(u.id) } };
            }),
        });
      case "GET /cluster":
        return json(clusterStatus());
      case "GET /cluster/setup":
        return json({ available: true, state: clusterSetupState });
      case "POST /cluster/setup/check": {
        const req = body as { mode: string; token: string; replace: boolean };
        if (req.mode === "join" && state.boot.setupComplete && !req.replace)
          return err(409, "replace_required", "Confirm replacement of this server’s identity.");
        if (req.mode === "join" && !req.token.startsWith("ether-join-v1:"))
          return err(400, "invalid_token", "The join token is not valid.");
        return json({ ok: true });
      }
      case "POST /cluster/setup/apply":
        state.clustered = true;
        clusterSetupState = "complete";
        return json({ state: "restarting" }, 202);
      case "PATCH /cluster/node": {
        const req = body as { weight?: number; draining?: boolean };
        if (req.weight !== undefined) state.config.server.clusterWeight = req.weight;
        if (req.draining !== undefined) nodePaused = req.draining;
        return json({ weight: state.config.server.clusterWeight || 1, draining: nodePaused });
      }
      case "GET /cluster/settings":
        return json(
          state.clustered
            ? {
                clustered: true,
                keyFingerprint: "3f9c2a71d04be6a8",
                sharedVersion: 42,
                // Match the active-only owner token list; retain historical fixtures
                // in state so UI filtering can also be exercised independently.
                joinTokens: state.joinTokens.filter(
                  (token) => !token.usedAt && Date.parse(token.expiresAt) > Date.now(),
                ),
              }
            : { clustered: false, keyFingerprint: "3f9c2a71d04be6a8" },
        );
      case "POST /cluster/join-tokens": {
        if (!state.clustered)
          return err(409, "not_clustered", "clustering is not enabled: this server uses SQLite.");
        const ttl = Number((body as { ttlMinutes?: number } | null)?.ttlMinutes ?? 0);
        const minutes = ttl > 0 && ttl < 60 ? ttl : 60;
        const id = `m${seq++}x`;
        const info: JoinTokenInfo = {
          id,
          createdAt: new Date().toISOString(),
          expiresAt: new Date(Date.now() + minutes * 60_000).toISOString(),
        };
        state.joinTokens.unshift(info);
        return json(
          {
            token: `ether-join-v1:mock-${id}-c2VjcmV0LWRvLW5vdC1zaGFyZQ`,
            id,
            expiresAt: info.expiresAt,
            command: "ether cluster join <token>",
            warning:
              "This token is a secret: it contains the cluster database password and the credential key that decrypts every stored credential. Use it once, within the hour, and do not paste it into chats or tickets.",
          },
          201,
        );
      }
      case "GET /adoption/backups":
        return json({
          snapshots: state.backups,
          settings: state.backupSettings,
          warning: "",
        });
      case "POST /adoption/backups/settings": {
        const next = body as {
          intervalHours: number;
          retain: number;
          sharedDir?: string;
          skipOnAirMedia?: boolean;
        };
        if (next.sharedDir && !state.clustered)
          return err(
            400,
            "invalid_request",
            "a shared backup directory applies only to clustered servers",
          );
        state.backupSettings = {
          ...state.backupSettings,
          intervalHours: next.intervalHours,
          retain: next.retain,
          skipOnAirMedia: !!next.skipOnAirMedia,
          ...(next.sharedDir !== undefined ? { sharedDir: next.sharedDir } : {}),
        };
        return json({ ok: true });
      }
      case "POST /adoption/backups": {
        const created = new Date().toISOString();
        const snap = {
          name: `ether-${created.slice(0, 19).replace(/[-:]/g, "")}.tar.gz`,
          size: 4_300_000,
          created,
          ...(state.clustered
            ? state.backupSettings.sharedDir
              ? { node: "den", nodeId: "n-den", shared: true }
              : { node: "den", nodeId: "n-den" }
            : {}),
        };
        state.backups.unshift(snap);
        return json(snap);
      }
      case "GET /logs": {
        const lvl = q.get("level") ?? "";
        const order = ["debug", "info", "warn", "error"];
        const needle = (q.get("q") ?? "").toLowerCase();
        const entries = state.logs.filter(
          (e) =>
            (!lvl || order.indexOf(e.level.toLowerCase()) >= order.indexOf(lvl)) &&
            (!needle || e.message.toLowerCase().includes(needle)),
        );
        return json({
          entries: entries.slice(-Number(q.get("limit") ?? 500)),
          total: state.logs.length,
        });
      }
      case "DELETE /logs":
        state.logs = [];
        return json({ ok: true });
      case "GET /logs/download":
        return new Response(
          "# Ether mock logs\n" +
            state.logs.map((e) => `${e.time} ${e.level} ${e.message}`).join("\n"),
          { status: 200, headers: { "Content-Type": "text/plain; charset=utf-8" } },
        );
      case "GET /collections":
        return json({ collections: state.collections });
      case "POST /collections": {
        const spec = normalizeSpec(body as CollectionSpec);
        const saved = {
          ...spec,
          id: `c${seq++}`,
          updatedAt: new Date().toISOString(),
          lastBuiltAt: new Date().toISOString(),
          itemCount:
            spec.limit > 0 ? Math.min(evaluate(spec).total, spec.limit) : evaluate(spec).total,
        };
        state.collections.push(saved);
        return json(saved);
      }
      case "POST /collections/preview":
        return json(evaluate(normalizeSpec(body as CollectionSpec)));
      case "GET /collections/templates":
        return json({ templates: TEMPLATES });
      case "GET /collections/template-bundles":
        return json({ bundles: TEMPLATE_BUNDLES });
      case "GET /collections/addon-imports": {
        const availableAddons = [
          {
            addonId: "cinemeta",
            name: "Cinemeta",
            host: "catalogue.example",
            enabled: true,
            catalogCount: 4,
            hasImport: state.addonImports.some((s) => s.addonId === "cinemeta"),
          },
        ];
        return json({
          imports: state.addonImports.map((sub) => {
            const linked = state.collections.filter((c) => c.addonSync?.addonId === sub.addonId);
            return {
              ...sub,
              managedCount: linked.length,
              removedCount: linked.filter((c) => c.addonSync?.removed).length,
            };
          }),
          availableAddons,
        });
      }
      case "POST /collections/addon-imports/preview": {
        const req = (body ?? {}) as { addonId?: string; url?: string };
        const addonId = req.addonId?.trim() || "addon.aiometadata";
        const isCinemeta = addonId === "cinemeta" || (req.url ?? "").includes("cinemeta");
        const resolvedId = isCinemeta ? "cinemeta" : addonId;
        const addonName = isCinemeta ? "Cinemeta" : "AIOMetadata";
        let host = isCinemeta ? "catalogue.example" : "aiometadata.example";
        if (req.url) {
          try {
            host = new URL(req.url.replace(/^stremio:\/\//i, "https://")).host || host;
          } catch {
            // keep default host
          }
        }
        const rawCatalogs: Omit<
          AddonCatalogPreviewItem,
          | "order"
          | "hasDuplicateName"
          | "imported"
          | "existingCollectionId"
          | "existingCollectionName"
          | "existingParams"
        >[] = isCinemeta
          ? [
              {
                key: "movie/popular",
                type: "movie",
                id: "popular",
                name: "Popular",
                mediaType: "movie",
                supportsSkip: true,
                standalone: true,
              },
              {
                key: "series/popular",
                type: "series",
                id: "popular",
                name: "Popular",
                mediaType: "show",
                supportsSkip: true,
                standalone: true,
              },
              {
                key: "movie/classics",
                type: "movie",
                id: "classics",
                name: "Classics",
                mediaType: "movie",
                supportsSkip: true,
                standalone: true,
              },
              {
                key: "movie/search",
                type: "movie",
                id: "search",
                name: "Search",
                mediaType: "movie",
                standalone: false,
                requiredExtras: ["search"],
                extras: [{ name: "search", label: "Search", required: true }],
              },
            ]
          : [
              {
                key: "movie/popular",
                type: "movie",
                id: "popular",
                name: "Popular",
                mediaType: "movie",
                supportsSkip: true,
                standalone: true,
              },
              {
                key: "series/popular",
                type: "series",
                id: "popular",
                name: "Popular",
                mediaType: "show",
                supportsSkip: true,
                standalone: true,
              },
              {
                key: "movie/trakt-trending",
                type: "movie",
                id: "trakt-trending",
                name: "Trending Now",
                mediaType: "movie",
                supportsSkip: true,
                standalone: true,
              },
              {
                key: "series/mdblist-acclaimed",
                type: "series",
                id: "mdblist-acclaimed",
                name: "Critically Acclaimed",
                mediaType: "show",
                supportsSkip: true,
                standalone: true,
              },
              {
                key: "movie/by-genre",
                type: "movie",
                id: "by-genre",
                name: "By Genre",
                mediaType: "movie",
                supportsSkip: true,
                standalone: false,
                requiredExtras: ["genre"],
                extras: [
                  {
                    name: "genre",
                    label: "Genre",
                    required: true,
                    options: ["Sci-Fi", "Horror", "Documentary", "Animation"],
                  },
                ],
              },
              {
                key: "movie/search",
                type: "movie",
                id: "search",
                name: "Search",
                mediaType: "movie",
                standalone: false,
                requiredExtras: ["search"],
                extras: [{ name: "search", label: "Search", required: true }],
              },
            ];
        const nameCounts = new Map<string, number>();
        for (const c of rawCatalogs) {
          const k = c.name.toLowerCase();
          nameCounts.set(k, (nameCounts.get(k) ?? 0) + 1);
        }
        const catalogs: AddonCatalogPreviewItem[] = rawCatalogs.map((c, idx) => {
          const existing = state.collections.find(
            (col) => col.addonSync?.addonId === resolvedId && col.addonSync?.catalogKey === c.key,
          );
          const dup = (nameCounts.get(c.name.toLowerCase()) ?? 0) > 1;
          return {
            ...c,
            order: idx + 1,
            hasDuplicateName: dup,
            imported: Boolean(existing?.id),
            ...(existing?.id
              ? { existingCollectionId: existing.id, existingCollectionName: existing.name }
              : {}),
          };
        });
        const preview: AddonCatalogPreview = {
          addonId: resolvedId,
          installed:
            resolvedId === "cinemeta" || state.addonImports.some((s) => s.addonId === resolvedId),
          addonName,
          addonVersion: "2.4.0",
          host,
          addonDescription: isCinemeta
            ? "Official Stremio movie and series catalogues"
            : "Unified Trakt, MDBList, and TMDB catalogue add-on",
          catalogs,
          subscription: state.addonImports.find((s) => s.addonId === resolvedId),
        };
        return json(preview);
      }
      case "POST /collections/addon-imports": {
        const req = (body ?? {}) as AddonCatalogImportRequest;
        const addonId = req.addonId?.trim() || "addon.aiometadata";
        const addonName = addonId === "cinemeta" ? "Cinemeta" : "AIOMetadata";
        const folder = (req.folder ?? addonName).trim() || addonName;
        const nameMode: AddonImportNameMode = req.nameMode ?? "autoType";
        const nameAffix = (req.nameAffix ?? "").trim();
        const now = new Date().toISOString();
        const createdIds: string[] = [];
        let createdCount = 0;
        let updatedCount = 0;
        const selections = req.items ?? [];
        const baseNameOf = (sel: (typeof selections)[number]) => {
          const keyParts = sel.key.split("/");
          const raw = (keyParts[1] ?? sel.key).replace(/[-_]+/g, " ");
          return raw.replace(/\b\w/g, (ch: string) => ch.toUpperCase());
        };
        const dupCounts = new Map<string, number>();
        for (const s of selections) {
          const n = (s.name?.trim() || baseNameOf(s)).toLowerCase();
          dupCounts.set(n, (dupCounts.get(n) ?? 0) + 1);
        }
        selections.forEach((sel, idx) => {
          const mediaType = sel.key.startsWith("series/") ? "show" : "movie";
          const base = sel.name?.trim() || baseNameOf(sel);
          const dup = (dupCounts.get(base.toLowerCase()) ?? 0) > 1;
          let resolvedName = base;
          if (!sel.name?.trim()) {
            const typeLabel = mediaType === "show" ? "Series" : "Movies";
            if (nameMode === "appendType" || (nameMode === "autoType" && dup)) {
              resolvedName = `${base} (${typeLabel})`;
            } else if (nameMode === "prefix" && nameAffix) {
              resolvedName = `${nameAffix} ${base}`;
            } else if (nameMode === "suffix" && nameAffix) {
              resolvedName = `${base} ${nameAffix}`;
            }
          }
          const existingIdx = state.collections.findIndex(
            (c) => c.addonSync?.addonId === addonId && c.addonSync?.catalogKey === sel.key,
          );
          const pluginSource = {
            kind: `plugin:${addonId}/${sel.key}`,
            ...(sel.params ? { params: sel.params } : {}),
          } as unknown as Source;
          const spec = normalizeSpec({
            name: resolvedName,
            mediaType,
            sources: [pluginSource],
            rules: [],
            limit: sel.maxItems ?? req.maxItems ?? 100,
            refresh: req.refresh ?? "daily",
            browseInApps: req.browseInApps ?? true,
            folder,
            sortOrder: idx + 1,
            enabled: req.enabled ?? true,
            addonSync: {
              addonId,
              addonName,
              catalogType: sel.key.split("/")[0] ?? "movie",
              catalogId: sel.key.split("/")[1] ?? sel.key,
              catalogKey: sel.key,
              managedName: resolvedName,
            },
          });
          if (existingIdx >= 0) {
            const existing = state.collections[existingIdx]!;
            const saved: CollectionSpec = {
              ...existing,
              ...spec,
              id: existing.id,
              updatedAt: now,
            };
            state.collections[existingIdx] = saved;
            updatedCount++;
            if (saved.id) createdIds.push(saved.id);
          } else {
            const id = `c${seq++}`;
            const saved: CollectionSpec = {
              ...spec,
              id,
              updatedAt: now,
              lastBuiltAt: now,
              itemCount: Math.min(req.maxItems || 100, evaluate(spec).total || 18),
            };
            state.collections.push(saved);
            createdCount++;
            createdIds.push(id);
          }
        });
        const sub: AddonCatalogSubscription = {
          addonId,
          addonName,
          host: addonId === "cinemeta" ? "catalogue.example" : "aiometadata.example",
          addonEnabled: true,
          folder,
          nameMode,
          nameAffix,
          followManifest: Boolean(req.followManifest),
          refresh: req.refresh ?? "daily",
          maxItems: req.maxItems ?? 100,
          enabled: req.enabled ?? true,
          browseInApps: req.browseInApps ?? true,
          catalogCount: addonId === "cinemeta" ? 4 : 6,
          managedCount: state.collections.filter((c) => c.addonSync?.addonId === addonId).length,
          removedCount: 0,
          lastSyncedAt: now,
          lastSummary: { created: createdCount, updated: updatedCount, removed: 0, restored: 0 },
        };
        const subIdx = state.addonImports.findIndex((s) => s.addonId === addonId);
        if (subIdx >= 0) state.addonImports[subIdx] = sub;
        else state.addonImports.push(sub);
        return json({
          addonId,
          addonName,
          created: createdCount,
          updated: updatedCount,
          removed: 0,
          restored: 0,
          collectionIds: createdIds,
          subscription: sub,
        });
      }
      case "POST /libraries/preview":
      case "POST /catalog/visibility/preview": {
        const reqLibId = Number((body as { libraryId?: number }).libraryId ?? 0);
        const reqLib = reqLibId ? mockLibs.find((l) => l.id === reqLibId) : undefined;
        const reqType = reqLib
          ? String(reqLib.mediaType ?? "")
          : String((body as { mediaType?: string }).mediaType ?? "");
        const scopedCatalog = reqType ? catalog.filter((t) => t.type === reqType) : catalog;
        const groups =
          (body as { exclusions: { mediaType: string; match: string; rules: Rule[] }[] })
            .exclusions ??
          (body as { rules?: { mediaType: string; match: string; rules: Rule[] }[] }).rules ??
          [];
        const hits = (g: (typeof groups)[number]) =>
          scopedCatalog.filter(
            (t) =>
              (g.mediaType === "mixed" || g.mediaType === t.type) &&
              (g.match === "any"
                ? g.rules.some(
                    (r) =>
                      (r.field === "group" ||
                        r.field === "viewer" ||
                        r.field.startsWith("browse:") ||
                        fieldValues(t, r.field).length > 0) &&
                      matches(t, r),
                  )
                : g.rules.every(
                    (r) =>
                      (r.field === "group" ||
                        r.field === "viewer" ||
                        r.field.startsWith("browse:") ||
                        fieldValues(t, r.field).length > 0) &&
                      matches(t, r),
                  )),
          );
        const items = scopedCatalog.filter((t) =>
          hiddenByRules(
            t,
            groups,
            String((body as { visibilityDefault?: string }).visibilityDefault ?? "show"),
          ),
        );
        const shown = scopedCatalog.filter((t) => !items.includes(t));
        return json({
          total: scopedCatalog.length,
          hidden: items.length,
          visible: shown.length,
          movies: items.filter((t) => t.type === "movie").length,
          shows: items.filter((t) => t.type === "show").length,
          rules: groups.map((g) => {
            const hit = hits(g);
            return {
              movies: hit.filter((t) => t.type === "movie").length,
              shows: hit.filter((t) => t.type === "show").length,
            };
          }),
          unknownCountry: scopedCatalog.filter((t) => !fieldValues(t, "country").length).length,
          items: items.slice(0, 48).map(card),
          shown: shown.slice(0, 48).map(card),
        });
      }
      case "GET /catalog/interpret":
        return json({ enabled: Boolean(state.config.llm.enabled && state.config.llm.model) });
      case "POST /catalog/interpret":
        return state.config.llm.enabled
          ? json({ chips: interpretDemo((body as { text: string }).text || "") })
          : err(501, "not_configured", "AI filters are unavailable");
      case "GET /catalog/browse": {
        const needle = (q.get("q") ?? "").toLowerCase();
        const advanced = JSON.parse(q.get("filter") || "{}");
        const type = q.get("type");
        const genre = q.get("genre");
        const decade = q.get("decade");
        const waiting = q.get("waiting") === "1";
        const sort = q.get("sort");
        const ruleVisibility = q.get("ruleVisibility");
        const ruleGroups = JSON.parse(q.get("rules") || "[]") as VisibilityGroup[];
        const items = catalog.filter(
          (t) =>
            (!ruleVisibility ||
              hiddenByRules(t, ruleGroups, q.get("ruleFallback") ?? "show") ===
                (ruleVisibility === "hidden")) &&
            matchesCatalogFilter(t, advanced, ownerTags, savedForViewer) &&
            t.title.toLowerCase().includes(needle) &&
            (!type || t.type === type) &&
            (!waiting || t.type === "movie") &&
            (!genre || t.genres?.includes(genre)) &&
            (!decade || decadeOf(t.year) === decade),
        );
        const compare = (a: FakeTitle, b: FakeTitle, field: string) =>
          field === "title"
            ? a.title.localeCompare(b.title)
            : field === "year"
              ? (a.year || 0) - (b.year || 0)
              : field === "duration"
                ? (a.runtimeMinutes || 0) - (b.runtimeMinutes || 0)
                : field === "rating"
                  ? (a.rating || 0) - (b.rating || 0)
                  : field === "released"
                    ? a.released.localeCompare(b.released)
                    : field === "added"
                      ? a.addedAt.localeCompare(b.addedAt)
                      : a.popularity - b.popularity;
        const desc = q.has("desc") ? q.get("desc") === "1" : sort !== "title";
        const secondary = q.get("secondarySort");
        items.sort(
          (a, b) =>
            compare(a, b, sort || "popularity") * (desc ? -1 : 1) ||
            (secondary
              ? compare(a, b, secondary) * (q.get("secondaryDesc") === "1" ? -1 : 1)
              : 0) ||
            a.key.localeCompare(b.key),
        );
        const after = q.get("after");
        const offset = after
          ? Math.max(0, items.findIndex((item) => item.key === after) + 1)
          : Number(q.get("offset") ?? 0);
        const limit = Number(q.get("limit") ?? 48);
        const page = items.slice(offset, offset + limit);
        const hasMore = offset + limit < items.length;
        return json({
          items: page.map(card),
          total: items.length,
          totalExact: true,
          hasMore,
          nextCursor: hasMore ? page.at(-1)?.key : "",
        });
      }
      case "GET /catalog/search": {
        const needle = (q.get("q") ?? "").toLowerCase();
        const type = q.get("type");
        const includeOrder = type === "show" && q.get("episodeOrder") === "1";
        const limit = Number(q.get("limit") ?? 24);
        return json({
          items: catalog
            .filter(
              (t) =>
                t.title.toLowerCase().includes(needle) &&
                (!type || type === "mixed" || t.type === type),
            )
            .slice(0, includeOrder ? Math.min(15, limit) : limit)
            .map((title) => {
              const item = card(title);
              if (!includeOrder) return item;
              const order = episodeOrder(title.key);
              return {
                ...item,
                episodeOrder: order && {
                  effective: order.effective,
                  override: order.override,
                  libraryDefault: order.libraryDefault,
                  libraryName: order.libraryName,
                },
              };
            }),
        });
      }
      case "GET /catalog/facets": {
        const facet = q.get("facet");
        const type = q.get("type");
        const pool = catalog.filter((t) => !type || type === "mixed" || t.type === type);
        if (facet === "contentRating") {
          const level = (t: FakeTitle) => fieldValues(t, "contentRating")[0];
          return json({
            values: [
              ...RATING_LEVELS.map((l) => ({
                key: l.value,
                title: l.labels,
                count: pool.filter((t) => level(t) === l.value).length,
                labels: [...l.labels.split(" · "), ...(MOCK_EQUIVALENTS[l.value] ?? [])],
              })),
              {
                key: NOT_RATED,
                title: NOT_RATED,
                count: pool.filter((t) => level(t) === NOT_RATED).length,
                labels: [NOT_RATED],
              },
            ],
          });
        }
        const counts = new Map<string, number>();
        for (const t of pool) {
          const vals =
            facet === "tag"
              ? ownerTags.get(t.key) || []
              : facet === "genre"
                ? (t.genres ?? [])
                : facet === "decade"
                  ? [decadeOf(t.year)]
                  : facet === "contentRating"
                    ? [t.contentRating ?? ""]
                    : facet === "country"
                      ? [t.country]
                      : facet === "language"
                        ? [t.language]
                        : facet === "studio"
                          ? [String(t.studio)]
                          : [];
          for (const v of vals) if (v) counts.set(v, (counts.get(v) ?? 0) + 1);
        }
        const title = (k: string) =>
          facet === "decade"
            ? `${k}s`
            : facet === "studio"
              ? (STUDIOS.find((s) => String(s.id) === k)?.name ?? k)
              : facet === "language"
                ? (new Intl.DisplayNames(["en"], { type: "language" }).of(k) ?? k)
                : facet === "country"
                  ? (new Intl.DisplayNames(["en"], { type: "region" }).of(k) ?? k)
                  : k;
        return json({
          values: [...counts.entries()]
            .sort((a, b) => (facet === "decade" ? a[0].localeCompare(b[0]) : b[1] - a[1]))
            .map(([key, count]) => ({ key, title: title(key), count })),
        });
      }
      case "GET /catalog/lookup": {
        const kind = q.get("kind");
        const needle = (q.get("q") ?? "").toLowerCase();
        const pool =
          kind === "person"
            ? PEOPLE.map((x) => ({
                id: x.id,
                name: x.name,
                subtitle: x.role,
                image: avatarFor(x.name),
              }))
            : kind === "keyword"
              ? KEYWORDS
              : kind === "company"
                ? STUDIOS
                : kind === "network"
                  ? NETWORKS
                  : kind === "collection"
                    ? COLLECTIONS
                    : kind === "watchProvider"
                      ? PROVIDERS.map((x) => ({ ...x, subtitle: q.get("region") ?? "US" }))
                      : [];
        return json({
          results: pool.filter((x) => x.name.toLowerCase().includes(needle)).slice(0, 10),
        });
      }
      case "GET /viewers":
        return json({
          viewers: [...VIEWERS, ...state.dlnaGuests, ...state.addedViewers]
            .filter((v) => !state.removedViewers.has(v.id))
            .map((v, i) => {
              const lifecycle = {
                ...(v.lifecycle ?? mockLifecycle(v)),
                paused: state.pausedViewers.has(v.id),
              };
              const accessStatus = v.isOwner
                ? "owner"
                : lifecycle.paused ||
                    (lifecycle.managedBy === "plex" && lifecycle.plexShared === false)
                  ? "inactive"
                  : (v.accessStatus ?? "active");
              return {
                ...v,
                accessStatus,
                thumb: avatarFor(v.title),
                lastActive: new Date(Date.now() - (i * 7 + 1) * 3600_000).toISOString(),
                lifecycle,
              };
            }),
        });
      case "GET /insights/ranker":
        return json({
          trainedAt: new Date(Date.now() - 3 * 3600e3).toISOString(),
          examples: 18420,
          positives: 612,
          auc: 0.78,
          baselineAuc: 0.69,
          active: true,
          features: [
            { name: "fit", label: "Taste fit", weight: 1.42 },
            { name: "session", label: "Matches what they're in the mood for", weight: 0.88 },
            { name: "recency", label: "Newly released", weight: 0.51 },
            { name: "fatigue", label: "Shown before without a play", weight: -0.94 },
            { name: "kind.top-10", label: "In Top 10 for You", weight: 0.47 },
            { name: "runtime", label: "Longer than they usually watch", weight: -0.33 },
          ],
        });
      case "GET /insights/rows":
        return json({
          rows: recommendationPerformance(7, "").rows.map((row) => ({
            hubId: row.kind,
            title: row.title,
            impressions7d: row.impressions,
            plays7d: row.plays,
            playRate: row.plays / row.impressions,
            lowSample: row.lowSample,
          })),
        });
      case "GET /insights/recommendations":
        return json(recommendationPerformance(Number(q.get("days") ?? 30), q.get("viewer") ?? ""));
      case "GET /insights/recommendations/row":
        return json(
          recommendationPerformanceDetail(
            Number(q.get("days") ?? 30),
            q.get("kind") ?? "",
            q.get("viewer") ?? "",
          ),
        );
      case "GET /insights/activity":
        return json({ days: activity() });
    }

    if (seg[0] === "collections" && seg[1] === "addon-imports" && seg[2]) {
      const addonId = decodeURIComponent(seg[2]);
      if (seg[3] === "sync" && method === "POST") {
        const sub = state.addonImports.find((s) => s.addonId === addonId);
        if (!sub) return err(404, "not_found", "no such add-on import");
        sub.lastSyncedAt = new Date().toISOString();
        return json({
          addonId,
          addonName: sub.addonName ?? addonId,
          manifestChanged: false,
          created: 0,
          updated: 0,
          removed: 0,
          restored: 0,
          subscription: sub,
        });
      }
      if (!seg[3] && method === "DELETE") {
        const idx = state.addonImports.findIndex((s) => s.addonId === addonId);
        if (idx >= 0) state.addonImports.splice(idx, 1);
        return json({ ok: true });
      }
    }

    if (
      seg[0] === "collections" &&
      seg[1] === "stremio" &&
      seg[2] === "manifest" &&
      method === "POST"
    ) {
      return json({
        id: "example.catalog",
        name: "Example catalog",
        catalogs: [{ id: "top", type: "movie", name: "Top movies" }],
      });
    }

    if (
      seg[0] === "collections" &&
      seg[1] === "template-bundles" &&
      seg[2] &&
      (seg[3] === "preview" || seg[3] === "apply") &&
      method === "POST"
    ) {
      const bundleId = decodeURIComponent(seg[2]);
      const bundle = TEMPLATE_BUNDLES.find((b) => b.id === bundleId);
      if (!bundle) return err(404, "not_found", "no such template bundle");
      const items = bundle.templateIds
        .map((tid) => {
          const tpl = TEMPLATES.find((t) => t.id === tid);
          if (!tpl) return null;
          const existing = state.collections.find(
            (c) => c.templateId === tid || c.name.toLowerCase() === tpl.name.toLowerCase(),
          );
          return {
            templateId: tid,
            name: tpl.name,
            category: tpl.category,
            action: existing ? ("skip_existing" as const) : ("create" as const),
            ...(existing?.id ? { existingId: existing.id } : {}),
          };
        })
        .filter((x): x is NonNullable<typeof x> => Boolean(x));
      if (seg[3] === "preview") {
        return json({
          bundle,
          items,
          willCreate: items.filter((i) => i.action === "create").length,
          willSkip: items.filter((i) => i.action === "skip_existing").length,
        });
      }
      const created: CollectionSpec[] = [];
      const skipped = items.filter((i) => i.action === "skip_existing");
      for (const item of items) {
        if (item.action !== "create") continue;
        const tpl = TEMPLATES.find((t) => t.id === item.templateId);
        if (!tpl) continue;
        const spec = normalizeSpec({
          ...tpl.spec,
          name: tpl.name,
          description: tpl.description,
          templateId: tpl.id,
          enabled: true,
        });
        const saved: CollectionSpec = {
          ...spec,
          id: `c${seq++}`,
          updatedAt: new Date().toISOString(),
          lastBuiltAt: new Date().toISOString(),
          itemCount: evaluate(spec).total,
        };
        state.collections.push(saved);
        created.push(saved);
      }
      return json({
        bundleId,
        bundle,
        items,
        created,
        skipped,
        createdCount: created.length,
        skippedCount: skipped.length,
      });
    }

    // Parameterised routes
    if (seg[0] === "collections" && seg[1]) {
      const id = decodeURIComponent(seg[1]);
      const idx = state.collections.findIndex((c) => c.id === id);
      if (seg[2] === "artwork" && method === "GET") {
        const c = state.collections[idx];
        const svg = decodeURIComponent(posterFor(id, c?.name ?? "Collection").split(",")[1] ?? "");
        return new Response(svg, { status: 200, headers: { "Content-Type": "image/svg+xml" } });
      }
      if (idx < 0) return err(404, "not_found", "no such collection");
      const current = state.collections[idx]!;
      if (seg[2] === "usage" && method === "GET") {
        return json({ uses: COLLECTION_USAGE[id] ?? [] } satisfies CollectionUsage);
      }
      if (seg[2] === "rebuild" && method === "POST") {
        const r = evaluate(current);
        current.itemCount = r.total;
        current.lastBuiltAt = new Date().toISOString();
        return json({ itemCount: r.total, lastBuiltAt: current.lastBuiltAt });
      }
      if (method === "GET") return json(current);
      if (method === "PUT") {
        const spec = normalizeSpec(body as CollectionSpec);
        const saved = {
          ...spec,
          id,
          updatedAt: new Date().toISOString(),
          itemCount: evaluate(spec).total,
          lastBuiltAt: new Date().toISOString(),
        };
        state.collections[idx] = saved;
        return json(saved);
      }
      if (method === "DELETE") {
        state.collections.splice(idx, 1);
        return new Response(null, { status: 204 });
      }
    }
    if (seg[0] === "catalog" && seg[1] === "title" && seg[2]) {
      const t = byKey.get(decodeURIComponent(seg[2]));
      const followed = t ? null : calendar.title(decodeURIComponent(seg[2]));
      if (followed) return json(followed);
      if (!t) return err(404, "not_found", "no such title");
      if (seg[3] === "refresh" && method === "POST") return new Response(null, { status: 204 });
      if (seg[3] === "episode-order" && method === "PUT") {
        const order = (body as { order: EpisodeOrder | null }).order;
        const details = t.type === "show" ? episodeOrder(t.key) : undefined;
        if (
          !details ||
          (order &&
            !details.options.some(
              (option) => option.source === order.source && option.kind === order.kind,
            ))
        )
          return err(400, "bad_input", "Episode order unavailable");
        if (order) episodeOverrides.set(t.key, order);
        else episodeOverrides.delete(t.key);
        return new Response(null, { status: 204 });
      }
      if ((seg[3] === "matches" && method === "GET") || (seg[3] === "match" && method === "PUT")) {
        const provider = body as { source: string; providerId: string } | null;
        const source = method === "GET" ? q.get("source") : provider?.source;
        if (
          !metadataSources().some(
            (entry) =>
              entry.id === source &&
              entry.configured &&
              (t.type === "show" ? entry.shows : entry.movies),
          )
        )
          return err(400, "bad_input", "Metadata source unavailable");
        if (method === "PUT") {
          const next = [...byKey.values()].find(
            (item) =>
              item.type === t.type && matchProviderId(item.key, source!) === provider?.providerId,
          );
          return next ? json({ key: next.key }) : err(404, "not_found", "no such title");
        }
        const search = (q.get("q") ?? "").toLowerCase();
        const year = Number(q.get("year"));
        return json(
          [...byKey.values()]
            .filter(
              (item) =>
                item.type === t.type &&
                item.title.toLowerCase().includes(search) &&
                (!year || item.year === year),
            )
            .slice(0, 20)
            .map((item) => ({
              ...card(item),
              source,
              providerId: matchProviderId(item.key, source!),
            })),
        );
      }
      if (seg[3] === "tags") {
        if (method === "PUT")
          ownerTags.set(t.key, [
            ...new Set(
              ((body as { tags: string[] }).tags || [])
                .map((tag) => tag.trim().toLowerCase())
                .filter(Boolean),
            ),
          ]);
        return json({ tags: ownerTags.get(t.key) || [] });
      }
      if (seg[3] === "library") {
        if (method === "PUT") ownerIncludes.add(t.key);
        else if (method === "DELETE") ownerIncludes.delete(t.key);
        const included = ownerIncludes.has(t.key);
        return json({
          browsable: true,
          included,
          inLibraryTier: true,
          canInclude: false,
          releaseGated: false,
          reasons: [],
          items: [],
        });
      }
      return json({
        ...card(t),
        episodeOrder: t.type === "show" ? episodeOrder(t.key) : undefined,
        tmdbId: Number(t.key) % 1e15,
        cast: t.people.map((id) => PEOPLE.find((p) => p.id === id)).filter(Boolean),
        directors: [],
        keywords: t.keywords.map((id) => KEYWORDS.find((k) => k.id === id)).filter(Boolean),
        collection: null,
      });
    }
    if (seg[0] === "viewers" && seg[1] === "plex-candidates" && method === "GET") {
      return json(mockPlexCandidateServers);
    }
    if (seg[0] === "viewers" && seg[1] === "import-plex" && !seg[2] && method === "POST") {
      const req = (body as { serverId?: number; accountId?: number; name?: string } | null) ?? {};
      const srv = mockPlexCandidateServers.find((s) => s.serverId === req.serverId);
      const acct = srv?.accounts.find((a) => a.accountId === req.accountId);
      if (!acct) return err(404, "not_found", "account not found on server");
      const newId = ++nextMockPlexViewerId;
      const name = req.name?.trim() || acct.accountName;
      acct.matchedViewerId = newId;
      acct.matchedViewerName = name;
      acct.matchedViewerUsername = acct.plexUsername;
      return json(
        {
          id: newId,
          name,
          inviteUrl: "",
          serverId: acct.serverId,
          accountId: acct.accountId,
          status: "imported",
          sharedPlex: true,
        },
        201,
      );
    }
    if (seg[0] === "viewers" && seg[1] === "match-plex" && method === "POST") {
      const req =
        (body as { serverId?: number; accountId?: number; viewerId?: number } | null) ?? {};
      const srv = mockPlexCandidateServers.find((s) => s.serverId === req.serverId);
      const acct = srv?.accounts.find((a) => a.accountId === req.accountId);
      const v = [...VIEWERS, ...state.dlnaGuests, ...state.addedViewers].find(
        (x) => x.id === req.viewerId,
      );
      if (!acct || !v) return err(404, "not_found", "account or viewer not found");
      acct.matchedViewerId = v.id;
      acct.matchedViewerName = v.title || v.username;
      acct.matchedViewerUsername = v.username;
      return json({
        ok: true,
        viewerId: v.id,
        serverId: acct.serverId,
        accountId: acct.accountId,
        sharedPlex: true,
      });
    }
    if (seg[0] === "viewers" && seg[1] === "import-plex" && seg[2] === "all" && method === "POST") {
      const results = mockPlexCandidateServers.flatMap((srv) =>
        srv.accounts.map((acct) => {
          if (acct.matchedViewerId) {
            return {
              id: acct.matchedViewerId,
              name: acct.accountName,
              serverId: acct.serverId,
              accountId: acct.accountId,
              status: "skipped",
              reason: "already_mapped",
              sharedPlex: false,
            };
          }
          const newId = ++nextMockPlexViewerId;
          acct.matchedViewerId = newId;
          acct.matchedViewerName = acct.accountName;
          acct.matchedViewerUsername = acct.plexUsername;
          return {
            id: newId,
            name: acct.accountName,
            serverId: acct.serverId,
            accountId: acct.accountId,
            status: "imported",
            sharedPlex: true,
          };
        }),
      );
      return json(results);
    }
    if (seg[0] === "viewers" && seg[1]) {
      const id = Number(seg[1]);
      const v = [...VIEWERS, ...state.dlnaGuests, ...state.addedViewers].find((x) => x.id === id);
      if (!v || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
      if (seg[2] === "catalog-rules") {
        if (method === "PUT") viewerCatalogRules.set(id, body);
        const globalRules = [
          {
            name: "French films",
            action: "hide",
            mediaType: "movie",
            match: "all",
            scope: "discovery",
            rules: [{ field: "language", op: "is", value: "fr" }],
          },
        ];
        return json({
          policy: viewerCatalogRules.get(id) ?? {
            mode: "inherit",
            exclusions: [],
            visibilityDefault: "show",
            exemptions: [],
          },
          globalRules,
          globalDefault: "show",
          globalIDs: globalRules.map((_, i) => `mock-rule-${i}`),
        });
      }
      if (seg[2] === "quickconnect" && method === "POST") {
        const code = String((body as { code?: string } | null)?.code ?? "");
        if (code === "EXPIRED") return err(400, "expired", "code expired");
        if (code === "USEDUP") return err(400, "used", "code already used");
        if (!/^[A-HJ-NP-Z2-9]{6}$/.test(code)) return err(400, "invalid", "invalid code");
        return json({ ok: true, device: "Living Room Apple TV", app: "Jellyfin" });
      }
      if (seg[2] === "favorites-mirror") {
        if (method === "POST")
          state.favoritesMirror.set(id, (body as { enabled: boolean }).enabled);
        return json({ enabled: state.favoritesMirror.get(id) ?? true });
      }
      if (!seg[2] && method === "GET") {
        const recent = pickTitles(`recent${id}`, 8).map((t, i) => ({
          title: card(t),
          at: new Date(Date.now() - (i * 19 + 2) * 3600_000).toISOString(),
          completed: i % 3 !== 1,
          progress: i % 3 === 1 ? 0.35 + (i % 4) * 0.1 : 1,
        }));
        const lifecycle = {
          ...(v.lifecycle ?? mockLifecycle(v)),
          paused: state.pausedViewers.has(id),
        };
        const accessStatus = v.isOwner
          ? "owner"
          : lifecycle.paused || (lifecycle.managedBy === "plex" && lifecycle.plexShared === false)
            ? "inactive"
            : (v.accessStatus ?? "active");
        return json({
          ...v,
          accessStatus,
          thumb: avatarFor(v.title),
          lastActive: new Date(Date.now() - 3600_000).toISOString(),
          lifecycle,
          recent,
        });
      }
      if (seg[2] === "taste" && !seg[3] && method === "GET") return json(taste(id));
      if (seg[2] === "taste" && seg[3] === "overrides" && method === "PUT") {
        state.overrides.set(id, (body as { overrides: TasteOverride[] }).overrides ?? []);
        return json(taste(id));
      }
      if (seg[2] === "refresh" && method === "POST") return new Response(null, { status: 204 });
      if (seg[2] === "plex-servers" && method === "GET") {
        const servers = (
          state as {
            plexServers?: {
              id: number;
              name: string;
              problem?: string;
              viewers?: {
                accountId: number;
                accountName: string;
                owner: boolean;
                userId: number;
                direction: number;
                interval: number;
                lastImport: number;
                lastSend: number;
                nextSync: number;
                problem: string;
                unmatched: number;
              }[];
            }[];
          }
        ).plexServers ?? [
          {
            id: 1,
            name: "Living room PMS",
            problem: "",
            viewers: [
              {
                accountId: 1,
                accountName: "owner",
                owner: true,
                userId: 1,
                direction: 3,
                interval: 10,
                lastImport: Math.floor(Date.now() / 1000) - 240,
                lastSend: Math.floor(Date.now() / 1000) - 240,
                nextSync: Math.floor(Date.now() / 1000) + 360,
                problem: "",
                unmatched: 1,
              },
            ],
          },
        ];
        return json(
          servers.flatMap((srv) =>
            (srv.viewers ?? [])
              .filter((m) => m.userId === id)
              .map((m) => ({
                serverId: srv.id,
                serverName: srv.name,
                accountId: m.accountId,
                accountName: m.accountName,
                owner: m.owner,
                direction: m.direction,
                interval: m.interval,
                lastImport: m.lastImport,
                lastSend: m.lastSend,
                nextSync: m.direction ? m.nextSync : 0,
                problem: m.problem || srv.problem || "",
                unmatched: m.unmatched,
              })),
          ),
        );
      }
      if (seg[2] === "plex-watch") {
        const cur = plexWatch.get(id) ?? {
          connected: false,
          direction: 0,
          importInterval: 10,
          lastImport: 0,
          lastFullImport: 0,
          lastSend: 0,
          nextImport: 0,
          problem: "",
          needsReconnect: false,
          pending: 0,
          failed: 0,
          restricted: false,
          accountMatch: true,
        };
        const action = seg[3];
        const nowSec = Math.floor(Date.now() / 1000);
        if (!action && method === "GET") return json(cur);
        if (action === "pin" && !seg[4] && method === "POST") {
          const next = {
            ...cur,
            pinCode: "MOCK",
            pinUrl: "https://app.plex.tv/auth#?code=MOCK",
            accountMatch: true,
          };
          plexWatch.set(id, next);
          return json(next);
        }
        if (action === "pin" && seg[4] === "finish" && method === "POST") {
          if (v.plexId <= 0) {
            return err(
              409,
              "account_mismatch",
              "the Plex account that approved the code is not this viewer",
            );
          }
          const next = {
            ...cur,
            connected: true,
            username: v.username,
            plexId: v.plexId,
            pinCode: undefined,
            pinUrl: undefined,
            needsReconnect: false,
            problem: "",
            accountMatch: true,
          };
          plexWatch.set(id, next);
          return json(next);
        }
        if (!action && method === "PUT") {
          const b = body as { direction?: number; interval?: number };
          const next = {
            ...cur,
            direction: b?.direction ?? cur.direction,
            importInterval: b?.interval ?? cur.importInterval,
          };
          plexWatch.set(id, next);
          return json(next);
        }
        if (action === "disconnect" && method === "POST") {
          const next = {
            ...cur,
            connected: false,
            direction: 0,
            needsReconnect: false,
            problem: "",
            username: undefined,
          };
          plexWatch.set(id, next);
          return json(next);
        }
        if (action === "import" && method === "POST") {
          const next = { ...cur, lastImport: nowSec, lastFullImport: nowSec };
          plexWatch.set(id, next);
          return json(next);
        }
        if ((action === "retry" || action === "export") && method === "POST") {
          const next = { ...cur, pending: 0, failed: 0, lastSend: nowSec };
          plexWatch.set(id, next);
          return json(next);
        }
      }
    }
    // Per-viewer safety policy (Viewer › Limits). The preset's rating limit
    // is for the user record; the rest is the policy.
    if (seg[0] === "safety" && seg[1] === "policies") {
      if (seg[2] === "presets" && seg[3] === "kids" && method === "GET") {
        const age = url.searchParams.get("age");
        const rating = age === "preschool" ? "TV-Y" : age === "teens" ? "PG-13" : "PG";
        return json({
          maxContentRating: rating,
          bedtime: {
            time: age === "preschool" ? "19:00" : age === "teens" ? "22:00" : "20:00",
            wakeTime: "06:00",
            timezone: "",
          },
          blockedGenres: null,
          blockedKeywords: ["pornography", "erotic movie"],
          remoteDisabled: false,
          maxBitrateKbps: 0,
          allowedHours: null,
          timezone: "",
        });
      }
      const id = Number(seg[2]);
      if (Number.isFinite(id) && method === "GET") return json(safetyPolicies.get(id) ?? {});
      if (Number.isFinite(id) && method === "PUT") {
        safetyPolicies.set(id, (body ?? {}) as Record<string, unknown>);
        return json(safetyPolicies.get(id));
      }
    }
    if (seg[0] === "users" && seg[1] && /^\d+$/.test(seg[1])) {
      const id = Number(seg[1]);
      const target = VIEWERS.find((x) => x.id === id);
      const managed = target?.lifecycle?.managedBy === "plex";
      const plexManaged = () => err(409, "managed_by_plex", "This viewer is managed on plex.tv.");
      if (seg[2] === "pause" && method === "POST") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        if (target.isOwner) return err(403, "forbidden", "The owner cannot be paused.");
        state.pausedViewers.add(id);
        return json({ paused: true });
      }
      if (seg[2] === "resume" && method === "POST") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        if (target.isOwner) return err(403, "forbidden", "The owner cannot be paused.");
        state.pausedViewers.delete(id);
        return json({ paused: false });
      }
      if (seg[2] === "signout" && method === "POST") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        return json({ revoked: 2 });
      }
      if (seg[2] === "merge" && method === "POST") {
        const into = Number((body as { into?: number } | null)?.into ?? 0);
        const dest = VIEWERS.find((x) => x.id === into);
        if (!target || !dest || state.removedViewers.has(id) || into === id)
          return err(400, "invalid", "invalid merge");
        if (target.isOwner || dest.isOwner)
          return err(403, "forbidden", "The owner cannot be merged.");
        if (managed && dest.lifecycle?.managedBy === "plex")
          return err(409, "conflict", "Two Plex viewers cannot be merged.");
        if (managed) return plexManaged();
        state.pausedViewers.delete(id);
        state.removedViewers.add(id);
        return json({ merged: true, into });
      }
      if (seg[2] === "trakt") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        const configured = Boolean(
          state.config.trakt.enabled && state.config.trakt.clientId.trim(),
        );
        const cur = traktAccounts.get(id) ?? { connected: false };
        const portalUrl = "https://ether.example/ether/me";
        if (!seg[3] && method === "GET") {
          return json({ configured, ...cur, portalUrl });
        }
        if (seg[3] === "device" && method === "POST") {
          if (!configured) return err(409, "not_configured", "Trakt is not configured");
          return json({
            configured: true,
            ...cur,
            status: "pending",
            portalUrl,
            device: {
              userCode: "TRKT-1234",
              verificationUrl: "https://trakt.tv/activate",
              expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
              interval: 5,
            },
          });
        }
        if (seg[3] === "poll" && method === "POST") {
          if (!configured) return err(409, "not_configured", "Trakt is not configured");
          const next = { connected: true, username: target.username || "viewer" };
          traktAccounts.set(id, next);
          return json({ configured: true, ...next, status: "authorized", portalUrl });
        }
        if (seg[3] === "disconnect" && method === "POST") {
          traktAccounts.delete(id);
          return json({ configured, connected: false, portalUrl });
        }
      }
      if (seg[2] === "invite" && method === "POST") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        if (target.isOwner) return err(403, "forbidden", "The owner needs no invite.");
        return json({ url: "/ether/me#invite=mock-secret" });
      }
      if (!seg[2] && method === "GET") {
        const user = state.users.find((u) => u.id === id);
        if (!user || state.removedViewers.has(id)) return err(404, "not_found", "no such user");
        return json({
          user: {
            ...user,
            lifecycle: user.lifecycle
              ? { ...user.lifecycle, paused: state.pausedViewers.has(id) }
              : user.lifecycle,
          },
        });
      }
      if (!seg[2] && method === "DELETE") {
        if (!target || state.removedViewers.has(id)) return err(404, "not_found", "no such viewer");
        if (target.isOwner) return err(403, "forbidden", "The owner cannot be removed.");
        if (managed) return plexManaged();
        state.pausedViewers.delete(id);
        state.removedViewers.add(id);
        return json({ removed: true });
      }
    }
    // One viewer's Simkl account (Viewer › Limits).
    if (seg[0] === "simkl" && seg[1] && /^\d+$/.test(seg[1])) {
      const id = Number(seg[1]);
      const cur = simklAccounts.get(id) ?? { connected: false };
      if (seg[2] === "status" && method === "GET") return json(cur);
      if (seg[2] === "start" && method === "POST")
        return json({
          userCode: "ABCD-EFGH",
          verificationUri: "https://simkl.com/pin",
          verificationUriComplete: "https://simkl.com/pin?user_code=ABCD-EFGH",
          expiresAt: new Date(Date.now() + 15 * 60_000).toISOString(),
          interval: 5,
        });
      if (seg[2] === "poll" && method === "POST") {
        simklAccounts.set(id, { connected: true });
        return json({ status: "authorized" });
      }
      if (seg[2] === "sync" && method === "POST") {
        simklAccounts.set(id, { ...cur, lastSync: new Date().toISOString() });
        return json({ imported: 3, skipped: 0 });
      }
      if (seg[2] === "disconnect" && method === "POST") {
        simklAccounts.delete(id);
        return json({ ok: true });
      }
    }
    // One viewer's MDBList account (Viewer › Limits).
    if (seg[0] === "mdblist" && seg[1] && /^\d+$/.test(seg[1])) {
      const id = Number(seg[1]);
      const cur = mdblistAccounts.get(id) ?? { connected: false };
      if (seg[2] === "status" && method === "GET") return json(cur);
      if (seg[2] === "connect" && method === "POST") {
        mdblistAccounts.set(id, { connected: true });
        return json({ connected: true });
      }
      if (seg[2] === "sync" && method === "POST") {
        mdblistAccounts.set(id, { ...cur, lastSync: "cursor-1" });
        return json({ imported: 3, skipped: 0 });
      }
      if (seg[2] === "disconnect" && method === "POST") {
        mdblistAccounts.delete(id);
        return json({ ok: true });
      }
    }
    if (seg[0] === "users" && seg[1] && (method === "PATCH" || method === "PUT")) {
      const id = Number(seg[1]);
      const i = state.users.findIndex((u) => u.id === id);
      if (i < 0) return err(404, "not_found", "no such user");
      state.users[i] = { ...state.users[i]!, ...(body as Partial<EtherUser>) };
      return json({ user: state.users[i] });
    }
    if (seg[0] === "config" && seg[1] && method === "GET") {
      const key =
        seg[1] === "opensubtitles"
          ? "openSubtitles"
          : seg[1] === "plexservers"
            ? "plexServers"
            : seg[1] === "livetv"
              ? "liveTV"
              : seg[1];
      return json({
        section: key,
        value: (state.config as unknown as Record<string, unknown>)[key],
        ...(key === "clientIdentity" ? { defaultUserAgent: DEFAULT_USER_AGENT } : {}),
      });
    }
    if (seg[0] === "config" && seg[1] && method === "PUT") {
      const key =
        seg[1] === "opensubtitles"
          ? "openSubtitles"
          : seg[1] === "plexservers"
            ? "plexServers"
            : seg[1] === "livetv"
              ? "liveTV"
              : seg[1];
      // Like the server: the body is merged onto the stored section.
      const prev = (state.config as unknown as Record<string, unknown>)[key];
      let value =
        prev &&
        typeof prev === "object" &&
        !Array.isArray(prev) &&
        body &&
        typeof body === "object" &&
        !Array.isArray(body)
          ? { ...(prev as Record<string, unknown>), ...(body as Record<string, unknown>) }
          : body;
      if (
        key === "plex" &&
        body !== null &&
        typeof body === "object" &&
        !Array.isArray(body) &&
        (body as { claimed?: boolean }).claimed === false
      ) {
        value = { ...(value as Record<string, unknown>), token: "" };
      }
      if (key === "catalog" && value && typeof value === "object") {
        const catalog = value as Record<string, unknown>;
        for (const field of ["tmdbKey", "tvdbKey"]) {
          if (catalog[field] && (body as Record<string, unknown>)?.[field]) catalog[field] = "••••";
        }
      }
      if (key === "quality" && value && typeof value === "object" && !Array.isArray(value)) {
        const q = value as { rules?: { id?: string }[] | null };
        if (Array.isArray(q.rules)) {
          let seq = q.rules.length;
          q.rules = q.rules.map((r) =>
            r.id ? r : { ...r, id: `r_${(++seq).toString(36).padStart(8, "0")}` },
          );
        }
      }
      (state.config as unknown as Record<string, unknown>)[key] = value;
      return json({ section: key, value });
    }
    if (seg[0] === "cluster" && seg[1] === "join-tokens" && seg[2] && method === "DELETE") {
      if (!state.clustered) return err(409, "not_clustered", "clustering is not enabled");
      const id = decodeURIComponent(seg[2]);
      const i = state.joinTokens.findIndex((t) => t.id === id && !t.usedAt);
      if (i < 0) return err(404, "not_found", "no unused token with that id");
      state.joinTokens.splice(i, 1);
      return new Response(null, { status: 204 });
    }
    if (seg[0] === "auth" && seg[1] === "pin" && seg[2])
      return json({ done: true, token: "mock-owner-token" });
    if (seg[0] === "plex" && seg[1] === "pin" && seg[2]) return json({ done: true });
    if (seg.join("/") === "status/database/compact") {
      return json({
        value: {
          supported: true,
          incremental: false,
          databaseBytes: 1_103_679_488,
          reclaimableBytes: 0,
          needBytes: 1_372_114_944,
          diskFreeBytes: 58_090_143_744,
          tempNeedBytes: 1_372_114_944,
          tempFreeBytes: 58_090_143_744,
          estimateSeconds: 48,
          ready: true,
          running: method === "POST",
          ...(method === "POST" ? { started: new Date().toISOString() } : {}),
        },
      });
    }
    if (method === "POST" && seg.join("/") === "status/database/check") {
      const value = dashboard().sections.database;
      if (value.integrity) value.integrity.checked = new Date().toISOString();
      return json({ value });
    }
    if (seg[0] === "status" && seg[1])
      return json({
        name: seg[1],
        value: (dashboard().sections as Record<string, unknown>)[seg[1]] ?? null,
      });
    if (seg[0] === "me") {
      const sub = seg.slice(1).join("/");
      if (method === "POST" && sub === "login") return json({ token: "viewer-grant" });
      if (method === "POST" && sub === "pin")
        return json({ handle: "pin1", code: "ABCD", authUrl: "https://plex.example/auth" });
      if (method === "GET" && sub === "pin/pin1")
        return json({ done: true, token: "viewer-grant" });
      if (method === "GET" && sub === "") return json({ id: 1, username: "alice", title: "Alice" });
      if (method === "POST" && sub === "quickconnect") {
        const code = String((body as { code?: string } | null)?.code ?? "");
        if (code === "EXPIRED") return err(400, "expired", "code expired");
        if (code === "USEDUP") return err(400, "used", "code already used");
        if (!/^[A-HJ-NP-Z2-9]{6}$/.test(code)) return err(400, "invalid", "invalid code");
        return json({ ok: true, device: "Living Room Apple TV", app: "Jellyfin" });
      }
      if (method === "GET" && sub === "identities")
        return json({ identities: [{ id: 1, provider: "plex", display: "alice" }] });
      if (method === "GET" && sub === "devices")
        return json({
          devices: [
            {
              hash: "abc",
              surface: "jellyfin",
              client: "Apple TV",
              product: "Jellyfin",
              version: "1",
              lastUsed: Date.now(),
            },
          ],
        });
      if (sub === "favorite-episode-alert") return json({ enabled: false });
      if (method === "POST" && sub === "invite/redeem") return json({ token: "viewer-grant" });
      if (method === "POST" && sub === "password") return json({ ok: true });
      if (method === "POST" && sub === "devices/revoke") return json({ ok: true });
    }
    if (seg[0] === "users" && seg[1] && seg[2] === "identities" && method === "GET") {
      const who = VIEWERS.find((x) => x.id === Number(seg[1]));
      const provider = who?.lifecycle?.managedBy === "plex" ? "plex" : "local";
      return json({
        identities: [{ id: 1, provider, display: who?.username ?? "owner" }],
      });
    }
    if (
      seg[0] === "users" &&
      seg[1] &&
      seg[2] === "devices" &&
      seg.length === 3 &&
      method === "GET"
    )
      return json({ devices: xtreamGrants.get(seg[1]!) ?? [] });
    if (seg[0] === "auth" && seg[1] === "lockouts" && seg[2] && method === "DELETE") {
      const key = decodeURIComponent(seg[2]);
      const before = state.lockouts.length;
      state.lockouts = state.lockouts.filter((item) => item.key !== key);
      return json({ ok: true, cleared: state.lockouts.length < before });
    }
    if (seg[0] === "users" && seg[2] === "devices" && seg[3] === "xtream" && method === "POST") {
      if (seg[4] === "reveal")
        return json({ available: true, username: "alice42", password: "k7m2-9qxr-4t" });
      return json({ ok: true });
    }
    if (seg[0] === "users" && seg[1] && seg[2] === "password" && method === "POST")
      return json({ ok: true });
    if (seg[0] === "users" && seg[1] && seg[2] === "devices" && seg[3] === "revoke") {
      const hash = (body as { hash?: string })?.hash;
      xtreamGrants.set(
        seg[1],
        (xtreamGrants.get(seg[1]) ?? []).filter((g) => g.hash !== hash),
      );
      return json({ ok: true });
    }
    const viewerHidden = /^\/viewers\/(\d+)\/not-interested(?:\/restore)?$/.exec(p);
    if (viewerHidden && (method === "GET" || method === "POST")) {
      if (method === "GET")
        return json({
          items: catalog
            .slice(3, 9)
            .filter((title) => !restoredHidden.has(`${viewerHidden[1]}:${title.key}`))
            .map((title) => ({
              handle: String(title.key),
              title: title.title,
              year: title.year,
              posterPath: undefined,
              poster: title.poster,
            })),
        });
      restoredHidden.add(`${viewerHidden[1]}:${(body as { handle: string }).handle}`);
      return json({ restored: true });
    }
    return err(404, "not_found", "no such API endpoint");
  }

  async function fetchImpl(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    const req = input instanceof Request ? input : null;
    const url = new URL(req ? req.url : String(input), "http://localhost");
    const method = (init?.method ?? req?.method ?? "GET").toUpperCase();
    let body: unknown = undefined;
    const raw = init?.body ?? null;
    if (raw instanceof FormData) body = raw;
    if (typeof raw === "string" && raw) {
      try {
        body = JSON.parse(raw);
      } catch {
        body = raw;
      }
    }
    state.calls.push({
      method,
      path: url.pathname.replace(/^\/ether\/api/, "") + url.search,
      body,
    });
    if (opts.latencyMs)
      await new Promise((r) => setTimeout(r, opts.latencyMs! * (0.5 + Math.random())));
    if (init?.signal?.aborted) throw new DOMException("Aborted", "AbortError");
    const seg = url.pathname
      .replace(/^\/ether\/api/, "")
      .split("/")
      .filter(Boolean);
    const gate = ops.handle(method, seg, body, new Headers(init?.headers ?? req?.headers));
    if (gate === UNREACHABLE) throw new TypeError("Failed to fetch");
    if (gate) return json(gate.body, gate.status);
    if (seg[0] === "admin" && seg[1] === "prerolls") return prerolls(url, init);
    const page = pagesMock.handle(method, seg, body, new Headers(init?.headers ?? req?.headers));
    if (page) return json(page.body, page.status);
    return handle(method, url, body);
  }

  return {
    fetch: fetchImpl,
    state,
    catalog,
    evaluate,
    releaseConsole,
    sites,
    velaBranding,
  };
}

export type FakeServer = ReturnType<typeof createFakeServer>;
export type { TitleCard };

const MOCK_LLM_PROVIDERS: LLMProvider[] = [
  {
    id: "anthropic",
    label: "Anthropic — Claude",
    api: "anthropic",
    baseUrl: "https://api.anthropic.com/v1",
    needsKey: true,
    local: false,
    requiresBaseUrl: false,
    keyHint: "sk-ant-…",
  },
  {
    id: "openai",
    label: "OpenAI — GPT",
    api: "openai",
    baseUrl: "https://api.openai.com/v1",
    needsKey: true,
    local: false,
    requiresBaseUrl: false,
    keyHint: "sk-…",
  },
  {
    id: "openrouter",
    label: "OpenRouter — any model, one key",
    api: "openai",
    baseUrl: "https://openrouter.ai/api/v1",
    needsKey: true,
    local: false,
    requiresBaseUrl: false,
    keyHint: "sk-or-…",
    modelHint: "Models are vendor/model slugs, e.g. openai/gpt-4o.",
  },
  {
    id: "ollama",
    label: "Ollama — on your network",
    api: "openai",
    baseUrl: "http://localhost:11434/v1",
    needsKey: false,
    local: true,
    requiresBaseUrl: false,
    keyHint: "Not needed",
  },
  {
    id: "custom",
    label: "Custom (OpenAI-compatible)",
    api: "openai",
    baseUrl: "",
    needsKey: false,
    local: true,
    requiresBaseUrl: true,
    keyHint: "Your provider API key",
  },
];

const MOCK_LLM_MODELS: Record<string, { id: string; label?: string }[]> = {
  anthropic: [
    { id: "claude-haiku-5-5", label: "Claude Haiku 5.5" },
    { id: "claude-opus-5-5", label: "Claude Opus 5.5" },
    { id: "claude-sonnet-5-5", label: "Claude Sonnet 5.5" },
  ],
  openai: [{ id: "gpt-4o" }, { id: "gpt-4o-mini" }, { id: "o4-mini" }],
  openrouter: [
    { id: "anthropic/claude-sonnet-5-5", label: "Anthropic: Claude Sonnet 5.5" },
    { id: "openai/gpt-4o", label: "OpenAI: GPT-4o" },
  ],
};
/** Plausible, gently moving machine load for the Overview. */
function mockSystemLoad(now: number): SystemLoad {
  const step = 2_000;
  const wave = (t: number, period: number, lo: number, hi: number) =>
    lo + ((Math.sin((t / period) * Math.PI * 2) + 1) / 2) * (hi - lo);
  const history: LoadSample[] = Array.from({ length: 150 }, (_, i) => {
    const t = now - (149 - i) * step;
    return {
      at: new Date(t).toISOString(),
      cpu: wave(t, 90_000, 8, 42),
      memUsed: wave(t, 240_000, 9.1, 10.4) * 2 ** 30,
      netRxBps: wave(t, 60_000, 1.5e6, 9e6),
      netTxBps: wave(t, 75_000, 0.8e6, 4.5e6),
      procCpu: wave(t, 90_000, 2, 14),
      procRss: 412 * 2 ** 20,
      gpu: wave(t, 50_000, 5, 38),
    };
  });
  return {
    cores: 12,
    memTotal: 24 * 2 ** 30,
    swapUsed: 0,
    swapTotal: 0,
    load: [1.84, 1.62, 1.4],
    intervalSeconds: 2,
    current: history.at(-1),
    history,
    gpus: [
      {
        name: "NVIDIA GeForce RTX 4080",
        util: history.at(-1)!.gpu,
        encoder: 18,
        decoder: 11,
        memUsed: 2.1 * 2 ** 30,
        memTotal: 16 * 2 ** 30,
      },
    ],
  };
}

// Mock Recommendations API (/ether/api/pages…) for `vite --mode mock` and the
// web tests. A small engine: pages of rows over the generated catalogue,
// Curated and Personalized arrangement, one-row-per-library fan-out (≤6),
// audience, devices, schedules, viewer variants and a kid viewer with an age
// limit, so every designer screen renders without a server.
import { t } from "../i18n";
import type { TitleCard } from "../lib/contract";
import type {
  PageDoc,
  PageLibrary,
  PageRow,
  PageSource,
  PageSummary,
  PagePreview,
  PreviewRow,
  RowPage,
  Schedule,
  SourcesDoc,
} from "../lib/pages";
import type { FakeTitle } from "./data";

interface Deps {
  catalog: FakeTitle[];
  metadataListsAvailable: () => boolean;
  card: (t: FakeTitle) => TitleCard;
  libraries: () => PageLibrary[];
  viewers: () => { id: number; title: string }[];
  collections: () => { id?: string; name: string }[];
}

type Json = { status: number; body: unknown };
const ok = (body: unknown): Json => ({ status: 200, body });
const fail = (status: number, code: string, message: string): Json => ({
  status,
  body: { error: { code, message } },
});

/** The kid viewer: age limit PG, no Anime Shows. */
const KID = 3;
const KID_LIMIT = "PG";
const KID_HIDDEN_LIBRARY = 7;

function hash(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return h >>> 0;
}

const CHOICE = (key: string, label: string, choices: [string, string][], def?: string) => ({
  key,
  label,
  type: "choice" as const,
  choices: choices.map(([value, l]) => ({ value, label: l })),
  default: def ?? choices[0]![0],
});

function builtinSources(): Omit<PageSource, "available" | "used" | "libraries">[] {
  const watch = ["charts"];
  return [
    {
      kind: "continue",
      title: "Continue",
      description: "What each viewer is part-way through, in any library",
      group: "keep",
      personal: true,
      needs: ["progress"],
      split: true,
      purpose: "continue",
      look: {},
    },
    {
      kind: "up-next",
      title: "Up Next",
      description: "The next episode of shows they're watching",
      group: "keep",
      personal: true,
      needs: ["episodes"],
      purpose: "nextUp",
      look: {},
    },
    {
      kind: "favorites",
      title: "Favorites",
      description: "Titles this viewer marked as favorites",
      group: "keep",
      personal: true,
      needs: ["added"],
      split: true,
      look: {},
    },
    {
      kind: "trending",
      title: "Trending",
      description: "What's trending on TMDB today or this week",
      group: "trending",
      personal: false,
      needs: watch,
      split: true,
      params: [
        CHOICE("window", "Window", [
          ["week", "This week"],
          ["day", "Today"],
        ]),
      ],
      look: {},
    },
    {
      kind: "popular",
      title: "Popular",
      description: "TMDB's most popular right now",
      group: "trending",
      personal: false,
      needs: watch,
      split: true,
      look: {},
    },
    {
      kind: "top-10-today",
      title: "Top 10 today",
      description: "Trending, popularity and what viewers here watch, numbered",
      group: "trending",
      personal: false,
      needs: watch,
      look: { numbered: true },
    },
    {
      kind: "popular-here",
      title: "Popular on this server",
      description: "What viewers here played most in 30 days",
      group: "trending",
      personal: false,
      needs: ["popularity"],
      split: true,
      look: {},
    },
    {
      kind: "new",
      title: "New",
      description: "Newest releases, or newly playable here",
      group: "new",
      personal: false,
      needs: ["released"],
      split: true,
      params: [
        CHOICE("by", "Which", [
          ["released", "Released recently"],
          ["available", "Newly available"],
        ]),
      ],
      look: {},
    },
    {
      kind: "recently-added",
      title: "Recently added",
      description: "Newest additions to a library",
      group: "new",
      personal: false,
      needs: ["added"],
      split: true,
      look: {},
    },
    {
      kind: "airing-today",
      title: "Airing today",
      description: "Shows with an episode out today",
      group: "new",
      personal: false,
      needs: ["episodes"],
      look: {},
    },
    {
      kind: "upcoming",
      title: "Upcoming",
      description: "Coming soon, weeks or months ahead",
      group: "new",
      personal: false,
      needs: watch,
      split: true,
      params: [
        CHOICE(
          "ahead",
          "How far ahead",
          [
            ["2w", "2 weeks"],
            ["1m", "A month"],
            ["3m", "3 months"],
          ],
          "1m",
        ),
      ],
      look: {},
    },
    {
      kind: "because-you-watched",
      title: "Because you watched",
      description: "Like something they just finished · up to 3 rows",
      group: "foryou",
      personal: true,
      needs: ["taste"],
      params: [
        {
          key: "count",
          label: "How many rows",
          type: "count" as const,
          min: 1,
          max: 3,
          default: "1",
        },
      ],
      look: {},
    },
    {
      kind: "top-10",
      title: "Top 10 for you",
      description: "The ten titles the taste model rates highest",
      group: "foryou",
      personal: true,
      needs: ["taste"],
      look: { numbered: true },
    },
    {
      kind: "hidden-gems",
      title: "Hidden gems",
      description: "Little-known titles that fit their taste",
      group: "foryou",
      personal: true,
      needs: ["taste"],
      look: {},
    },
    {
      kind: "top-rated",
      title: "Top rated",
      description: "TMDB's top rated, optionally in one genre",
      group: "top",
      personal: false,
      needs: watch,
      split: true,
      params: [
        { key: "genre", label: "Genre", type: "facetValue" as const, facet: "genre" },
        CHOICE(
          "votes",
          "Minimum votes",
          [
            ["", "Ether decides"],
            ["1000", "1,000"],
            ["10000", "10,000"],
          ],
          "",
        ),
      ].map((p, i) => (i === 1 ? { ...p, more: true } : p)),
      look: {},
    },
    {
      kind: "by-genre",
      title: "By genre",
      description: "Ether picks a genre per viewer, or pin one (Anime, Horror…)",
      group: "browse",
      personal: false,
      needs: ["facet:genre"],
      split: true,
      params: [{ key: "genre", label: "Genre", type: "facetValue" as const, facet: "genre" }],
      look: {},
    },
    {
      kind: "by-decade",
      title: "By decade",
      description: "Ether picks, or pin one (1990s…)",
      group: "browse",
      personal: false,
      needs: ["facet:decade"],
      split: true,
      params: [{ key: "decade", label: "Decade", type: "facetValue" as const, facet: "decade" }],
      look: {},
    },
    {
      kind: "collection",
      title: "Collection",
      description: "One of your collections",
      group: "collections",
      personal: false,
      needs: ["catalogue"],
      split: true,
      params: [{ key: "collection", label: "Collection", type: "ref" as const, required: true }],
      look: {},
    },
    {
      kind: "watchlist",
      title: t("Watchlist"),
      description: t("Titles on the viewer’s watchlist."),
      group: "keep",
      personal: true,
      homeOnly: true,
      params: [
        {
          key: "splitByAvailability",
          label: t("Split by availability"),
          type: "bool",
          default: "false",
        },
      ],
      look: {},
    },
    {
      kind: "collection-cards",
      title: t("Collections"),
      description: t("Show collections as clickable cards."),
      group: "collections",
      personal: false,
      homeOnly: true,
      params: [
        { key: "collections", label: t("Collections"), type: "collectionList", max: 50 },
        { key: "match", label: t("Collection name starts with"), type: "text", max: 100 },
      ],
      look: { card: "poster" },
    },
    {
      kind: "surface:sports:tonight",
      title: "Tonight",
      description: "Live and upcoming games",
      group: "plugins",
      personal: false,
      plugin: "Sports",
      homeOnly: true,
      look: {},
    },
    {
      kind: "shelf",
      title: "Plugin shelf",
      description: "Rows the library's plugin declares",
      group: "plugins",
      personal: false,
      needs: ["shelves"],
      params: [{ key: "shelf", label: "Shelf", type: "ref" as const, required: true }],
      look: {},
    },
    {
      kind: "live-now",
      title: "Live now",
      description: "What's on your channels right now",
      group: "live",
      personal: false,
      homeOnly: true,
      purpose: "live",
      params: [
        CHOICE(
          "group",
          "Channel group",
          [
            ["", "All channels"],
            ["favourites", "Favourites"],
          ],
          "",
        ),
      ],
      look: { style: "wide" },
    },
  ];
}

function capsOf(l: PageLibrary): string[] {
  if (l.caps?.length) return l.caps;
  if (l.experience === "listen") return ["progress", "added", "released", "shelves", "popularity"];
  if (l.experience === "read") return ["progress", "added", "released", "popularity"];
  if (l.plugin) return ["progress", "added", "released", "popularity", "shelves"];
  const base = [
    "charts",
    "taste",
    "progress",
    "added",
    "released",
    "popularity",
    "catalogue",
    "facet:genre",
    "facet:decade",
  ];
  return l.mediaType === "show" ? [...base, "episodes"] : [...base, "movies"];
}

function scheduleAllows(s: Schedule | null | undefined, at: Date): boolean {
  if (!s) return true;
  const day = at.getDay();
  const hour = at.getHours();
  const part =
    hour >= 22 || hour < 5 ? "late" : hour < 12 ? "morning" : hour < 17 ? "afternoon" : "evening";
  if (s.days?.length && !s.days.includes(day)) return false;
  if (s.dayparts?.length && !s.dayparts.includes(part as never)) return false;
  if (s.months?.length && !s.months.includes(at.getMonth() + 1)) return false;
  if (s.holidays?.length) {
    const m = at.getMonth() + 1;
    const d = at.getDate();
    const inHoliday = s.holidays.some(
      (h) =>
        (h === "christmas" && m === 12 && d <= 26) ||
        (h === "halloween" && m === 10) ||
        (h === "valentines" && m === 2 && d >= 7 && d <= 14) ||
        (h === "new-year" && ((m === 12 && d >= 26) || (m === 1 && d === 1))),
    );
    if (!inHoliday) return false;
  }
  return true;
}

function nextAllowed(s: Schedule, from: Date): Date | undefined {
  const d = new Date(from);
  for (let i = 0; i < 24 * 400; i++) {
    d.setHours(d.getHours() + 1);
    if (scheduleAllows(s, d)) return d;
  }
  return undefined;
}

export function createPagesMock(deps: Deps) {
  const saved = new Map<string, RowPage>();
  let idSeq = 0;
  const rid = () => `rmock${String(++idSeq).padStart(4, "0")}`;

  const libs = () => deps.libraries();
  const libOf = (key: string): PageLibrary | undefined => {
    const m = /^library:(\d+)\/([a-z0-9_-]+)$/.exec(key);
    return m ? libs().find((l) => l.id === Number(m[1])) : undefined;
  };
  const tabsOf = (l: PageLibrary) =>
    l.rowsTabs ?? [
      { key: l.experience === "listen" ? "home" : "recommended", label: "Recommendations" },
    ];
  const pageKeys = () => [
    "home",
    ...libs().flatMap((l) => tabsOf(l).map((t) => `library:${l.id}/${t.key}`)),
  ];
  const pageName = (key: string) => {
    if (key === "home") return "Home";
    const l = libOf(key);
    const tab = l && tabsOf(l).find((t) => key.endsWith(`/${t.key}`));
    return l ? `${l.name} › ${tab?.label ?? "Recommendations"}` : key;
  };

  function seeded(key: string): RowPage {
    const row = (kind: string, extra: Partial<PageRow> = {}): PageRow => ({
      id: rid(),
      source: { kind, params: extra.source?.params },
      scope: extra.scope ?? {},
      name: extra.name,
      look: extra.look ?? {},
      placement: extra.placement ?? { mode: "show", audience: { mode: "everyone" } },
    });
    if (key === "home") {
      const kid = deps.viewers().find((v) => v.id === KID);
      const col = deps.collections()[0];
      return {
        key,
        mode: "personalized",
        length: { rows: 9 },
        picks: true,
        rows: [
          row("continue", {
            placement: { mode: "pinned", position: 1, audience: { mode: "everyone" } },
          }),
          row("surface:sports:tonight", {
            placement: { mode: "pinned", position: 2, audience: { mode: "everyone" } },
          }),
          row("trending", {
            source: { kind: "trending", params: { window: "week" } },
            scope: { libraries: [1, 2], perLibrary: true },
            look: { style: "hero" },
            placement: { mode: "pinned", position: 3, audience: { mode: "everyone" } },
          }),
          row("new", {
            source: { kind: "new", params: { by: "released" } },
            scope: { libraries: [1, 2], perLibrary: true },
            placement: { mode: "show", audience: { mode: "everyone" } },
          }),
          row("because-you-watched", {
            placement: { mode: "auto", audience: { mode: "everyone" } },
          }),
          ...(col
            ? [
                row("collection", {
                  source: { kind: "collection", params: { collection: String(col.id ?? "1") } },
                  scope: { libraries: [1] },
                  look: { style: "wide" },
                  placement: {
                    mode: "show",
                    when: { days: [5, 6], dayparts: ["evening", "late"] },
                    audience: kid ? { mode: "only", viewers: [kid.id] } : { mode: "everyone" },
                  },
                }),
              ]
            : []),
          row("top-rated", {
            name: "Christmas Movies",
            scope: { libraries: [1] },
            placement: {
              mode: "pinned",
              position: 2,
              when: { holidays: ["christmas"] },
              audience: { mode: "everyone" },
            },
          }),
          row("by-decade", { placement: { mode: "hide", audience: { mode: "everyone" } } }),
        ],
        variants: kid
          ? [
              {
                viewer: kid.id,
                rows: [],
              },
            ]
          : undefined,
        revision: 0,
      };
    }
    const lib = libOf(key);
    if (lib && (lib.experience === "listen" || lib.plugin)) {
      return {
        key,
        mode: "curated",
        length: {},
        picks: false,
        rows: [
          row("continue", { look: { style: lib.experience === "listen" ? "tiles" : "" } }),
          row("shelf", { source: { kind: "shelf", params: { shelf: "new" } } }),
          row("recently-added", { look: { card: lib.experience === "listen" ? "square" : "" } }),
          ...(lib.experience === "listen"
            ? [
                row("shelf", {
                  source: { kind: "shelf", params: { shelf: "top-songs" } },
                  look: { style: "tracks" },
                }),
              ]
            : []),
        ],
        revision: 0,
      };
    }
    return {
      key,
      mode: "personalized",
      length: { rows: 8 },
      picks: true,
      rows: [
        row("recently-added", {
          placement: { mode: "pinned", position: 1, audience: { mode: "everyone" } },
        }),
        row("new"),
        row("trending", { source: { kind: "trending", params: { window: "week" } } }),
        row("top-rated", { placement: { mode: "auto", audience: { mode: "everyone" } } }),
        row("because-you-watched", { placement: { mode: "auto", audience: { mode: "everyone" } } }),
      ],
      revision: 0,
    };
  }

  const seededCache = new Map<string, RowPage>();
  function current(key: string): { page: RowPage; saved: boolean } {
    const s = saved.get(key);
    if (s) return { page: structuredClone(s), saved: true };
    if (!seededCache.has(key)) seededCache.set(key, seeded(key));
    return { page: structuredClone(seededCache.get(key)!), saved: false };
  }

  function docFor(key: string): PageDoc {
    const { page, saved: isSaved } = current(key);
    const lib = libOf(key);
    const names = new Map(deps.viewers().map((v) => [v.id, v.title]));
    return {
      page,
      saved: isSaved,
      library: lib ? { ...lib, caps: capsOf(lib) } : undefined,
      libraries: key === "home" ? libs().map((l) => ({ ...l, caps: capsOf(l) })) : undefined,
      viewersWithChanges: (page.variants ?? [])
        .map((v) => ({
          id: v.viewer,
          name: names.get(v.viewer) ?? "",
          changes: v.rows.length + (v.mode ? 1 : 0) + (v.length ? 1 : 0),
        }))
        .filter((v) => v.changes > 0)
        .sort((a, b) => b.changes - a.changes),
      defaultLength: key === "home" ? 10 : 8,
    };
  }

  function sourcesFor(key: string): SourcesDoc {
    const lib = libOf(key);
    const pageLibs = lib ? [lib] : libs();
    const { page } = current(key);
    const used = new Map<string, number>();
    for (const r of page.rows) used.set(r.source.kind, (used.get(r.source.kind) ?? 0) + 1);
    const sources: PageSource[] = builtinSources().map((s) => {
      const fits = pageLibs.filter((l) => (s.needs ?? []).every((n) => capsOf(l).includes(n)));
      const missingMetadata =
        ["trending", "popular", "airing-today", "upcoming"].includes(s.kind) &&
        !deps.metadataListsAvailable();
      const available =
        !missingMetadata && (s.homeOnly ? !lib : (s.needs ?? []).length === 0 || fits.length > 0);
      const refs =
        s.kind === "collection"
          ? deps.collections().map((c, i) => ({ value: String(c.id ?? i + 1), label: c.name }))
          : s.kind === "collection-cards"
            ? deps
                .collections()
                .map((c, i) => ({ value: String(5_000_000_000_000_000 + i + 1), label: c.name }))
            : s.kind === "shelf"
              ? [
                  { value: "new", label: "New releases" },
                  { value: "charts", label: "Charts" },
                  { value: "playlists", label: "Playlists" },
                  { value: "top-songs", label: "Top songs this week" },
                ]
              : undefined;
      return {
        ...s,
        available,
        unavailable: missingMetadata
          ? "metadata_source_unavailable"
          : available
            ? undefined
            : "No library here has what it needs",
        libraries: fits.map((l) => l.id),
        used: used.get(s.kind) ?? 0,
        refs,
      };
    });
    return {
      sources,
      libraries: pageLibs.map((l) => ({ ...l, caps: capsOf(l) })),
      collections: deps
        .collections()
        .map((c, i) => ({ id: String(c.id ?? i + 1), name: c.name, count: 24 })),
    };
  }

  function sourceInfo(kind: string) {
    return builtinSources().find((s) => s.kind === kind);
  }

  function titleFor(r: PageRow, libName: string | undefined, key: string): string {
    const info = sourceInfo(r.source.kind);
    let base = info?.title ?? r.source.kind;
    if (r.source.kind === "collection") {
      base =
        deps.collections().find((c, i) => String(c.id ?? i + 1) === r.source.params?.collection)
          ?.name ?? "Collection";
    }
    if (r.source.kind === "shelf") {
      base =
        sourcesFor(key)
          .sources.find((s) => s.kind === "shelf")
          ?.refs?.find((x) => x.value === r.source.params?.shelf)?.label ?? base;
    }
    if (r.source.kind === "continue") base = "Continue Watching";
    if (r.name)
      return libName
        ? r.name.replaceAll("{library}", libName)
        : r.name.replaceAll(" {library}", "").replaceAll("{library}", "");
    return libName ? `${base} ${libName}` : base;
  }

  function items(seed: string, lib: PageLibrary | undefined, kid: boolean, n = 12): TitleCard[] {
    const pool = deps.catalog.filter((t) =>
      !lib || lib.experience !== "watch" || lib.plugin
        ? true
        : lib.mediaType === "show"
          ? t.type === "show"
          : t.type === "movie",
    );
    const start = hash(seed) % Math.max(1, pool.length - n);
    return pool.slice(start, start + n).map((t) => {
      const c = deps.card(t);
      return kid ? { ...c, contentRating: ["G", "PG", "TV-Y7", "TV-PG"][hash(c.key) % 4] } : c;
    });
  }

  function collectionCards(row: PageRow, count: number): TitleCard[] {
    const prefix = row.source.params?.match?.trim().toLowerCase();
    const refs = deps.collections().map((collection, index) => ({
      ...collection,
      key: String(5_000_000_000_000_000 + index + 1),
      art: deps.catalog[index],
    }));
    const chosen = prefix
      ? refs
          .filter((c) => c.name.toLowerCase().startsWith(prefix))
          .sort((a, b) => a.name.localeCompare(b.name))
      : (row.source.params?.collections ?? "")
          .split(",")
          .flatMap((id) => refs.filter((c) => c.key === id));
    return chosen
      .slice(0, count)
      .flatMap((c) => (c.art ? [{ ...deps.card(c.art), key: c.key, title: c.name }] : []));
  }

  function preview(
    key: string,
    body: { page?: RowPage; viewer?: number; device?: string; at?: string; count?: number },
  ): PagePreview {
    const perRow = body.count ?? 12;
    const page = body.page ?? current(key).page;
    const at = body.at ? new Date(body.at) : new Date();
    const device = body.device ?? "tv";
    const viewer = body.viewer ?? 0;
    const kid = viewer === KID;
    const variant = page.variants?.find((v) => v.viewer === viewer);
    const mode = variant?.mode ?? page.mode;
    const length =
      (variant?.length ?? page.length).byDevice?.[device as "tv"] ??
      (variant?.length ?? page.length).rows ??
      (key === "home" ? 10 : 8);
    const pageLib = libOf(key);
    const names = new Map(deps.viewers().map((v) => [v.id, v.title]));
    const out: PreviewRow[] = [];
    const notShown: PagePreview["notShown"] = [];
    const pinned: { pos: number; rows: PreviewRow[] }[] = [];
    const shown: PreviewRow[][] = [];
    const auto: PreviewRow[][] = [];
    for (const base of page.rows) {
      const o = variant?.rows.find((x) => x.row === base.id);
      const r: PageRow = {
        ...base,
        look: o?.look ?? base.look,
        placement: o?.placement ?? base.placement,
      };
      const pl = r.placement;
      const title = titleFor(r, undefined, key);
      if (pl.mode === "hide") {
        notShown.push({ row: r.id, title, reason: "Hidden on this page" });
        continue;
      }
      const aud = pl.audience;
      if (viewer && aud?.mode === "only" && !aud.viewers?.includes(viewer)) {
        notShown.push({
          row: r.id,
          title,
          reason: `Not for ${names.get(viewer) ?? "this viewer"}`,
          previewAs: aud.viewers?.[0],
        });
        continue;
      }
      if (viewer && aud?.mode === "except" && aud.viewers?.includes(viewer)) {
        notShown.push({
          row: r.id,
          title,
          reason: `Not for ${names.get(viewer) ?? "this viewer"}`,
        });
        continue;
      }
      if (!viewer && aud?.mode === "only") {
        notShown.push({
          row: r.id,
          title,
          reason: `Only for ${(aud.viewers ?? []).map((v) => names.get(v)).join(", ")}`,
          previewAs: aud.viewers?.[0],
        });
        continue;
      }
      if (pl.devices?.length && !pl.devices.includes(device as never)) {
        notShown.push({ row: r.id, title, reason: "Not on this device" });
        continue;
      }
      const info = sourceInfo(r.source.kind);
      const surface = r.source.kind.startsWith("surface:");
      const scopeLibs = pageLib
        ? [pageLib]
        : ((r.scope.libraries?.length
            ? r.scope.libraries.map((id) => libs().find((l) => l.id === id)).filter(Boolean)
            : libs().filter((l) =>
                (info?.needs ?? []).every((n) => capsOf(l).includes(n)),
              )) as PageLibrary[]);
      const visibleLibs = kid ? scopeLibs.filter((l) => l.id !== KID_HIDDEN_LIBRARY) : scopeLibs;
      const fan = !pageLib && r.scope.perLibrary ? visibleLibs.slice(0, 6) : [undefined];
      if (kid && r.source.kind === "top-rated" && r.name?.includes("Horror")) {
        notShown.push({
          row: r.id,
          title,
          reason: `Nothing within ${names.get(viewer)}'s ${KID_LIMIT} limit`,
        });
        continue;
      }
      if (!scheduleAllows(pl.when, at)) {
        const when = pl.when ? nextAllowed(pl.when, at) : undefined;
        notShown.push({
          row: r.id,
          title,
          reason: pl.when?.holidays?.length
            ? "Shows in its holiday season"
            : "Scheduled for another time",
          previewAt: when?.toISOString(),
        });
        out.push({
          hubId: `${r.id}~ghost`,
          rowId: r.id,
          mode: pl.mode as never,
          why: "Scheduled away right now",
          look: r.look,
          personal: Boolean(info?.personal),
          ghost: true,
          present: { rowId: r.id },
          title,
          kind: r.source.kind,
          items: [],
        });
        continue;
      }
      const built: PreviewRow[] = fan.map((l) => {
        const libName = l?.name;
        return {
          hubId: l ? `${r.id}@${l.id}` : r.id,
          rowId: r.id,
          library: l?.id,
          mode: pl.mode as never,
          why: "",
          note:
            !info?.personal && !r.look.keepWhole && out.length > 1
              ? "2 skipped (shown above)"
              : undefined,
          look: r.look,
          personal: Boolean(info?.personal),
          flags:
            kid && r.source.kind === "collection"
              ? ["1 unrated title (hidden for this viewer)"]
              : kid && surface
                ? ["Sports Tonight · TV-PG (declared by the plugin)"]
                : undefined,
          present: {
            rowId: r.id,
            layout: r.look.style || info?.look.style || undefined,
            card: r.look.card || (pageLib?.experience === "listen" ? "square" : undefined),
            library: l?.id,
            numbered: r.look.numbered ?? info?.look.numbered ?? undefined,
            surface: surface ? { plugin: "Sports", surface: "sports.tonight" } : undefined,
          },
          title: titleFor(r, libName, key),
          kind: r.source.kind,
          items: surface
            ? []
            : r.source.kind === "collection-cards"
              ? collectionCards(r, perRow)
              : items(
                  `${r.id}${r.source.kind}${JSON.stringify(r.source.params)}${l?.id ?? ""}`,
                  l ?? pageLib,
                  kid,
                  perRow,
                ),
        };
      });
      if (mode === "curated") {
        built.forEach((b) => (b.why = "Curated: in your order"));
        shown.push(built);
      } else if (pl.mode === "pinned") {
        built.forEach(
          (b, i) =>
            (b.why = i === 0 ? `Pinned to #${pl.position} for everyone` : "Same row, next library"),
        );
        pinned.push({ pos: pl.position ?? 1, rows: built });
      } else if (pl.mode === "show") {
        built.forEach((b) => (b.why = "Always shown; its place follows what this viewer plays"));
        shown.push(built);
      } else {
        built.forEach((b) => (b.why = "Fits what this viewer plays"));
        auto.push(built);
      }
    }
    // Arrange: pinned blocks at their positions, then shown, then Auto up to the length.
    const ghosts = out.filter((r) => r.ghost);
    let rows: PreviewRow[] = [];
    if (mode === "curated") rows = shown.flat();
    else {
      const flow = [...shown];
      let room = Math.max(
        0,
        length - pinned.reduce((n, p) => n + p.rows.length, 0) - flow.flat().length,
      );
      for (const a of auto) {
        if (room <= 0) break;
        flow.push(a);
        room -= a.length;
      }
      const picksOn = page.picks;
      const extra: PreviewRow[][] = [];
      // Like the engine: Top 10 Today leads as one of Ether's picks (no rowId).
      if (picksOn && room > 0 && !page.rows.some((r) => r.source.kind === "top-10-today")) {
        extra.push([
          {
            hubId: "ether.top-10-today",
            why: "Fits this page: the server's Top 10 today",
            look: {},
            personal: false,
            present: { numbered: true },
            title: "Top 10 on Living Room Ether Today",
            kind: "top-10-today",
            items: items("top-10-today", pageLib, kid, 10),
          },
        ]);
        room -= 1;
      }
      if (picksOn && room > 0 && !page.rows.some((r) => r.source.kind === "popular-here")) {
        extra.push([
          {
            hubId: "ether.popular-here",
            why: "Fits this page: what viewers here play most",
            look: {},
            personal: false,
            present: {},
            title: "Popular on this server",
            kind: "popular-here",
            items: items("popular-here", pageLib, kid),
          },
        ]);
      }
      const order = [...flow, ...extra];
      // Equal positions: the row listed first in the page wins the slot.
      const sortedPins = [...pinned].reverse().sort((a, b) => a.pos - b.pos);
      for (const p of sortedPins) {
        const at = Math.min(Math.max(0, p.pos - 1), rows.length);
        while (rows.length < at && order.length) rows.push(...order.shift()!);
        rows.splice(Math.min(at, rows.length), 0, ...p.rows);
      }
      while (order.length) rows.push(...order.shift()!);
    }
    // Ghosts sit where their row would be in the owner's order.
    for (const g of ghosts) {
      const idx = page.rows.findIndex((r) => r.id === g.rowId);
      const after = page.rows.slice(idx + 1).map((r) => r.id);
      const pos = rows.findIndex((r) => r.rowId && after.includes(r.rowId));
      rows.splice(pos < 0 ? rows.length : pos, 0, g);
    }
    const counted = rows.filter((r) => !r.ghost && !r.present.surface);
    const kidViewer = kid ? deps.viewers().find((v) => v.id === KID) : undefined;
    return {
      summary: {
        rows: counted.length,
        length,
        pinned: counted.filter((r) => r.mode === "pinned").length,
        shown: counted.filter((r) => r.mode === "show").length,
        auto: counted.filter((r) => !r.mode || r.mode === "auto").length,
        picks: mode === "curated" ? 0 : 3,
        picksOn: mode !== "curated" && page.picks,
        chosen: counted.filter((r) => r.rowId).length,
        appOnly: rows.filter((r) => !r.ghost && r.present.surface).length,
      },
      rows,
      picks:
        mode === "curated"
          ? []
          : [
              {
                kind: "popular",
                title: "Popular",
                personal: false,
                why: "Fits this page; not on it yet",
              },
              {
                kind: "hidden-gems",
                title: "Hidden gems",
                personal: true,
                why: "Strong for viewers who explore",
              },
              {
                kind: "by-genre",
                title: "By genre",
                personal: false,
                why: "Ether picks a genre per viewer",
              },
            ],
      notShown,
      access: kidViewer
        ? {
            viewer: kidViewer.id,
            name: kidViewer.title,
            ageLimit: KID_LIMIT,
            unrated: "hidden",
            librariesSeen: libs().length - 1,
            librariesTotal: libs().length,
            hiddenLibraries: libs()
              .filter((l) => l.id === KID_HIDDEN_LIBRARY)
              .map((l) => l.name),
          }
        : null,
    };
  }

  function validate(page: RowPage): string | null {
    if (page.mode !== "curated" && page.mode !== "personalized")
      return "mode: must be curated or personalized";
    const seen = new Set<string>();
    for (const [i, r] of page.rows.entries()) {
      if (!/^r[a-z0-9]{8}$/.test(r.id) || seen.has(r.id))
        return `rows[${i}]: id is missing, malformed or repeated`;
      seen.add(r.id);
      if (r.placement.mode === "pinned" && !(r.placement.position && r.placement.position >= 1))
        return `rows[${i}]: placement: a pinned row needs a position from 1 to 50`;
    }
    return null;
  }

  function handle(method: string, seg: string[], body: unknown, headers: Headers): Json | null {
    if (seg[0] === "row-sources" && seg[1] === "facets" && method === "GET") {
      const facet = seg[3];
      const values =
        facet === "decade"
          ? ["1970s", "1980s", "1990s", "2000s", "2010s", "2020s"]
          : [
              "Action",
              "Animation",
              "Anime",
              "Comedy",
              "Documentary",
              "Drama",
              "Family",
              "Horror",
              "Romance",
              "Science Fiction",
              "Thriller",
            ];
      return ok({
        values: values.map((v) => ({
          value: v.toLowerCase(),
          label: v,
          count: 10 + (hash(v) % 90),
        })),
      });
    }
    if (seg[0] !== "pages") return null;
    if (seg.length === 1 && method === "GET") {
      const out: PageSummary[] = pageKeys().map((key) => {
        const { page, saved: isSaved } = current(key);
        const l = libOf(key);
        return {
          key,
          name: pageName(key),
          library: l?.id,
          tab: l ? key.split("/")[1] : undefined,
          experience: l?.experience ?? "",
          mode: page.mode,
          rowCount: page.rows.length,
          saved: isSaved,
          apps:
            l?.experience === "listen"
              ? ["vela", "plex", "mediaBrowser"]
              : ["plex", "mediaBrowser", "vela", "silo"],
        };
      });
      return ok(out);
    }
    const key = decodeURIComponent(seg[1] ?? "").replaceAll("~", "/");
    if (!pageKeys().includes(key)) return fail(404, "not_found", "no such page");
    const sub = seg[2];
    if (!sub) {
      if (method === "GET") return ok(docFor(key));
      if (method === "DELETE") {
        saved.delete(key);
        seededCache.delete(key);
        return ok(docFor(key));
      }
      if (method === "PUT") {
        const page = body as RowPage;
        const ifMatch = headers.get("If-Match");
        if (ifMatch === null) return fail(428, "precondition_required", "If-Match is required");
        const cur = current(key).page;
        if (Number(ifMatch) !== cur.revision)
          return fail(
            409,
            "conflict",
            "Someone else saved this page. Reload it to see their changes.",
          );
        const problem = validate(page);
        if (problem) return fail(400, "validation", problem);
        saved.set(key, {
          ...structuredClone(page),
          key,
          revision: cur.revision + 1,
          updatedAt: new Date().toISOString(),
        });
        return ok(docFor(key));
      }
    }
    if (sub === "sources" && method === "GET") return ok(sourcesFor(key));
    if (sub === "preview" && method === "POST") {
      const b = (body ?? {}) as { page?: RowPage };
      if (b.page) {
        const problem = validate(b.page);
        if (problem) return fail(400, "validation", problem);
      }
      return ok(preview(key, body as never));
    }
    if (sub === "rows" && seg[3] === "preview" && method === "POST") {
      const b = body as {
        row: PageRow;
        viewer?: number;
        device?: string;
        at?: string;
        count?: number;
      };
      const page = {
        ...current(key).page,
        mode: "curated" as const,
        picks: false,
        rows: [
          {
            ...b.row,
            placement: { mode: "show" as const, audience: { mode: "everyone" as const } },
          },
        ],
      };
      const p = preview(key, {
        page,
        viewer: b.viewer,
        device: b.device,
        at: b.at,
        count: b.count,
      });
      const rows = p.rows?.filter((r) => !r.ghost) ?? [];
      return ok({ rows, total: rows.reduce((n, r) => n + (r.items?.length ?? 0), 0) });
    }
    if (sub === "impact" && method === "POST") {
      const page = (body as { page?: RowPage }).page;
      return ok({
        viewers: deps.viewers().length,
        withChanges: (page?.variants ?? []).filter((v) => v.rows.length || v.mode || v.length)
          .length,
      });
    }
    return fail(404, "not_found", "no such API endpoint");
  }

  return { handle, preview, saved };
}

export type PagesMock = ReturnType<typeof createPagesMock>;

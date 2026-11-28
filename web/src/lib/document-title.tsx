import {
  createContext,
  useContext,
  useEffect,
  useId,
  useMemo,
  useRef,
  type ReactNode,
} from "react";
import { t } from "../i18n";
import { pluginLocalId, type PageMeta, type PageSectionMeta } from "./console-descriptors";
import { BUILTIN, textOf, type Descriptor } from "./extend";

export type TitlePart = string | null | undefined;

export interface RouteDocumentTitle {
  /** Whether the current pathname matched a known console route. */
  matched: boolean;
  parts: readonly string[];
}

export interface ResolveRouteTitleOptions {
  registryLoading?: boolean;
  /** Built-in descriptor ids mounted as top-level routes in the router. */
  mountedPageIds?: ReadonlySet<string>;
}

/**
 * Format browser tab title segments with the server name brand:
 * `<Page> · <Sub-tab> — <server name or "Ether">`.
 * Empty segments are omitted and adjacent duplicates are collapsed.
 */
export function formatDocumentTitle(parts: readonly TitlePart[], brand?: string | null): string {
  const server = brand?.trim() || "Ether";
  const cleaned: string[] = [];
  for (const raw of parts) {
    const part = raw?.trim();
    if (!part) continue;
    const prev = cleaned[cleaned.length - 1];
    if (prev && prev.toLocaleLowerCase() === part.toLocaleLowerCase()) continue;
    cleaned.push(part);
  }
  return cleaned.length > 0 ? `${cleaned.join(" · ")} — ${server}` : server;
}

function safeDecode(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

function normalizePath(pathname: string): string {
  if (!pathname || pathname === "/") return "/";
  const trimmed = pathname.replace(/\/+$/, "");
  return trimmed || "/";
}

function stylesLabel(query: URLSearchParams): string {
  switch (query.get("namePreview")) {
    case "themes":
      return t("Themes");
    case "branding":
      return t("Branding");
    case "package":
      return t("On-air package");
    default:
      return t("Styles");
  }
}

function resolveTab(
  raw: string | undefined,
  labels: Record<string, () => string>,
  fallback?: string,
): string | null {
  if (raw && raw in labels) return labels[raw]!();
  if (fallback && fallback in labels) return labels[fallback]!();
  return null;
}

function resolveSectionFallback(
  rest: readonly string[],
  sections: readonly PageSectionMeta[] | undefined,
): string[] {
  if (!sections?.length || rest.length === 0) return [];
  const full = rest.join("/");
  const exact = sections.find((s) => s.key === full);
  if (exact) {
    const label = textOf(exact.label);
    return label ? [label] : [];
  }
  const first = sections.find((s) => s.key === rest[0]);
  if (first) {
    const label = textOf(first.label);
    return label ? [label] : [];
  }
  return [];
}

function resolveBuiltinSubparts(
  pageId: string,
  rest: readonly string[],
  query: URLSearchParams,
  sections?: readonly PageSectionMeta[],
): string[] {
  const tab = rest[0];
  const section = rest[1];
  switch (pageId) {
    case "overview":
    case "home":
    case "general":
      return [];
    case "activity": {
      const label = resolveTab(
        tab,
        {
          now: () => t("Now playing"),
          history: () => t("History"),
          viewing: () => t("Viewing"),
          delivery: () => t("Delivery"),
          experience: () => t("Experience"),
          recommendations: () => t("Recommendations"),
        },
        "now",
      );
      return label ? [label] : [];
    }
    case "viewers": {
      if (tab === "watchlists") return [t("Watchlists")];
      if (tab === "security") return [t("Security")];
      if (!tab || !/^[1-9]\d*$/.test(tab)) return [];
      const parts: string[] = [];
      const subtab = resolveTab(section, {
        home: () => t("Home"),
        taste: () => t("Taste"),
        activity: () => t("Recent activity"),
        playback: () => t("Audio & subtitles"),
        limits: () => t("Limits"),
        "sign-in": () => t("Sign-in & devices"),
        lists: () => t("Lists & alerts"),
      });
      if (subtab) parts.push(subtab);
      if (section === "sign-in" && query.get("signin") === "sync") {
        parts.push(t("Connected accounts"));
      } else if (section === "lists") {
        const listTab = query.get("list");
        if (listTab === "alerts") parts.push(t("Alerts"));
        else if (listTab === "hidden") parts.push(t("Not interested"));
      }
      return parts;
    }
    case "collections": {
      if (tab === "new") return [t("New collection")];
      if (tab) return [t("Edit collection")];
      return query.get("tab") === "templates" ? [t("Templates")] : [];
    }
    case "livetv": {
      if (tab === "library") return [t("Library")];
      if (tab === "looks") return [stylesLabel(query)];
      if (tab === "guide" || tab === "guide-data" || (!tab && query.has("guide"))) {
        return [t("Guide")];
      }
      return [t("Channels")];
    }
    case "calendar": {
      const label = resolveTab(tab, {
        month: () => t("Month"),
        week: () => t("Week"),
        agenda: () => t("Agenda"),
      });
      return label ? [label] : [];
    }
    case "libraries": {
      if (!tab) return [];
      const label = resolveTab(section, {
        rules: () => t("Rules"),
        tabs: () => t("Tabs"),
        settings: () => t("Settings"),
      });
      return label ? [label] : [];
    }
    case "metadata": {
      const label = resolveTab(
        tab,
        {
          artwork: () => t("Artwork"),
          ratings: () => t("Ratings"),
          episodes: () => t("Episode order"),
        },
        "artwork",
      );
      return label ? [label] : [];
    }
    case "sources": {
      const label = resolveTab(
        tab,
        {
          debrid: () => t("Debrid"),
          usenet: () => t("Usenet"),
          iptv: () => t("IPTV"),
          indexers: () => t("Indexers"),
          statistics: () => t("Statistics"),
          usage: () => t("Usage"),
        },
        "debrid",
      );
      return label ? [label] : [];
    }
    case "releases": {
      const label = resolveTab(
        tab,
        {
          profiles: () => t("Profiles"),
          rules: () => t("Rules"),
          check: () => t("Who gets what"),
          searching: () => t("Searching"),
          blocklist: () => t("Blocklist"),
          pinned: () => t("Pinned"),
          history: () => t("History"),
        },
        "profiles",
      );
      return label ? [label] : [];
    }
    case "cache": {
      const label = resolveTab(tab, {
        explorer: () => t("Explorer"),
        library: () => t("Library"),
        preloading: () => t("Preloading"),
        libraries: () => t("Libraries"),
      });
      return label ? [label] : [];
    }
    case "playback": {
      const tabLabel = resolveTab(
        tab,
        {
          audio: () => t("Audio"),
          subtitles: () => t("Subtitles"),
          transcoding: () => t("Streams & transcoding"),
          devices: () => t("App compatibility"),
          extras: () => t("Extras"),
          prerolls: () => t("Pre-rolls"),
        },
        "audio",
      );
      const parts = tabLabel ? [tabLabel] : [];
      if (tab === "devices") {
        const sub = resolveTab(section, {
          apps: () => t("Apps"),
          devices: () => t("Device profiles"),
          seen: () => t("Seen devices"),
        });
        if (sub) parts.push(sub);
      }
      return parts;
    }
    case "notifications": {
      const label = resolveTab(
        tab,
        {
          channels: () => t("Channels"),
          report: () => t("Weekly report"),
          deliveries: () => t("Delivery history"),
        },
        "channels",
      );
      return label ? [label] : [];
    }
    case "flows": {
      if (tab === "list") return [];
      if (tab === "templates") return [t("Templates")];
      if (tab === "runs") return [t("Runs")];
      return [];
    }
    case "apps": {
      if (!tab || tab === "overview") return [];
      const appName = resolveTab(tab, {
        vela: () => "Vela",
        plex: () => "Plex",
        jellyfin: () => "Jellyfin",
        emby: () => "Emby",
        stremio: () => "Stremio",
        silo: () => "Silo",
        dlna: () => "DLNA",
        files: () => t("Files (WebDAV)"),
        m3u: () => t("M3U & XMLTV"),
        xtream: () => t("IPTV apps (Xtream)"),
      });
      const parts = appName ? [appName] : [];
      if (tab === "plex") {
        const sub = resolveTab(section, {
          account: () => t("Account"),
          remote: () => t("Remote access"),
          downloads: () => t("Downloads"),
        });
        if (sub) parts.push(sub);
      }
      return parts;
    }
    case "integrations": {
      if (!tab || tab === "overview") return [];
      const serviceLabel = resolveTab(tab, {
        postersplus: () => "PostersPlus",
        xrdb: () => "XRDB",
        betterposters: () => "BetterPosters",
        rpdb: () => "RPDB",
        custom: () => t("Custom posters"),
        tmdb: () => "TMDB",
        trakt: () => "Trakt",
        simkl: () => "Simkl",
        mdblist: () => "MDBList",
        plexservers: () => t("Plex Media Server Sync"),
        llm: () => t("Language model"),
        opensubtitles: () => "OpenSubtitles",
        subdl: () => "SubDL",
        subsource: () => "SubSource",
        ratings: () => t("Ratings"),
        introdb: () => "TheIntroDB",
      });
      return serviceLabel ? [serviceLabel] : [];
    }
    case "cluster": {
      const qTab = query.get("tab");
      if (qTab === "nodes") return [t("Nodes")];
      if (qTab === "recovery") return [t("Recovery history")];
      if (qTab === "tokens") return [t("Join tokens")];
      return [];
    }
    case "maintenance": {
      if (tab === "updates") return [t("Updates")];
      if (tab === "health") return [t("Health")];
      if (tab === "logs") return [t("Logs")];
      const parts = [t("Backups & import")];
      if (section === "import") parts.push(t("Import & export"));
      else if (section === "backups") parts.push(t("Backups"));
      return parts;
    }
    default:
      return resolveSectionFallback(rest, sections);
  }
}

function descriptorLabel(
  descriptors: readonly Descriptor<PageMeta>[],
  id: string,
  fallback: string,
): string {
  const match = descriptors.find((d) => d.kind === "page" && d.owner === BUILTIN && d.id === id);
  return (match && textOf(match.label)) || fallback;
}

/**
 * Derive the console tab title structurally from the active URL and page
 * descriptors so every built-in page, sub-tab, plugin page, setup/portal
 * screen, and 404 has a translated title without per-page boilerplate.
 */
export function resolveRouteTitle(
  pathname: string,
  search: string,
  descriptors: readonly Descriptor<PageMeta>[],
  options: ResolveRouteTitleOptions = {},
): RouteDocumentTitle {
  const path = normalizePath(pathname);
  const query = new URLSearchParams(search);

  if (path === "/me") return { matched: true, parts: [t("Approve a TV")] };
  if (path === "/setup") return { matched: true, parts: [t("Setup")] };
  if (path === "/mcp/authorize") return { matched: true, parts: [t("Connect an AI app")] };

  if (path === "/plugins/reference" || path.startsWith("/plugins/reference/")) {
    return {
      matched: true,
      parts: [descriptorLabel(descriptors, "plugins", t("Plugins")), t("Developer reference")],
    };
  }
  if (path === "/plugins/studio") {
    return {
      matched: true,
      parts: [descriptorLabel(descriptors, "plugins", t("Plugins")), t("Studio")],
    };
  }
  if (path === "/plugins" || path.startsWith("/plugins/")) {
    const rest =
      path === "/plugins" ? [] : path.slice("/plugins/".length).split("/").map(safeDecode);
    const subtab = resolveTab(
      rest[0],
      {
        installed: () => t("Installed"),
        marketplace: () => t("Marketplace"),
        settings: () => t("Settings"),
      },
      "installed",
    );
    return {
      matched: true,
      parts: [descriptorLabel(descriptors, "plugins", t("Plugins")), ...(subtab ? [subtab] : [])],
    };
  }

  if (path.startsWith("/p/")) {
    const segments = path.slice(3).split("/").map(safeDecode);
    const plugin = segments[0];
    const page = segments[1];
    if (plugin && page) {
      const fullPage = segments.slice(1).join("/");
      const match = descriptors.find(
        (d) =>
          d.kind === "page" &&
          d.owner === plugin &&
          (pluginLocalId(d) === fullPage || pluginLocalId(d) === page),
      );
      const label = (match && textOf(match.label)) || page;
      return { matched: true, parts: [label] };
    }
  }

  if (path === "/catalog") {
    return { matched: true, parts: [descriptorLabel(descriptors, "libraries", t("Libraries"))] };
  }

  const pageDescriptors = descriptors.filter((d) => {
    if (d.kind !== "page" || d.owner !== BUILTIN || !d.meta?.route) return false;
    return options.mountedPageIds ? options.mountedPageIds.has(d.id) : Boolean(d.meta.inNav);
  });

  let matched: Descriptor<PageMeta> | null = null;
  let matchedLen = -1;
  let matchedRest: string[] = [];

  for (const d of pageDescriptors) {
    const base = normalizePath(d.meta!.route!);
    if (base === "/") {
      if (path === "/" && matchedLen < 1) {
        matched = d;
        matchedLen = 1;
        matchedRest = [];
      }
      continue;
    }
    if (path === base || path.startsWith(`${base}/`)) {
      if (base.length > matchedLen) {
        matched = d;
        matchedLen = base.length;
        matchedRest =
          path === base
            ? []
            : path
                .slice(base.length + 1)
                .split("/")
                .map(safeDecode);
      }
    }
  }

  if (matched) {
    const label = textOf(matched.label) || matched.id;
    const subParts = resolveBuiltinSubparts(matched.id, matchedRest, query, matched.meta?.sections);
    return { matched: true, parts: [label, ...subParts] };
  }

  if (options.registryLoading) {
    if (descriptors.length === 0 && path === "/recommendations") {
      return { matched: true, parts: [t("Recommendations")] };
    }
    return { matched: false, parts: [] };
  }

  return { matched: false, parts: [t("Not found")] };
}

interface DocumentTitleContextValue {
  setBrand: (id: string, brand: string) => void;
  removeBrand: (id: string) => void;
  setRouteTitle: (id: string, title: RouteDocumentTitle) => void;
  removeRouteTitle: (id: string) => void;
  setPageOverride: (id: string, parts: readonly string[]) => void;
  removePageOverride: (id: string) => void;
  setFallback: (id: string, parts: readonly string[]) => void;
  removeFallback: (id: string) => void;
  setDrawer: (id: string, title: string) => void;
  removeDrawer: (id: string) => void;
}

const DocumentTitleContext = createContext<DocumentTitleContextValue | null>(null);

function sameParts(a: readonly string[] | undefined, b: readonly string[]): boolean {
  if (!a || a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return false;
  }
  return true;
}

function serializeParts(parts: readonly TitlePart[] | null | undefined): string | null {
  if (!parts) return null;
  return parts
    .map((p) => p?.trim() ?? "")
    .filter(Boolean)
    .join("\0");
}

function lastValue<V>(map: Map<string, V>): V | undefined {
  let out: V | undefined;
  for (const v of map.values()) out = v;
  return out;
}

/**
 * Single owner of `document.title` for the console. Combines server brand,
 * route-derived titles, dynamic entity refinements, layout fallbacks for any
 * unregistered page, and URL-backed drawer titles without re-rendering the
 * React tree when the title changes.
 */
export function DocumentTitleProvider({ children }: { children: ReactNode }) {
  const storeRef = useRef({
    brands: new Map<string, string>(),
    routes: new Map<string, RouteDocumentTitle>(),
    overrides: new Map<string, readonly string[]>(),
    fallbacks: new Map<string, readonly string[]>(),
    drawers: new Map<string, string>(),
  });

  const value = useMemo<DocumentTitleContextValue>(() => {
    const sync = () => {
      if (typeof document === "undefined") return;
      const store = storeRef.current;
      const brand = lastValue(store.brands);
      const route = lastValue(store.routes);
      const override = lastValue(store.overrides);
      const fallback = lastValue(store.fallbacks);
      const baseParts =
        override ?? (route?.matched ? route.parts : (fallback ?? route?.parts ?? []));
      const drawerParts = Array.from(store.drawers.values());
      const nextTitle = formatDocumentTitle([...baseParts, ...drawerParts], brand);
      if (document.title !== nextTitle) {
        document.title = nextTitle;
      }
    };

    return {
      setBrand(id, brand) {
        if (storeRef.current.brands.get(id) === brand) return;
        storeRef.current.brands.set(id, brand);
        sync();
      },
      removeBrand(id) {
        if (!storeRef.current.brands.delete(id)) return;
        sync();
      },
      setRouteTitle(id, title) {
        const existing = storeRef.current.routes.get(id);
        if (
          existing &&
          existing.matched === title.matched &&
          sameParts(existing.parts, title.parts)
        ) {
          return;
        }
        storeRef.current.routes.set(id, title);
        sync();
      },
      removeRouteTitle(id) {
        if (!storeRef.current.routes.delete(id)) return;
        sync();
      },
      setPageOverride(id, parts) {
        if (sameParts(storeRef.current.overrides.get(id), parts)) return;
        storeRef.current.overrides.set(id, parts);
        sync();
      },
      removePageOverride(id) {
        if (!storeRef.current.overrides.delete(id)) return;
        sync();
      },
      setFallback(id, parts) {
        if (sameParts(storeRef.current.fallbacks.get(id), parts)) return;
        storeRef.current.fallbacks.set(id, parts);
        sync();
      },
      removeFallback(id) {
        if (!storeRef.current.fallbacks.delete(id)) return;
        sync();
      },
      setDrawer(id, title) {
        if (storeRef.current.drawers.get(id) === title) return;
        storeRef.current.drawers.set(id, title);
        sync();
      },
      removeDrawer(id) {
        if (!storeRef.current.drawers.delete(id)) return;
        sync();
      },
    };
    // i18n: memo holds no copy
  }, []);

  return <DocumentTitleContext.Provider value={value}>{children}</DocumentTitleContext.Provider>;
}

/** Register the server name used as the trailing brand (`— <server>`). */
export function useDocumentBrand(brand: string | null | undefined): void {
  const ctx = useContext(DocumentTitleContext);
  const id = useId();
  const normalized = brand?.trim() || null;
  useEffect(() => {
    if (!ctx) return;
    if (!normalized) {
      ctx.removeBrand(id);
      return;
    }
    ctx.setBrand(id, normalized);
    return () => ctx.removeBrand(id);
  }, [ctx, id, normalized]);
}

/** Register the router-level base title for the active console screen. */
export function useRouteDocumentTitle(routeTitle: RouteDocumentTitle | null): void {
  const ctx = useContext(DocumentTitleContext);
  const id = useId();
  const matched = routeTitle?.matched ?? false;
  const key = routeTitle ? serializeParts(routeTitle.parts) : null;
  useEffect(() => {
    if (!ctx) return;
    if (key === null) {
      ctx.removeRouteTitle(id);
      return;
    }
    ctx.setRouteTitle(id, { matched, parts: key ? key.split("\0") : [] });
    return () => ctx.removeRouteTitle(id);
  }, [ctx, id, matched, key]);
}

/**
 * Refine the active route's browser tab title once dynamic data loads
 * (for example a library's name, a viewer's name, a plugin document title,
 * or the active setup step). Pass `null` to defer to the route title.
 */
export function usePageTitle(parts: readonly TitlePart[] | null | undefined): void {
  const ctx = useContext(DocumentTitleContext);
  const id = useId();
  const key = serializeParts(parts);
  useEffect(() => {
    if (!ctx) return;
    if (key === null) {
      ctx.removePageOverride(id);
      return;
    }
    ctx.setPageOverride(id, key ? key.split("\0") : []);
    return () => ctx.removePageOverride(id);
  }, [ctx, id, key]);
}

/**
 * Fallback title contributed by shared page layouts (`DomainPage`, `Workspace`).
 * Only applies when the current route is not matched by `resolveRouteTitle` and
 * no explicit `usePageTitle` is active, so new pages cannot forget a title.
 */
export function useFallbackDocumentTitle(parts: readonly TitlePart[] | null | undefined): void {
  const ctx = useContext(DocumentTitleContext);
  const id = useId();
  const key = serializeParts(parts);
  useEffect(() => {
    if (!ctx) return;
    if (key === null || key === "") {
      ctx.removeFallback(id);
      return;
    }
    ctx.setFallback(id, key.split("\0"));
    return () => ctx.removeFallback(id);
  }, [ctx, id, key]);
}

/** Append an open URL-backed drawer's title onto the current page title. */
export function useDrawerTitle(title: string | null | undefined): void {
  const ctx = useContext(DocumentTitleContext);
  const id = useId();
  const normalized = title?.trim() || null;
  useEffect(() => {
    if (!ctx) return;
    if (!normalized) {
      ctx.removeDrawer(id);
      return;
    }
    ctx.setDrawer(id, normalized);
    return () => ctx.removeDrawer(id);
  }, [ctx, id, normalized]);
}

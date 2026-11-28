import { McpAuthorize } from "./components/agent/McpAuthorize";
import {
  AppShell,
  Button,
  ConfirmRoot,
  DrawerTextProvider,
  PromptRoot,
  Spinner,
  ThemeProvider,
  ThemeToggle,
  Toaster,
  UiLinkProvider,
  type LinkRender,
  type SidebarNavGroup,
} from "@braedonsaunders/appkit-ui";
import {
  Activity as ActivityIcon,
  HardDrive,
  Library,
  Boxes,
  Globe,
  LifeBuoy,
  Plug,
  Puzzle,
  Server,
  Bell,
  CalendarDays,
  Gauge,
  LayoutTemplate,
  Layers,
  ListFilter,
  LogOut,
  UserRound,
  MonitorPlay,
  Network,
  Tags,
  Tv,
  Users,
  Workflow,
} from "lucide-react";
import {
  forwardRef,
  lazy,
  Suspense,
  useCallback,
  useEffect,
  useRef,
  useState,
  type AnchorHTMLAttributes,
  type ReactNode,
  type RefObject,
} from "react";
import {
  Link,
  Navigate,
  Route,
  Routes,
  useLocation,
  useNavigate,
  useOutlet,
  useParams,
  useOutletContext,
  useSearchParams,
} from "react-router-dom";
import { AppProvider, useApp } from "./app-context";
import { useMediaQuery } from "./lib/hooks";
import { t } from "./i18n";
import { I18nProvider, LanguagePicker } from "./i18n/react";
import { Brand, BrandSplash, EtherMark } from "./components/brand";
import { CommandSearch } from "./components/CommandSearch";
import { WatchButton } from "./components/WatchButton";
import { AskEther } from "./components/agent/AskEther";
import { Drawer } from "./components/Drawer";
import { ErrorPanel } from "./components/feedback";
import { SaveBarHost } from "./components/SaveBar";
import { SettingFocus } from "./components/SettingFocus";
import { AutomaticWhatsNew } from "./components/UpdateNotesDrawer";
import { UpdateReady } from "./components/UpdateReady";
import { HealthPill, useServerVerdict } from "./components/HealthPill";
import { useOwnerViewerPath } from "./components/pickers";
import { api, getToken, setToken } from "./lib/api";
import { BUILTIN, textOf, useRegistry, type Descriptor } from "./lib/extend";
import {
  SYSTEM_NAV_GROUP,
  pluginNavGroup,
  pluginPageRoute,
  type NavItemMeta,
  type PageMeta,
} from "./lib/console-descriptors";
import {
  DocumentTitleProvider,
  resolveRouteTitle,
  useDocumentBrand,
  useRouteDocumentTitle,
  type RouteDocumentTitle,
} from "./lib/document-title";
import type { Bootstrap } from "./lib/types";
import { theme } from "./theme";
import Home from "./pages/Home";
import Login from "./pages/Login";
import Startup, { isStarting } from "./pages/Startup";
import { studioBackLink } from "./pages/collections/links";

const loadRecommendations = () => import("./pages/recommendations/Recommendations");
const CollectionsGallery = lazy(() => import("./pages/collections/Gallery"));
const CollectionEditor = lazy(() => import("./pages/collections/Editor"));
const Recommendations = lazy(loadRecommendations);
const ViewersList = lazy(() => import("./pages/viewers/ViewersList"));
const ViewerProfile = lazy(() => import("./pages/viewers/ViewerProfile"));
const settingsPage = <K extends keyof typeof import("./pages/settings/pages")>(name: K) =>
  lazy(() => import("./pages/settings/pages").then((m) => ({ default: m[name] })));
const GeneralPage = settingsPage("GeneralPage");
const Apps = lazy(() => import("./pages/apps/Apps"));
const ServicesPage = settingsPage("ServicesPage");
const MaintenancePage = settingsPage("MaintenancePage");
const ClusterPage = settingsPage("ClusterPage");
const Setup = lazy(() => import("./pages/Setup"));
const Activity = lazy(() => import("./pages/activity/Activity"));
const Notifications = lazy(() => import("./pages/notifications/Notifications"));
const Flows = lazy(() => import("./pages/flows/Flows"));
const LiveTV = lazy(() => import("./pages/livetv/LiveTV"));
const Calendar = lazy(() => import("./pages/calendar/Calendar"));
const Libraries = lazy(() => import("./pages/libraries/Libraries"));
const Metadata = lazy(() => import("./pages/metadata/Metadata"));
const Sources = lazy(() => import("./pages/sources/Sources"));
const Releases = lazy(() => import("./pages/releases/Releases"));
const Cache = lazy(() => import("./pages/cache/Cache"));
const Playback = lazy(() => import("./pages/playback/Playback"));
const DeveloperReference = lazy(() => import("./pages/plugins/DeveloperReference"));
const Plugins = lazy(() => import("./pages/plugins/Plugins"));
const Studio = lazy(() => import("./pages/plugins/Studio"));
const PluginPage = lazy(() => import("./viewspec/PluginPage"));

/** react-router links for every AppKit navigation primitive. */
const RouterAnchor = forwardRef<
  HTMLAnchorElement,
  AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }
>(function RouterAnchor({ href, ...rest }, ref) {
  if (/^(https?:)?\/\//.test(href)) return <a ref={ref} href={href} {...rest} />;
  return <Link ref={ref} to={href} {...rest} />;
});

const linkRender: LinkRender = ({ href, children, className, title, ariaCurrent, role }) => (
  <Link to={href} className={className} title={title} aria-current={ariaCurrent} role={role}>
    {children}
  </Link>
);

/** Lucide icons by the name page descriptors carry. */
const NAV_ICONS = {
  Gauge,
  Activity: ActivityIcon,
  Users,
  Library,
  Layers,
  LayoutTemplate,
  Tv,
  CalendarDays,
  Tags,
  Network,
  ListFilter,
  HardDrive,
  MonitorPlay,
  Server,
  Globe,
  Plug,
  Puzzle,
  Bell,
  Boxes,
  LifeBuoy,
  Workflow,
} as const;

function navIcon(icon: string | undefined): ReactNode {
  const size = 18;
  if (icon && /^(https?:)?\/\//.test(icon)) {
    return <img src={icon} alt="" width={size} height={size} className="rounded-sm" />;
  }
  const Icon = (icon && NAV_ICONS[icon as keyof typeof NAV_ICONS]) || Puzzle;
  return <Icon size={size} />;
}

/** Sidebar groups in display order; the top group has no heading. */
const NAV_GROUPS: { key: string; label: () => string }[] = [
  { key: "", label: () => "" },
  { key: "library", label: () => t("Content") },
  { key: "streaming", label: () => t("Streaming") },
  { key: "connect", label: () => t("Connect") },
  { key: "automate", label: () => t("Automate") },
  { key: SYSTEM_NAV_GROUP, label: () => t("System") },
];

/**
 * The sidebar, from registry descriptors (WS1d). Built-in navItems come from
 * Go; a plugin page joins its declared nav group (or System) with the
 * plugin's icon, after the built-ins of the same order.
 * Grouped by the owner's questions: what's happening and who watches; what
 * viewers see in their apps; how titles reach the screen; what Ether talks
 * to; what it does on its own; how the server runs.
 */
export function buildNavigation(descriptors: Descriptor[]): SidebarNavGroup[] {
  type Item = {
    group: string;
    order: number;
    builtin: boolean;
    item: SidebarNavGroup["items"][number];
  };
  const items: Item[] = [];
  const navPages = new Set<string>();
  const pageById = new Map<string, Descriptor<PageMeta>>();
  for (const d of descriptors) {
    if (d.kind === "page") {
      pageById.set(d.id, d as Descriptor<PageMeta>);
    }
  }
  for (const d of descriptors) {
    // Plugins do not add sidebar entries (yet); their pages open from the
    // plugin's drawer and from the surfaces they extend.
    if (d.kind !== "navItem" || d.owner !== BUILTIN) continue;
    const meta = d.meta as NavItemMeta | undefined;
    if (!meta?.route) continue;
    const pageId = meta.page ?? (meta.pageKey ? `plugin:${d.owner}/${meta.pageKey}` : undefined);
    if (pageId) navPages.add(pageId);
    navPages.add(d.id);
    const page = pageId ? pageById.get(pageId) : undefined;
    const pageMeta = page?.meta;
    const group =
      d.owner === BUILTIN
        ? (d.group ?? "")
        : ((page ? pluginNavGroup(page) : undefined) ?? SYSTEM_NAV_GROUP);
    const href =
      d.owner !== BUILTIN && meta.pageKey ? pluginPageRoute(d.owner, meta.pageKey) : meta.route;
    items.push({
      group,
      order: d.order ?? page?.order ?? 0,
      builtin: d.owner === BUILTIN,
      item: {
        href,
        label: textOf(d.label),
        icon: navIcon(d.icon ?? page?.icon ?? pageMeta?.icon),
        exact: meta.exact || undefined,
        mobile: meta.mobile || undefined,
      },
    });
  }
  items.sort((a, b) => a.order - b.order || Number(b.builtin) - Number(a.builtin));
  const groups: SidebarNavGroup[] = NAV_GROUPS.map((g) => ({
    label: g.label(),
    items: items.filter((i) => i.group === g.key).map((i) => i.item),
  }));
  // A plugin may declare a group the console does not know; keep it visible
  // under System rather than dropping the page.
  const known = new Set(NAV_GROUPS.map((g) => g.key));
  const stray = items.filter((i) => !known.has(i.group)).map((i) => i.item);
  if (stray.length) groups[groups.length - 1]!.items.push(...stray);
  return groups.filter((g) => g.items.length > 0);
}

/** AppShell owns the mobile menu state internally; its header close button dismisses it. */
function closeMobileDrawer(el: HTMLElement | null) {
  el?.closest('[role="dialog"]')?.querySelector<HTMLButtonElement>("header button")?.click();
}

/** The server's health pill: the same one the Overview shows, on every page. */
function HealthIndicator({ mobile = false }: { mobile?: boolean } = {}) {
  const { health } = useApp();
  const verdict = useServerVerdict();
  const ref = useRef<HTMLSpanElement>(null);
  return (
    <span
      ref={ref}
      className={
        mobile ? "inline-flex h-9 items-center px-2" : "inline-flex h-9 items-center px-1.5"
      }
    >
      <HealthPill
        items={health}
        verdict={verdict}
        label={(status) => t("Server health: {status}", { status })}
        onNavigate={mobile ? () => closeMobileDrawer(ref.current) : undefined}
      />
    </span>
  );
}

/** "v1.2.3" for releases, the raw string ("dev") otherwise. */
/**
 * AppShell 0.4.2 leaves its sidebar toggle named in English: it does not pass
 * AppSidebar's expandLabel / collapseLabel through. Until AppKit does, the
 * button is named here, again each time the shell re-renders it.
 */
function useSidebarToggleName(shell: RefObject<HTMLElement | null>) {
  const collapse = t("Collapse sidebar");
  const expand = t("Expand sidebar");
  useEffect(() => {
    const root = shell.current;
    if (!root || typeof MutationObserver === "undefined") return;
    const name = () => {
      for (const el of root.querySelectorAll<HTMLElement>("aside button[aria-label]")) {
        const label = el.getAttribute("aria-label");
        if (label === "Collapse sidebar") el.dataset.sidebarToggle = "collapse";
        else if (label === "Expand sidebar") el.dataset.sidebarToggle = "expand";
        const kind = el.dataset.sidebarToggle;
        if (!kind) continue;
        const translated = kind === "collapse" ? collapse : expand;
        if (label !== translated) el.setAttribute("aria-label", translated);
        if (el.title !== translated) el.title = translated;
      }
    };
    name();
    const observer = new MutationObserver(name);
    observer.observe(root, {
      subtree: true,
      childList: true,
      attributes: true,
      attributeFilter: ["aria-label"],
    });
    return () => observer.disconnect();
  }, [shell, collapse, expand]);
}

function versionLabel(v: string): string {
  return /^\d/.test(v) ? `v${v}` : v;
}

/** Keep the library mounted behind every collection drawer, including lazy editor loads. */
function CollectionsLayout() {
  const location = useLocation();
  const navigate = useNavigate();
  const editing = location.pathname.replace(/\/$/, "") !== "/collections";
  const browseSearch = useRef(editing ? "" : location.search);
  if (!editing) browseSearch.current = location.search;
  const outlet = useOutlet({ close: () => navigate(`/collections${browseSearch.current}`) });
  return (
    <>
      <div
        aria-hidden={editing ? true : undefined}
        inert={editing}
        className="flex min-h-0 flex-1 flex-col"
      >
        <CollectionsGallery
          browseSearch={editing ? browseSearch.current : undefined}
          active={!editing}
        />
      </div>
      {outlet}
    </>
  );
}

/** Collection Studio opens over the persistent library as a record drawer. */
function CollectionDrawer({ fresh = false }: { fresh?: boolean }) {
  const { close } = useOutletContext<{ close: () => void }>();
  const { id } = useParams();
  const [search] = useSearchParams();
  const navigate = useNavigate();
  const back = studioBackLink(search);
  return (
    <Drawer
      open
      onClose={back ? () => navigate(back) : close}
      title={fresh ? t("New collection") : t("Edit collection")}
      size="2xl"
      bodyClassName="flex min-h-0 flex-1 flex-col overflow-hidden p-0"
    >
      <CollectionEditor key={fresh ? (search.get("draft") ?? "new") : `edit:${id ?? ""}`} />
    </Drawer>
  );
}

function PageFallback() {
  return (
    <div className="grid flex-1 place-items-center p-10">
      <Spinner size={22} label={t("Loading")} />
    </div>
  );
}

// Compiled components and local parameters; base paths come from page descriptors.
const BUILTIN_PAGES: Record<
  string,
  {
    element: ReactNode;
    suffix?: string;
    children?: ReactNode;
    extra?: { suffix: string; element: ReactNode }[];
  }
> = {
  overview: { element: <Home /> },
  activity: { element: <Activity />, suffix: "/:tab?" },
  viewers: {
    element: <ViewersList />,
    extra: [
      { suffix: "/watchlists", element: <ViewersList tab="watchlists" /> },
      { suffix: "/security", element: <ViewersList tab="security" /> },
      { suffix: "/:id/:tab?", element: <ViewerById /> },
    ],
  },
  recommendations: { element: <Recommendations /> },
  collections: {
    element: <CollectionsLayout />,
    children: (
      <>
        <Route path="new" element={<CollectionDrawer fresh />} />
        <Route path=":id" element={<CollectionDrawer />} />
      </>
    ),
  },
  livetv: { element: <LiveTV />, suffix: "/:tab?" },
  calendar: { element: <Calendar />, suffix: "/:view?" },
  libraries: { element: <Libraries />, suffix: "/*" },
  metadata: { element: <Metadata />, suffix: "/:tab?" },
  sources: { element: <Sources />, suffix: "/:tab?/:section?" },
  releases: { element: <Releases />, suffix: "/:tab?" },
  cache: { element: <Cache />, suffix: "/:tab?" },
  playback: { element: <Playback />, suffix: "/:tab?/:section?" },
  notifications: { element: <Notifications />, suffix: "/:tab?" },
  flows: { element: <Flows />, suffix: "/*" },
  general: { element: <GeneralPage /> },
  apps: { element: <Apps />, suffix: "/:tab?/:section?" },
  integrations: { element: <ServicesPage />, suffix: "/:tab?" },
  cluster: { element: <ClusterPage /> },
  maintenance: { element: <MaintenancePage />, suffix: "/:tab?/:backupTab?" },
};

const BUILTIN_PAGE_IDS = new Set(Object.keys(BUILTIN_PAGES));

/** Old catalogue bookmarks land in the matching library, query intact (one release). */
function CatalogRedirect() {
  const location = useLocation();
  const params = new URLSearchParams(location.search);
  const id = params.get("type") === "show" ? 2 : 1;
  return <Navigate to={`/libraries/${id}${location.search}`} replace />;
}

function Shell() {
  const { boot } = useApp();
  const ownerPath = useOwnerViewerPath();
  const location = useLocation();
  const isDesktop = useMediaQuery("(min-width: 1024px)");
  const wideSearch = useMediaQuery("(min-width: 640px)");
  const firstRunSetupRef = useRef(false);
  const shellRef = useRef<HTMLDivElement>(null);
  useSidebarToggleName(shellRef);
  if (location.pathname !== "/setup") {
    firstRunSetupRef.current = false;
  } else if (!boot.setupComplete) {
    firstRunSetupRef.current = true;
  }
  const hideSetupNav = location.pathname === "/setup" && firstRunSetupRef.current;
  const {
    descriptors: navDescriptors,
    loading: registryLoading,
    error: registryError,
    reload: reloadRegistry,
  } = useRegistry<PageMeta>("page", "navItem", "command");
  const remoteSession = boot.authRequired && Boolean(getToken());
  const version = versionLabel(boot.version);
  const serverName = boot.serverName?.trim() || "Ether";
  useDocumentBrand(serverName);
  useRouteDocumentTitle(
    resolveRouteTitle(location.pathname, location.search, navDescriptors, {
      registryLoading,
      mountedPageIds: BUILTIN_PAGE_IDS,
    }),
  );
  const signOut = () => {
    // End the session on the server too, so a copy cannot outlive
    // sign-out; leave the browser either way.
    void api
      .signOut()
      .catch(() => undefined)
      .finally(() => {
        setToken("");
        window.location.reload();
      });
  };
  const themeLabels = {
    label: t("Theme"),
    light: t("Light"),
    dark: t("Dark"),
    system: t("System"),
    current: (mode: string) =>
      t("Theme: {mode}", {
        // AppKit passes the already-translated option label.
        mode:
          ({ light: t("Light"), dark: t("Dark"), system: t("System") } as Record<string, string>)[
            mode
          ] ?? mode,
      }),
  };
  return (
    <div
      ref={shellRef}
      className={hideSetupNav ? "ether-shell ether-setup contents" : "ether-shell contents"}
    >
      <AppShell
        groups={hideSetupNav ? [] : buildNavigation(navDescriptors)}
        pathname={location.pathname}
        navigationMode="sidebar"
        linkRender={linkRender}
        brand={
          <Link
            to="/"
            aria-label={t("Ether home")}
            className="block rounded-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            <Brand />
          </Link>
        }
        headerMiddle={
          wideSearch ? (
            <CommandSearch />
          ) : hideSetupNav ? (
            <span
              className="inline-flex min-w-0 max-w-full items-center gap-2 py-1 text-sm font-medium text-fg"
              title={serverName}
            >
              <EtherMark size={22} />
              <span className="truncate">{serverName}</span>
            </span>
          ) : (
            <Link
              to="/"
              className="inline-flex min-w-0 max-w-full items-center gap-2 rounded-md py-1 text-sm font-medium text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
              title={serverName}
            >
              <EtherMark size={22} />
              <span className="truncate">{serverName}</span>
            </Link>
          )
        }
        header={
          isDesktop ? (
            <>
              <span
                className="inline-flex h-9 min-w-0 max-w-[9rem] items-center gap-1.5 rounded-md px-2.5 text-sm text-fg-muted sm:max-w-[14rem]"
                title={serverName}
              >
                <Server size={15} className="shrink-0" aria-hidden />
                <span className="hidden truncate sm:inline">{serverName}</span>
              </span>
              <HealthIndicator />
              <WatchButton />
              <AskEther />
              <ThemeToggle collapsed labels={themeLabels} />
              <Link
                to={ownerPath}
                aria-label={t("Your account")}
                className="inline-flex h-9 items-center gap-2 rounded-md px-2.5 text-sm text-fg-muted transition-colors hover:bg-surface-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
              >
                <UserRound size={15} />
                <span className="hidden sm:inline">{t("Your account")}</span>
              </Link>
              {remoteSession ? (
                <Button variant="ghost" size="sm" onClick={signOut}>
                  <LogOut size={15} /> <span className="hidden sm:inline">{t("Sign out")}</span>
                </Button>
              ) : null}
            </>
          ) : (
            <>
              {wideSearch ? (
                <span
                  className="inline-flex h-9 min-w-0 max-w-[14rem] items-center gap-1.5 rounded-md px-2.5 text-sm text-fg-muted"
                  title={serverName}
                >
                  <Server size={15} className="shrink-0" aria-hidden />
                  <span className="truncate">{serverName}</span>
                </span>
              ) : (
                <CommandSearch compact />
              )}
              <AskEther />
            </>
          )
        }
        mobileFooter={
          isDesktop ? undefined : (
            <div className="flex w-full flex-col gap-2 pb-[env(safe-area-inset-bottom)]">
              <div className="-mx-2 flex flex-wrap items-center justify-between gap-1">
                <HealthIndicator mobile />
                <WatchButton popoverSide="top" />
              </div>
              <div className="-mx-2 flex flex-wrap items-center justify-between gap-1 border-t border-border-subtle pt-2">
                <Link
                  to={ownerPath}
                  aria-label={t("Your account")}
                  onClick={(e) => closeMobileDrawer(e.currentTarget)}
                  className="inline-flex h-9 items-center gap-2 rounded-md px-2 text-sm text-fg-muted transition-colors hover:bg-surface-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                >
                  <UserRound size={15} />
                  <span>{t("Your account")}</span>
                </Link>
                {remoteSession ? (
                  <Button variant="ghost" size="sm" onClick={signOut}>
                    <LogOut size={15} /> <span>{t("Sign out")}</span>
                  </Button>
                ) : null}
              </div>
              <div className="flex items-center justify-between gap-2 border-t border-border-subtle pt-2">
                <p className="min-w-0 truncate text-xs text-fg-subtle" title={version}>
                  {version}
                </p>
                <div className="-mr-1 flex shrink-0 items-center gap-1">
                  <LanguagePicker className="h-8 shrink-0 px-2 text-xs" />
                  <ThemeToggle collapsed labels={themeLabels} />
                </div>
              </div>
            </div>
          )
        }
        sidebarFooter={
          <div className="flex min-w-0 items-center justify-between gap-2">
            <p className="min-w-0 truncate px-1 text-xs text-fg-subtle" title={version}>
              {version}
            </p>
            <LanguagePicker className="h-7 shrink-0 px-1.5 text-xs" />
          </div>
        }
        sidebarCollapsedFooter={
          <div className="flex flex-col items-center gap-2">
            <LanguagePicker iconOnly className="h-7 px-1.5" />
            <EtherMark size={20} />
            <p
              className="max-w-full truncate text-center text-[10px] leading-tight text-fg-subtle"
              title={version}
            >
              {version}
            </p>
          </div>
        }
        moreLabel={t("More")}
        menuLabel={t("Menu")}
        openNavigationLabel={t("Open navigation")}
        primaryNavigationLabel={t("Primary navigation")}
        mobileTabCount={4}
      >
        <SaveBarHost>
          <SettingFocus />
          <Suspense fallback={<PageFallback />}>
            <Routes>
              <Route path="/setup" element={<Setup />} />
              <Route path="/mcp/authorize" element={<McpAuthorize />} />
              <Route path="/plugins/reference/:topic?" element={<DeveloperReference />} />
              <Route path="/plugins/studio" element={<Studio />} />
              <Route path="/plugins/:tab?/*" element={<Plugins />} />
              <Route path="/p/:plugin/:page/*" element={<PluginPage />} />
              {registryLoading && navDescriptors.length === 0 ? (
                <Route
                  key="recommendations"
                  path="/recommendations"
                  element={BUILTIN_PAGES.recommendations!.element}
                />
              ) : null}
              {navDescriptors
                .filter((d) => d.kind === "page" && d.owner === BUILTIN && d.meta?.route)
                .flatMap((d) => {
                  const page = BUILTIN_PAGES[d.id];
                  if (!page) return [];
                  const base = d.meta!.route!;
                  const element =
                    d.id === "overview" && !(boot.setupComplete || boot.ownerConfigured) ? (
                      <Navigate to="/setup" replace />
                    ) : (
                      page.element
                    );
                  return [
                    <Route
                      key={d.id}
                      path={`${base === "/" ? "" : base}${page.suffix ?? ""}` || "/"}
                      element={element}
                    >
                      {page.children}
                    </Route>,
                    ...(page.extra ?? []).map(({ suffix, element }) => (
                      <Route
                        key={`${d.id}:${suffix}`}
                        path={`${base}${suffix}`}
                        element={element}
                      />
                    )),
                  ];
                })}
              <Route path="/catalog" element={<CatalogRedirect />} />
              <Route
                path="*"
                element={
                  registryLoading ? (
                    <PageFallback />
                  ) : registryError ? (
                    <ErrorPanel error={registryError} onRetry={reloadRegistry} />
                  ) : (
                    <Navigate to="/" replace />
                  )
                }
              />
            </Routes>
          </Suspense>
        </SaveBarHost>
      </AppShell>
    </div>
  );
}

/** Viewer profiles are numeric ids. Anything else is the people list. */
function ViewerById() {
  const { id } = useParams();
  if (!id || !/^[1-9]\d*$/.test(id)) return <Navigate to="/viewers" replace />;
  return <ViewerProfile />;
}

function Gate({ onReady }: { onReady: (animation?: boolean, version?: string) => void }) {
  const [boot, setBoot] = useState<Bootstrap | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [unlocked, setUnlocked] = useState(false);

  const loadBoot = useCallback(async () => {
    try {
      const next = await api.bootstrap();
      setBoot(next);
      onReady(next.startupAnimation !== false, next.version);
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, [onReady]);
  useEffect(() => {
    void loadBoot();
  }, [loadBoot]);
  useEffect(() => {
    if (error) onReady();
  }, [boot, error, onReady]);

  const gateTitle: RouteDocumentTitle | null =
    !boot && isStarting(error)
      ? { matched: true, parts: [t("Starting")] }
      : !boot
        ? { matched: true, parts: [] }
        : boot.authRequired && !boot.authenticated && !unlocked
          ? { matched: true, parts: [t("Sign in")] }
          : null;
  useDocumentBrand(boot?.serverName);
  useRouteDocumentTitle(gateTitle);

  if (!boot && isStarting(error)) return <Startup onReady={() => void loadBoot()} />;
  if (!boot) {
    return (
      <main className="grid min-h-screen place-items-center bg-bg-subtle p-4">
        {error ? (
          <div className="w-full max-w-md">
            <ErrorPanel error={error} onRetry={() => void loadBoot()} />
          </div>
        ) : (
          <div className="flex flex-col items-center gap-3 text-sm text-fg-muted">
            <EtherMark size={40} motion="loop" />
            <span role="status">{t("Connecting to Ether…")}</span>
          </div>
        )}
      </main>
    );
  }
  if (boot.authRequired && !boot.authenticated && !unlocked) {
    return (
      <Login
        boot={boot}
        onReady={() => {
          setUnlocked(true);
          void loadBoot();
        }}
      />
    );
  }
  return (
    <AppProvider boot={boot} reloadBoot={loadBoot}>
      <Shell />
      {boot.setupComplete && boot.authenticated && boot.version && boot.version !== "dev" ? (
        <AutomaticWhatsNew key={boot.version} version={boot.version} />
      ) : null}
    </AppProvider>
  );
}

/** AppKit's shared dialogs and drawers, labelled in the active language. */
function Chrome({ children }: { children: ReactNode }) {
  return (
    <DrawerTextProvider
      value={{
        close: t("Close"),
        fullscreen: t("Fullscreen"),
        exitFullscreen: t("Exit fullscreen"),
      }}
    >
      {children}
      <ConfirmRoot
        labels={{
          defaultTitle: t("Are you sure?"),
          dangerTitle: t("Are you sure?"),
          confirm: t("Confirm"),
          cancel: t("Cancel"),
        }}
      />
      <PromptRoot defaultConfirmLabel={t("Save")} defaultCancelLabel={t("Cancel")} />
    </DrawerTextProvider>
  );
}

const Me = lazy(() => import("./pages/me/Me"));

function MeScreen() {
  useRouteDocumentTitle({ matched: true, parts: [t("Approve a TV")] });
  return (
    <Suspense fallback={<PageFallback />}>
      <Me />
    </Suspense>
  );
}

export default function App() {
  const location = useLocation();
  const [ready, setReady] = useState(false);
  const [bootVersion, setBootVersion] = useState<string | null>(null);
  // Decided at page open from the last known server value, so the splash's
  // minimum time runs from load (not from bootstrap) and never flashes on a
  // server that turned it off. The server's answer can only hide it.
  const [animation, setAnimation] = useState<boolean>(() => {
    try {
      return localStorage.getItem("ether.startupAnimation") !== "false";
    } catch {
      return true;
    }
  });
  useEffect(() => {
    if (location.pathname === "/recommendations") {
      void loadRecommendations();
    }
  }, [location.pathname]);
  const onReady = useCallback((enabled?: boolean, version?: string) => {
    if (enabled !== undefined) {
      try {
        localStorage.setItem("ether.startupAnimation", String(enabled));
      } catch {
        /* Storage is optional. */
      }
      if (!enabled) setAnimation(false);
    }
    if (version) setBootVersion(version);
    setReady(true);
  }, []);
  return (
    <I18nProvider>
      {() => (
        <DocumentTitleProvider>
          <ThemeProvider {...theme}>
            <UiLinkProvider link={RouterAnchor}>
              <Chrome>
                {location.pathname === "/me" ? <MeScreen /> : <Gate onReady={onReady} />}
              </Chrome>
              {location.pathname !== "/me" && animation ? <BrandSplash ready={ready} /> : null}
              <UpdateReady bootVersion={bootVersion} />
              <Toaster richColors closeButton position="bottom-right" />
            </UiLinkProvider>
          </ThemeProvider>
        </DocumentTitleProvider>
      )}
    </I18nProvider>
  );
}

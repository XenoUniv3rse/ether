import { PluginFailureDetails, pluginFailureSentence } from "../../../components/PluginFailure";
import { ActionJobs } from "./ActionJobs";
import { addRowHref } from "../../../lib/recommendations";
import { DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import { Badge, Button, Skeleton, Switch, cn, toast } from "@braedonsaunders/appkit-ui";
import {
  AlertTriangle,
  ArrowUpRight,
  Film,
  LayoutGrid,
  Library,
  Plus,
  Radio,
  RefreshCw,
  Subtitles,
  Tv,
} from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Artwork } from "../../../components/Artwork";
import { getLocale, t, tn } from "../../../i18n";
import { useLocale } from "../../../i18n/react";
import {
  API_BASE,
  SESSION_HEADER,
  apiFetch,
  errorMessage,
  getToken,
  isAbort,
} from "../../../lib/api";
import { invalidateRegistry, textOf, useRegistry } from "../../../lib/extend";
import { formatDateTime, formatNumber, parseTime, relativeTime } from "../../../lib/format";
import { createStreamParser } from "../../../lib/stream";
import {
  librariesApi,
  type Library as LibraryRecord,
  type LibrarySyncRequest,
} from "../../libraries/api";
import {
  DeleteGuardChoice,
  formatItemStorageSummary,
  formatSyncStatusLine,
  notifySyncError,
  notifySyncResponse,
  resolveDeleteGuard,
} from "../../libraries/syncStatus";
import { GuideStrip, type GuideChannel } from "../Stage";
import { SidecarPairing } from "../SidecarPairing";
import {
  buildPlacements,
  type PluginDetail,
  type PluginFeedOverview,
  type PluginLiveEventSummary,
  type PluginLiveEventsOverview,
  type PluginScreenOverview,
} from "../types";
import { callImdb, resolveTitles, resultCount, type PluginCall, type TitleCardLite } from "./calls";
import { formatPollInterval, healthSummary, type HealthSummary } from "./health";
import { liveTVKind, primaryContribution } from "./kinds";

/**
 * The plugin at a glance: what it is doing right now in the surface it
 * feeds and where it shows up. Problems get a card of
 * their own only when there is one.
 */
export function PluginOverview({
  detail,
  calls,
  onClose,
  onRetry,
  onFresh,
  onActivity,
  onLiveFeeds,
}: {
  detail: PluginDetail;
  calls: PluginCall[] | null;
  onClose: () => void;
  onRetry: () => Promise<void>;
  /** Reload the plugin quietly after a live call, so its health is current. */
  onFresh?: () => void;
  /** Open the Activity tab, where the technical detail lives. */
  onActivity?: () => void;
  /** Share live feed health with the drawer header and Activity tiles. */
  onLiveFeeds?: (feeds?: PluginFeedOverview[]) => void;
}) {
  const liveEvents = usePluginLiveEvents(detail, onLiveFeeds);
  const health = healthSummary(detail, liveEvents.data?.feeds);
  const primary = primaryContribution(detail);
  const routes = new Set<string>();
  const failedCall = calls?.find((call) => {
    if (routes.has(call.route)) return false;
    routes.add(call.route);
    return (
      call.failure &&
      call.route !== "/ether/health" &&
      !/^\/ether\/v1\/kinds\/[^/]+\/(browse|changes)$/.test(call.route)
    );
  })?.failure;
  const handleRetry = useCallback(async () => {
    await onRetry();
    liveEvents.reload();
  }, [onRetry, liveEvents.reload]);

  return (
    <div className="space-y-5">
      {detail.manifest.ether?.runtime?.kind === "remote" ? (
        <SidecarPairing key={detail.id} id={detail.id} sidecar={detail.manifest.ether?.sidecar} />
      ) : null}
      {health.card ? (
        <ProblemCard health={health} onRetry={handleRetry} onActivity={onActivity} />
      ) : null}
      {detail.manifest.ether?.contributes?.mediaKinds?.length ? (
        <LibrarySyncNow detail={detail} onClose={onClose} onSyncActivity={onFresh} />
      ) : null}
      {failedCall ? (
        <DashboardPanel title={t("Recent call failure")}>
          <p className="text-sm text-danger">{pluginFailureSentence(failedCall)}</p>
          <PluginFailureDetails failure={failedCall} />
        </DashboardPanel>
      ) : null}
      <LiveNow
        detail={detail}
        calls={calls}
        liveEvents={liveEvents}
        onClose={onClose}
        onFresh={onFresh}
      />
      {liveEvents.hasLiveFeeds && primary !== "live_events" ? (
        <LiveEventsNow detail={detail} state={liveEvents} onClose={onClose} />
      ) : null}
      {detail.manifest.ether?.runtime?.kind && detail.manifest.ether.runtime.kind !== "data" ? (
        <ActionJobs id={detail.id} />
      ) : null}
      <Placements
        detail={detail}
        liveEvents={liveEvents.data}
        onReload={liveEvents.reload}
        onClose={onClose}
      />
    </div>
  );
}

interface PluginLiveEventsState {
  needed: boolean;
  hasLiveFeeds: boolean;
  hasScreens: boolean;
  data: PluginLiveEventsOverview | null;
  loading: boolean;
  error: string;
  reload: () => void;
}

const LIVE_EVENTS_LIMIT = 12;

function usePluginLiveEvents(
  detail: PluginDetail,
  onFeeds?: (feeds?: PluginFeedOverview[]) => void,
): PluginLiveEventsState {
  const caps = detail.capabilities ?? detail.manifest.ether?.capabilities ?? [];
  const hasLiveFeeds =
    Boolean(detail.manifest.ether?.contributes?.liveFeeds?.length) || caps.includes("live_events");
  const hasScreens = Boolean(detail.manifest.ether?.contributes?.screens?.length);
  const needed = hasLiveFeeds || hasScreens;

  const [data, setData] = useState<PluginLiveEventsOverview | null>(null);
  const [loading, setLoading] = useState(needed);
  const [error, setError] = useState("");
  const [tick, setTick] = useState(0);
  const onFeedsRef = useRef(onFeeds);
  onFeedsRef.current = onFeeds;

  const reload = useCallback(() => setTick((n) => n + 1), []);

  useEffect(() => {
    if (!needed) {
      setData(null);
      setLoading(false);
      setError("");
      onFeedsRef.current?.(undefined);
      return;
    }

    let cancelled = false;
    const ctrl = new AbortController();
    setLoading(true);
    setError("");

    const path = `/plugins/${encodeURIComponent(detail.id)}/live-events?limit=${LIVE_EVENTS_LIMIT}`;

    void (async () => {
      try {
        const snapshot = await apiFetch<PluginLiveEventsOverview>(path, { signal: ctrl.signal });
        if (cancelled) return;
        setData(snapshot);
        setLoading(false);
        onFeedsRef.current?.(snapshot.feeds);
      } catch (e) {
        if (cancelled || isAbort(e)) return;
        setLoading(false);
        setError(errorMessage(e));
      }

      // Subscribe briefly while the drawer Overview is open, following hub
      // subscriber rules without forcing extra plugin polling.
      if (cancelled || !hasLiveFeeds || !detail.enabled || detail.shadow) return;
      try {
        const headers: Record<string, string> = { Accept: "text/event-stream" };
        const token = getToken();
        if (token) headers[SESSION_HEADER] = token;
        const res = await fetch(`${API_BASE}${path}&stream=1`, {
          headers,
          signal: ctrl.signal,
        });
        const ct = res.headers.get("content-type") ?? "";
        if (!res.ok || !ct.includes("text/event-stream") || !res.body) return;

        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        const parser = createStreamParser((ev) => {
          if (ev.type !== "overview" || !ev.data || cancelled) return;
          try {
            const parsed = JSON.parse(ev.data) as PluginLiveEventsOverview;
            setData(parsed);
            setError("");
            onFeedsRef.current?.(parsed.feeds);
          } catch {
            // Ignore malformed SSE frame.
          }
        });
        while (!cancelled) {
          const { done, value } = await reader.read();
          if (done) break;
          parser.push(decoder.decode(value, { stream: true }));
        }
      } catch {
        // Closing the drawer or running against a non-SSE mock ends here.
      }
    })();

    return () => {
      cancelled = true;
      ctrl.abort();
    };
  }, [detail.id, detail.enabled, detail.shadow, needed, hasLiveFeeds, tick]);

  return { needed, hasLiveFeeds, hasScreens, data, loading, error, reload };
}

function ProblemCard({
  health,
  onRetry,
  onActivity,
}: {
  health: HealthSummary;
  onRetry: () => Promise<void>;
  onActivity?: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const card = health.card!;
  const severe = health.level === "error";
  return (
    <div
      role="status"
      className={cn(
        "flex flex-wrap items-center gap-3 rounded-xl border p-4",
        severe ? "border-danger/40 bg-danger/5" : "border-warning/40 bg-warning/5",
      )}
    >
      <AlertTriangle
        size={18}
        className={cn("shrink-0", severe ? "text-danger" : "text-warning")}
        aria-hidden
      />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-fg">{card.title}</p>
        <p className="mt-0.5 text-xs text-fg-muted">{card.line}</p>
        {card.meta ? <p className="mt-1 text-xs text-fg-subtle">{card.meta}</p> : null}
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {onActivity ? (
          <Button size="sm" variant="ghost" onClick={onActivity}>
            {t("See activity")}
          </Button>
        ) : null}
        {card.retry ? (
          <Button
            size="sm"
            variant="outline"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              void onRetry().finally(() => setBusy(false));
            }}
          >
            <RefreshCw size={13} className={cn(busy && "animate-spin")} /> {t("Try again")}
          </Button>
        ) : null}
      </div>
    </div>
  );
}

/* ---------------------------------------------------------------- live card */

type OverviewSyncAction = "sync" | "full" | "keep" | "force";

function LibrarySyncNow({
  detail,
  onClose,
  onSyncActivity,
}: {
  detail: PluginDetail;
  onClose: () => void;
  onSyncActivity?: () => void;
}) {
  const [libs, setLibs] = useState<LibraryRecord[] | null>(null);
  const [busyLib, setBusyLib] = useState<{ id: number; action: OverviewSyncAction } | null>(null);

  const anyRunning = Boolean(libs?.some((l) => l.sync?.running));
  const anyPendingFirstSync = Boolean(
    detail.enabled &&
    libs?.some(
      (l) =>
        l.kind?.sync?.mode !== "push" &&
        !l.sync?.lastOk &&
        !l.sync?.lastError &&
        (l.storedItems ?? l.itemCount ?? l.sync?.items ?? 0) === 0,
    ),
  );
  const shouldPoll = anyRunning || anyPendingFirstSync;
  const wasRunning = useRef(false);

  useEffect(() => {
    if (wasRunning.current && !anyRunning) {
      onSyncActivity?.();
    }
    wasRunning.current = anyRunning;
  }, [anyRunning, onSyncActivity]);

  useEffect(() => {
    let cancelled = false;
    const ctrl = new AbortController();
    const load = async () => {
      try {
        const all = await librariesApi.list(ctrl.signal);
        if (!cancelled) {
          setLibs(all.filter((l) => l.pluginId === detail.id));
        }
      } catch {
        // Ignore abort / transient errors while polling.
      }
    };
    void load();
    if (!shouldPoll) {
      return () => {
        cancelled = true;
        ctrl.abort();
      };
    }
    const timer = window.setInterval(() => {
      void load();
    }, 1500);
    return () => {
      cancelled = true;
      ctrl.abort();
      window.clearInterval(timer);
    };
  }, [detail.id, shouldPoll]);

  const runSync = async (id: number, action: OverviewSyncAction, opts: LibrarySyncRequest) => {
    setBusyLib({ id, action });
    try {
      const res = await librariesApi.sync(id, opts);
      notifySyncResponse(res);
    } catch (e) {
      notifySyncError(e);
    } finally {
      setBusyLib(null);
      try {
        const all = await librariesApi.list();
        setLibs(all.filter((l) => l.pluginId === detail.id));
      } catch {
        // Ignore transient refresh failure.
      }
      onSyncActivity?.();
    }
  };

  if (!libs || libs.length === 0) return null;

  return (
    <DashboardPanel title={t("Library sync")}>
      <p className="mb-3 text-sm text-muted-foreground">
        {anyRunning
          ? t("Syncing items in the background. Items appear in your apps as pages arrive.")
          : t("Libraries populated by this plugin.")}
      </p>
      <div className="space-y-3">
        {libs.map((lib) => {
          const sync = lib.sync;
          const running = Boolean(sync?.running);
          const guard = running ? null : resolveDeleteGuard(sync);
          const count = lib.storedItems ?? lib.itemCount ?? sync?.items ?? 0;
          const capSummary = formatItemStorageSummary(lib);
          const statusLine = guard
            ? ""
            : formatSyncStatusLine(sync, { count, preferRelative: true });
          const line =
            !running && !guard && !sync?.lastError && !sync?.lastOk && count === 0
              ? t("Waiting for first sync")
              : capSummary && statusLine
                ? `${capSummary} · ${statusLine}`
                : capSummary || statusLine;
          const libAction = busyLib?.id === lib.id ? busyLib.action : null;
          const isBusy = running || Boolean(libAction);
          return (
            <div
              key={lib.id}
              className="rounded-lg border border-border/60 bg-surface-muted/40 px-3.5 py-2.5"
            >
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="truncate text-sm font-medium text-fg">
                      {lib.kind?.label ?? lib.name}
                    </span>
                    {running ? (
                      <Badge variant="secondary" className="gap-1">
                        <RefreshCw size={11} className="animate-spin" aria-hidden />
                        {sync?.phase === "reindexing" ? t("Reindexing") : t("Syncing")}
                      </Badge>
                    ) : guard ? (
                      <Badge variant="outline" className="gap-1 border-warning/50 text-warning">
                        <AlertTriangle size={11} aria-hidden />
                        {t("Needs review")}
                      </Badge>
                    ) : null}
                  </div>
                  <p className="mt-0.5 text-xs text-fg-muted">{line}</p>
                  {!running && !guard && sync?.failure ? (
                    <PluginFailureDetails failure={sync.failure} />
                  ) : null}
                </div>
                <div className="flex shrink-0 flex-wrap items-center gap-2">
                  {detail.enabled && lib.kind?.sync?.mode !== "push" ? (
                    <>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={isBusy}
                        onClick={() => void runSync(lib.id, "full", { fullWalk: true })}
                      >
                        {libAction === "full" ? (
                          <RefreshCw size={13} className="animate-spin" aria-hidden />
                        ) : null}
                        {t("Full re-sync")}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={isBusy}
                        onClick={() => void runSync(lib.id, "sync", { fullWalk: false })}
                      >
                        <RefreshCw
                          size={13}
                          className={cn((running || libAction === "sync") && "animate-spin")}
                          aria-hidden
                        />{" "}
                        {running ? t("Syncing…") : t("Sync now")}
                      </Button>
                    </>
                  ) : null}
                  <Button size="sm" variant="outline" asChild>
                    <Link to={`/libraries/${lib.id}`} onClick={onClose}>
                      {t("Open")} <ArrowUpRight size={13} />
                    </Link>
                  </Button>
                </div>
              </div>
              {guard ? (
                <DeleteGuardChoice
                  guard={guard}
                  busyAction={libAction}
                  onKeep={() => void runSync(lib.id, "keep", { keepExisting: true })}
                  onRemoveMissing={() =>
                    void runSync(lib.id, "force", {
                      force: true,
                      fullWalk: guard.source === "walk",
                    })
                  }
                />
              ) : null}
            </div>
          );
        })}
      </div>
    </DashboardPanel>
  );
}

function LiveNow({
  detail,
  calls,
  liveEvents,
  onClose,
  onFresh,
}: {
  detail: PluginDetail;
  calls: PluginCall[] | null;
  liveEvents: PluginLiveEventsState;
  onClose: () => void;
  onFresh?: () => void;
}) {
  switch (primaryContribution(detail)) {
    case "live_events":
      return <LiveEventsNow detail={detail} state={liveEvents} onClose={onClose} />;
    case "livetv_channels":
      return <LiveTVNow detail={detail} onClose={onClose} />;
    case "list_source":
      return <CatalogueNow detail={detail} onClose={onClose} onFresh={onFresh} />;
    case "stream_source":
      return (
        <LookupsNow
          detail={detail}
          calls={calls}
          resource="stream"
          icon={<Film size={16} />}
          title={t("Recent source lookups")}
          empty={t("Titles show here after someone presses play and Ether asks {name}.", {
            name: detail.name,
          })}
          count={(n) => tn(n, "{count} source", "{count} sources")}
          none={t("No sources")}
        />
      );
    case "subtitles":
      return (
        <LookupsNow
          detail={detail}
          calls={calls}
          resource="subtitles"
          icon={<Subtitles size={16} />}
          title={t("Recent subtitle lookups")}
          empty={t("Titles show here after a viewer looks for subtitles.")}
          count={(n) => tn(n, "{count} subtitle", "{count} subtitles")}
          none={t("No subtitles")}
        />
      );
    case "metadata":
      return (
        <LookupsNow
          detail={detail}
          calls={calls}
          resource="meta"
          icon={<Library size={16} />}
          title={t("Details it filled in")}
          empty={t("Titles show here after Ether asks {name} for details.", { name: detail.name })}
        />
      );
    default:
      return null;
  }
}

function formatUpcomingWhen(ev: PluginLiveEventSummary): string {
  if (ev.status.detail) return ev.status.detail;
  const d = parseTime(ev.start);
  if (!d) return t("Upcoming");
  const diffDays = Math.abs(d.getTime() - Date.now()) / (24 * 60 * 60 * 1000);
  if (diffDays <= 6) {
    return d.toLocaleString(getLocale(), {
      weekday: "short",
      hour: "numeric",
      minute: "2-digit",
    });
  }
  return formatDateTime(d);
}

function orderedCompetitors(ev: PluginLiveEventSummary) {
  const list = ev.competitors ?? [];
  if (list.length !== 2) return list;
  const [a, b] = list;
  if (a?.home && !b?.home) return [b!, a!];
  return [a!, b!];
}

function LiveEventsNow({
  detail,
  state,
  onClose,
}: {
  detail: PluginDetail;
  state: PluginLiveEventsState;
  onClose: () => void;
}) {
  useLocale();
  const { data, loading, error } = state;
  const hasVelaScreen = Boolean(
    (data?.screens ?? []).some((s) => s.frontends.some((fe) => fe.id === "vela")) ||
    (detail.manifest.ether?.contributes?.screens ?? []).some(
      (s) => !s.frontends?.length || s.frontends.includes("vela"),
    ),
  );
  const anyFailing = Boolean(
    data?.feeds?.some((f) => f.health.consecutiveErrors > 0 || Boolean(f.health.lastError)),
  );

  const actions = (
    <div className="flex flex-wrap items-center gap-2">
      {detail.enabled && !detail.shadow && data ? (
        <>
          {data.liveCount > 0 ? (
            <Badge variant="success">
              {t("{count} live", { count: formatNumber(data.liveCount, 0) })}
            </Badge>
          ) : null}
          {data.upcomingCount > 0 ? (
            <Badge variant="secondary">
              {t("{count} upcoming", { count: formatNumber(data.upcomingCount, 0) })}
            </Badge>
          ) : null}
        </>
      ) : null}
      {hasVelaScreen ? <OpenLink to="/apps/vela" label={t("Open Vela")} onClose={onClose} /> : null}
    </div>
  );

  let body: ReactNode;
  if (!detail.enabled) {
    body = <p className="text-sm text-fg-muted">{t("Turn it on to show live events.")}</p>;
  } else if (detail.shadow) {
    body = <p className="text-sm text-fg-muted">{t("Make it live to show live events.")}</p>;
  } else if (loading && !data) {
    body = (
      <div className="space-y-2">
        {Array.from({ length: 3 }, (_, i) => (
          <Skeleton key={i} className="h-14 rounded-lg" />
        ))}
      </div>
    );
  } else if (error && !data) {
    body = <p className="text-sm text-danger">{error}</p>;
  } else if (!data || data.events.length === 0) {
    body = (
      <div>
        <p className="text-sm text-fg-muted">
          {anyFailing
            ? t("Live events will appear here when the feed recovers.")
            : t("No live or upcoming events right now.")}
        </p>
        {data && data.totalEvents > 0 ? (
          <p className="mt-1 text-xs text-fg-subtle">
            {tn(data.totalEvents, "{count} recent event cached", "{count} recent events cached")}
          </p>
        ) : null}
      </div>
    );
  } else {
    body = (
      <ul className="space-y-2">
        {data.events.map((ev) => {
          const comps = orderedCompetitors(ev);
          const pair = comps.length === 2 ? ([comps[0]!, comps[1]!] as const) : null;
          const hasScore = Boolean(pair && (pair[0].score || pair[1].score));
          const leagueLabel = ev.league?.abbr || ev.league?.name || "";
          const leagueTitle = ev.league?.name || ev.league?.abbr || "";
          const leader = ev.ranking?.[0];
          return (
            <li
              key={ev.id}
              className="rounded-lg border border-border/60 bg-surface-muted/40 px-3.5 py-2.5"
            >
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                  {leagueLabel ? (
                    <Badge variant="secondary" title={leagueTitle}>
                      {leagueLabel}
                    </Badge>
                  ) : null}
                  {ev.status.state === "in" ? (
                    <span className="inline-flex items-center gap-1.5 text-xs font-medium text-success">
                      <span className="size-1.5 rounded-full bg-success" aria-hidden />
                      {ev.status.detail || t("Live")}
                    </span>
                  ) : ev.status.state === "delayed" ? (
                    <Badge variant="warning">{ev.status.detail || t("Delayed")}</Badge>
                  ) : (
                    <span className="text-xs text-fg-muted">{formatUpcomingWhen(ev)}</span>
                  )}
                </div>
                <span
                  className={cn(
                    "inline-flex shrink-0 items-center gap-1 text-xs",
                    ev.watchCount > 0 ? "font-medium text-fg-muted" : "text-fg-subtle",
                  )}
                >
                  <Tv size={12} aria-hidden />
                  {tn(ev.watchCount, "{count} watch target", "{count} watch targets")}
                </span>
              </div>
              <div className="mt-1.5 flex flex-wrap items-center justify-between gap-2">
                {pair && hasScore ? (
                  <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-2 gap-y-1 text-sm text-fg">
                    <span
                      className={cn("truncate", pair[0].winner ? "font-semibold" : "font-medium")}
                    >
                      {pair[0].name || pair[0].short || pair[0].abbr}
                    </span>
                    <span className="inline-flex shrink-0 items-center gap-1 rounded-md bg-surface px-2 py-0.5 text-xs font-semibold tabular-nums text-fg ring-1 ring-border/70">
                      <span>{pair[0].score || "0"}</span>
                      <span className="text-fg-subtle" aria-hidden>
                        –
                      </span>
                      <span>{pair[1].score || "0"}</span>
                    </span>
                    <span
                      className={cn("truncate", pair[1].winner ? "font-semibold" : "font-medium")}
                    >
                      {pair[1].name || pair[1].short || pair[1].abbr}
                    </span>
                  </div>
                ) : (
                  <p className="min-w-0 flex-1 truncate text-sm font-medium text-fg">
                    {ev.title ||
                      (pair
                        ? t("{first} vs {second}", {
                            first: pair[0].name || pair[0].short || pair[0].abbr || "",
                            second: pair[1].name || pair[1].short || pair[1].abbr || "",
                          })
                        : ev.id)}
                  </p>
                )}
                {!pair && leader ? (
                  <span className="shrink-0 text-xs tabular-nums text-fg-muted">
                    {leader.pos}. {leader.name}
                    {leader.detail ? ` · ${leader.detail}` : ""}
                  </span>
                ) : null}
              </div>
            </li>
          );
        })}
      </ul>
    );
  }

  return (
    <DashboardPanel title={t("Live now")} icon={<Radio size={16} />} actions={actions}>
      {body}
    </DashboardPanel>
  );
}

interface LiveSource {
  id: string;
  name: string;
  kind: string;
  channelCount?: number;
}

interface LiveChannel {
  id: string;
  number: string;
  name: string;
  enabled: boolean;
  sourceId?: string;
}

interface Programme {
  channelId: string;
  start: string;
  end: string;
  item?: { title?: string };
}

function LiveTVNow({ detail, onClose }: { detail: PluginDetail; onClose: () => void }) {
  const kind = liveTVKind(detail.id);
  const [source, setSource] = useState<LiveSource | null | undefined>(undefined);
  const [rows, setRows] = useState<GuideChannel[] | null>(null);
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");
  const [revision, setRevision] = useState(0);

  useEffect(() => {
    let live = true;
    setRows(null);
    void (async () => {
      try {
        const sources = await apiFetch<LiveSource[]>("/livetv/sources");
        const src = sources.find((s) => s.kind === kind) ?? null;
        if (!live) return;
        setSource(src);
        if (!src) return;
        const now = new Date();
        const to = new Date(now.getTime() + 3 * 3600_000);
        const [channels, guide] = await Promise.all([
          apiFetch<LiveChannel[]>("/livetv/channels"),
          apiFetch<Programme[]>(
            `/livetv/guide?from=${encodeURIComponent(now.toISOString())}&to=${encodeURIComponent(to.toISOString())}`,
          ).catch(() => [] as Programme[]),
        ]);
        const byChannel = new Map<string, Programme[]>();
        for (const p of guide) {
          const list = byChannel.get(p.channelId) ?? [];
          list.push(p);
          byChannel.set(p.channelId, list);
        }
        const mine = channels
          .filter((c) => c.sourceId === src.id && c.enabled)
          .sort((a, b) => Number(a.number) - Number(b.number));
        const out: GuideChannel[] = [];
        for (const c of mine) {
          const progs = (byChannel.get(c.id) ?? []).sort((a, b) => a.start.localeCompare(b.start));
          const idx = progs.findIndex(
            (p) => Date.parse(p.start) <= now.getTime() && now.getTime() < Date.parse(p.end),
          );
          if (idx < 0) continue;
          const cur = progs[idx]!;
          const start = Date.parse(cur.start);
          const end = Date.parse(cur.end);
          out.push({
            number: Number(c.number) || out.length + 1,
            name: c.name,
            now: cur.item?.title ?? "",
            next: progs[idx + 1]?.item?.title ?? "",
            progress: Math.min(1, Math.max(0, (now.getTime() - start) / Math.max(1, end - start))),
          });
          if (out.length >= 6) break;
        }
        if (live) setRows(out);
      } catch (e) {
        if (live) setError(errorMessage(e));
      }
    })();
    return () => {
      live = false;
    };
  }, [kind, revision]);

  const add = async () => {
    setAdding(true);
    setError("");
    try {
      await apiFetch("/livetv/sources", {
        method: "POST",
        body: JSON.stringify({ name: detail.name, kind, playlistUrl: kind }),
      });
      invalidateRegistry();
      setRevision((r) => r + 1);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setAdding(false);
    }
  };

  if (source === null) {
    return (
      <DashboardPanel title={t("Not in Live TV yet")} icon={<Tv size={16} />}>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-fg-muted">{t("Add its channels and guide to Live TV.")}</p>
          <Button size="sm" disabled={adding || !detail.enabled} onClick={() => void add()}>
            <Plus size={14} /> {adding ? t("Adding…") : t("Add to Live TV")}
          </Button>
        </div>
        {error ? <p className="mt-2 text-sm text-danger">{error}</p> : null}
      </DashboardPanel>
    );
  }
  return (
    <DashboardPanel
      title={
        source?.channelCount
          ? tn(source.channelCount, "{count} channel in Live TV", "{count} channels in Live TV")
          : t("Live TV")
      }
      icon={<Tv size={16} />}
      actions={<OpenLink to="/livetv" label={t("Open Live TV")} onClose={onClose} />}
    >
      {rows === null && !error ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-10 rounded-md" />
          ))}
        </div>
      ) : rows?.length ? (
        <GuideStrip channels={rows} />
      ) : (
        <p className="text-sm text-fg-muted">
          {error || t("No programme guide for the next few hours.")}
        </p>
      )}
    </DashboardPanel>
  );
}

interface PreviewItem {
  key: string;
  title: string;
  year?: number;
  poster?: string;
}

/** How long the drawer waits for a slow add-on's catalogue. */
const PREVIEW_WAIT_S = 30;

function CatalogueNow({
  detail,
  onClose,
  onFresh,
}: {
  detail: PluginDetail;
  onClose: () => void;
  onFresh?: () => void;
}) {
  const { descriptors } = useRegistry<{ params?: { required?: boolean }[]; catalog?: string }>(
    "collectionSource",
  );
  const { locale } = useLocale();
  // Lists that show something without a value to fill in, the add-on's
  // headline lists (Popular, Top, Trending) first.
  const lists = useMemo(() => {
    const headline = /popular|top|trending|featured/i;
    return descriptors
      .filter((d) => d.owner === detail.id && !(d.meta?.params ?? []).some((p) => p.required))
      .sort(
        (a, b) => Number(headline.test(textOf(b.label))) - Number(headline.test(textOf(a.label))),
      );
  }, [descriptors, detail.id, locale]);
  const [pick, setPick] = useState<string>("");
  const current = lists.find((d) => d.id === pick) ?? lists[0];
  const [items, setItems] = useState<PreviewItem[] | null>(null);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState("");
  const [waited, setWaited] = useState(0);
  const [attempt, setAttempt] = useState(0);
  const source = detail.host || detail.name;
  // Stable across quiet reloads of the plugin, so a refresh never refetches.
  const ownCatalogs = Boolean(detail.manifest.catalogs?.length);

  // The drawer opens at once; the catalogue arrives when the add-on answers.
  // Public add-on servers can take many seconds, so the panel says it is
  // still loading and gives up, in plain words, after PREVIEW_WAIT_S.
  useEffect(() => {
    if (!current) return;
    const ctl = new AbortController();
    const started = Date.now();
    let live = true;
    setItems(null);
    setError("");
    setWaited(0);
    const tick = window.setInterval(
      () => setWaited(Math.floor((Date.now() - started) / 1000)),
      1000,
    );
    const giveUp = window.setTimeout(() => ctl.abort(), PREVIEW_WAIT_S * 1000);
    const finish = () => {
      window.clearInterval(tick);
      window.clearTimeout(giveUp);
    };
    const label = textOf(current.label);
    const ref = typeof current.meta?.catalog === "string" ? current.meta.catalog : "";
    const slash = ref.indexOf("/");
    const load: Promise<{ items: PreviewItem[]; total: number }> =
      slash > 0 && ownCatalogs
        ? // The add-on's own catalogue page, exactly as it answers it.
          apiFetch<{
            metas?: {
              id: string;
              name?: string;
              poster?: string;
              releaseInfo?: string;
              year?: number;
            }[];
          }>(
            `/plugins/${encodeURIComponent(detail.id)}/resources/catalog?type=${encodeURIComponent(ref.slice(0, slash))}&contentId=${encodeURIComponent(ref.slice(slash + 1))}`,
            { signal: ctl.signal },
          ).then((r) => {
            const metas = r.metas ?? [];
            return {
              total: metas.length,
              items: metas.slice(0, 12).map((m) => ({
                key: m.id,
                title: m.name ?? m.id,
                poster: m.poster,
                year: m.year ?? (Number.parseInt(String(m.releaseInfo ?? ""), 10) || undefined),
              })),
            };
          })
        : apiFetch<{ items: PreviewItem[]; total: number }>("/collections/preview?n=12", {
            method: "POST",
            signal: ctl.signal,
            body: JSON.stringify({
              name: label,
              mediaType: /series|show|tv/i.test(label) ? "show" : "movie",
              sources: [{ kind: current.id }],
            }),
          });
    load
      .then((r) => {
        if (!live) return;
        setItems(r.items ?? []);
        setTotal(r.total ?? 0);
        // The add-on just answered: re-read its health so the header,
        // tiles and card describe this answer.
        onFresh?.();
      })
      .catch((e) => {
        if (!live) return;
        setError(
          ctl.signal.aborted
            ? t(
                "{source} didn't send this catalogue within {seconds} seconds. Its server may be busy; try again in a minute.",
                {
                  source,
                  seconds: PREVIEW_WAIT_S,
                },
              )
            : errorMessage(e),
        );
      })
      .finally(() => {
        finish();
        // A failure or give-up is news for the health readout too.
        if (live && ctl.signal.aborted) onFresh?.();
      });
    return () => {
      live = false;
      finish();
      ctl.abort();
    };
  }, [current, attempt, detail.id, ownCatalogs, source]);

  if (!lists.length) {
    return (
      <DashboardPanel title={t("Catalogues")} icon={<Library size={16} />}>
        <p className="text-sm text-fg-muted">
          {detail.enabled
            ? t("It offers no catalogues Ether can use right now.")
            : t("Turn it on to use its catalogues.")}
        </p>
      </DashboardPanel>
    );
  }
  return (
    <DashboardPanel
      title={t("{plugin} · {catalog}", {
        plugin: detail.name,
        catalog: textOf(current!.label).split("·")[0]!.trim(),
      })}
      icon={<Library size={16} />}
      actions={
        <OpenLink to={addRowHref(current!.id)} label={t("Add as a row")} onClose={onClose} />
      }
    >
      {lists.length > 1 ? (
        <div className="mb-3 flex flex-wrap gap-1.5" role="group" aria-label={t("Catalogues")}>
          {lists.slice(0, 8).map((d) => (
            <button
              key={d.id}
              type="button"
              aria-pressed={d.id === current?.id}
              onClick={() => setPick(d.id)}
              className={cn(
                "rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors",
                d.id === current?.id
                  ? "border-primary/40 bg-primary-subtle text-primary"
                  : "border-border text-fg-muted hover:text-fg",
              )}
            >
              {textOf(d.label)}
            </button>
          ))}
        </div>
      ) : null}
      {items === null && !error && waited >= 3 ? (
        <p className="mb-3 flex items-center gap-2 text-xs text-fg-muted" role="status">
          <RefreshCw size={12} className="animate-spin" aria-hidden />
          {t("Still loading from {source}… {seconds}s", { source, seconds: waited })}
        </p>
      ) : null}
      {error ? (
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-fg-muted">{error}</p>
          <Button size="sm" variant="outline" onClick={() => setAttempt((a) => a + 1)}>
            <RefreshCw size={13} /> {t("Try again")}
          </Button>
        </div>
      ) : (
        <PosterRow
          items={items?.map((i) => ({
            key: i.key,
            title: i.title,
            poster: i.poster,
            caption: i.year ? String(i.year) : "",
          }))}
          empty={t("This catalogue is empty right now.")}
        />
      )}
      {items?.length ? (
        <div className="mt-3 flex justify-end">
          <Badge variant="secondary">{tn(total, "{count} title", "{count} titles")}</Badge>
        </div>
      ) : null}
    </DashboardPanel>
  );
}

function LookupsNow({
  detail,
  calls,
  resource,
  icon,
  title,
  empty,
  count,
  none,
}: {
  detail: PluginDetail;
  calls: PluginCall[] | null;
  resource: "stream" | "subtitles" | "meta";
  icon: ReactNode;
  title: string;
  empty: string;
  count?: (n: number) => string;
  none?: string;
}) {
  const recent = useMemo(() => {
    const seen = new Set<string>();
    const out: { imdb: string; call: PluginCall }[] = [];
    for (const c of calls ?? []) {
      if (!c.route.startsWith(`/${resource}/`) || c.route.startsWith("/stream/tv/")) continue;
      const imdb = callImdb(c.route);
      if (!imdb || seen.has(imdb)) continue;
      seen.add(imdb);
      out.push({ imdb, call: c });
      if (out.length >= 12) break;
    }
    return out;
  }, [calls, resource]);
  const [titles, setTitles] = useState<Record<string, TitleCardLite> | null>(null);
  useEffect(() => {
    let live = true;
    setTitles(null);
    void resolveTitles(recent.map((r) => r.imdb)).then((m) => live && setTitles(m));
    return () => {
      live = false;
    };
  }, [recent]);

  const items =
    calls === null || titles === null
      ? undefined
      : recent
          .filter((r) => titles[r.imdb])
          .map((r) => {
            const n = resultCount(r.call);
            const caption =
              r.call.status !== 200
                ? t("Failed")
                : count && n !== undefined
                  ? n > 0
                    ? count(n)
                    : (none ?? "")
                  : relativeTime(r.call.created);
            return {
              key: r.imdb,
              title: titles[r.imdb]!.title,
              poster: titles[r.imdb]!.poster,
              caption,
              muted: r.call.status !== 200 || !n,
            };
          });
  return (
    <DashboardPanel title={title} icon={icon}>
      <PosterRow items={items} empty={detail.enabled ? empty : t("Turn it on to use it.")} />
    </DashboardPanel>
  );
}

function PosterRow({
  items,
  error,
  empty,
}: {
  items?: { key: string; title: string; poster?: string; caption?: string; muted?: boolean }[];
  error?: string;
  empty: string;
}) {
  if (error) return <p className="text-sm text-danger">{error}</p>;
  if (!items) {
    return (
      <div className="grid grid-cols-4 gap-3 sm:grid-cols-6">
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className="aspect-[2/3] rounded-md" />
        ))}
      </div>
    );
  }
  if (!items.length) return <p className="text-sm text-fg-muted">{empty}</p>;
  return (
    <ul className="grid grid-cols-4 gap-3 sm:grid-cols-6">
      {items.slice(0, 12).map((it) => (
        <li key={it.key} className="min-w-0">
          <Artwork
            src={it.poster}
            alt={it.title}
            title={it.title}
            className="aspect-[2/3] w-full overflow-hidden rounded-md shadow-sm ring-1 ring-border-subtle"
          />
          <p className="mt-1.5 truncate text-xs font-medium text-fg" title={it.title}>
            {it.title}
          </p>
          {it.caption ? (
            <p className={cn("truncate text-[11px]", it.muted ? "text-fg-subtle" : "text-success")}>
              {it.caption}
            </p>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

function OpenLink({ to, label, onClose }: { to: string; label: string; onClose: () => void }) {
  return (
    <Link
      to={to}
      onClick={onClose}
      className="inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline"
    >
      {label} <ArrowUpRight size={13} aria-hidden />
    </Link>
  );
}

/* ---------------------------------------------------------- where it appears */

function fallbackScreens(detail: PluginDetail): PluginScreenOverview[] {
  const declared = detail.manifest.ether?.contributes?.screens ?? [];
  const published = detail.enabled && !detail.shadow;
  const reason = !detail.enabled ? "plugin_disabled" : detail.shadow ? "shadow" : "published";
  return declared.map((sc) => ({
    id: sc.id,
    title: sc.title && !sc.title.startsWith("$t:") ? sc.title : sc.id,
    icon: sc.icon,
    frontends: (sc.frontends?.length ? sc.frontends : ["vela"]).map((id) => ({
      id,
      name: id === "vela" ? "Vela" : id,
      published,
      reason,
    })),
  }));
}

function fallbackFeeds(detail: PluginDetail): PluginFeedOverview[] {
  const declared = detail.manifest.ether?.contributes?.liveFeeds ?? [];
  return declared.map((lf) => ({
    id: lf.id,
    title: lf.title && !lf.title.startsWith("$t:") ? lf.title : lf.id,
    minIntervalSec: lf.minIntervalSec ?? 0,
    entityKinds: lf.entityKinds,
    active: detail.enabled && !detail.shadow,
    health: { consecutiveErrors: 0, subscribers: 0, polling: false },
    eventCount: 0,
    liveCount: 0,
    upcomingCount: 0,
    leagues: [],
    entityTotal: 0,
  }));
}

function screenPublishCopy(
  fe: { name: string; published: boolean; reason?: string },
  surface = false,
): {
  badge: string;
  variant: "success" | "info" | "warning" | "secondary";
  detail: string;
} {
  if (fe.published) {
    return {
      badge: t("Published"),
      variant: "success",
      detail: surface
        ? t("Shown in {app} where it applies.", { app: fe.name })
        : t("Shown in {app} navigation.", { app: fe.name }),
    };
  }
  switch (fe.reason) {
    case "owner_off":
      return {
        badge: t("Hidden"),
        variant: "secondary",
        detail: t("You turned this off for {app}.", { app: fe.name }),
      };
    case "shadow":
      return {
        badge: t("Trying out"),
        variant: "info",
        detail: t("Make this plugin live to publish this screen."),
      };
    case "frontend_disabled":
      return {
        badge: t("{app} off", { app: fe.name }),
        variant: "warning",
        detail: t("Turn on {app} in Apps to publish this screen.", { app: fe.name }),
      };
    case "capability_off":
      return {
        badge: t("Not published"),
        variant: "secondary",
        detail: t("Live events are turned off for this plugin."),
      };
    default:
      return {
        badge: t("Not published"),
        variant: "secondary",
        detail: t("Turn on this plugin to publish this screen."),
      };
  }
}

function Placements({
  detail,
  liveEvents,
  onReload,
  onClose,
}: {
  detail: PluginDetail;
  liveEvents: PluginLiveEventsOverview | null;
  onReload?: () => void;
  onClose: () => void;
}) {
  const [saving, setSaving] = useState<string | null>(null);
  const setShown = async (kind: string, id: string, frontend: string, enabled: boolean) => {
    const key = `${kind}:${id}:${frontend}`;
    setSaving(key);
    try {
      await apiFetch(`/plugins/${encodeURIComponent(detail.id)}/screen-publish`, {
        method: "PUT",
        body: JSON.stringify({ kind, id, frontend, enabled }),
      });
      onReload?.();
    } catch {
      toast.error(t("Could not change where this shows."));
    } finally {
      setSaving(null);
    }
  };
  const { descriptors } = useRegistry();
  const placements = buildPlacements(descriptors, detail.id, detail.manifest);
  const [collections, setCollections] = useState(0);
  useEffect(() => {
    let live = true;
    apiFetch<{ collections: { sources?: { kind?: string }[] }[] }>("/collections")
      .then((r) => {
        const prefix = `plugin:${detail.id}/`;
        const n = (r.collections ?? []).filter((c) =>
          (c.sources ?? []).some((s) => s.kind?.startsWith(prefix)),
        ).length;
        if (live) setCollections(n);
      })
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, [detail.id]);

  const screens = liveEvents?.screens?.length ? liveEvents.screens : fallbackScreens(detail);
  const feeds = liveEvents?.feeds?.length ? liveEvents.feeds : fallbackFeeds(detail);
  const hasRichPlacements = screens.length > 0 || feeds.length > 0;

  // One chip per surface; a plugin with many catalogues still reads as one.
  const chips = new Map<string, { label: string; href: string }>();
  for (const p of placements) {
    if (p.kind === "console") continue;
    if (hasRichPlacements && (p.kind === "screen" || p.kind === "liveFeed")) continue;
    const label = p.kind === "page" ? p.label : p.area;
    if (!chips.has(label)) chips.set(label, { label, href: p.href });
  }
  if (collections > 0) {
    chips.set("collections-used", {
      label: tn(collections, "Used by {count} collection", "Used by {count} collections"),
      href: "/collections",
    });
  }
  if (!hasRichPlacements && !chips.size) return null;

  return (
    <div className="space-y-4">
      {hasRichPlacements ? (
        <DashboardPanel title={t("Placements")} icon={<LayoutGrid size={16} />}>
          <div className="space-y-2.5">
            {screens.flatMap((sc) =>
              sc.frontends.map((fe) => {
                const status = screenPublishCopy(fe, sc.kind === "surface");
                const switchKey = `${sc.kind ?? "screen"}:${sc.id}:${fe.id}`;
                return (
                  <div
                    key={`screen:${sc.id}:${fe.id}`}
                    className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border/60 bg-surface-muted/40 px-3.5 py-2.5"
                  >
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="truncate text-sm font-medium text-fg">
                          {t("{app} › {screen}", { app: fe.name, screen: sc.title })}
                        </span>
                        <Badge variant={status.variant}>{status.badge}</Badge>
                      </div>
                      <p className="mt-0.5 text-xs text-fg-muted">{status.detail}</p>
                    </div>
                    {fe.enabled !== undefined ? (
                      <Switch
                        aria-label={t("Show {screen} in {app}", { screen: sc.title, app: fe.name })}
                        checked={fe.enabled}
                        disabled={saving === switchKey}
                        onChange={(e) =>
                          void setShown(sc.kind ?? "screen", sc.id, fe.id, e.target.checked)
                        }
                      />
                    ) : (
                      <Button size="sm" variant="outline" asChild>
                        <Link to={`/apps/${encodeURIComponent(fe.id)}`} onClick={onClose}>
                          {t("Manage in Apps")} <ArrowUpRight size={13} />
                        </Link>
                      </Button>
                    )}
                  </div>
                );
              }),
            )}
            {feeds.map((feed) => {
              const leagues = feed.leagues ?? [];
              const entityEntries = Object.entries(feed.entityCounts ?? {})
                .filter(([, n]) => n > 0)
                .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
              const summaryParts: string[] = [
                leagues.length > 0
                  ? tn(leagues.length, "{count} league", "{count} leagues")
                  : t("No leagues cached"),
                tn(feed.entityTotal, "{count} entity", "{count} entities"),
              ];
              const interval = formatPollInterval(feed.minIntervalSec);
              if (interval) {
                summaryParts.push(t("Polls every {interval}", { interval }));
              }
              return (
                <div
                  key={`feed:${feed.id}`}
                  className="rounded-lg border border-border/60 bg-surface-muted/40 px-3.5 py-2.5"
                >
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div className="flex min-w-0 flex-wrap items-center gap-2">
                      <span className="truncate text-sm font-medium text-fg">{feed.title}</span>
                      <Badge variant="secondary">{t("Live feed")}</Badge>
                    </div>
                    {feed.health.lastOkAt &&
                    parseTime(feed.health.lastOkAt) &&
                    !feed.health.lastError ? (
                      <span className="text-xs text-fg-subtle">
                        {t("Updated {when}", { when: relativeTime(feed.health.lastOkAt) })}
                      </span>
                    ) : null}
                  </div>
                  <p className="mt-0.5 text-xs text-fg-muted">{summaryParts.join(" · ")}</p>
                  {leagues.length > 0 ? (
                    <div className="mt-2 flex flex-wrap gap-1.5">
                      {leagues.slice(0, 8).map((lg) => (
                        <Badge
                          key={lg.id}
                          variant="outline"
                          title={lg.name || lg.abbr}
                          className="text-[11px] font-normal"
                        >
                          {lg.abbr || lg.name}
                          {lg.eventCount > 0 ? ` · ${formatNumber(lg.eventCount, 0)}` : ""}
                        </Badge>
                      ))}
                      {leagues.length > 8 ? (
                        <Badge variant="secondary" className="text-[11px] font-normal">
                          {t("+{count} more", { count: formatNumber(leagues.length - 8, 0) })}
                        </Badge>
                      ) : null}
                    </div>
                  ) : null}
                  {entityEntries.length > 0 ? (
                    <p className="mt-1.5 text-[11px] text-fg-subtle">
                      {entityEntries
                        .map(([kind, n]) => `${formatNumber(n, 0)} ${kind}`)
                        .join(" · ")}
                    </p>
                  ) : null}
                </div>
              );
            })}
          </div>
        </DashboardPanel>
      ) : null}
      {chips.size > 0 ? (
        <div className="flex flex-wrap items-center gap-2" aria-label={t("Where it appears")}>
          <span className="text-xs font-medium text-fg-subtle">{t("Appears in")}</span>
          {[...chips.values()].map((c) => (
            <Link
              key={c.label}
              to={c.href}
              onClick={onClose}
              className="inline-flex items-center gap-1 rounded-full border border-border bg-surface px-3 py-1 text-xs font-medium text-fg transition-colors hover:border-primary/40 hover:text-primary"
            >
              {c.label} <ArrowUpRight size={12} aria-hidden />
            </Link>
          ))}
        </div>
      ) : null}
    </div>
  );
}

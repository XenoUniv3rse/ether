import { PanelSlots } from "../viewspec/slots/Slots";
import { DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  EmptyState,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  cn,
} from "@braedonsaunders/appkit-ui";
import {
  Activity,
  BarChart3,
  Film,
  HeartPulse,
  MonitorPlay,
  PlayCircle,
  Table2,
  TrendingUp,
  X,
} from "lucide-react";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useApp, useLiveDashboard } from "../app-context";
import { Artwork } from "../components/Artwork";
import { SessionArtwork } from "../components/SessionArtwork";
import { Meter, TimeBars } from "../components/charts";
import { ErrorPanel, LoadingRows, MutationError } from "../components/feedback";
import {
  HealthPill,
  healthProblems,
  healthVerdict,
  useServerVerdict,
  type HealthVerdict,
} from "../components/HealthPill";
import { ServerPanel } from "../components/ServerPanel";
import { StatusDot } from "../components/status";
import { getLocale, localized, t, tn } from "../i18n";
import { api } from "../lib/api";
import type { ActivityDay, RowInsight, Viewer } from "../lib/contract";
import { fixed, formatNumber, formatPercent, formatRate, relativeTime } from "../lib/format";
import { channelLabel, isPausedSession, type HealthItem, type NowPlaying } from "../lib/health";
import { useAsync, useInterval } from "../lib/hooks";
import { fetchLiveSessions, useLiveSessionsRefresh, useSessionsChanged } from "../lib/sessions";
import { useTitle } from "../lib/titles";
import type { EtherConfig } from "../lib/types";
import { useLocale } from "../i18n/react";
import { PlayDrawer, usePlayParam } from "./activity/PlayDrawer";
import { hoursOf } from "./activity/types";
import { connectDevicePath } from "./setup/connect-path";

const GETTING_STARTED_KEY = "ether.getting-started.dismissed";

function sourceReady(cfg: EtherConfig): boolean {
  const debrid = (cfg.debrid?.providers ?? []).some(
    (provider) => provider.enabled && provider.apiKey.trim() !== "",
  );
  const usenet = (cfg.usenet?.providers ?? []).some(
    (provider) => provider.enabled && provider.host.trim() !== "",
  );
  return debrid || usenet;
}

function GettingStarted({ sessions }: { sessions: NowPlaying[] }) {
  const { dashboard } = useApp();
  const [dismissed, setDismissed] = useState(
    () => localStorage.getItem(GETTING_STARTED_KEY) === "1",
  );
  const config = useAsync(() => api.getConfig(), [dashboard]);
  const viewers = useAsync(() => api.viewers(), [dashboard]);
  if (dismissed) return null;
  const cfg = config.data;
  const people = viewers.data;
  if (!cfg || !people) return null;
  const apps = Object.entries(cfg.frontends ?? {})
    .filter(([, row]) => row?.enabled)
    .map(([id]) => id);
  const profile = cfg.quality?.default?.trim() || "4K";
  const steps = [
    { id: "source", todo: !sourceReady(cfg), label: t("Add a source"), to: "/sources" },
    {
      id: "plex",
      todo: apps.includes("plex") && !(cfg.plex?.claimed || cfg.plex?.token),
      label: t("Connect Plex"),
      to: "/apps/plex/account",
    },
    {
      id: "device",
      todo:
        apps.length > 0 &&
        sessions.length === 0 &&
        !people.some((viewer: Viewer) => !viewer.isOwner && Boolean(viewer.lastActive)),
      label: t("Connect a device"),
      to: connectDevicePath(cfg.frontends),
    },
    {
      id: "viewers",
      todo: !people.some((viewer) => !viewer.isOwner),
      label: t("Add viewers"),
      to: "/viewers",
    },
  ].filter((step) => step.todo);
  if (steps.length === 0) return <span hidden data-testid="getting-started-clear" />;
  // The quality chip only reports the current default. It is never a todo,
  // so a deliberate choice (say 1080p) cannot keep this bar alive.
  const qualityChip = {
    id: "quality",
    label: t("Playback quality: {profile} at home · change", { profile }),
    to: "/releases/profiles",
  };
  return (
    <section
      aria-label={t("Next steps")}
      className="flex shrink-0 flex-wrap items-center gap-2 rounded-lg border border-border bg-surface py-1.5 pl-3 pr-1.5"
    >
      <span className="text-xs font-medium text-fg-muted">{t("Next steps")}</span>
      {[...steps, qualityChip].map((step) => (
        <Link
          key={step.id}
          to={step.to}
          className="rounded-full border border-border px-3 py-1 text-xs font-medium text-fg transition hover:border-primary hover:text-primary focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
        >
          {step.label}
        </Link>
      ))}
      <Button
        variant="ghost"
        size="icon"
        className="ml-auto size-7"
        aria-label={t("Dismiss")}
        onClick={() => {
          localStorage.setItem(GETTING_STARTED_KEY, "1");
          setDismissed(true);
        }}
      >
        <X size={14} aria-hidden />
      </Button>
    </section>
  );
}

function SetupNotice() {
  const { reloadBoot } = useApp();
  const navigate = useNavigate();
  const status = useAsync(() => api.setupStatus(), []);
  const [error, setError] = useState<unknown>(null);
  const ready = status.data?.requiredComplete === true;
  // A server whose required steps are all done is set up: finish quietly,
  // with no banner, however it was configured.
  useEffect(() => {
    if (!ready) return;
    let cancelled = false;
    void (async () => {
      try {
        const completed = await api.setupComplete();
        sessionStorage.removeItem("ether.setup.step");
        sessionStorage.removeItem("ether.setup.skipped");
        if (cancelled) return;
        await reloadBoot();
        if (completed.cacheRestartRequired) navigate("/setup?resume=1");
      } catch (e) {
        if (!cancelled) setError(e);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [ready, reloadBoot, navigate]);
  if (!status.data && !status.error) return null;
  if (ready && !error) return null;
  return (
    <Alert variant="warning" className="shrink-0">
      <AlertTitle>{t("Setup isn't finished")}</AlertTitle>
      <AlertDescription>
        <p>{t("Your saved settings are kept. Continue with the remaining setup steps.")}</p>
        <Link className="font-medium underline" to="/setup?resume=1">
          {t("Continue setup")}
        </Link>
        <MutationError error={error} />
        {status.error ? (
          <ErrorPanel error={status.error} onRetry={() => void status.reload()} />
        ) : null}
      </AlertDescription>
    </Alert>
  );
}

/** How often the slow-moving panels (30-day activity, best rows) re-read. */
const OVERVIEW_POLL_MS = 30_000;

/**
 * Overview keeps itself current: the dashboard and the live streams arrive
 * over the console stream, and the slower panels re-read quietly while the
 * tab is visible. On a desktop window the page fits the height and each
 * panel scrolls inside itself; narrower screens stack and scroll the page.
 */
export default function Home() {
  useLiveDashboard();
  const { boot, dashboard, dashboardError, refreshDashboard, health } = useApp();
  const verdict = useServerVerdict();
  const activity = useAsync((_s) => api.activity(), []);
  const rows = useAsync((_s) => api.rowInsights(), []);
  const { locale } = useLocale();
  // Now playing reads the same live list as Activity, under the same rule.
  const live = useAsync((signal) => fetchLiveSessions(signal), [locale]);
  useLiveSessionsRefresh(live.reload);
  const reloadSlow = () => {
    void activity.reload();
    void rows.reload();
  };
  useSessionsChanged(reloadSlow);
  useInterval(reloadSlow, OVERVIEW_POLL_MS);
  const [playId, openPlay] = usePlayParam();
  const sessions = live.data ?? [];
  const activeSession = playId ? (sessions.find((s) => (s.id || s.key) === playId) ?? null) : null;
  const hw = dashboard?.sections.hwaccel as
    { hwaccel?: { name?: string }; ffmpeg?: string; error?: string } | undefined;

  return (
    <div className="@container flex min-h-0 flex-1 flex-col">
      <div className="app-scroll min-h-0 flex-1 overflow-y-auto">
        <div className="reveal mx-auto flex w-full max-w-(--breakpoint-2xl) flex-col gap-4 p-4 sm:p-6 @5xl:h-full @5xl:min-h-[41rem] @5xl:max-w-none">
          <h1 className="sr-only">{t("Overview")}</h1>
          {!boot.setupComplete ? <SetupNotice /> : <GettingStarted sessions={sessions} />}
          {dashboardError && !dashboard ? (
            <div className="shrink-0">
              <ErrorPanel error={dashboardError} onRetry={() => void refreshDashboard()} />
            </div>
          ) : null}

          <div className="grid items-start gap-4 @3xl:grid-cols-2 @5xl:min-h-0 @5xl:flex-1 @5xl:grid-cols-12 @5xl:grid-rows-[minmax(0,1fr)] @5xl:items-stretch">
            <div className="flex min-h-0 min-w-0 flex-col gap-4 @3xl:col-span-2 @5xl:col-span-5">
              <NowPlayingPanel
                loading={!live.data && !live.error}
                error={live.data ? null : live.error}
                onRetry={() => void live.reload()}
                sessions={sessions}
                onSelect={openPlay}
              />
              <ActivityPanel state={activity} className="@5xl:min-h-72 @5xl:flex-1 @5xl:basis-0" />
            </div>
            <ServerPanel hwaccel={hw?.hwaccel?.name} className="@5xl:col-span-4" />
            <div className="flex min-h-0 min-w-0 flex-col gap-4 @5xl:col-span-3">
              <HealthPanel loading={!dashboard} items={health} verdict={verdict} />
              <RowsPanel state={rows} className="@5xl:min-h-56 @5xl:flex-1 @5xl:basis-0" />
            </div>
          </div>
        </div>
        {/* Plugin panels sit below the fold so the built-in view always fits. */}
        <div className="mx-auto w-full max-w-(--breakpoint-2xl) px-4 pb-4 empty:hidden sm:px-6 sm:pb-6 @5xl:max-w-none">
          <PanelSlots slot="dashboard.panel" />
        </div>
      </div>
      <PlayDrawer id={playId} session={activeSession} onClose={() => openPlay(null)} />
    </div>
  );
}

function NowPlayingPanel({
  loading,
  error,
  onRetry,
  sessions,
  onSelect,
  className,
}: {
  loading: boolean;
  error: unknown;
  onRetry: () => void;
  sessions: NowPlaying[];
  onSelect: (id: string) => void;
  className?: string;
}) {
  const activeSessions = sessions.filter((s) => !isPausedSession(s));
  const pausedCount = sessions.length - activeSessions.length;
  const total = activeSessions.reduce((n, s) => n + s.speedBps, 0);
  return (
    <DashboardPanel
      title={t("Now playing")}
      icon={<PlayCircle size={16} />}
      className={className}
      bodyClassName="@5xl:overflow-y-auto"
      actions={
        <span className="flex items-center gap-2">
          {activeSessions.length ? (
            <Badge variant="info">
              {tn(activeSessions.length, "{count} stream", "{count} streams")}
              {pausedCount > 0 ? ` · ${t("{count} paused", { count: pausedCount })}` : ""}
              {total > 0 ? ` · ${formatRate(total)}` : ""}
            </Badge>
          ) : pausedCount > 0 ? (
            <Badge variant="warning">{t("{count} paused", { count: pausedCount })}</Badge>
          ) : null}
          <Link to="/activity/now" className="text-xs font-medium text-primary hover:underline">
            {t("Activity")}
          </Link>
        </span>
      }
    >
      {loading ? (
        <LoadingRows rows={2} />
      ) : error ? (
        <ErrorPanel error={error} onRetry={onRetry} />
      ) : sessions.length === 0 ? (
        <EmptyState
          icon={<MonitorPlay />}
          title={t("Nothing playing")}
          description={t("Streams appear here when someone presses play.")}
          className="py-6"
        />
      ) : (
        <ul className="space-y-3">
          {sessions.map((s) => (
            <SessionCard key={s.id || s.key} s={s} onOpen={() => onSelect(s.id || s.key)} />
          ))}
        </ul>
      )}
    </DashboardPanel>
  );
}

function SessionCard({ s, onOpen }: { s: NowPlaying; onOpen: () => void }) {
  const title = useTitle(s.key);
  const displayTitle = title?.title ?? s.title;
  return (
    <li
      onClick={(e) => {
        if ((e.target as Element | null)?.closest?.("button, a")) return;
        onOpen();
      }}
      className="relative cursor-pointer overflow-hidden rounded-lg border border-border transition hover:border-primary/40 has-[button[data-stream-open]:focus-visible]:ring-2 has-[button[data-stream-open]:focus-visible]:ring-ring/60"
    >
      <Artwork src={title?.backdrop} alt="" title="" className="absolute inset-0" icon={<span />} />
      <div className="scrim-left absolute inset-0" />
      <div className="relative flex gap-3 p-3">
        <SessionArtwork
          s={s}
          poster={title?.poster}
          posterTitle={s.title}
          className="aspect-[2/3] w-14 shrink-0 rounded-md shadow-md sm:w-16"
        />
        <div className="min-w-0 flex-1 text-on-scrim">
          <p className="truncate font-semibold">
            <button
              type="button"
              data-stream-open
              onClick={onOpen}
              className="max-w-full cursor-pointer truncate text-left hover:underline focus-visible:outline-none"
            >
              {displayTitle}
            </button>
          </p>
          {channelLabel(s) && channelLabel(s) !== displayTitle ? (
            <p className="truncate text-sm opacity-80">{channelLabel(s)}</p>
          ) : null}
          <p className="truncate text-sm opacity-80">
            {s.user} · {s.device}
            {s.product && s.product !== s.device ? ` (${s.product})` : ""}
          </p>
          <div className="mt-1.5 flex flex-wrap gap-1.5">
            {isPausedSession(s) ? <Badge variant="warning">{t("Paused")}</Badge> : null}
            <Badge
              variant={s.decision === "transcode" ? "warning" : "success"}
              title={s.decisionReason || undefined}
            >
              {s.decision === "transcode"
                ? t("Transcoding")
                : s.decision === "copy"
                  ? t("Direct stream")
                  : s.decision
                    ? t("Direct play")
                    : t("Starting")}
            </Badge>
            {s.origin ? <Badge variant="secondary">{s.origin}</Badge> : null}
            {s.remote ? <Badge variant="info">{t("Remote")}</Badge> : null}
            {s.failovers ? (
              <Badge variant="warning">
                {tn(s.failovers, "{count} failover", "{count} failovers")}
              </Badge>
            ) : null}
            {s.speedBps ? <Badge variant="secondary">{formatRate(s.speedBps)}</Badge> : null}
          </div>
          <div className="mt-2 max-w-md">
            {s.isLive ? (
              <p className="text-xs opacity-80">{t("Live")}</p>
            ) : s.progress !== null ? (
              <Meter
                onScrim
                value={s.progress}
                buffered={s.buffered}
                label={t("Started {time}", { time: relativeTime(s.startedAt) })}
                valueLabel={
                  s.buffered > s.progress
                    ? t("{watched} watched · {buffered} buffered", {
                        watched: formatPercent(s.progress),
                        buffered: formatPercent(s.buffered),
                      })
                    : t("{percent}% watched", { percent: Math.round(s.progress * 100) })
                }
              />
            ) : (
              <p className="text-xs opacity-80">{t("Waiting for playback position")}</p>
            )}
          </div>
          {s.release ? (
            <p className="mt-1.5 truncate font-mono text-[11px] opacity-70" title={s.release}>
              {s.release}
            </p>
          ) : null}
        </div>
      </div>
    </li>
  );
}

/**
 * Health at a glance: the verdict pill, anything that needs attention with
 * its reason and fix, then one line per healthy item. The pill opens the
 * full detail in a drawer.
 */
export function HealthPanel({
  loading,
  items,
  verdict,
  className,
}: {
  loading: boolean;
  items: HealthItem[];
  /** Defaults to the verdict these items give on their own. */
  verdict?: HealthVerdict;
  className?: string;
}) {
  const problems = healthProblems(items);
  const rest = items.filter((h) => !problems.includes(h));
  const severe = problems.some((h) => h.level === "error");
  return (
    <DashboardPanel
      title={t("Health")}
      icon={<HeartPulse size={16} />}
      className={className}
      bodyClassName="px-2 py-2 @5xl:overflow-y-auto"
      actions={
        <HealthPill items={items} verdict={verdict ?? healthVerdict(items, { pending: loading })} />
      }
    >
      {loading ? (
        <LoadingRows rows={4} />
      ) : (
        <>
          {problems.length ? (
            <section
              role="region"
              aria-label={t("Needs attention")}
              className={cn(
                "mb-1.5 rounded-lg p-1",
                severe ? "bg-danger-subtle" : "bg-warning-subtle",
              )}
            >
              <ul className="space-y-0.5">
                {problems.map((h) => (
                  <HealthRow key={h.id} h={h} fix />
                ))}
              </ul>
            </section>
          ) : null}
          <ul>
            {rest.map((h) => (
              <HealthRow key={h.id} h={h} />
            ))}
          </ul>
        </>
      )}
    </DashboardPanel>
  );
}

const LEVEL_TEXT: Record<HealthItem["level"], string> = localized(() => ({
  ok: t("Healthy"),
  warn: t("Degraded"),
  error: t("Failing"),
  off: t("Off"),
  unknown: t("Unknown"),
}));

const LEVEL_TONE: Record<HealthItem["level"], string> = {
  ok: "text-success",
  warn: "text-warning",
  error: "text-danger",
  off: "text-fg-subtle",
  unknown: "text-fg-subtle",
};

/**
 * One health item. A healthy one is a single line; anything else also says
 * why. The whole row leads to the page that manages it.
 */
function HealthRow({ h, fix = false }: { h: HealthItem; fix?: boolean }) {
  const explain = fix || (h.level !== "ok" && h.level !== "off");
  const more = [explain ? "" : h.summary, ...h.details].filter(Boolean).join("\n");
  return (
    <li className="relative rounded-md px-2 py-1 hover:bg-surface-hover" title={more || undefined}>
      <span className="flex items-center gap-2.5">
        <StatusDot level={h.level} pulse={fix} />
        <Link
          to={h.href}
          className="shrink-0 text-sm font-medium text-fg after:absolute after:inset-0 focus-visible:outline-none focus-visible:after:rounded-md focus-visible:after:ring-2 focus-visible:after:ring-ring/50"
        >
          {h.label}
        </Link>
        <span className="min-w-0 flex-1">
          {h.fill !== undefined ? (
            <Meter
              value={h.fill}
              tone={h.fill > 0.95 && h.id !== "cache" ? "warning" : "primary"}
            />
          ) : null}
        </span>
        {fix ? (
          <Link
            to={h.href}
            className="relative z-10 shrink-0 text-xs font-medium text-primary hover:underline"
          >
            {t("Fix")}
          </Link>
        ) : (
          <span className={cn("shrink-0 text-xs font-medium", LEVEL_TONE[h.level])}>
            {h.statusLabel ?? LEVEL_TEXT[h.level]}
          </span>
        )}
      </span>
      {explain ? (
        <span className="block text-pretty pl-5 text-xs text-fg-muted">{h.summary}</span>
      ) : null}
    </li>
  );
}

/** A headline figure inside a panel: label, value, one line of context. */
function Readout({ label, value, detail }: { label: string; value: ReactNode; detail: ReactNode }) {
  return (
    <div className="min-w-0 border-l border-border-subtle pl-3 first:border-l-0 first:pl-0">
      <dt className="truncate text-[11px] font-medium uppercase tracking-wide text-fg-subtle">
        {label}
      </dt>
      <dd className="truncate text-xl font-semibold text-fg">{value}</dd>
      <dd className="truncate text-xs text-fg-muted">{detail}</dd>
    </div>
  );
}

function ActivityPanel({
  state,
  className,
}: {
  state: ReturnType<typeof useAsync<ActivityDay[]>>;
  className?: string;
}) {
  const [metric, setMetric] = useState<"plays" | "hours">("plays");
  const [asTable, setAsTable] = useState(false);
  const days = state.data ?? [];
  const totals = useMemo(() => {
    const plays = days.reduce((n, x) => n + x.plays, 0);
    const hasWatchedMs = days.some((x) => x.watchedMs !== undefined);
    const hours = hasWatchedMs
      ? hoursOf(days.reduce((n, x) => n + (x.watchedMs ?? 0), 0))
      : days.reduce((n, x) => n + x.hours, 0);
    const peakViewers = days.reduce((n, x) => Math.max(n, x.uniqueViewers), 0);
    const half = Math.floor(days.length / 2);
    const recent = days.slice(half).reduce((n, x) => n + x.plays, 0);
    const earlier = days.slice(0, half).reduce((n, x) => n + x.plays, 0);
    return { plays, hours, peakViewers, change: earlier > 0 ? (recent - earlier) / earlier : null };
  }, [days]);
  const data = days.map((d) => ({
    key: d.date,
    label: new Date(`${d.date}T12:00:00`).toLocaleDateString(getLocale(), {
      month: "short",
      day: "numeric",
    }),
    value: d[metric],
    detail:
      metric === "plays"
        ? `${t("{hours} h", { hours: formatNumber(d.hours, 1) })} · ${tn(d.uniqueViewers, "{count} viewer", "{count} viewers")}`
        : `${tn(d.plays, "{count} play", "{count} plays")} · ${tn(d.uniqueViewers, "{count} viewer", "{count} viewers")}`,
  }));
  // A failed quiet refresh keeps the last good figures instead of an error.
  const error = state.data ? null : state.error;
  const na = error ? "—" : undefined;
  return (
    <DashboardPanel
      title={t("Activity · last 30 days")}
      icon={<BarChart3 size={16} />}
      className={cn(asTable && "overflow-clip", className)}
      bodyClassName="flex flex-col gap-3"
      actions={
        <div className="flex items-center gap-1">
          <div
            role="radiogroup"
            aria-label={t("Metric")}
            className="inline-flex rounded-md border border-border bg-bg-subtle p-0.5"
          >
            {(["plays", "hours"] as const).map((m) => (
              <button
                key={m}
                type="button"
                role="radio"
                aria-checked={metric === m}
                onClick={() => setMetric(m)}
                className={`rounded px-2 py-0.5 text-xs font-medium ${metric === m ? "bg-surface text-fg shadow-sm" : "text-fg-muted hover:text-fg"}`}
              >
                {m === "plays" ? t("Plays") : t("Hours")}
              </button>
            ))}
          </div>
          <Button
            variant="ghost"
            size="sm"
            className="h-7 px-2"
            onClick={() => setAsTable((on) => !on)}
            aria-pressed={asTable}
            title={asTable ? t("Show chart") : t("Show as table")}
          >
            {asTable ? <BarChart3 size={14} /> : <Table2 size={14} />}
            <span className="sr-only">{asTable ? t("Show chart") : t("Show as table")}</span>
          </Button>
        </div>
      }
    >
      {state.loading && !state.data ? (
        <Skeleton className="h-44 w-full" />
      ) : (
        <>
          <dl className="grid shrink-0 grid-cols-3 gap-3">
            <Readout
              label={t("Plays · 30 days")}
              value={na ?? formatNumber(totals.plays, 0)}
              detail={
                totals.change === null
                  ? t("Across every viewer")
                  : t("{change} vs. prior", {
                      change: `${totals.change >= 0 ? "+" : "−"}${formatPercent(Math.abs(totals.change))}`,
                    })
              }
            />
            <Readout
              label={t("Hours watched")}
              value={na ?? fixed(totals.hours, 1)}
              detail={t("Last 30 days")}
            />
            <Readout
              label={t("Busiest day")}
              value={na ?? tn(totals.peakViewers, "{count} viewer", "{count} viewers")}
              detail={t("Peak in one day")}
            />
          </dl>
          {error ? (
            <ErrorPanel error={error} onRetry={() => void state.reload()} />
          ) : days.length === 0 ? (
            <EmptyState
              icon={<Activity />}
              title={t("No plays yet")}
              description={t("Activity appears after the first title is watched.")}
              className="py-6"
            />
          ) : asTable ? (
            <Table containerClassName="overflow-visible rounded-none border-0 shadow-none @5xl:min-h-0 @5xl:flex-1 @5xl:overflow-y-auto">
              <TableHeader className="sticky top-0 z-10 bg-surface">
                <TableRow>
                  <TableHead>{t("Day")}</TableHead>
                  <TableHead className="text-right">{t("Plays")}</TableHead>
                  <TableHead className="text-right">{t("Hours")}</TableHead>
                  <TableHead className="text-right">{t("Viewers")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {[...days].reverse().map((d) => (
                  <TableRow key={d.date}>
                    <TableCell>{d.date}</TableCell>
                    <TableCell className="text-right tabular-nums">{d.plays}</TableCell>
                    <TableCell className="text-right tabular-nums">
                      {formatNumber(d.hours, 1)}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{d.uniqueViewers}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ) : (
            <TimeBars
              fill
              height={176}
              className="h-44 @5xl:h-auto @5xl:min-h-24 @5xl:flex-1"
              data={data}
              ariaLabel={
                metric === "plays"
                  ? t("Plays per day over the last 30 days")
                  : t("Hours watched per day over the last 30 days")
              }
              valueFormat={(v) => formatNumber(v, metric === "hours" ? 1 : 0)}
            />
          )}
        </>
      )}
    </DashboardPanel>
  );
}

/** The most Best rows the fitted layout can show; a stacked page stops at six. */
const ROWS_SHOWN_FITTED = 12;

function RowsPanel({
  state,
  className,
}: {
  state: ReturnType<typeof useAsync<RowInsight[]>>;
  className?: string;
}) {
  const rows = (state.data ?? []).slice(0, ROWS_SHOWN_FITTED);
  const maxRate = Math.max(0.0001, ...rows.map((r) => (r.lowSample ? 0 : r.playRate)));
  return (
    <DashboardPanel
      title={t("Best rows")}
      icon={<TrendingUp size={16} />}
      className={className}
      bodyClassName="flex flex-col"
      actions={
        <Link
          to="/activity/recommendations"
          className="text-xs font-medium text-primary hover:underline"
        >
          {t("Recommendations")}
        </Link>
      }
    >
      {state.error && !state.data ? (
        <ErrorPanel error={state.error} onRetry={() => void state.reload()} />
      ) : state.loading && !state.data ? (
        <LoadingRows rows={4} />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={<Film />}
          title={t("No row data yet")}
          description={t("Play a title to see it here.")}
          className="py-6"
        />
      ) : (
        // Fitted to the window, the list shows as many whole rows as there is
        // room for: a row that would be cut off wraps out of sight instead.
        <ol
          className={cn(
            "flex flex-col gap-y-2.5 @max-5xl:[&>li:nth-child(n+7)]:hidden",
            "@5xl:min-h-0 @5xl:flex-1 @5xl:flex-wrap @5xl:content-start @5xl:gap-x-8 @5xl:overflow-hidden",
          )}
        >
          {rows.map((r, i) => {
            const rate = r.lowSample ? null : r.playRate;
            return (
              <li
                key={r.hubId}
                className="w-full"
                title={tn(
                  r.plays7d,
                  "{count} play from {impressions} impressions in 7 days",
                  "{count} plays from {impressions} impressions in 7 days",
                  { impressions: formatNumber(r.impressions7d, 0) },
                )}
              >
                <div className="mb-1 flex items-baseline justify-between gap-2 text-sm">
                  <span className="min-w-0 truncate text-fg">
                    <span className="mr-1.5 tabular-nums text-fg-subtle">{i + 1}</span>
                    {r.title}
                  </span>
                  <span className="shrink-0 tabular-nums text-fg-muted">
                    {rate !== null ? formatPercent(rate, 1) : t("Not enough data yet")}
                  </span>
                </div>
                <div className="h-1.5 rounded-full bg-bg-subtle">
                  <div
                    className="h-full rounded-full bg-primary"
                    style={{ width: `${rate !== null ? (rate / maxRate) * 100 : 0}%` }}
                  />
                </div>
                <p className="mt-0.5 text-[11px] text-fg-subtle">
                  {tn(r.plays7d, "{count} play", "{count} plays")} ·{" "}
                  {tn(r.impressions7d, "{count} impression", "{count} impressions")}
                </p>
              </li>
            );
          })}
        </ol>
      )}
    </DashboardPanel>
  );
}

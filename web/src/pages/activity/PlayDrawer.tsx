import { Badge, Switch } from "@braedonsaunders/appkit-ui";
import {
  AlertCircle,
  AlertTriangle,
  ArrowRight,
  Bug,
  CheckCircle2,
  Info,
  XCircle,
} from "lucide-react";
import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { Artwork } from "../../components/Artwork";
import { Meter } from "../../components/charts";
import { CopyButton } from "../../components/CopyButton";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Kpi } from "../../components/Kpi";
import { SubtabNav } from "../../components/layout";
import { LiveChannelArtwork, SessionArtwork } from "../../components/SessionArtwork";
import { getLocale, t, tn } from "../../i18n";
import { apiFetch } from "../../lib/api";
import {
  fixed,
  formatBitrate,
  formatBytes,
  formatDateTime,
  formatPercent,
  relativeTime,
} from "../../lib/format";
import {
  channelLabel,
  decisionLabel as liveDecisionLabel,
  type NowPlaying as Stream,
} from "../../lib/health";
import { useAsync, useInterval } from "../../lib/hooks";
import { qualityFallback } from "../../lib/quality-fallback";
import { useTitle } from "../../lib/titles";
import { ToneValue } from "./ExperienceDrawer";
import {
  DASH,
  decisionLabel as historyDecisionLabel,
  failedReason,
  formatSeconds,
  reasonPhrase,
  startTone,
} from "./experience-format";
import { LiveProvider } from "./LiveProvider";
import type { PlayDetail, PlayTimeline, TimelineEntry, TimelineLevel } from "./types";

type PlayDrawerTab = "overview" | "timeline";

export const clockOf = (ms: number) => {
  const total = Math.max(0, Math.round(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const sec = String(total % 60).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${sec}` : `${m}:${sec}`;
};

/** Offset from session start (`+0.0 s`, `+2.4 s`, `+1m 12.0s`). */
export function formatTimelineOffset(offsetMs: number): string {
  const sec = Math.max(0, offsetMs) / 1000;
  if (sec < 60) {
    return `+${sec.toFixed(1)} s`;
  }
  const mins = Math.floor(sec / 60);
  const rem = sec - mins * 60;
  return `+${mins}m ${rem.toFixed(1)}s`;
}

function levelIcon(entry: TimelineEntry) {
  switch (entry.level) {
    case "error":
      return <XCircle size={15} className="shrink-0 text-danger" aria-label={t("Error")} />;
    case "warn":
      return (
        <AlertTriangle size={15} className="shrink-0 text-warning" aria-label={t("Warning")} />
      );
    case "debug":
      return <Bug size={15} className="shrink-0 text-fg-subtle" aria-label={t("Diagnostic")} />;
    default:
      if (entry.kind === "completed") {
        return (
          <CheckCircle2 size={15} className="shrink-0 text-success" aria-label={t("Completed")} />
        );
      }
      return <Info size={15} className="shrink-0 text-info" aria-label={t("Info")} />;
  }
}

function levelTag(level: TimelineLevel): string {
  switch (level) {
    case "error":
      return "ERROR";
    case "warn":
      return "WARN";
    case "debug":
      return "DEBUG";
    default:
      return "INFO";
  }
}

function formatTimelineCopy(
  title: string,
  startedAt: Date | null,
  failureCause: string,
  timeline: PlayTimeline,
): string {
  const lines: string[] = [title];
  if (startedAt) {
    lines.push(`${t("Started")}: ${formatDateTime(startedAt)}`);
  }
  if (failureCause) {
    lines.push(`${t("Playback failed")}: ${failureCause}`);
  }
  lines.push("");
  for (const e of timeline.entries) {
    const kind = e.type === "log" ? " (log)" : "";
    const detail = e.detail ? ` — ${e.detail}` : "";
    lines.push(
      `${formatTimelineOffset(e.offsetMs)} [${levelTag(e.level)}]${kind} ${e.message}${detail}`,
    );
  }
  return lines.join("\n");
}

/** Watched share, plus the read-ahead buffer when it runs past the playhead. */
export function progressLabel(progress: number, buffered: number) {
  return buffered > progress
    ? t("{watched} watched · {buffered} buffered", {
        watched: formatPercent(progress),
        buffered: formatPercent(buffered),
      })
    : t("{percent}% watched", { percent: Math.round(progress * 100) });
}

/** Source → output, or just the source when it's passed through unchanged. */
export function Flow({ from, to }: { from: string; to: string }) {
  if (!from && !to) return null;
  if (!to || to === from) return <>{from}</>;
  return (
    <span>
      <span className="text-fg-muted">{from || DASH}</span>
      <ArrowRight size={12} className="mx-1 inline text-fg-subtle" aria-label={t("becomes")} />
      <span>{to}</span>
    </span>
  );
}

/** Deep-linkable `?play=` query parameter shared by Overview, Now playing and History. */
export function usePlayParam(): [string | null, (id: string | null) => void] {
  const [params, setParams] = useSearchParams();
  const playId = params.get("play");
  const openPlay = (id: string | null) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (id) next.set("play", id);
        else next.delete("play");
        return next;
      },
      { preventScrollReset: true },
    );
  return [playId, openPlay];
}

function Detail({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[9rem_1fr] gap-3 py-2 text-sm">
      <dt className="text-fg-muted">{label}</dt>
      <dd className="min-w-0 break-words text-fg">{children}</dd>
    </div>
  );
}

const isStreamConverting = (s: Stream) =>
  s.decision === "transcode" || (s.decision === "copy" && !s.audioCopy);

function TimelinePanel({
  id,
  live,
  play,
  displayTitle,
  startedAt,
}: {
  id: string;
  live: Stream | null;
  play: PlayDetail | null;
  displayTitle: string;
  startedAt: Date | null;
}) {
  const [showAll, setShowAll] = useState(false);
  useEffect(() => {
    setShowAll(false);
  }, [id]);

  const timeline = useAsync(
    (signal) =>
      apiFetch<PlayTimeline>(`/activity/plays/${encodeURIComponent(id)}/timeline`, { signal }),
    [id],
  );
  useInterval(() => void timeline.reload(), live ? 5_000 : null);

  const tl = timeline.data && timeline.data.playId === id ? timeline.data : null;
  if (timeline.error && !tl) {
    return <ErrorPanel error={timeline.error} onRetry={() => void timeline.reload()} />;
  }
  if (!tl) {
    return <LoadingRows rows={4} />;
  }

  const hasFailure = Boolean(tl.failed || play?.failed || tl.error || play?.error);
  const failureCause = tl.error || play?.error || (hasFailure ? failedReason("") : "");
  const entries = tl.entries ?? [];
  const visibleEntries = entries.filter(
    (entry) =>
      showAll || entry.type === "event" || entry.level === "warn" || entry.level === "error",
  );
  const hiddenCount = entries.length - visibleEntries.length;

  return (
    <div className="space-y-4">
      {hasFailure ? (
        <div
          role="alert"
          className="flex items-start gap-2.5 rounded-xl border border-danger/30 bg-danger/10 px-4 py-3 text-sm"
        >
          <AlertCircle size={16} className="mt-0.5 shrink-0 text-danger" aria-hidden="true" />
          <div className="min-w-0 space-y-0.5">
            <p className="font-medium text-danger">{t("Playback failed")}</p>
            <p className="break-words text-fg">{failureCause}</p>
          </div>
        </div>
      ) : null}

      {entries.length === 0 ? (
        <div className="rounded-xl border border-border bg-surface px-4 py-6 text-center text-sm text-fg-muted">
          {t("No timeline was recorded for this play.")}
        </div>
      ) : (
        <section
          aria-label={t("Events and session logs")}
          className="overflow-hidden rounded-xl border border-border bg-surface"
        >
          <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-4 py-3">
            <div className="min-w-0">
              <h3 className="text-sm font-semibold text-fg">{t("Events and session logs")}</h3>
              {!showAll && hiddenCount > 0 ? (
                <p className="text-xs text-fg-muted">
                  {tn(
                    hiddenCount,
                    "{count} routine log line hidden",
                    "{count} routine log lines hidden",
                  )}
                </p>
              ) : tl.capped ? (
                <p className="text-xs text-fg-muted">{t("Showing opening and final entries")}</p>
              ) : null}
            </div>
            <div className="flex flex-wrap items-center gap-3">
              <label className="inline-flex cursor-pointer items-center gap-2 text-xs font-medium text-fg">
                <Switch
                  aria-label={t("Show all")}
                  checked={showAll}
                  onChange={(e) => setShowAll(e.currentTarget.checked)}
                />
                <span>{t("Show all")}</span>
              </label>
              <CopyButton
                value={formatTimelineCopy(displayTitle, startedAt, failureCause, tl)}
                label={t("Copy timeline")}
              />
            </div>
          </div>

          <div className="px-4 py-4">
            {visibleEntries.length === 0 ? (
              <p className="text-sm text-fg-muted">
                {t("No warnings or errors were logged. Turn on Show all to inspect session logs.")}
              </p>
            ) : (
              <ol
                aria-label={t("Playback timeline")}
                className="relative ml-2 space-y-3 border-l border-border pl-4"
              >
                {visibleEntries.map((entry) => {
                  const isLog = entry.type === "log";
                  return (
                    <li key={entry.seq} className="relative text-sm">
                      <span
                        aria-hidden="true"
                        className="absolute -left-[1.35rem] top-1.5 h-2 w-2 rounded-full bg-border"
                      />
                      <div className="flex items-start gap-2.5">
                        <span className="mt-0.5">{levelIcon(entry)}</span>
                        <div className="min-w-0 flex-1 space-y-0.5">
                          <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                            <span className="font-mono text-xs tabular-nums text-fg-subtle">
                              {formatTimelineOffset(entry.offsetMs)}
                            </span>
                            <span
                              className={
                                isLog
                                  ? "break-words font-mono text-xs text-fg-muted"
                                  : entry.level === "error"
                                    ? "break-words font-medium text-danger"
                                    : "break-words font-medium text-fg"
                              }
                            >
                              {entry.message}
                            </span>
                          </div>
                          {entry.detail ? (
                            <p className="break-words font-mono text-xs text-fg-muted">
                              {entry.detail}
                            </p>
                          ) : null}
                        </div>
                      </div>
                    </li>
                  );
                })}
              </ol>
            )}
          </div>
        </section>
      )}
    </div>
  );
}

/**
 * One recorded or currently streaming play, opened from Overview, Activity › Now
 * playing, Activity › History, or a `?play=` link.
 */
export function PlayDrawer({
  id,
  session,
  onClose,
}: {
  id: string | null;
  session?: Stream | null;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<PlayDrawerTab>("overview");
  useEffect(() => {
    setTab("overview");
  }, [id]);

  const play = useAsync(
    (signal) =>
      id
        ? apiFetch<PlayDetail>(`/stats/plays/${encodeURIComponent(id)}`, { signal })
        : Promise.resolve(null),
    [id],
  );
  useInterval(() => void play.reload(), id && session ? 10_000 : null);

  const p = play.data && play.data.id === id ? play.data : null;
  const live = id && session ? session : null;
  const isLivePlay = Boolean(live?.isLive || p?.channelId || p?.channelName);
  const ratingKey = live?.ratingKey || p?.ratingKey;
  const titleKey =
    !isLivePlay && ratingKey && ratingKey > 0
      ? String(ratingKey)
      : !isLivePlay && live?.key && live.key !== "0"
        ? live.key
        : undefined;
  const media = useTitle(id ? titleKey : undefined);

  const displayTitle =
    (live ? media?.title : undefined) ||
    live?.title ||
    (isLivePlay ? p?.channelName || p?.title : p?.title) ||
    t("Play");
  const subtitle =
    media?.type === "episode" && media.showTitle
      ? `${media.showTitle} · ${t("S{season} E{episode}", { season: media.season ?? 0, episode: media.episode ?? 0 })}`
      : media?.year
        ? String(media.year)
        : isLivePlay && p?.title && p.title !== displayTitle
          ? p.title
          : "";
  const channel = live
    ? channelLabel(live)
    : p?.channelName || p?.channelNumber
      ? channelLabel({ channelName: p.channelName ?? "", channelNumber: p.channelNumber ?? "" })
      : "";

  const converting = live ? isStreamConverting(live) : p?.decision === "transcode";
  const decision = live ? liveDecisionLabel(live) : p ? historyDecisionLabel(p.decision) : "";
  const why = p && p.decision !== "directplay" ? reasonPhrase(p.reasonKind, p.reasonDetail) : "";

  const viewer = live?.user || p?.username || t("Unknown viewer");
  const userId = live?.userId || p?.userId || 0;
  const device = live?.device || p?.device || p?.platform || t("Unknown");
  const platform = live?.platform || p?.platform || "";
  const product = live?.product && live.product !== device ? live.product : "";
  const secondaryDevice = [
    product,
    platform && platform !== device && platform !== product ? platform : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const remote = live ? live.remote : Boolean(p?.remote);
  const ip = live?.ip || p?.ip || "";
  const networkReason = live?.networkReason || p?.networkReason || "";
  const origin = live?.origin || p?.origin || "";
  const release = live?.release || p?.release || "";
  const fallback = live?.qualityFallback || p?.qualityFallback;
  const bytes = Math.max(live?.bytesServed ?? 0, p?.bytes ?? 0);
  const switches = Math.max(live?.failovers ?? 0, p?.switches ?? p?.failovers ?? 0);
  const sourceKbps = live?.sourceKbps || p?.sourceKbps || 0;
  const uhd = Boolean(p?.uhd || (live && /\b(2160p|4k)\b/i.test(live.sourceVideo)));
  const hdr = Boolean(
    p?.hdr || (live && /\b(hdr10\+?|hdr|dolby vision|hlg)\b/i.test(live.sourceVideo)),
  );

  const measured = Boolean(
    p?.measured || live?.startMs !== undefined || live?.bufferEvents !== undefined,
  );
  const startMs = p?.measured && p.startMs != null ? p.startMs : (live?.startMs ?? null);
  const bufferEvents =
    p?.measured && p.bufferEvents != null
      ? Math.max(p.bufferEvents, live?.bufferEvents ?? 0)
      : (live?.bufferEvents ?? null);
  const bufferMs =
    p?.measured && p.bufferMs != null
      ? Math.max(p.bufferMs, live?.bufferMs ?? 0)
      : (live?.bufferMs ?? null);

  const startedAt = live?.startedAt ?? (p?.started ? new Date(p.started * 1000) : null);
  const elapsed =
    live && live.progress !== null && live.durationMs ? live.progress * live.durationMs : null;

  const description = live
    ? live.startedAt
      ? t("Started {time}", { time: relativeTime(live.startedAt) })
      : startedAt
        ? startedAt.toLocaleString(getLocale())
        : t("Streaming now")
    : p
      ? new Date(p.started * 1000).toLocaleString(getLocale())
      : undefined;

  return (
    <Drawer
      open={id != null}
      onClose={onClose}
      size="md"
      title={displayTitle}
      documentTitle={displayTitle}
      description={description}
      subtabs={
        id ? (
          <div className="[&>div]:mb-0">
            <SubtabNav
              ariaLabel={t("Play sections")}
              active={tab}
              onSelect={(k) => setTab(k as PlayDrawerTab)}
              tabs={[
                { key: "overview", label: t("Overview") },
                { key: "timeline", label: t("Timeline") },
              ]}
            />
          </div>
        ) : undefined
      }
    >
      {tab === "timeline" && id ? (
        <TimelinePanel
          id={id}
          live={live}
          play={p}
          displayTitle={displayTitle}
          startedAt={startedAt}
        />
      ) : !live && play.error && !p ? (
        <ErrorPanel error={play.error} onRetry={() => void play.reload()} />
      ) : !live && !p ? (
        <LoadingRows rows={4} />
      ) : (
        <div className="space-y-5">
          <div className="flex items-center gap-3">
            {live ? (
              <SessionArtwork
                s={live}
                poster={media?.poster}
                posterTitle={displayTitle}
                className="aspect-[2/3] w-14 shrink-0 rounded-md"
              />
            ) : isLivePlay ? (
              <LiveChannelArtwork
                channelId={p?.channelId}
                name={p?.channelName || p?.channelNumber || displayTitle}
                className="aspect-[2/3] w-14 shrink-0 rounded-md"
              />
            ) : (
              <Artwork
                src={media?.poster}
                alt={displayTitle}
                title={displayTitle}
                className="aspect-[2/3] w-14 shrink-0 rounded-md"
              />
            )}
            <div className="min-w-0 flex-1 space-y-1.5">
              <div className="flex flex-wrap gap-1">
                {live ? (
                  <Badge variant="info">{live.isLive ? t("Live") : t("Streaming now")}</Badge>
                ) : (
                  <>
                    {isLivePlay ? <Badge variant="info">{t("Live TV")}</Badge> : null}
                    <Badge variant={p!.completed ? "success" : "secondary"}>
                      {p!.completed ? t("Completed") : p!.ended ? t("Ended") : t("Last observed")}
                    </Badge>
                  </>
                )}
                {!live && p?.failed ? (
                  <Badge variant="destructive">{t("Never started")}</Badge>
                ) : null}
                {decision ? (
                  <Badge variant={live ? (converting ? "warning" : "success") : "secondary"}>
                    {decision}
                  </Badge>
                ) : null}
                {live?.bitrateCapSource === "upload_budget" ? (
                  <Badge variant="info">{t("Upload budget")}</Badge>
                ) : null}
                {uhd ? <Badge variant="secondary">4K</Badge> : null}
                {hdr ? <Badge variant="secondary">HDR</Badge> : null}
                {live && switches ? (
                  <Badge variant="warning">
                    {tn(switches, "{count} failover", "{count} failovers")}
                  </Badge>
                ) : null}
              </div>
              {channel && channel !== displayTitle ? (
                <p className="truncate text-xs text-fg-muted">{channel}</p>
              ) : null}
              <LiveProvider
                provider={
                  live?.liveProvider ??
                  (p?.liveSourceName
                    ? {
                        sourceName: p.liveSourceName,
                        loginLabel: p.liveLoginLabel,
                        loginNumber: p.liveLoginNumber,
                      }
                    : undefined)
                }
                previous={live?.previousLiveProvider}
              />
              {subtitle ? <p className="truncate text-xs text-fg-muted">{subtitle}</p> : null}
              <p className="text-sm text-fg-muted">
                {viewer} · {device}
              </p>
            </div>
          </div>

          {live ? (
            live.isLive ? (
              <p className="text-xs text-fg-muted">{t("Live")}</p>
            ) : live.progress !== null ? (
              <Meter
                value={live.progress}
                buffered={live.buffered}
                label={
                  elapsed !== null
                    ? t("{elapsed} of {total}", {
                        elapsed: clockOf(elapsed),
                        total: clockOf(live.durationMs),
                      })
                    : undefined
                }
                valueLabel={progressLabel(live.progress, live.buffered)}
              />
            ) : (
              <p className="text-xs text-fg-muted">{t("Waiting for playback position")}</p>
            )
          ) : null}

          {fallback ? <p className="text-sm text-fg-muted">{qualityFallback(fallback)}</p> : null}

          {live ? (
            <div className="grid grid-cols-2 gap-3">
              <Kpi
                label={t("Bitrate")}
                value={
                  live.bitrateKbps
                    ? formatBitrate(live.bitrateKbps * 1000)
                    : p?.avgBitrateKbps
                      ? t("{rate} Mbps", { rate: fixed(p.avgBitrateKbps / 1000, 1) })
                      : DASH
                }
                detail={
                  live.bitrateCapSource === "upload_budget" && live.bitrateCapKbps
                    ? t("Shared upload budget ({rate} cap)", {
                        rate: formatBitrate(live.bitrateCapKbps * 1000),
                      })
                    : sourceKbps
                      ? t("Source {rate} Mbps", { rate: fixed(sourceKbps / 1000, 1) })
                      : undefined
                }
              />
              <Kpi
                label={t("Throughput")}
                value={formatBitrate((live.speedBps || 0) * 8)}
                detail={bytes ? formatBytes(bytes) : undefined}
              />
              <Kpi
                label={t("Transferred")}
                value={bytes ? formatBytes(bytes) : DASH}
                detail={
                  p?.watchedMs
                    ? t("{minutes} min", { minutes: fixed(p.watchedMs / 60000, 0) })
                    : undefined
                }
              />
              <Kpi
                label={t("Start time")}
                value={
                  startMs != null ? (
                    <ToneValue value={formatSeconds(startMs)} tone={startTone(startMs)} />
                  ) : (
                    DASH
                  )
                }
              />
            </div>
          ) : p ? (
            <div className="grid grid-cols-2 gap-3">
              <Kpi
                label={t("Watched")}
                value={t("{minutes} min", { minutes: fixed(p.watchedMs / 60000, 0) })}
                detail={
                  !isLivePlay && p.durationMs
                    ? t("{percent} of {length}", {
                        percent: formatPercent(Math.min(1, p.watchedMs / p.durationMs)),
                        length: t("{minutes} min", { minutes: fixed(p.durationMs / 60000, 0) }),
                      })
                    : undefined
                }
              />
              <Kpi label={t("Transferred")} value={p.bytes ? formatBytes(p.bytes) : DASH} />
              <Kpi
                label={t("Average bitrate")}
                value={
                  p.avgBitrateKbps
                    ? t("{rate} Mbps", { rate: fixed(p.avgBitrateKbps / 1000, 1) })
                    : DASH
                }
                detail={
                  p.sourceKbps
                    ? t("Source {rate} Mbps", { rate: fixed(p.sourceKbps / 1000, 1) })
                    : undefined
                }
              />
              <Kpi
                label={t("Start time")}
                value={
                  p.failed ? (
                    <span className="text-danger">{t("Failed")}</span>
                  ) : p.measured ? (
                    <ToneValue value={formatSeconds(p.startMs)} tone={startTone(p.startMs)} />
                  ) : (
                    DASH
                  )
                }
                detail={p.failed ? failedReason(p.error) : undefined}
              />
            </div>
          ) : null}

          <section aria-label={t("Viewer and network")}>
            <h3 className="mb-1 text-xs font-semibold uppercase tracking-wider text-fg-subtle">
              {t("Viewer and network")}
            </h3>
            <dl className="divide-y divide-border">
              <Detail label={t("Viewer")}>
                {userId ? (
                  <Link
                    to={`/activity/experience?viewer=${userId}`}
                    className="text-primary hover:underline"
                  >
                    {viewer}
                  </Link>
                ) : (
                  viewer
                )}
              </Detail>
              <Detail label={t("Device")}>
                {device}
                {secondaryDevice ? (
                  <span className="block text-xs text-fg-muted">{secondaryDevice}</span>
                ) : null}
              </Detail>
              {live?.protocol ? <Detail label={t("App")}>{live.protocol}</Detail> : null}
              <Detail label={t("Network")}>
                {remote ? t("Away from home") : t("At home")}
                {ip ? <span className="block font-mono text-xs text-fg-muted">{ip}</span> : null}
                {networkReason ? (
                  <span className="block text-xs text-fg-muted">{networkReason}</span>
                ) : null}
              </Detail>
            </dl>
          </section>

          <section aria-label={t("Playback")}>
            <h3 className="mb-1 text-xs font-semibold uppercase tracking-wider text-fg-subtle">
              {t("Playback")}
            </h3>
            <dl className="divide-y divide-border">
              <Detail label={t("Delivered as")}>
                {decision || DASH}
                {why ? (
                  <span className="block text-xs text-fg-muted">
                    {t("Converted because of {reason}", { reason: why })}
                  </span>
                ) : null}
                {live && live.decision !== "directplay" && live.decisionReason ? (
                  <span className="block text-xs text-fg-muted">{live.decisionReason}</span>
                ) : null}
              </Detail>
              {live?.bitrateCapSource === "upload_budget" && live.bitrateCapKbps ? (
                <Detail label={t("Bitrate cap")}>
                  <span>{t("Shared upload budget")}</span>
                  <span className="block text-xs text-fg-muted">
                    {t("Capped to {rate} to share remote upload capacity", {
                      rate: formatBitrate(live.bitrateCapKbps * 1000),
                    })}
                  </span>
                </Detail>
              ) : null}
              {live && (live.sourceVideo || live.outputVideo) ? (
                <Detail label={t("Video")}>
                  <Flow from={live.sourceVideo} to={live.outputVideo} />
                </Detail>
              ) : null}
              {live && (live.sourceAudio || live.outputAudio) ? (
                <Detail label={t("Audio")}>
                  <Flow from={live.sourceAudio} to={live.outputAudio} />
                </Detail>
              ) : null}
              {live?.container ? <Detail label={t("Container")}>{live.container}</Detail> : null}
              {live?.subtitle ? <Detail label={t("Subtitles")}>{live.subtitle}</Detail> : null}
              <Detail label={t("Buffering")}>
                {measured && bufferEvents != null
                  ? bufferEvents
                    ? tn(bufferEvents, "{count} time, {time}", "{count} times, {time}", {
                        time: formatSeconds(bufferMs),
                      })
                    : t("none")
                  : DASH}
              </Detail>
              <Detail label={t("Source switches")}>{String(switches)}</Detail>
              <Detail label={t("Source")}>
                {origin || DASH}
                {release ? (
                  <span className="block break-all font-mono text-xs text-fg-muted">{release}</span>
                ) : null}
              </Detail>
            </dl>
          </section>

          {live?.cluster ? (
            <section aria-label={t("Cluster")}>
              <h3 className="mb-1 text-xs font-semibold uppercase tracking-wider text-fg-subtle">
                {t("Cluster")}
              </h3>
              <dl className="divide-y divide-border">
                <Detail label={t("Source node")}>{live.cluster.source}</Detail>
                <Detail label={t("Transcoding node")}>{live.cluster.encoder}</Detail>
                <Detail label={t("Delivery")}>{live.cluster.delivery}</Detail>
              </dl>
              {live.cluster.recovery ? (
                <Link
                  className="mt-2 inline-flex items-center gap-2 text-sm text-primary underline"
                  to={live.cluster.recovery}
                >
                  {live.cluster.recovering ? (
                    <Badge variant="warning">{t("Recovering")}</Badge>
                  ) : null}
                  {t("Recovery history")}
                </Link>
              ) : null}
            </section>
          ) : null}

          <section aria-label={t("Timing")}>
            <h3 className="mb-1 text-xs font-semibold uppercase tracking-wider text-fg-subtle">
              {t("Timing")}
            </h3>
            <dl className="divide-y divide-border">
              <Detail label={t("Started")}>{startedAt ? formatDateTime(startedAt) : DASH}</Detail>
              {!live && p?.ended ? (
                <Detail label={t("Ended")}>{formatDateTime(new Date(p.ended * 1000))}</Detail>
              ) : null}
              {p?.error && !p.failed ? (
                <Detail label={t("Error")}>
                  <span className="text-danger">{p.error}</span>
                </Detail>
              ) : null}
            </dl>
          </section>
          {!live && p && !p.measured ? (
            <p className="text-xs text-fg-muted">
              {t("This play was recorded before start time and buffering were measured.")}
            </p>
          ) : null}
        </div>
      )}
    </Drawer>
  );
}

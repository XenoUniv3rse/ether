import { Badge, EmptyState, Input, PagedTable, Select } from "@braedonsaunders/appkit-ui";
import { CheckCircle2, Clock, History as HistoryIcon, PlayCircle, Users } from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Kpi, KpiRow } from "../../components/Kpi";
import { apiFetch } from "../../lib/api";
import { qualityFallback } from "../../lib/quality-fallback";
import { fixed, formatDateTime, formatNumber, formatPercent } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { ToneValue } from "./ExperienceDrawer";
import {
  DASH,
  decisionLabel,
  failedReason,
  formatSeconds,
  reasonPhrase,
  startTone,
} from "./experience-format";
import { getLocale, t, tn } from "../../i18n";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import type { EtherUser } from "../../lib/types";
import { viewerLabel, type Play, type PlayDetail, type Summary, type ViewingFilter } from "./types";

/** Period, viewer and year-in-review filters on History's and Stats' tab row. */
export function ViewingFilters({
  value,
  onChange,
  viewers,
}: {
  value: ViewingFilter;
  onChange: (next: ViewingFilter) => void;
  viewers: EtherUser[];
}) {
  return (
    <div className="flex flex-wrap items-center gap-2" role="group" aria-label={t("Filters")}>
      <Select
        aria-label={t("Period")}
        className="h-8 w-40 text-sm"
        value={value.days}
        onChange={(e) => onChange({ ...value, days: e.target.value, year: "" })}
      >
        {[7, 30, 90, 365].map((n) => (
          <option key={n} value={String(n)}>
            {tn(n, "Last {count} day", "Last {count} days")}
          </option>
        ))}
      </Select>
      <Select
        aria-label={t("Viewer")}
        className="h-8 w-44 text-sm"
        value={value.user}
        onChange={(e) => onChange({ ...value, user: e.target.value })}
      >
        <option value="">{t("Everyone")}</option>
        {viewers.map((v) => (
          <option key={v.id} value={String(v.id)}>
            {viewerLabel(v)}
          </option>
        ))}
      </Select>
      <Input
        aria-label={t("Year in review")}
        className="h-8 w-28 text-sm"
        type="number"
        min={2000}
        max={new Date().getFullYear()}
        placeholder={t("Year")}
        value={value.year}
        onChange={(e) => onChange({ ...value, year: e.target.value })}
      />
    </div>
  );
}

function statusCell(p: Play) {
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap gap-1">
        <Badge variant={p.completed ? "success" : "secondary"}>
          {p.completed ? t("Completed") : p.ended ? t("Ended") : t("Last observed")}
        </Badge>
        {p.failovers ? (
          <Badge variant="warning">
            {tn(p.failovers, "{count} failover", "{count} failovers")}
          </Badge>
        ) : null}
      </div>
      {p.error ? <p className="text-xs text-danger">{p.error}</p> : null}
    </div>
  );
}

/** Recorded plays for the selected period and viewer. */
export function PlayHistory({
  history,
  summary,
  error,
  onRetry,
}: {
  history: Play[] | undefined;
  summary: Summary | undefined;
  error: unknown;
  onRetry: () => void;
}) {
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
  const drawer = <PlayDrawer id={playId} onClose={() => openPlay(null)} />;
  if (error && !history)
    return (
      <>
        <ErrorPanel error={error} onRetry={onRetry} />
        {drawer}
      </>
    );
  return (
    <>
      <KpiRow>
        <Kpi
          label={t("Plays")}
          value={summary ? formatNumber(summary.plays, 0) : DASH}
          icon={<PlayCircle size={18} />}
        />
        <Kpi
          label={t("Viewers")}
          value={summary ? formatNumber(summary.users.length, 0) : DASH}
          icon={<Users size={18} />}
          tone="info"
        />
        <Kpi
          label={t("Average session")}
          value={
            summary && summary.plays
              ? t("{minutes} min", { minutes: fixed(summary.watchedMs / summary.plays / 60000, 0) })
              : DASH
          }
          icon={<Clock size={18} />}
          tone="info"
        />
        <Kpi
          label={t("Completion rate")}
          value={summary && summary.plays ? formatPercent(summary.completed / summary.plays) : DASH}
          icon={<CheckCircle2 size={18} />}
          tone="success"
        />
      </KpiRow>
      {!history ? (
        <LoadingRows rows={4} />
      ) : history.length ? (
        <PagedTable
          rows={history}
          rowKey={(p) => p.id}
          pageSize={20}
          onRowClick={(p) => openPlay(p.id)}
          empty={t("No recorded plays in this period.")}
          defaultSort={{ key: "started", dir: "desc" }}
          labels={pagedTableLabels({
            searchLabel: t("Search play history"),
            searchPlaceholder: t("Find a title, viewer or device…"),
          })}
          columns={[
            {
              key: "title",
              header: t("Title / viewer"),
              search: (p) => p.title + " " + p.username,
              sortValue: (p) => p.title,
              cell: (p) => (
                <>
                  <p className="font-medium text-fg">{p.title}</p>
                  <p className="text-xs text-fg-muted">{p.username || t("Unknown viewer")}</p>
                </>
              ),
            },
            {
              key: "started",
              header: t("Started"),
              sortValue: (p) => p.started,
              cell: (p) => new Date(p.started * 1000).toLocaleString(getLocale()),
            },
            {
              key: "device",
              header: t("Device"),
              search: (p) => (p.device || p.platform) + " " + p.decision,
              cell: (p) => (
                <>
                  {p.device || p.platform || t("Unknown")}
                  <span className="block text-xs text-fg-muted">{p.decision}</span>
                </>
              ),
            },
            {
              key: "duration",
              header: t("Viewing time"),
              align: "right",
              sortValue: (p) => p.watchedMs,
              cell: (p) => t("{minutes} min", { minutes: fixed(p.watchedMs / 60000, 0) }),
            },
            { key: "status", header: t("Status"), cell: statusCell },
          ]}
        />
      ) : (
        <EmptyState
          icon={<HistoryIcon />}
          title={t("No plays yet")}
          description={t(
            "History starts when Ether observes playback. There are no recorded plays in this period.",
          )}
        />
      )}
      {drawer}
    </>
  );
}

function Detail({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-[9rem_1fr] gap-3 py-2 text-sm">
      <dt className="text-fg-muted">{label}</dt>
      <dd className="min-w-0 break-words text-fg">{children}</dd>
    </div>
  );
}

/** One recorded play, opened from a History row or a `?play=` link. */
function PlayDrawer({ id, onClose }: { id: string | null; onClose: () => void }) {
  const play = useAsync(
    (signal) =>
      id
        ? apiFetch<PlayDetail>(`/stats/plays/${encodeURIComponent(id)}`, { signal })
        : Promise.resolve(null),
    [id],
  );
  const p = play.data && play.data.id === id ? play.data : null;
  const decision = p ? decisionLabel(p.decision) : "";
  const why = p && p.decision !== "directplay" ? reasonPhrase(p.reasonKind, p.reasonDetail) : "";
  return (
    <Drawer
      open={id != null}
      onClose={onClose}
      size="md"
      title={p?.title || t("Play")}
      description={p ? new Date(p.started * 1000).toLocaleString(getLocale()) : undefined}
    >
      {play.error && !p ? (
        <ErrorPanel error={play.error} onRetry={() => void play.reload()} />
      ) : !p ? (
        <LoadingRows rows={4} />
      ) : (
        <div className="space-y-4">
          <div className="flex flex-wrap gap-1">
            <Badge variant={p.completed ? "success" : "secondary"}>
              {p.completed ? t("Completed") : p.ended ? t("Ended") : t("Last observed")}
            </Badge>
            {p.failed ? <Badge variant="destructive">{t("Never started")}</Badge> : null}
            {decision ? <Badge variant="secondary">{decision}</Badge> : null}
            {p.remote ? <Badge variant="secondary">{t("Away")}</Badge> : null}
          </div>
          {p.qualityFallback ? (
            <p className="text-sm text-fg-muted">{qualityFallback(p.qualityFallback)}</p>
          ) : null}
          <dl className="divide-y divide-border">
            <Detail label={t("Viewer")}>
              {p.userId ? (
                <Link
                  to={`/activity/experience?viewer=${p.userId}`}
                  className="text-primary hover:underline"
                >
                  {p.username || t("Unknown viewer")}
                </Link>
              ) : (
                p.username || t("Unknown viewer")
              )}
            </Detail>
            <Detail label={t("Device")}>{p.device || p.platform || t("Unknown")}</Detail>
            <Detail label={t("Start time")}>
              {p.failed ? (
                <span className="text-danger">{failedReason(p.error)}</span>
              ) : p.measured ? (
                <ToneValue value={formatSeconds(p.startMs)} tone={startTone(p.startMs)} />
              ) : (
                DASH
              )}
            </Detail>
            <Detail label={t("Buffering")}>
              {p.measured && p.bufferEvents != null
                ? p.bufferEvents
                  ? tn(p.bufferEvents, "{count} time, {time}", "{count} times, {time}", {
                      time: formatSeconds(p.bufferMs),
                    })
                  : t("none")
                : DASH}
            </Detail>
            <Detail label={t("Playback")}>
              {decision || DASH}
              {why ? (
                <span className="block text-xs text-fg-muted">
                  {t("Converted because of {reason}", { reason: why })}
                </span>
              ) : null}
            </Detail>
            <Detail label={t("Source")}>
              {p.origin || DASH}
              {p.release ? (
                <span className="block break-all font-mono text-xs text-fg-muted">{p.release}</span>
              ) : null}
            </Detail>
            <Detail label={t("Source switches")}>{String(p.switches ?? p.failovers ?? 0)}</Detail>
            <Detail label={t("Viewing time")}>
              {t("{minutes} min", { minutes: fixed(p.watchedMs / 60000, 0) })}
            </Detail>
            {p.ended ? (
              <Detail label={t("Ended")}>{formatDateTime(new Date(p.ended * 1000))}</Detail>
            ) : null}
            {p.error && !p.failed ? (
              <Detail label={t("Error")}>
                <span className="text-danger">{p.error}</span>
              </Detail>
            ) : null}
          </dl>
          {!p.measured ? (
            <p className="text-xs text-fg-muted">
              {t("This play was recorded before start time and buffering were measured.")}
            </p>
          ) : null}
        </div>
      )}
    </Drawer>
  );
}

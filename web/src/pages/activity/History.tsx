import { Badge, EmptyState, Input, PagedTable, Select } from "@braedonsaunders/appkit-ui";
import { CheckCircle2, Clock, History as HistoryIcon, PlayCircle, Users } from "lucide-react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Kpi, KpiRow } from "../../components/Kpi";
import { LiveChannelArtwork } from "../../components/SessionArtwork";
import { fixed, formatNumber, formatPercent } from "../../lib/format";
import { LiveProvider } from "./LiveProvider";
import { channelLabel } from "../../lib/health";
import { DASH } from "./experience-format";
import { getLocale, t, tn } from "../../i18n";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import type { EtherUser } from "../../lib/types";
import { PlayDrawer, usePlayParam } from "./PlayDrawer";
import { viewerLabel, type Play, type Summary, type ViewingFilter } from "./types";

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

const isLivePlay = (p: Play) => Boolean(p.channelId || p.channelName);

function playChannelTitle(p: Play): string {
  return (
    channelLabel({
      channelName: p.channelName ?? "",
      channelNumber: p.channelNumber ?? "",
    }) || p.title
  );
}

function statusCell(p: Play) {
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap gap-1">
        {isLivePlay(p) ? <Badge variant="info">{t("Live TV")}</Badge> : null}
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
  const [playId, openPlay] = usePlayParam();
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
              search: (p) =>
                [
                  p.channelNumber,
                  p.channelName,
                  p.title,
                  p.username,
                  p.liveSourceName,
                  p.liveLoginLabel,
                ]
                  .filter(Boolean)
                  .join(" "),
              sortValue: (p) => (isLivePlay(p) ? p.channelName || p.title : p.title),
              cell: (p) => {
                if (isLivePlay(p)) {
                  const channel = playChannelTitle(p);
                  const programme =
                    p.title && p.title !== p.channelName && p.title !== channel ? p.title : "";
                  const sub = [programme, p.username || t("Unknown viewer")]
                    .filter(Boolean)
                    .join(" · ");
                  return (
                    <div className="flex items-center gap-2.5">
                      <LiveChannelArtwork
                        channelId={p.channelId}
                        name={p.channelName || p.channelNumber || p.title}
                        className="size-8 shrink-0 rounded-md"
                        imgClassName="p-1"
                      />
                      <div className="min-w-0">
                        <p className="truncate font-medium text-fg">{channel}</p>
                        <p className="truncate text-xs text-fg-muted">{sub}</p>
                        {p.liveSourceName && (
                          <LiveProvider
                            provider={{
                              sourceName: p.liveSourceName,
                              loginLabel: p.liveLoginLabel,
                              loginNumber: p.liveLoginNumber,
                            }}
                          />
                        )}
                      </div>
                    </div>
                  );
                }
                return (
                  <>
                    <p className="font-medium text-fg">{p.title}</p>
                    <p className="text-xs text-fg-muted">{p.username || t("Unknown viewer")}</p>
                  </>
                );
              },
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

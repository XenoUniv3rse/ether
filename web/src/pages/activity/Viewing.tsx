import { DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import { CheckCircle2, Clock, PlayCircle, Users2 } from "lucide-react";
import { RankedBars, SplitBar, TimeBars, WeekHeatmap } from "../../components/charts";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Kpi, KpiRow } from "../../components/Kpi";
import { t, tn } from "../../i18n";
import { fixed, formatNumber, formatPercent } from "../../lib/format";
import { hoursOf, type Bucket, type Summary } from "./types";

const rank = (items: Bucket[] | undefined, by: "watchedMs" | "plays" = "watchedMs") =>
  (items ?? [])
    .filter((b) => b.label)
    .slice(0, 10)
    .map((b, i) => ({
      key: `${b.label}-${i}`,
      label: b.label,
      value: by === "watchedMs" ? hoursOf(b.watchedMs) : b.plays,
      detail: tn(b.plays, "{count} play", "{count} plays"),
    }));

const noPlays = (
  <p className="py-4 text-sm text-fg-muted">{t("No recorded plays in this period.")}</p>
);

/** What people watched, when, and who watched most, for the period and viewer. */
export function Viewing({
  summary,
  error,
  onRetry,
}: {
  summary: Summary | undefined;
  error: unknown;
  onRetry: () => void;
}) {
  if (error && !summary) return <ErrorPanel error={error} onRetry={onRetry} />;
  if (!summary) return <LoadingRows rows={4} />;
  const s = summary;
  const kinds = (s.kinds ?? []).map((b) => ({
    key: b.label,
    label: b.label === "movie" ? t("Movies") : b.label === "episode" ? t("TV episodes") : b.label,
    value: hoursOf(b.watchedMs),
  }));
  return (
    <>
      <KpiRow>
        <Kpi
          label={t("Hours watched")}
          value={fixed(hoursOf(s.watchedMs), 1)}
          detail={
            s.plays
              ? t("{minutes} min per play", { minutes: fixed(s.watchedMs / s.plays / 60000, 0) })
              : undefined
          }
          icon={<Clock size={18} />}
        />
        <Kpi
          label={t("Plays")}
          value={formatNumber(s.plays, 0)}
          detail={tn(s.users.length, "{count} viewer", "{count} viewers")}
          icon={<PlayCircle size={18} />}
          tone="info"
        />
        <Kpi
          label={t("Completed")}
          value={formatNumber(s.completed, 0)}
          detail={
            s.plays
              ? t("{percent} of plays", { percent: formatPercent(s.completed / s.plays) })
              : undefined
          }
          icon={<CheckCircle2 size={18} />}
          tone="success"
        />
        <Kpi
          label={t("Peak streams")}
          value={s.peakStreams != null ? formatNumber(s.peakStreams, 0) : "—"}
          detail={t("Playing at the same time")}
          icon={<Users2 size={18} />}
          tone="info"
        />
      </KpiRow>

      <DashboardPanel title={t("Watch time")}>
        {s.daily.length ? (
          <TimeBars
            ariaLabel={t("Hours watched by day")}
            height={180}
            valueFormat={(v) => fixed(v, v < 10 ? 1 : 0)}
            data={s.daily.map((b, i) => ({
              key: `${b.label}-${i}`,
              label: b.label,
              value: hoursOf(b.watchedMs),
              detail: tn(b.plays, "{count} play", "{count} plays"),
            }))}
          />
        ) : (
          noPlays
        )}
      </DashboardPanel>

      <div className="grid gap-6 lg:grid-cols-5">
        {s.heatmap ? (
          <DashboardPanel title={t("When people watch")} className="lg:col-span-3">
            <WeekHeatmap
              ariaLabel={t("Plays by weekday and hour")}
              cells={s.heatmap.map((c) => ({ weekday: c.weekday, hour: c.hour, value: c.plays }))}
              valueFormat={(v) => tn(v, "{count} play", "{count} plays")}
            />
          </DashboardPanel>
        ) : null}
        <div className={s.heatmap ? "space-y-6 lg:col-span-2" : "space-y-6 lg:col-span-5"}>
          {kinds.length ? (
            <DashboardPanel title={t("Movies and TV")}>
              <SplitBar
                ariaLabel={t("Watch time by kind")}
                data={kinds}
                valueFormat={(v) => t("{hours} h", { hours: fixed(v, 1) })}
              />
            </DashboardPanel>
          ) : null}
          {s.genres ? (
            <DashboardPanel title={t("Top genres")}>
              <RankedBars
                ariaLabel={t("Top genres")}
                data={rank(s.genres).slice(0, 6)}
                valueFormat={(v) => t("{hours} h", { hours: fixed(v, 1) })}
                empty={noPlays}
              />
            </DashboardPanel>
          ) : null}
        </div>
      </div>

      <div className="grid gap-6 lg:grid-cols-3">
        <DashboardPanel title={t("Top titles")}>
          <RankedBars
            ariaLabel={t("Top titles")}
            data={rank(s.titles)}
            valueFormat={(v) => t("{hours} h", { hours: fixed(v, 1) })}
            empty={noPlays}
          />
        </DashboardPanel>
        {s.shows ? (
          <DashboardPanel title={t("Top shows")}>
            <RankedBars
              ariaLabel={t("Top shows")}
              data={rank(s.shows)}
              valueFormat={(v) => t("{hours} h", { hours: fixed(v, 1) })}
              empty={noPlays}
            />
          </DashboardPanel>
        ) : null}
        <DashboardPanel title={t("Top viewers")}>
          <RankedBars
            ariaLabel={t("Top viewers")}
            data={rank(s.users)}
            valueFormat={(v) => t("{hours} h", { hours: fixed(v, 1) })}
            empty={noPlays}
          />
        </DashboardPanel>
      </div>
      <p className="text-xs text-fg-muted">
        {t(
          "Days and hours use this browser's time zone. Watched time leaves out gaps longer than 60 seconds and simultaneous sessions that can't be matched to a device.",
        )}
      </p>
    </>
  );
}

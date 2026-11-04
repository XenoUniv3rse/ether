import { DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import { Cpu, Gauge, HardDriveDownload, PlayCircle } from "lucide-react";
import { RankedBars, SplitBar, StackedColumns } from "../../components/charts";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Kpi, KpiRow } from "../../components/Kpi";
import { t, tn } from "../../i18n";
import { fixed, formatBytes, formatNumber, formatPercent } from "../../lib/format";
import type { Bucket, Summary } from "./types";

const DECISIONS: Record<string, () => string> = {
  "direct-play": () => t("Direct play"),
  "direct-stream": () => t("Direct stream"),
  "audio-transcode": () => t("Audio transcode"),
  transcode: () => t("Transcode"),
};

const playsOf = (items: Bucket[] | undefined, label: (b: Bucket) => string = (b) => b.label) =>
  (items ?? []).map((b) => ({
    key: b.label || "other",
    label: label(b) || t("Other"),
    value: b.plays,
  }));

const mbps = (kbps: number) =>
  t("{rate} Mbps", { rate: fixed(kbps / 1000, kbps >= 10000 ? 0 : 1) });

/** How plays reached viewers: direct or converted, from where, at what quality. */
export function Delivery({
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
  const decisions = s.decisions ?? [];
  const count = (label: string) => decisions.find((d) => d.label === label)?.plays ?? 0;
  const direct = count("direct-play") + count("direct-stream");
  const converted = count("transcode") + count("audio-transcode");
  const measured = direct + converted;
  const plays = (v: number) => tn(v, "{count} play", "{count} plays");
  return (
    <>
      <KpiRow>
        <Kpi
          label={t("Direct")}
          value={measured ? formatPercent(direct / measured) : "—"}
          detail={t("Played or streamed as-is")}
          icon={<PlayCircle size={18} />}
          tone="success"
        />
        <Kpi
          label={t("Transcodes")}
          value={formatNumber(converted, 0)}
          detail={
            count("audio-transcode")
              ? t("{count} audio only", { count: formatNumber(count("audio-transcode"), 0) })
              : t("Video or audio converted")
          }
          icon={<Cpu size={18} />}
          tone="warning"
        />
        <Kpi
          label={t("Transferred")}
          value={formatBytes(s.bytes)}
          detail={
            s.plays ? t("{size} per play", { size: formatBytes(s.bytes / s.plays) }) : undefined
          }
          icon={<HardDriveDownload size={18} />}
          tone="info"
        />
        <Kpi
          label={t("Average bitrate")}
          value={s.avgBitrateKbps ? mbps(s.avgBitrateKbps) : "—"}
          detail={t("Delivered while playing")}
          icon={<Gauge size={18} />}
          tone="info"
        />
      </KpiRow>

      {s.dailyDelivery ? (
        <DashboardPanel title={t("Direct and transcoded plays")}>
          <StackedColumns
            ariaLabel={t("Direct and transcoded plays by day")}
            aLabel={t("Direct")}
            bLabel={t("Transcoded")}
            data={s.dailyDelivery.map((d) => ({
              key: d.label,
              label: d.label,
              a: d.direct,
              b: d.transcode,
            }))}
          />
        </DashboardPanel>
      ) : null}

      <div className="grid gap-6 lg:grid-cols-2">
        {decisions.length ? (
          <DashboardPanel title={t("How plays were delivered")}>
            <SplitBar
              ariaLabel={t("How plays were delivered")}
              data={playsOf(decisions, (b) => DECISIONS[b.label]?.() ?? b.label)}
              valueFormat={plays}
            />
          </DashboardPanel>
        ) : null}
        {s.origins ? (
          <DashboardPanel title={t("Where streams came from")}>
            <SplitBar
              ariaLabel={t("Where streams came from")}
              data={playsOf(s.origins)}
              valueFormat={plays}
            />
          </DashboardPanel>
        ) : null}
        {s.qualities ? (
          <DashboardPanel title={t("Picture quality")}>
            <SplitBar
              ariaLabel={t("Picture quality")}
              data={playsOf(s.qualities)}
              valueFormat={plays}
            />
          </DashboardPanel>
        ) : null}
        {s.places ? (
          <DashboardPanel title={t("Home and away")}>
            <SplitBar
              ariaLabel={t("Home and away")}
              data={playsOf(s.places, (b) =>
                b.label === "away" ? t("Away from home") : t("At home"),
              )}
              valueFormat={plays}
            />
          </DashboardPanel>
        ) : null}
      </div>

      <DashboardPanel title={t("Apps and devices")}>
        <RankedBars
          ariaLabel={t("Apps and devices")}
          data={s.platforms.slice(0, 10).map((b, i) => ({
            key: `${b.label}-${i}`,
            label: b.label || t("Unknown"),
            value: b.plays,
            detail: formatBytes(b.bytes),
          }))}
          valueFormat={plays}
          empty={
            <p className="py-4 text-sm text-fg-muted">{t("No recorded plays in this period.")}</p>
          }
        />
      </DashboardPanel>
    </>
  );
}

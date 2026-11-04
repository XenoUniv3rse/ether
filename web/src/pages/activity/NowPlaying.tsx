import { Badge, Button, EmptyState, Select, toast } from "@braedonsaunders/appkit-ui";
import { Cpu, Gauge, PlayCircle, Radio, Square } from "lucide-react";
import { useState } from "react";
import { Artwork } from "../../components/Artwork";
import { Meter } from "../../components/charts";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Kpi, KpiRow } from "../../components/Kpi";
import { Section } from "../../components/layout";
import { apiFetch, errorMessage } from "../../lib/api";
import { t, tn } from "../../i18n";
import { formatNumber, formatPercent, formatRate, relativeTime } from "../../lib/format";
import { decisionLabel, type NowPlaying as Stream } from "../../lib/health";
import type { AsyncState } from "../../lib/hooks";
import { useTitle } from "../../lib/titles";

function StreamRow({ s, stopping, onStop }: { s: Stream; stopping: boolean; onStop: () => void }) {
  const title = useTitle(s.key || undefined);
  return (
    <li className="flex flex-wrap items-center gap-4 py-4 first:pt-0 last:pb-0">
      <Artwork
        src={title?.poster}
        alt={s.title}
        title=""
        className="aspect-[2/3] w-12 shrink-0 rounded-md"
      />
      <div className="min-w-0 flex-1 space-y-2">
        <div className="min-w-0">
          <p className="truncate font-medium text-fg">{s.title}</p>
          <p className="truncate text-sm text-fg-muted">
            {s.user} · {s.device}
            {s.product && s.product !== s.device ? ` (${s.product})` : ""}
          </p>
        </div>
        <div className="flex flex-wrap gap-1.5">
          <Badge variant="secondary">{s.protocol}</Badge>
          <Badge
            variant={
              s.decision === "transcode" || (s.videoCopy && !s.audioCopy && s.decision === "copy")
                ? "warning"
                : "success"
            }
          >
            {decisionLabel(s)}
          </Badge>
          {s.origin ? <Badge variant="secondary">{s.origin}</Badge> : null}
          {s.remote ? <Badge variant="info">{t("Remote")}</Badge> : null}
          {s.failovers ? (
            <Badge variant="warning">
              {tn(s.failovers, "{count} failover", "{count} failovers")}
            </Badge>
          ) : null}
          <Badge variant="secondary">{formatRate(s.speedBps)}</Badge>
        </div>
        <div className="max-w-sm">
          {s.progress !== null ? (
            <Meter
              value={s.progress}
              buffered={s.buffered}
              label={t("Started {when}", { when: relativeTime(s.startedAt) })}
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
            <p className="text-xs text-fg-muted">{t("Waiting for playback position")}</p>
          )}
        </div>
        {s.release ? (
          <p className="truncate font-mono text-[11px] text-fg-subtle" title={s.release}>
            {s.release}
          </p>
        ) : null}
      </div>
      <Button variant="outline" size="sm" disabled={stopping} onClick={onStop}>
        <Square size={14} /> {t("Stop stream")}
      </Button>
    </li>
  );
}

/** Live sessions with a stop control for each. */
/** The protocols of the live streams, for the tab row's filter. */
export const streamProtocols = (streams: Stream[]) =>
  [...new Set(streams.map((stream) => stream.protocol))].sort();

/** A protocol filter for Now Playing's tab row, shown once streams differ. */
export function ProtocolFilter({
  streams,
  value,
  onChange,
}: {
  streams: Stream[];
  value: string;
  onChange: (v: string) => void;
}) {
  const protocols = streamProtocols(streams);
  if (protocols.length < 2 && !value) return null;
  return (
    <Select
      aria-label={t("Protocol")}
      className="h-8 w-40 text-sm"
      value={value}
      onChange={(event) => onChange(event.target.value)}
    >
      <option value="">{t("All protocols")}</option>
      {protocols.map((p) => (
        <option key={p} value={p}>
          {p}
        </option>
      ))}
    </Select>
  );
}

const transcoding = (s: Stream) =>
  s.decision === "transcode" || (s.decision === "copy" && !s.audioCopy);

export function NowPlaying({ state, protocol }: { state: AsyncState<Stream[]>; protocol: string }) {
  const [stopping, setStopping] = useState<string | null>(null);
  const streams = state.data ?? [];
  const filtered = protocol ? streams.filter((stream) => stream.protocol === protocol) : streams;
  const converting = filtered.filter(transcoding).length;
  const throughput = filtered.reduce((sum, s) => sum + (s.speedBps || 0), 0);

  const stop = async (id: string) => {
    setStopping(id);
    try {
      await apiFetch(`/stats/sessions/${encodeURIComponent(id)}/stop`, {
        method: "POST",
        body: "{}",
      });
      toast.success(t("Stream stopped"));
      await state.reload();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setStopping(null);
    }
  };

  if (state.error && !state.data)
    return <ErrorPanel error={state.error} onRetry={() => void state.reload()} />;
  const ready = Boolean(state.data);
  return (
    <>
      <KpiRow>
        <Kpi
          label={t("Streaming now")}
          value={ready ? formatNumber(filtered.length, 0) : "—"}
          icon={<Radio size={18} />}
        />
        <Kpi
          label={t("Direct")}
          value={ready ? formatNumber(filtered.length - converting, 0) : "—"}
          detail={t("Played or streamed as-is")}
          icon={<PlayCircle size={18} />}
          tone="success"
        />
        <Kpi
          label={t("Transcoding")}
          value={ready ? formatNumber(converting, 0) : "—"}
          detail={t("Video or audio converted")}
          icon={<Cpu size={18} />}
          tone="warning"
        />
        <Kpi
          label={t("Throughput")}
          value={ready ? formatRate(throughput) : "—"}
          icon={<Gauge size={18} />}
          tone="info"
        />
      </KpiRow>
      <Section>
        {!state.data ? (
          <LoadingRows rows={2} />
        ) : filtered.length ? (
          <ul className="divide-y divide-border">
            {filtered.map((s) => (
              <StreamRow
                key={s.id || s.key}
                s={s}
                stopping={stopping === s.id}
                onStop={() => void stop(s.id)}
              />
            ))}
          </ul>
        ) : streams.length ? (
          <p>{t("No streams match this protocol.")}</p>
        ) : (
          <EmptyState
            icon={<Radio />}
            title={t("Nothing is playing")}
            description={t("Streams appear here as soon as a viewer presses play.")}
          />
        )}
      </Section>
    </>
  );
}

import { DashboardMetricCard } from "@braedonsaunders/appkit-dashboard/primitives";
import { Tooltip } from "@braedonsaunders/appkit-ui";
import { Activity as ActivityIcon, CheckCircle2, Gauge, Globe, Info } from "lucide-react";
import { t, tn } from "../../../i18n";
import { formatBytes, formatNumber } from "../../../lib/format";
import type { PluginDetail } from "../types";
import { healthSummary } from "./health";

/* --------------------------------------------------------------- health tiles */

/** How the plugin is answering: responding, speed, and the sites it reaches. */
export function HealthTiles({ detail }: { detail: PluginDetail }) {
  const health = healthSummary(detail);
  const level = health.level;
  const im = detail.impact;
  const undeclared = detail.privacy.undeclared ?? [];
  const hosts = Object.keys(detail.privacy.observedHosts ?? {});
  const declared = detail.privacy.declaredHosts ?? [];
  const slow = im.p95Ms > 2000;
  // Latency only counts answers; failed calls never set it.
  const answered = im.calls > im.errors;
  const tone =
    level === "ok" ? "success" : level === "off" ? "info" : level === "warn" ? "warning" : "danger";
  return (
    <div className="grid gap-3 sm:grid-cols-3">
      <DashboardMetricCard
        label={t("Responding")}
        value={health.word}
        detail={
          <Detail
            text={health.tile}
            more={[
              tn(im.calls, "{count} recent call", "{count} recent calls"),
              t("Failed: {count}", { count: formatNumber(im.errors, 0) }),
              t("Restarts: {count}", { count: formatNumber(im.restarts, 0) }),
              t("Storage: {size}", { size: formatBytes(im.kvBytes) }),
            ]}
          />
        }
        icon={level === "ok" ? <CheckCircle2 size={16} /> : <ActivityIcon size={16} />}
        tone={tone}
      />
      <DashboardMetricCard
        label={t("Speed")}
        value={answered ? duration(im.p50Ms) : "—"}
        detail={
          answered
            ? t("Slowest {time}", { time: duration(im.p95Ms) })
            : im.calls
              ? t("No answers yet")
              : t("Not used yet")
        }
        icon={<Gauge size={16} />}
        tone={slow ? "warning" : "primary"}
      />
      <DashboardMetricCard
        label={t("Network")}
        value={
          undeclared.length
            ? tn(undeclared.length, "{count} site blocked", "{count} sites blocked")
            : detail.host
              ? t("Its own site only")
              : t("As declared")
        }
        detail={
          <Detail
            text={
              undeclared.length
                ? undeclared.slice(0, 2).join(", ")
                : detail.host
                  ? detail.host
                  : tn(Math.max(hosts.length, declared.length), "{count} site", "{count} sites")
            }
            more={[
              ...(hosts.length ? hosts : declared).slice(0, 6),
              t("Sent: {size}", { size: formatBytes(im.egressBytes) }),
            ]}
          />
        }
        icon={<Globe size={16} />}
        tone={undeclared.length ? "warning" : "success"}
      />
    </div>
  );
}

/** A tile's second line, with its details behind an ⓘ that keeps its own space. */
function Detail({ text, more }: { text: string; more: string[] }) {
  return (
    <span className="flex min-w-0 items-center gap-1.5">
      <span className="min-w-0 truncate" title={text}>
        {text}
      </span>
      <span className="shrink-0">
        <More lines={more} />
      </span>
    </span>
  );
}

/** The detail behind a tile, on hover or focus. */
function More({ lines }: { lines: string[] }) {
  return (
    <Tooltip
      content={
        <span className="block space-y-0.5 text-left">
          {lines.map((l) => (
            <span key={l} className="block">
              {l}
            </span>
          ))}
        </span>
      }
    >
      <button
        type="button"
        aria-label={t("More details")}
        className="inline-flex text-fg-subtle hover:text-fg focus-visible:text-fg"
      >
        <Info size={13} aria-hidden />
      </button>
    </Tooltip>
  );
}

/** A response time the way a person reads it: 180 ms, 17 s. */
function duration(ms: number): string {
  if (ms < 1) return t("Under 1 ms");
  if (ms < 1000) return t("{ms} ms", { ms: formatNumber(ms, 0) });
  return t("{seconds} s", { seconds: formatNumber(ms / 1000, ms < 10000 ? 1 : 0) });
}

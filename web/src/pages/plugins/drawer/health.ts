import { t, tn } from "../../../i18n";
import type { HealthLevel } from "../../../lib/health";
import { formatNumber, relativeTime } from "../../../lib/format";
import { pluginHealthLevel, type PluginDetail } from "../types";

/**
 * One reading of a plugin's health for every place the drawer shows it:
 * the header word, the Responding tile and the problem card. They all come
 * from here, so they can never disagree. Plain words only; the raw error
 * lives in Activity.
 */
export interface HealthSummary {
  level: HealthLevel;
  /** One or two words for the header and the tile. */
  word: string;
  /** The tile's second line. */
  tile: string;
  /** Present only when the owner should look. */
  card?: { title: string; line: string; retry: boolean };
}

export function healthSummary(detail: PluginDetail): HealthSummary {
  const h = detail.healthDetail;
  const im = detail.impact;
  const level = pluginHealthLevel(h.status, detail.enabled);
  const calls = im.calls;
  const seconds = Math.round(im.p50Ms / 1000);
  const failures = t("{errors} of {calls} recent calls failed", {
    errors: formatNumber(im.errors, 0),
    calls: formatNumber(calls, 0),
  });
  if (level === "off") {
    return { level, word: t("Off"), tile: t("Turned off") };
  }
  if (level === "ok" || level === "unknown") {
    return {
      level,
      word: t("Healthy"),
      tile: calls ? tn(calls, "{count} recent call", "{count} recent calls") : t("Not used yet"),
    };
  }
  const slow = /slow/i.test(h.reason ?? "") || (im.slow > 0 && im.errors === 0);
  if (level === "warn" && slow) {
    return {
      level,
      word: t("Slow"),
      tile:
        seconds >= 1 ? t("About {seconds} s per answer", { seconds }) : t("Some answers are slow"),
      card: {
        title:
          seconds >= 1
            ? t("Works, but slow: about {seconds} s per answer", { seconds })
            : t("Works, but some answers are slow"),
        line: t("Anything that uses it waits for it."),
        retry: false,
      },
    };
  }
  if (level === "warn") {
    return {
      level,
      word: t("Needs attention"),
      tile: im.errors ? failures : t("Needs attention"),
      card: {
        title: t("{name} needs attention", { name: detail.name }),
        line: t("Activity shows what happened."),
        retry: true,
      },
    };
  }
  const lastOk = h.lastOk && Date.parse(h.lastOk) > Date.UTC(2000, 0, 1) ? h.lastOk : "";
  return {
    level,
    word: t("Not answering"),
    tile: im.errors ? failures : t("Not answering"),
    card: {
      title: t("{name} isn't answering", { name: detail.name }),
      line: lastOk
        ? t("It last worked {when}.", { when: relativeTime(lastOk) })
        : t("It hasn't answered yet."),
      retry: true,
    },
  };
}

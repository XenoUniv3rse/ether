import { getLocale, t } from "../i18n";
import { formatNumber } from "../lib/format";

export interface PluginFailure {
  code: string;
  message?: string;
  body?: string;
  route?: string;
  status?: number;
  timeoutSeconds?: number;
  retryAfterSeconds?: number;
}

export function pluginFailureSentence(failure: PluginFailure): string {
  let reason = failure.code === "crashed" ? undefined : failure.message;
  if (!reason) {
    switch (failure.code) {
      case "auth":
        reason = t("The upstream service rejected the plugin’s credentials.");
        break;
      case "rate_limited":
        reason = t("The upstream service is rate limiting the plugin.");
        break;
      case "upstream_unavailable":
        reason = t("The upstream service is unavailable.");
        break;
      case "not_found":
        reason = t("The requested resource was not found.");
        break;
      case "invalid_settings":
        reason = t("The plugin rejected its settings.");
        break;
      case "timeout": {
        const seconds = formatNumber(failure.timeoutSeconds ?? 0, 0);
        reason = /\/(browse|changes)$/.test(failure.route ?? "")
          ? t("Timed out after {seconds} s fetching the catalogue.", { seconds })
          : t("The plugin timed out after {seconds} s.", { seconds });
        break;
      }
      case "malformed":
        reason = t("The plugin returned malformed data.");
        break;
      case "crashed":
        reason = t("The plugin crashed.");
        break;
      case "unavailable":
        reason = t("Ether could not start or reach the plugin.");
        break;
      default:
        reason = t("The plugin could not complete the request.");
    }
  }
  if (!failure.retryAfterSeconds) return reason;
  const seconds = failure.retryAfterSeconds;
  const wholeMinutes = seconds >= 60 && seconds % 60 === 0;
  const delay = new Intl.NumberFormat(getLocale(), {
    style: "unit",
    unit: wholeMinutes ? "minute" : "second",
    unitDisplay: "short",
  }).format(wholeMinutes ? seconds / 60 : seconds);
  return t("{reason} (try again in {delay})", {
    reason: reason.replace(/[.!?。！？]+$/u, ""),
    delay,
  });
}

export function PluginFailureTechnical({ failure }: { failure: PluginFailure }) {
  return (
    <div className="space-y-1">
      {failure.route ? <div className="break-all font-mono">{failure.route}</div> : null}
      {failure.status ? <div>{t("HTTP {status}", { status: failure.status })}</div> : null}
      {failure.body ? (
        <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-all font-mono">
          {failure.body}
        </pre>
      ) : null}
    </div>
  );
}

export function PluginFailureDetails({ failure }: { failure: PluginFailure }) {
  if (!failure.route && !failure.status && !failure.body) return null;
  return (
    <details className="text-xs text-fg-muted">
      <summary className="w-fit cursor-pointer">{t("Details")}</summary>
      <PluginFailureTechnical failure={failure} />
    </details>
  );
}

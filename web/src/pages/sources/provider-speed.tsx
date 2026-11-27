import { Button, Spinner } from "@braedonsaunders/appkit-ui";
import { CircleCheck, Gauge, TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { t, tn } from "../../i18n";
import { api } from "../../lib/api";
import { fixed, formatBitrate, formatNumber, relativeTime } from "../../lib/format";
import type { ProviderSpeedResult } from "../../lib/types";

type Kind = "usenet" | "debrid";

export function useProviderSpeed(kind: Kind, id: string | undefined) {
  const [result, setResult] = useState<ProviderSpeedResult>();
  const [measurements, setMeasurements] = useState<Record<string, ProviderSpeedResult>>({});
  const [running, setRunning] = useState(false);
  const request = useRef<AbortController | undefined>(undefined);

  useEffect(() => {
    setResult(undefined);
    setRunning(false);
    return () => {
      request.current?.abort();
      request.current = undefined;
    };
  }, [id]);

  async function run() {
    if (!id || request.current) return;
    const controller = new AbortController();
    request.current = controller;
    setResult(undefined);
    setRunning(true);
    try {
      const next = await api.testProviderSpeed(kind, id, controller.signal);
      if (!controller.signal.aborted) {
        setResult(next);
        if (next.status === "ok") {
          setMeasurements((previous) => ({ ...previous, [id]: next }));
        }
      }
    } catch {
      if (!controller.signal.aborted) setResult({ status: "unavailable" });
    } finally {
      if (request.current === controller) {
        request.current = undefined;
        setRunning(false);
      }
    }
  }

  const measurement = id ? measurements[id] : undefined;
  return { result: result ?? measurement, measurement, running, run };
}

export function ProviderSpeedButton({
  running,
  disabled,
  run,
}: {
  running: boolean;
  disabled: boolean;
  run: () => Promise<void>;
}) {
  return (
    <Button
      size="sm"
      type="button"
      variant="outline"
      disabled={disabled || running}
      onClick={() => void run()}
      title={t("Uses the saved provider settings")}
    >
      <Gauge size={14} />
      {t("Test speed")}
    </Button>
  );
}

function outcome(status: string): string {
  switch (status) {
    case "empty_vault":
      return t("The NZB vault is empty. Play a Usenet title and try again.");
    case "no_readable_nzb":
      return t("Ether could not read a recent NZB from the vault.");
    case "sample_missing":
      return t("This provider is missing too many articles from the test releases.");
    case "no_recent_file":
      return t("Play a cached file on this debrid account and try again.");
    case "paused":
      return t("This provider is temporarily paused. Try again later.");
    case "busy":
      return t("No spare connection is available. Try again when playback is idle.");
    case "already_running":
      return t("A speed test is already running for this provider.");
    default:
      return t("Ether could not measure this provider’s speed.");
  }
}

function speedRate(mbps: number | undefined): string {
  return t("{rate} MB/s", { rate: formatNumber(mbps ?? 0) });
}

export function providerSpeedMetric(measurement?: ProviderSpeedResult) {
  if (!measurement) return undefined;
  return {
    value: speedRate(measurement.mbps),
    subtitle: measurement.at
      ? t("Measured {time}", { time: relativeTime(measurement.at) })
      : undefined,
  };
}

export function ProviderSpeedReport({
  kind,
  result,
  running,
}: {
  kind: Kind;
  result?: ProviderSpeedResult;
  running: boolean;
}) {
  const measured = result?.status === "ok";
  const typical = result?.typicalMbps ?? 0;
  const available = result?.streamMbps ?? 0;
  const rate = formatBitrate(available * 1e6);
  const comparable = measured && typical > 0;
  const slow = comparable && available < typical;
  const playing = result?.streamsPlaying ?? 0;
  const connections = result?.connections ?? 0;
  let headline = result ? outcome(result.status) : "";
  if (measured) {
    headline = slow
      ? t("Too slow for your recent streams")
      : comparable
        ? t("Fast enough for your streams (about {rate})", { rate })
        : t("Enough for streams up to about {rate}", { rate });
  }

  return (
    <div role="status" aria-live="polite">
      {running ? (
        <div className="flex items-center gap-2 rounded-xl border border-border bg-surface px-4 py-3 text-sm text-fg-muted">
          <Spinner size={16} />
          {t("Measuring speed for about 10 seconds…")}
        </div>
      ) : result ? (
        <div className="overflow-hidden rounded-xl border border-border bg-surface text-sm">
          <div
            className={`space-y-1 px-4 py-3 ${slow ? "bg-warning/10" : comparable ? "bg-success/10" : ""}`}
          >
            <p
              className={`flex items-start gap-2 font-semibold ${slow ? "text-warning" : comparable ? "text-success" : ""}`}
            >
              {slow ? (
                <TriangleAlert size={16} className="mt-0.5 shrink-0" aria-hidden />
              ) : comparable ? (
                <CircleCheck size={16} className="mt-0.5 shrink-0" aria-hidden />
              ) : null}
              {headline}
            </p>
            {slow && (
              <>
                <p>
                  {t("About {available} available. Your recent streams use {typical}.", {
                    available: rate,
                    typical: formatBitrate(typical * 1e6),
                  })}
                </p>
                {!result.refused && (
                  <p className="text-fg-muted">
                    {kind === "usenet"
                      ? t("Add connections, or try a faster or backfill provider.")
                      : t("Try a faster provider.")}
                  </p>
                )}
              </>
            )}
          </div>
          {!!result.seconds && (
            <div className="space-y-1 border-t border-border px-4 py-2.5 text-xs text-fg-muted">
              {measured && (
                <p>
                  {t("{total} total · {perConnection} per connection", {
                    total: speedRate(result.mbps),
                    perConnection: speedRate(result.mbpsPerConnection),
                  })}
                </p>
              )}
              <p>
                {t("Connections used: {used} · Opened: {opened} · Refused: {refused}", {
                  used: fixed(connections, 0),
                  opened: fixed(result.opened ?? 0, 0),
                  refused: fixed(result.refused ?? 0, 0),
                })}
              </p>
              {kind === "usenet" && (
                <p>{t("Article misses: {count}", { count: fixed(result.misses ?? 0, 0) })}</p>
              )}
              {result.spareOnly && result.status !== "busy" && (
                <p>
                  {playing > 0
                    ? t(
                        "Playback in progress ({streams}). The test used spare connections ({connections}).",
                        {
                          streams: tn(playing, "{count} stream", "{count} streams"),
                          connections: fixed(connections, 0),
                        },
                      )
                    : t("Other source activity limited the test to spare connections.")}
                </p>
              )}
            </div>
          )}
        </div>
      ) : null}
    </div>
  );
}

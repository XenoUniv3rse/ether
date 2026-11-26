import { Badge } from "@braedonsaunders/appkit-ui";
import { Cpu } from "lucide-react";
import type { ReactNode } from "react";
import apple from "../../assets/brands/apple.svg?no-inline";
import amd from "../../assets/brands/amd.svg?no-inline";
import intel from "../../assets/brands/intel.svg?no-inline";
import nvidia from "../../assets/brands/nvidia.svg?no-inline";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Section } from "../../components/layout";
import { t, tn } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync, useInterval, type AsyncState } from "../../lib/hooks";
import { codecLabel, encodeCodecs } from "./hardwareCodecs";

export type Hardware = {
  os?: string;
  arch?: string;
  hwaccel?: { name: string; encoders: string[] | null; device?: string };
  /** GPU vendor of the VA-API render node: "intel", "amd", "nvidia". */
  vendor?: string;
  ffmpeg?: unknown;
  error?: string;
  notice?: string;
  playbackError?: string;
  filtersProbed?: boolean;
  videoEncoderLimit?: number;
  hardwareToneMap?: boolean;
  toneMapDecision?: {
    route: "cpu" | "gpu" | "unavailable";
    output: number;
    cpu?: number;
    gpu?: number;
  };
  toneMap?: string[];
  colour?: string[];
  enhancement?: {
    backend: string;
    setupState?: string;
    available: boolean;
    reason?: string;
    version?: string;
    protocols?: string[];
    active: number;
  };
};

/** True when the server runs on Linux hardware that can use a VA-API render device. */
export function canUseVaapi(hw?: Hardware | null): boolean {
  if (!hw) return false;
  const name = hw.hwaccel?.name;
  if (name === "vaapi" || hw.hwaccel?.device?.startsWith("/dev/dri/")) return true;
  if (hw.os && hw.os !== "linux") return false;
  if (!hw.os) return false;
  return name !== "nvenc" && hw.vendor !== "nvidia";
}

/** True when the server runs on Windows x64 with NVIDIA hardware that can use RTX Video. */
export function canUseRtxVideo(hw?: Hardware | null): boolean {
  if (!hw) return false;
  const enh = hw.enhancement;
  if (
    enh?.available ||
    enh?.setupState === "installing" ||
    enh?.setupState === "checking" ||
    (enh?.active ?? 0) > 0
  ) {
    return true;
  }
  const name = hw.hwaccel?.name;
  if (name === "videotoolbox" || name === "vaapi" || hw.hwaccel?.device?.startsWith("/dev/dri/")) {
    return false;
  }
  if ((hw.os && hw.os !== "windows") || (hw.arch && hw.arch !== "amd64")) {
    return false;
  }
  if (
    enh?.setupState === "unavailable" &&
    enh.reason &&
    /requires.*RTX|cannot detect.*RTX|requires.*Windows/i.test(enh.reason)
  ) {
    return false;
  }
  return name === "nvenc" || (hw.os === "windows" && hw.vendor === "nvidia");
}

export function useHardwareStatus(enabled = true): AsyncState<Hardware | null> {
  const hw = useAsync(
    async (signal) => {
      if (!enabled) return null;
      return (
        (await apiFetch<{ value: Hardware | null }>("/status/hwaccel", { signal })).value ?? {}
      );
    },
    [enabled],
  );
  useInterval(
    () => {
      if (!hw.loading) void hw.reload();
    },
    enabled ? 5000 : null,
  );
  return hw;
}

function BrandMark({ src, color }: { src: string; color: string }) {
  return (
    <span
      aria-hidden
      className="block size-8 bg-current"
      style={{
        color,
        maskImage: `url("${src}")`,
        maskPosition: "center",
        maskSize: "contain",
        maskRepeat: "no-repeat",
        WebkitMaskImage: `url("${src}")`,
        WebkitMaskPosition: "center",
        WebkitMaskSize: "contain",
        WebkitMaskRepeat: "no-repeat",
      }}
    />
  );
}

// VA-API runs on Intel and AMD alike; the server reads the GPU's PCI vendor.
function vendorMark(name: string | undefined, vendor?: string): ReactNode {
  if (name === "vaapi" && vendor === "intel") return <BrandMark src={intel} color="#0071C5" />;
  if (name === "vaapi" && vendor !== "amd") return <Cpu size={22} aria-hidden />;
  switch (name) {
    case "nvenc":
      return <BrandMark src={nvidia} color="#76B900" />;
    case "qsv":
      return <BrandMark src={intel} color="#0071C5" />;
    case "vaapi":
      return <BrandMark src={amd} color="#ED1C24" />;
    case "videotoolbox":
      return <BrandMark src={apple} color="currentColor" />;
    default:
      return <Cpu size={22} aria-hidden />;
  }
}

function vendorCopy(name: string | undefined, vendor?: string): { title: string; detail: string } {
  if (name === "vaapi" && vendor !== "amd")
    return {
      title: vendor === "intel" ? t("Intel VA-API") : t("VA-API"),
      detail: t("This computer converts video through VA-API."),
    };
  switch (name) {
    case "videotoolbox":
      return {
        title: t("Apple VideoToolbox"),
        detail: t("This Mac converts video in hardware."),
      };
    case "nvenc":
      return {
        title: t("NVIDIA NVENC"),
        detail: t("This computer's NVIDIA card converts video."),
      };
    case "qsv":
      return {
        title: t("Intel Quick Sync"),
        detail: t("This computer's Intel graphics convert video."),
      };
    case "vaapi":
      return {
        title: t("AMD VA-API"),
        detail: t("This computer converts video through VA-API."),
      };
    default:
      return { title: t("Software only"), detail: t("Transcoding uses the CPU.") };
  }
}

function toneMapName(path: string): string {
  switch (path) {
    case "zscale":
    case "tonemapx":
      return t("Software");
    case "libplacebo":
      return "libplacebo";
    case "opencl":
      return "OpenCL";
    case "vaapi":
      return "VA-API";
    case "videotoolbox":
      return "VideoToolbox";
    case "cuda":
      return "NVIDIA";
    case "qsv":
      return "Quick Sync";
    default:
      return path;
  }
}

/** Detected transcoding hardware: the graphics vendor, codecs, and HDR paths. */
export function HardwareStatus({ state }: { state?: AsyncState<Hardware | null> } = {}) {
  const own = useHardwareStatus(!state);
  const hw = state ?? own;
  const data = hw.data;
  const accel = data?.hwaccel;
  const encoders = accel?.encoders ?? [];
  const none = !accel?.name || accel.name === "none";
  const ffmpeg = typeof data?.ffmpeg === "string" ? data.ffmpeg : "";
  const codecs = encodeCodecs(encoders);
  const toneMap = data?.toneMap ?? [];
  const toneMapLabels = Array.from(new Set(toneMap.map(toneMapName)));
  const toneDecision = data?.toneMapDecision;
  const toneRoute =
    toneDecision?.route === "cpu"
      ? t("Software")
      : toneDecision?.route === "gpu"
        ? t("Hardware tone mapping")
        : t("Unavailable");
  const formatSpeed = (speed: number) =>
    `${new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(speed)}×`;

  const colour = data?.colour ?? [];
  const vendor = typeof data?.vendor === "string" ? data.vendor : undefined;
  const copy = vendorCopy(none ? "none" : accel?.name, vendor);

  return (
    <Section
      title={t("Transcoding hardware")}
      description={t(
        "What this machine can use to convert video for devices that can't play it directly.",
      )}
    >
      {hw.error && !data ? (
        <ErrorPanel error={hw.error} onRetry={() => void hw.reload()} />
      ) : !data ? (
        <LoadingRows rows={1} />
      ) : data.error ? (
        <p role="alert" className="text-sm text-danger">
          {data.error}
        </p>
      ) : (
        <div className="space-y-4">
          <div className="flex items-center gap-4">
            <span className="grid size-14 shrink-0 place-items-center rounded-2xl bg-bg-subtle text-fg">
              {vendorMark(none ? undefined : accel?.name, vendor)}
            </span>
            <div className="min-w-0">
              <p className="text-base font-semibold text-fg">{copy.title}</p>
              <p className="text-sm text-fg-muted">{copy.detail}</p>
              <p className="mt-1 text-xs text-fg-muted">
                {[
                  accel?.device ? t("Device {device}", { device: accel.device }) : "",
                  ffmpeg ? t("FFmpeg {version}", { version: ffmpeg }) : "",
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </p>
            </div>
          </div>
          {typeof data.videoEncoderLimit === "number" ? (
            <p className="text-sm text-fg-muted">
              {data.videoEncoderLimit > 0
                ? t("This server can convert up to {count} video streams at once.", {
                    count: data.videoEncoderLimit,
                  })
                : t("This server has no set limit on simultaneous video conversions.")}
            </p>
          ) : null}
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="rounded-lg border border-border bg-bg-subtle p-3">
              <p className="text-xs font-medium text-fg-muted">{t("Encodes")}</p>
              <div className="mt-2 flex flex-wrap gap-1.5" aria-label={t("Hardware encoders")}>
                {codecs.length ? (
                  codecs.map((codec) => (
                    <Badge key={codec} variant="secondary">
                      {codecLabel(codec)}
                    </Badge>
                  ))
                ) : (
                  <span className="text-sm text-fg-muted">
                    {t("No hardware encoders detected")}
                  </span>
                )}
              </div>
            </div>
            <div className="rounded-lg border border-border bg-bg-subtle p-3">
              <p className="text-xs font-medium text-fg-muted">{t("HDR")}</p>
              <div className="mt-2 flex flex-wrap gap-1.5" aria-label={t("HDR")}>
                {toneMap.length ? (
                  <>
                    <Badge variant="secondary">{t("HDR10")}</Badge>
                    {data.hardwareToneMap === true ? (
                      <Badge variant="secondary">{t("Hardware tone mapping")}</Badge>
                    ) : null}
                  </>
                ) : (
                  <span className="text-sm text-fg-muted">
                    {t("No HDR conversion on this computer.")}
                  </span>
                )}
              </div>
              {toneDecision || toneMapLabels.length ? (
                <p className="mt-2 text-xs text-fg-muted">
                  {t("HDR to SDR uses {paths}.", {
                    paths: toneDecision ? toneRoute : toneMapLabels.join(", "),
                  })}
                </p>
              ) : null}
              {toneDecision && (toneDecision.cpu || toneDecision.gpu) ? (
                <div className="mt-2 flex flex-wrap gap-1.5">
                  <Badge variant="secondary">
                    {t("{height}p", { height: toneDecision.output })}
                  </Badge>
                  {toneDecision.cpu ? (
                    <Badge variant="secondary">
                      {t("Software")} {formatSpeed(toneDecision.cpu)}
                    </Badge>
                  ) : null}
                  {toneDecision.gpu ? (
                    <Badge variant="secondary">
                      {t("Hardware tone mapping")} {formatSpeed(toneDecision.gpu)}
                    </Badge>
                  ) : null}
                </div>
              ) : null}
              {data.hardwareToneMap === false && !none ? (
                <p className="mt-1 text-xs text-fg-muted">
                  {t("Hardware HDR tone mapping is unavailable on this device.")}
                </p>
              ) : null}
              {colour.length ? (
                <p className="mt-1 text-xs text-fg-muted">{t("Colour conversion is available.")}</p>
              ) : null}
            </div>
          </div>
          {data.filtersProbed === false && !data.notice && !data.playbackError ? (
            <p className="text-sm text-fg-muted">{t("Optional filters were not read.")}</p>
          ) : null}
          {data.playbackError || data.notice ? (
            <p role="alert" className="text-sm text-danger">
              {data.playbackError || data.notice}
            </p>
          ) : null}
          {data.enhancement && canUseRtxVideo(data) ? (
            <div
              className="rounded-lg border border-border p-3"
              aria-label={t("Server enhancement status")}
            >
              <p className="text-sm font-medium text-fg">
                {t("RTX Video: {status}", {
                  status:
                    data.enhancement.active > 0
                      ? tn(data.enhancement.active, "{count} active", "{count} active")
                      : data.enhancement.available
                        ? t("Detected · idle")
                        : data.enhancement.setupState === "off"
                          ? t("Off")
                          : data.enhancement.setupState === "installing"
                            ? t("Installing runtime")
                            : data.enhancement.setupState === "checking"
                              ? t("Checking support")
                              : t("Unavailable"),
                })}
              </p>
              <p className="mt-1 text-xs text-fg-muted">
                {data.enhancement.reason ||
                  t("Detected support is verified when an eligible stream starts.")}
                {data.enhancement.version ? ` · ${data.enhancement.version}` : ""}
              </p>
            </div>
          ) : null}
        </div>
      )}
    </Section>
  );
}

/**
 * Fake recorded plays for Activity: History, Stats and Experience. Plays from
 * before `measuredSince` carry no start time or buffering, like a server that
 * upgraded part-way through the period.
 */
import type {
  ExperienceCause,
  ExperienceMetrics,
  ExperienceOverview,
  ExperiencePlay,
  ExperienceViewer,
  ReasonKind,
} from "../pages/activity/types";

type Result = { status: number; body: unknown };

type MockPlay = ExperiencePlay & {
  userId: number;
  username: string;
  ended: number;
  bytes: number;
  completed: boolean;
  failovers: number;
  downSwitches: number;
  sourceKbps: number;
  deliveredKbps: number;
  uhd: boolean;
  hdr: boolean;
};

const DAY = 86_400;

/** Small deterministic PRNG so screenshots and tests are stable. */
function rng(seed: number) {
  let s = seed >>> 0;
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0;
    return s / 2 ** 32;
  };
}

const TITLES = [
  "The Long Night (2024)",
  "Harbor Lights - S02E04 - Low Tide",
  "Northbound (2023)",
  "Small Hours - S01E07 - Static",
  "Glass Orchard (2025)",
  "The Cartographers - S03E01 - Terra Incognita",
  "Paper Moons (2022)",
  "Quiet Engines (2024)",
  "Harbor Lights - S02E05 - Undertow",
  "Saltwater Kids (2021)",
];

type Persona = {
  id: number;
  name: string;
  devices: string[];
  perDay: number;
  start: [number, number];
  buffersPerPlay: number;
  remote: number;
  transcode: number;
  reason?: { kind: ReasonKind; detail: string; device: string };
  failRate: number;
  failReason: string;
  switchRate: number;
  kbps: number;
  uhd: number;
};

const PERSONAS: Persona[] = [
  {
    id: 1,
    name: "Alex Rivera",
    devices: ["Living Room Apple TV", "Alex's iPhone"],
    perDay: 1.6,
    start: [1400, 3800],
    buffersPerPlay: 0.05,
    remote: 0.1,
    transcode: 0.05,
    reason: { kind: "container", detail: "avi", device: "Alex's iPhone" },
    failRate: 0,
    failReason: "",
    switchRate: 0.02,
    kbps: 18_000,
    uhd: 0.6,
  },
  {
    id: 2,
    name: "Sam Rivera",
    devices: ["Sam's iPad", "Hotel TV"],
    perDay: 1.1,
    start: [4500, 12_500],
    buffersPerPlay: 2.4,
    remote: 0.85,
    transcode: 0.1,
    reason: { kind: "bitrate", detail: "", device: "Hotel TV" },
    failRate: 0.04,
    failReason: "",
    switchRate: 0.1,
    kbps: 38_000,
    uhd: 0.5,
  },
  {
    id: 3,
    name: "The Kids",
    devices: ["Playroom Fire TV"],
    perDay: 2.2,
    start: [3000, 7500],
    buffersPerPlay: 0.3,
    remote: 0,
    transcode: 0.8,
    reason: { kind: "audio_codec", detail: "dts", device: "Playroom Fire TV" },
    failRate: 0.02,
    failReason: "",
    switchRate: 0.03,
    kbps: 9_000,
    uhd: 0,
  },
  {
    id: 4,
    name: "Jordan Lee",
    devices: ["Jordan's Shield", "Chrome"],
    perDay: 0.8,
    start: [2500, 9000],
    buffersPerPlay: 0.4,
    remote: 0.2,
    transcode: 0.15,
    reason: { kind: "hdr", detail: "", device: "Chrome" },
    failRate: 0.22,
    failReason: "Every release failed: article not found on any provider",
    switchRate: 0.6,
    kbps: 22_000,
    uhd: 0.3,
  },
];

function buildPlays(now: number, measuredSince: number): MockPlay[] {
  const out: MockPlay[] = [];
  for (const p of PERSONAS) {
    const r = rng(p.id * 7919);
    for (let d = 0; d < 34; d++) {
      const count = Math.floor(p.perDay + r() * 1.2);
      for (let i = 0; i < count; i++) {
        const started = now - d * DAY - Math.floor(r() * DAY * 0.9) - 600;
        const measured = started >= measuredSince;
        const failed = r() < p.failRate;
        const transcode = !!p.reason && r() < p.transcode;
        const copy = !transcode && r() < 0.15;
        const device = transcode && p.reason ? p.reason.device : p.devices[i % p.devices.length]!;
        const durationMs = (40 + Math.floor(r() * 90)) * 60_000;
        const abandoned = !failed && r() < 0.1;
        const watchedMs = failed
          ? 0
          : abandoned
            ? Math.floor(r() * 100_000)
            : Math.floor(durationMs * (0.4 + r() * 0.6));
        const completed = !failed && !abandoned && watchedMs > durationMs * 0.9;
        const buffers = failed
          ? 0
          : Math.floor(p.buffersPerPlay * (watchedMs / 3_600_000) * 2 * r());
        const switches = r() < p.switchRate ? 1 + Math.floor(r() * 2) : 0;
        const uhd = !transcode && r() < p.uhd;
        const remote = r() < p.remote;
        const title = TITLES[Math.floor(r() * TITLES.length)]!;
        const sourceKbps = Math.round(p.kbps * (0.6 + r() * 0.8));
        const startMs = Math.round(p.start[0] + r() * (p.start[1] - p.start[0]));
        out.push({
          id: `play-${p.id}-${d}-${i}`,
          userId: p.id,
          username: p.name,
          title,
          device,
          platform: device.includes("Chrome") ? "Chrome" : "tvOS",
          started,
          ended: d === 0 && i === 0 ? 0 : started + Math.round(watchedMs / 1000) + 30,
          measured,
          startMs: measured && !failed ? startMs : null,
          bufferEvents: measured ? buffers : null,
          bufferMs: measured ? buffers * Math.round(2000 + r() * 6000) : null,
          failed,
          error: failed && p.failReason && r() < 0.7 ? p.failReason : "",
          decision: transcode ? "transcode" : copy ? "copy" : "directplay",
          reasonKind: transcode && p.reason ? p.reason.kind : copy ? "container" : "",
          reasonDetail: transcode && p.reason ? p.reason.detail : copy ? "mkv" : "",
          origin: r() < 0.7 ? "Usenet" : "Real-Debrid",
          release: `${title.replace(/[^A-Za-z0-9]+/g, ".")}.${uhd ? "2160p" : "1080p"}.WEB-DL.DDP5.1.H.265-GRP`,
          remote,
          watchedMs,
          durationMs,
          switches,
          downSwitches: switches && r() < 0.5 ? 1 : 0,
          bytes: Math.round((sourceKbps * 1000 * watchedMs) / 8000),
          completed,
          failovers: switches,
          sourceKbps,
          deliveredKbps: transcode ? Math.min(sourceKbps, 8000) : sourceKbps,
          uhd,
          hdr: uhd && r() < 0.8,
        });
      }
    }
  }
  return out.sort((a, b) => b.started - a.started);
}

function quantile(sorted: number[], q: number): number | null {
  if (!sorted.length) return null;
  return sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))]!;
}

function metrics(plays: MockPlay[]): ExperienceMetrics {
  const measured = plays.filter((p) => p.measured);
  const starts = measured
    .filter((p) => p.startMs != null)
    .map((p) => p.startMs!)
    .sort((a, b) => a - b);
  const measuredWatchedMs = measured.reduce((n, p) => n + p.watchedMs, 0);
  const bufferEvents = measured.reduce((n, p) => n + (p.bufferEvents ?? 0), 0);
  const failed = plays.filter((p) => p.ended && p.failed);
  const reasons = new Map<string, number>();
  for (const p of failed) reasons.set(p.error, (reasons.get(p.error) ?? 0) + 1);
  const transcodes = new Map<string, { kind: ReasonKind; detail: string; count: number }>();
  for (const p of plays)
    if (p.decision === "transcode" && p.reasonKind) {
      const key = `${p.reasonKind}:${p.reasonDetail}`;
      const cur = transcodes.get(key) ?? { kind: p.reasonKind, detail: p.reasonDetail, count: 0 };
      cur.count++;
      transcodes.set(key, cur);
    }
  const watched = plays.reduce((n, p) => n + p.watchedMs, 0);
  const share = (f: (p: MockPlay) => boolean) =>
    watched ? (plays.filter(f).reduce((n, p) => n + p.watchedMs, 0) / watched) * 100 : null;
  const weighted = (f: (p: MockPlay) => number) =>
    watched ? Math.round(plays.reduce((n, p) => n + f(p) * p.watchedMs, 0) / watched) : null;
  return {
    plays: plays.length,
    measuredPlays: measured.length,
    startMedianMs: quantile(starts, 0.5),
    startP90Ms: quantile(starts, 0.9),
    bufferEventsPerHour: measuredWatchedMs ? bufferEvents / (measuredWatchedMs / 3_600_000) : null,
    bufferMs: measured.reduce((n, p) => n + (p.bufferMs ?? 0), 0),
    measuredWatchedMs,
    failedStarts: failed.length,
    failedReasons: [...reasons]
      .map(([reason, count]) => ({ reason, count }))
      .sort((a, b) => b.count - a.count)
      .slice(0, 3),
    decisions: {
      directplay: plays.filter((p) => p.decision === "directplay").length,
      copy: plays.filter((p) => p.decision === "copy").length,
      transcode: plays.filter((p) => p.decision === "transcode").length,
    },
    topTranscodeReason: [...transcodes.values()].sort((a, b) => b.count - a.count)[0] ?? null,
    sourceKbps: weighted((p) => p.sourceKbps),
    deliveredKbps: weighted((p) => p.deliveredKbps),
    uhdPct: share((p) => p.uhd),
    hdrPct: share((p) => p.hdr),
    switches: plays.reduce((n, p) => n + p.switches, 0),
    downSwitches: plays.reduce((n, p) => n + p.downSwitches, 0),
    finished: plays.filter((p) => p.completed).length,
    abandoned: plays.filter((p) => p.ended && !p.completed && !p.failed && p.watchedMs < 120_000)
      .length,
    remotePct: share((p) => p.remote),
    watchedMs: plays.reduce((n, p) => n + p.watchedMs, 0),
    topDevice: topDevice(plays),
  };
}

function topDevice(plays: MockPlay[]): ExperienceMetrics["topDevice"] {
  const counts = new Map<string, number>();
  for (const p of plays) {
    const name = p.device || p.platform;
    if (name) counts.set(name, (counts.get(name) ?? 0) + 1);
  }
  const [name, n] = [...counts].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))[0] ?? [];
  return name ? { name, plays: n! } : null;
}

function cause(plays: MockPlay[], m: ExperienceMetrics): ExperienceCause {
  if (m.measuredPlays < 3) return { kind: "not_enough_data" };
  if (m.failedStarts / Math.max(1, m.plays) > 0.1)
    return {
      kind: "failed_starts",
      reason: m.failedReasons[0]?.reason ?? "",
      count: m.failedStarts,
    };
  if ((m.remotePct ?? 0) > 50 && (m.sourceKbps ?? 0) > 20_000)
    return { kind: "away_bitrate", remotePct: m.remotePct!, sourceKbps: m.sourceKbps! };
  const t = m.topTranscodeReason;
  if (t && m.decisions.transcode / Math.max(1, m.plays) > 0.5) {
    const onDevice = plays.filter((p) => p.decision === "transcode");
    const device = onDevice[0]?.device ?? "";
    return {
      kind: "transcode_device",
      device,
      reasonKind: t.kind,
      reasonDetail: t.detail,
      share: (m.decisions.transcode / m.plays) * 100,
    };
  }
  if (m.switches / Math.max(1, m.plays) > 0.3)
    return { kind: "source_switches", switches: m.switches, plays: m.plays };
  if ((m.startMedianMs ?? 0) > 5000) return { kind: "slow_starts", medianMs: m.startMedianMs! };
  if ((m.bufferEventsPerHour ?? 0) >= 1)
    return { kind: "buffering", perHour: m.bufferEventsPerHour! };
  return { kind: "none" };
}

function publicPlay(p: MockPlay): ExperiencePlay {
  const {
    userId: _u,
    username: _n,
    ended: _e,
    bytes: _b,
    completed: _c,
    failovers: _f,
    downSwitches: _d,
    sourceKbps: _s,
    deliveredKbps: _k,
    uhd: _h,
    hdr: _r,
    ...rest
  } = p;
  return rest;
}

export function createExperienceMock(nowMs = Date.now()) {
  const now = Math.floor(nowMs / 1000);
  const measuredSince = now - 9 * DAY - 3 * 3600;
  const plays = buildPlays(now, measuredSince);
  const window = (days: number) => plays.filter((p) => p.started >= now - days * DAY);
  const windowDays = (q: URLSearchParams) => (q.get("days") === "30" ? 30 : 7);

  function overview(days: number): ExperienceOverview {
    const inWindow = window(days);
    return {
      days,
      since: now - days * DAY,
      measuredSince,
      household: metrics(inWindow),
      viewers: PERSONAS.map((p) => ({
        userId: p.id,
        username: p.name,
        metrics: metrics(inWindow.filter((x) => x.userId === p.id)),
      })).filter((v) => v.metrics.plays),
    };
  }

  function viewer(id: number, days: number): ExperienceViewer | null {
    const persona = PERSONAS.find((p) => p.id === id);
    if (!persona) return null;
    const mine = plays.filter((p) => p.userId === id);
    const inWindow = mine.filter((p) => p.started >= now - days * DAY);
    const m = metrics(inWindow);
    const trend = Array.from({ length: days }, (_, i) => {
      const start = now - (days - i) * DAY;
      const dayPlays = inWindow.filter((p) => p.started >= start && p.started < start + DAY);
      const dm = metrics(dayPlays);
      return {
        day: new Date(start * 1000).toISOString().slice(0, 10),
        plays: dayPlays.length,
        startMedianMs: dm.startMedianMs,
        bufferEventsPerHour: dm.bufferEventsPerHour,
      };
    });
    return {
      userId: id,
      username: persona.name,
      days,
      since: now - days * DAY,
      measuredSince,
      metrics: m,
      trend,
      plays: mine.slice(0, 20).map(publicPlay),
      cause: cause(inWindow, m),
    };
  }

  function historyPlay(p: MockPlay) {
    return {
      ...publicPlay(p),
      userId: p.userId,
      username: p.username,
      ratingKey: 0,
      updated: p.ended || now,
      ended: p.ended,
      bytes: p.bytes,
      completed: p.completed,
      failovers: p.failovers,
    };
  }

  function handle(method: string, seg: string[], q: URLSearchParams): Result | null {
    if (method !== "GET") return null;
    if (seg[0] === "experience" && seg.length === 1)
      return { status: 200, body: overview(windowDays(q)) };
    if (seg[0] === "experience" && seg[1] === "viewers" && seg[2] && seg.length === 3) {
      const v = viewer(Number(seg[2]), windowDays(q));
      return v
        ? { status: 200, body: v }
        : { status: 404, body: { error: { code: "not_found", message: "no such viewer" } } };
    }
    if (seg[0] === "stats" && seg[1] === "plays" && seg[2] && seg.length === 3) {
      const p = plays.find((x) => x.id === decodeURIComponent(seg[2]!));
      return p
        ? { status: 200, body: historyPlay(p) }
        : { status: 404, body: { error: { code: "stats", message: "Play not found" } } };
    }
    if (seg[0] === "stats" && (seg[1] === "history" || seg[1] === "summary")) {
      const days = Number(q.get("days")) || 30;
      const user = Number(q.get("user")) || 0;
      const rows = window(days).filter((p) => !user || p.userId === user);
      if (seg[1] === "history") return { status: 200, body: rows.slice(0, 500).map(historyPlay) };
      const group = (key: (p: MockPlay) => string) => {
        const m = new Map<
          string,
          { label: string; plays: number; watchedMs: number; bytes: number }
        >();
        for (const p of rows) {
          const k = key(p);
          const b = m.get(k) ?? { label: k, plays: 0, watchedMs: 0, bytes: 0 };
          b.plays++;
          b.watchedMs += p.watchedMs;
          b.bytes += p.bytes;
          m.set(k, b);
        }
        return [...m.values()];
      };
      return {
        status: 200,
        body: {
          plays: rows.length,
          watchedMs: rows.reduce((n, p) => n + p.watchedMs, 0),
          bytes: rows.reduce((n, p) => n + p.bytes, 0),
          completed: rows.filter((p) => p.completed).length,
          daily: group((p) => new Date(p.started * 1000).toISOString().slice(0, 10)).sort((a, b) =>
            a.label.localeCompare(b.label),
          ),
          hourly: group((p) =>
            String(new Date(p.started * 1000).getUTCHours()).padStart(2, "0"),
          ).sort((a, b) => a.label.localeCompare(b.label)),
          platforms: group((p) => p.platform).sort((a, b) => b.plays - a.plays),
          titles: group((p) => p.title).sort((a, b) => b.plays - a.plays),
          users: group((p) => `${p.username} (${p.userId})`).sort((a, b) => b.plays - a.plays),
        },
      };
    }
    return null;
  }

  return { handle };
}

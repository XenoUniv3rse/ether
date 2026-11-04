import { t } from "../../i18n";
import type { EtherUser } from "../../lib/types";

/** One aggregate bucket from /stats/summary (a day, an hour, a device, a title, a viewer). */
export type Bucket = { label: string; plays: number; watchedMs: number; bytes: number };

export type Summary = {
  plays: number;
  watchedMs: number;
  bytes: number;
  completed: number;
  daily: Bucket[];
  hourly: Bucket[];
  platforms: Bucket[];
  titles: Bucket[];
  users: Bucket[];
};

export type Play = {
  id: string;
  userId: number;
  username: string;
  title: string;
  device: string;
  platform: string;
  decision: string;
  started: number;
  ended: number;
  bytes: number;
  watchedMs: number;
  completed: boolean;
  failovers: number;
  error: string;
};

/** Period, viewer and year filters shared by History and Stats. */
export interface ViewingFilter {
  days: string;
  user: string;
  year: string;
}

export const viewerLabel = (viewer: EtherUser) => {
  const name = viewer.title || viewer.username;
  return viewer.isOwner ? t("{name} (owner)", { name }) : name;
};

export function viewingQuery(f: ViewingFilter): string {
  return `?days=${f.days}&user=${encodeURIComponent(f.user)}${f.year ? `&year=${f.year}` : ""}`;
}

/** Why a play was not direct played, grouped for plain-language display. */
export type ReasonKind =
  | "audio_codec"
  | "audio_limit"
  | "audio_processing"
  | "video_codec"
  | "video_profile"
  | "resolution"
  | "bitrate"
  | "subtitles"
  | "container"
  | "hdr"
  | "client_setting"
  | "other";

/** Viewer-experience measures for one household or viewer over a window. */
export type ExperienceMetrics = {
  plays: number;
  measuredPlays: number;
  startMedianMs: number | null;
  startP90Ms: number | null;
  bufferEventsPerHour: number | null;
  bufferMs: number;
  measuredWatchedMs: number;
  failedStarts: number;
  failedReasons: { reason: string; count: number }[];
  decisions: { directplay: number; copy: number; transcode: number };
  topTranscodeReason: { kind: ReasonKind; detail: string; count: number } | null;
  sourceKbps: number | null;
  deliveredKbps: number | null;
  uhdPct: number | null;
  hdrPct: number | null;
  switches: number;
  downSwitches: number;
  finished: number;
  abandoned: number;
  remotePct: number | null;
  /** Total viewing time in the window. */
  watchedMs: number;
  /** The device with the most plays, as recorded. */
  topDevice: { name: string; plays: number } | null;
};

export type ExperienceOverview = {
  days: number;
  since: number;
  measuredSince: number;
  household: ExperienceMetrics;
  viewers: { userId: number; username: string; metrics: ExperienceMetrics }[];
};

export type ExperiencePlay = {
  id: string;
  title: string;
  device: string;
  platform: string;
  started: number;
  measured: boolean;
  startMs: number | null;
  bufferEvents: number | null;
  bufferMs: number | null;
  failed: boolean;
  error: string;
  decision: string;
  reasonKind: ReasonKind | "";
  reasonDetail: string;
  qualityFallback?: string;
  origin: string;
  release: string;
  remote: boolean;
  watchedMs: number;
  durationMs: number;
  switches: number;
};

export type ExperienceCause =
  | { kind: "none" }
  | { kind: "not_enough_data" }
  | { kind: "failed_starts"; reason: string; count: number }
  | { kind: "away_bitrate"; remotePct: number; sourceKbps: number }
  | {
      kind: "transcode_device";
      device: string;
      reasonKind: ReasonKind;
      reasonDetail: string;
      share: number;
    }
  | { kind: "source_switches"; switches: number; plays: number }
  | { kind: "slow_starts"; medianMs: number }
  | { kind: "buffering"; perHour: number };

export type ExperienceViewer = {
  userId: number;
  username: string;
  days: number;
  since: number;
  measuredSince: number;
  metrics: ExperienceMetrics;
  trend: {
    day: string;
    plays: number;
    startMedianMs: number | null;
    bufferEventsPerHour: number | null;
  }[];
  plays: ExperiencePlay[];
  cause: ExperienceCause;
};

/** One play from /stats/plays/{id}: the History row plus its experience. */
export type PlayDetail = Play &
  Omit<ExperiencePlay, keyof Play> & {
    ratingKey?: number;
    platform: string;
    durationMs: number;
  };

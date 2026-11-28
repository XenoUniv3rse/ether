/**
 * Every individual owner setting ⌘K can jump to. Schema-driven forms are
 * listed from their field groups; hand-built sections are listed by the
 * stable row key they render. `more` names the "More options" disclosure
 * (its data-more key) that hides the row, so the jump can open it.
 */

import { t } from "../i18n";
import type { FieldDef, FieldGroup } from "../pages/settings/SchemaForm";
import * as S from "../pages/settings/schema";
import { UpcomingEpisodeSettings } from "../pages/libraries/browser/HomeReleaseSettings";

export interface SettingHit {
  /** Stable row id, independent of display language. */
  key: string;
  title: string;
  /** Section or page the setting sits in, shown as the search subtitle. */
  where: string;
  href: string;
  /** Stable id of the "More options" disclosure hiding the row. */
  more?: string;
  keywords?: string;
}

function fieldHits(
  defs: FieldDef[],
  href: string,
  where: string,
  more: string | undefined,
): SettingHit[] {
  return defs.flatMap((f) => {
    if (f.kind === "hidden") return [];
    if (f.kind === "group") return fieldHits(f.fields, href, where, more);
    return [
      {
        key: f.key,
        title: f.label,
        where,
        href,
        more,
        keywords: typeof f.help === "string" ? f.help : undefined,
      },
    ];
  });
}

function groupHits(
  groups: readonly FieldGroup[],
  href: string,
  page: string,
  moreKey?: (g: FieldGroup) => string | undefined,
): SettingHit[] {
  return groups.flatMap((g) => {
    const where = g.title ? `${page} › ${g.title}` : page;
    return [
      ...fieldHits(g.fields, href, where, undefined),
      ...fieldHits(g.more ?? [], href, where, moreKey ? moreKey(g) : g.id),
    ];
  });
}

function rows(
  href: string,
  where: string,
  titles: [string, string][],
  more?: string,
): SettingHit[] {
  return titles.map(([key, title]) => ({ key, title, where, href, more }));
}

export function settingsIndex(): SettingHit[] {
  const general = t("General");
  const apps = t("Apps");
  const serverAddress = `${general} › ${t("Server address")}`;
  const releases = t("Releases");
  const playback = t("Playback");
  const cachePreloading = `${t("Cache")} › ${t("Preloading")}`;
  const tmdbSettings = "TMDB";
  const viewers = t("Viewers");
  const plex = "Plex";
  const away = t("Away from home");
  const connectGroup = S.SERVER.find((g) => g.id === "connect");
  return [
    {
      key: "looks",
      title: t("Styles"),
      where: t("Live TV"),
      href: "/livetv/looks",
      keywords: "on air graphics brand styles themes assets media collections reels",
    },
    {
      key: "personal-channel",
      title: t("Personal channel"),
      where: t("Live TV"),
      href: "/livetv",
      keywords: "recipe viewer personal",
    },
    {
      key: "poster-source",
      title: t("Poster source"),
      where: t("Artwork"),
      href: "/metadata/artwork",
      keywords: "TMDB RPDB Custom PostersPlus XRDB BetterPosters",
    },
    ...["PostersPlus", "XRDB", "BetterPosters", "RPDB"].map((title) => ({
      key: title.toLowerCase(),
      title,
      where: t("Integrations"),
      href: "/services/overview",
      keywords: "poster API key",
    })),
    // Cache › Preloading
    ...rows("/cache/preloading", cachePreloading, [
      ["allow-opening-and-resume-warming", t("Allow opening and resume warming")],
      ["warm-detail-pages", t("Warm detail pages")],
      ["prepare-the-next-episode", t("Prepare the next episode")],
      ["keep-resume-points-warm", t("Keep resume points warm")],
      ["warm-up-new-arrivals", t("Warm up new arrivals")],
      ["read-ahead-while-playing", t("Read ahead while playing")],
      ["preload-movies-and-tv", t("Preload movies & TV")],
      ["keep-warm", t("Keep warm")],
      ["speculative-warming", t("Speculative warming")],
      ["preload-allocation", t("Preload allocation")],
      ["include-in-your-shelf", t("Include in your shelf")],
      ["selection-strategy", t("Selection strategy")],
      ["household-influence", t("Household influence")],
      ["learn-from", t("Learn from")],
      ["episode-window-per-show", t("Episode window per show")],
    ]),
    ...rows(
      "/cache/preloading",
      cachePreloading,
      [
        ["opening-window", t("Opening window")],
        ["next-episode-resume-window", t("Next episode / resume window")],
        ["prepare-next-episode-at", t("Prepare next episode at")],
      ],
      "playback-warming",
    ),
    ...rows(
      "/cache/preloading",
      cachePreloading,
      [
        ["household-history-window", t("Household history window")],
        ["maximum-preloaded-titles", t("Maximum preloaded titles")],
        ["preload-speed", t("Preload speed")],
        ["keep-disk-space-free", t("Keep disk space free")],
        ["preload-during-playback", t("Preload during playback")],
      ],
      "local-shelf",
    ),
    // General
    ...groupHits(
      S.SERVER.filter((g) => g.id !== "connect"),
      "/server",
      general,
    ),
    ...rows("/server", general, [
      [
        "send-indexers-debrid-and-usenet-through-a-proxy",
        t("Send indexers, debrid and Usenet through a proxy"),
      ],
      ["proxy-address", t("Proxy address")],
    ]),
    ...rows(
      "/server",
      general,
      [
        ["username", t("Username")],
        ["password", t("Password")],
        ["names-that-stay-on-this-network", t("Names that stay on this network")],
      ],
      "how-ether-reaches-the-internet",
    ),
    // General › Server address
    ...rows("/server", serverAddress, [
      ["server-port", t("Main port")],
      ["local-address", t("Local")],
      ["remote-address", t("Remote")],
      ["tailscale-address", t("Tailscale")],
    ]),
    ...fieldHits(
      (connectGroup?.more ?? []).filter((f) => f.key === "allowRemoteConsole"),
      "/server",
      serverAddress,
      undefined,
    ),
    ...fieldHits(
      (connectGroup?.more ?? []).filter((f) => f.key !== "allowRemoteConsole"),
      "/server",
      serverAddress,
      "connect",
    ),
    ...groupHits(S.AWAY, "/apps/overview", apps).map((h) => ({
      ...h,
      where: `${apps} › ${away}`,
    })),
    ...groupHits(
      S.PLEX.filter((g) => g.id === "compatibility"),
      "/apps/plex/account",
      plex,
    ),
    ...rows("/apps/plex/account", plex, [
      ["use-plex-s-metadata-for-plex-apps", t("Use Plex's metadata for Plex apps")],
    ]),
    ...groupHits(
      S.PLEX.filter((g) => g.id !== "compatibility"),
      "/apps/plex/remote",
      plex,
    ),
    ...groupHits(S.DOWNLOADS, "/apps/plex/downloads", plex),
    // Libraries (TMDB source & per-library new releases)
    ...groupHits(S.CATALOG, "/services/tmdb", tmdbSettings),
    ...groupHits([UpcomingEpisodeSettings], "/services/tmdb", tmdbSettings),
    ...rows("/services/tmdb", tmdbSettings, [["hide-obscure-titles", t("Hide obscure titles")]]),
    ...rows("/services/tmdb", `${tmdbSettings} › ${t("New releases")}`, [
      ["when-they-can-play", t("When they can play")],
      ["on-digital-release", t("On digital release")],
      ["home-release-offset", t("Home-release offset (days)")],
      ["right-away", t("Right away")],
    ]),
    // Viewers (Watchlist monitoring)
    ...rows("/viewers/watchlists", `${viewers} › ${t("Watchlists")}`, [
      ["watchlist-monitoring", t("Watchlist monitoring")],
      ["plex-watchlists", t("Plex watchlists")],
      ["trakt-watchlists", t("Trakt watchlists")],
      ["tell-me-too", t("Tell me too")],
      ["plex-friends-watchlist-rss", t("Friends' Watchlist RSS")],
      ["plex-your-watchlist-rss", t("Your Watchlist RSS")],
      ["check-every", t("Check every")],
    ]),
    // Sources
    ...groupHits(S.DEBRID, "/sources/debrid", t("Debrid"), () => "debrid"),
    ...rows(
      "/sources/indexers",
      t("Indexers"),
      [
        ["daily-budget-for-new-release-checks", t("Daily budget for new-release checks")],
        ["daily-plugin-usenet-checks", t("Daily background Usenet checks per plugin")],
      ],
      "daily-budgets",
    ),
    ...rows(
      "/sources/indexers",
      t("Site definitions"),
      [
        ["definitions-source", t("Definitions source")],
        ["flaresolverr", "FlareSolverr"],
      ],
      "site-definitions",
    ),
    // Releases
    ...rows("/releases/profiles", releases, [["source-order", t("Source order")]]),
    ...rows("/releases/check", releases, [["check-quality", t("Who gets what")]]),
    ...groupHits(S.PLAY_VERSIONS, "/releases/profiles", releases),
    ...rows("/releases/searching", `${releases} › ${t("Start speed")}`, [
      ["start-before-the-health-check-finishes", t("Start before the health check finishes")],
    ]),
    ...rows(
      "/releases/searching",
      `${releases} › ${t("Start speed")}`,
      [["concurrent-checks", t("Concurrent checks")]],
      "start-speed",
    ),
    ...groupHits(S.RELEASE_SEARCH, "/releases/searching", releases),
    ...groupHits(S.RELEASE_BLOCKS, "/releases/blocklist", releases),
    // Playback
    ...rows("/playback/audio", `${playback} › ${t("Audio")}`, [
      ["audio-languages", t("Audio languages")],
      ["prefer-the-original-language", t("Prefer the original language")],
    ]),
    ...rows("/playback/subtitles", `${playback} › ${t("Subtitles")}`, [
      ["subtitle-languages", t("Subtitle languages")],
      ["show-subtitles", t("Show subtitles")],
      ["subtitle-style", t("Subtitle style")],
    ]),
    ...rows(
      "/playback/subtitles",
      `${playback} › ${t("Subtitle appearance")}`,
      [
        ["size", t("Size")],
        ["text-colour", t("Text colour")],
        ["background", t("Background")],
        ["position", t("Position")],
      ],
      "subtitle-appearance",
    ),
    ...rows(
      "/playback/audio",
      `${playback} › ${t("Audio processing")}`,
      [
        ["normalize-loudness", t("Normalize loudness")],
        ["night-mode-compression", t("Night mode compression")],
        ["boost-centre-channel-dialogue", t("Boost centre-channel dialogue")],
      ],
      "audio-processing",
    ),
    ...groupHits(S.STREAM_LIMITS, "/playback/transcoding", playback),
    ...groupHits(S.TRANSCODE, "/playback/transcoding", playback),
    ...groupHits(S.EXTRAS, "/playback/prerolls", playback),
    ...rows("/playback/extras", `${playback} › ${t("Skip buttons")}`, [
      ["skip-intro", t("Skip intro")],
      ["skip-credits", t("Skip credits")],
    ]),
    ...groupHits(S.UP_NEXT, "/playback/extras", playback),
    // Home
    ...groupHits(S.PERSONAL_TOUCHES, "/recommendations", t("Recommendations")),
    // Notifications
    ...rows("/notifications/report", t("Weekly report"), [
      ["email-the-weekly-report", t("Email the weekly report")],
      ["day", t("Day")],
      ["time", t("Time")],
      ["report-viewer", t("Report viewer")],
    ]),
    // Maintenance
    ...groupHits(S.UPDATES, "/maintenance/updates", t("Updates")),
    ...rows("/maintenance/health", t("Health"), [
      ["send-uptime-heartbeat", t("Send uptime heartbeat")],
      ["secret-heartbeat-url", t("Secret heartbeat URL")],
    ]),
    ...groupHits(S.SPIKE, "/maintenance/health", t("Health")),
    ...groupHits(S.LOG_LEVEL, "/maintenance/logs", t("Logs")),
  ];
}

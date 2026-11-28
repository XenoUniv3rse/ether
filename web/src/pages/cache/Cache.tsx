import {
  Badge,
  Button,
  Input,
  SettingsSection,
  Spinner,
  Switch,
  cn,
  toast,
} from "@braedonsaunders/appkit-ui";
import { ArrowLeft, ArrowUpRight, HardDrive, Network, Settings2, Sparkles } from "lucide-react";
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useApp } from "../../app-context";
import { MoreOptions } from "../../components/ConfigSection";
import { Drawer } from "../../components/Drawer";
import { RestartButton } from "../../components/RestartDrawer";
import { SaveBar, useDraft, useUnsavedGuard } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { ToolbarPicker } from "../../components/ToolbarPicker";
import { ErrorPanel } from "../../components/feedback";
import { Field, NumberInput } from "../../components/fields";
import { OverflowTabs } from "../../components/OverflowTabs";
import { useRouteTab } from "../../components/layout";
import { Workspace } from "../../components/workspace";
import { localized, t, tn } from "../../i18n";
import { api, apiFetch } from "../../lib/api";
import { formatBytes, formatNumber } from "../../lib/format";
import { useAsync, useInterval } from "../../lib/hooks";
import type { ReleaseInfo } from "../../lib/releases";
import type { EtherConfig } from "../../lib/types";
import { LibraryCaches } from "../libraries/LibraryCache";
import { formatDuration, parseDuration, type DurationUnit } from "../settings/duration";
import { useDiscoverySettings } from "../viewers/sections/WatchlistDefaults";
import { CacheLibrary } from "./CacheLibrary";
import { CacheExplorer } from "./CacheExplorer";
import { CacheOverview } from "./CacheOverview";
import { WarmingPolicy } from "./WarmingControls";
import { CACHE_PRESETS, CachePresetCards, cachePresetKey } from "./presets";
import { selectionReason, shelfStateLabel } from "./reasons";

const CACHE_TABS = localized(
  () =>
    [
      { key: "overview", label: t("Overview") },
      { key: "explorer", label: t("Explorer") },
      { key: "library", label: t("Library") },
      { key: "preloading", label: t("Preloading") },
      { key: "libraries", label: t("Libraries") },
    ] as const,
);

type Policy = EtherConfig["cache"];
export type CacheCause = {
  code: string;
  firstSeen?: string;
  shelf?: { reasonCodes?: string[]; score?: number; state?: string };
};
export type CacheRetention = {
  kind: "pinned" | "expiry" | "replay" | "lru";
  pins?: string[];
  days?: number;
  hours?: number;
  until?: string;
};
export type ReleaseReason = {
  causes: CacheCause[];
  retention: CacheRetention;
  file: string;
  source: string;
  indexer?: string;
  origin?: string;
  ratingKey?: number;
  cachedAt?: string;
  info?: ReleaseInfo;
};
export type SegmentSpan = {
  start: number;
  end: number;
};
export type ReleaseDetail = {
  releaseId: string;
  segments: number;
  estimatedTotal?: number;
  spanCount: number;
  spans: SegmentSpan[];
  coverage?: number[];
  firstCached?: string;
};
type Release = {
  ReleaseID: string;
  Title: string;
  Bytes: number;
  Size: number;
  Segments: number;
  Plays: number;
  LastUsed: string;
  LastPlayed: string;
  Pins: string[] | null;
  reason?: ReleaseReason;
};
type PreloadStatus = {
  state: string;
  message: string;
  title: string;
  fetched: number;
  size: number;
  allocated: number;
  ready: number;
  failed: number;
  paused: boolean;
  viewers?: number;
  selection?: {
    key: number;
    title: string;
    type: string;
    score: number;
    catalogue: number;
    household: number;
    reason: string;
    reasonCodes?: string[];
    reasonArgs?: Record<string, number>;
    state?: string;
  }[];
  items: {
    key: number;
    title: string;
    releaseId: string;
    size: number;
    complete: boolean;
    state?: string;
  }[];
};
export type CacheSnapshot = {
  files?: {
    libraryKey: string;
    library: string;
    ref: string;
    name: string;
    size: number;
    lastOpen: string;
    inUse: boolean;
  }[];
  policy?: Policy;
  shared?: boolean;
  clustered?: boolean;
  restartRequired?: boolean;
  storageError?: string;
  preload: PreloadStatus;
  used: number;
  budget: number;
  directory: string;
  protectedBytes: number;
  releases: Release[];
  metrics: { hits: number; misses: number; servedBytes: number; evictedBytes: number };
  prefetch: {
    queued: number;
    running: number;
    completed: number;
    failed: number;
    dropped: number;
    current?: { title: string; cause: string };
  };
};

const GiB = 1024 ** 3,
  MiB = 1024 ** 2;

function DurationPicker({
  id,
  label,
  value,
  disabled,
  onChange,
}: {
  id: string;
  label: string;
  value: string;
  disabled?: boolean;
  onChange: (next: string) => void;
}) {
  const parsed = parseDuration(value);
  const count = Number(parsed.amount) || 0;
  return (
    <span className="flex items-center gap-2">
      <NumberInput
        id={id}
        aria-label={label}
        className="w-24"
        min={0}
        step={1}
        disabled={disabled}
        value={count}
        onChange={(n) => onChange(formatDuration(String(Math.max(0, n)), parsed.unit))}
      />
      <ToolbarPicker
        ariaLabel={t("{label} unit", { label })}
        disabled={disabled}
        value={parsed.unit}
        onChange={(u) => onChange(formatDuration(String(count), u as DurationUnit))}
        searchable={false}
        className="h-9 px-3 text-xs"
        menuClassName="w-32"
        options={[
          { value: "m", label: tn(count, "Minute", "Minutes") },
          { value: "h", label: tn(count, "Hour", "Hours") },
          { value: "d", label: tn(count, "Day", "Days") },
        ]}
      />
    </span>
  );
}

export default function Cache() {
  const { health } = useApp();
  const config = useAsync(() => api.getConfig(), []);
  const tab = useRouteTab(CACHE_TABS);
  const explorer = tab === "explorer";
  const live = useAsync(
    (signal) => apiFetch<CacheSnapshot>(explorer ? "/cache/?files=1" : "/cache/", { signal }),
    [explorer],
  );
  const discovery = useDiscoverySettings();

  // Keep the explorer live, and back off after failures.
  const [pollFailures, setPollFailures] = useState(0);
  useEffect(() => {
    if (live.error) setPollFailures((n) => Math.min(n + 1, 4));
    else if (live.data) setPollFailures(0);
  }, [live.error, live.data]);
  useInterval(
    () => {
      if (!live.loading) void live.reload();
    },
    Math.min(5000 * 2 ** pollFailures, 60_000),
  );
  const { draft, setDraft, dirty, reset } = useDraft<Policy>(config.data?.cache);
  useUnsavedGuard(dirty);
  const [saving, setSaving] = useState(false),
    [error, setError] = useState<unknown>(null);
  const [editor, setEditor] = useState<"profiles" | "policy" | null>(null);
  const [profileChoice, setProfileChoice] = useState<{
    key: string;
    name: string;
    values: Partial<Policy>;
  } | null>(null);
  const navigate = useNavigate();
  const [unit, setUnit] = useState<"GiB" | "TiB">("GiB");
  const [busy, setBusy] = useState<string | null>(null);
  const activeDirectory = live.data?.directory ?? config.data?.cache.dir ?? "";
  const disk = useAsync(() => api.disk(activeDirectory), [activeDirectory]);
  const patch = (v: Partial<Policy>) => setDraft((d) => (d ? { ...d, ...v } : d));

  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      await api.putSection("cache", draft);
      await config.reload();
      await live.reload();
      setEditor(null);
      setProfileChoice(null);
      toast.success(t("Cache policy saved"));
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  async function action(path: string, body?: unknown) {
    setBusy(path);
    setError(null);
    try {
      await apiFetch(`/cache/${path}`, {
        method: "POST",
        ...(body ? { body: JSON.stringify(body) } : {}),
      });
      await live.reload();
      if (path === "warm") toast.success(t("Resume warming requested"));
    } catch (e) {
      setError(e);
    } finally {
      setBusy(null);
    }
  }

  async function clearAll() {
    if (
      !window.confirm(
        t(
          "Clear cache? Everything cached is removed, including the openings kept for titles people are partway through; they cache again on the next play. Stop playback first.",
        ),
      )
    )
      return;
    setBusy("clear-all");
    setError(null);
    try {
      const result = await apiFetch<{ freedBytes: number; complete: boolean }>("/cache/clear-all", {
        method: "POST",
      });
      toast.success(t("Freed {size}", { size: formatBytes(result.freedBytes) }));
      if (!result.complete)
        toast.info(
          t("A few files are still being written and were kept. Clear again in a minute."),
        );
      await live.reload();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(null);
    }
  }

  async function clearRelease(releaseId: string, title: string, block: boolean): Promise<boolean> {
    const question = block
      ? t(
          "Clear cached bytes for {title} and block this release from future playback? Active or paused playback must be stopped first. Protected bytes may remain until background work ends. You can unblock it in the release controls.",
          { title },
        )
      : t(
          "Clear cached bytes for {title}? The release stays playable and is fetched again the next time it plays. Active or paused playback must be stopped first. Protected bytes may remain until background work ends.",
          { title },
        );
    if (!window.confirm(question)) return false;
    const path = block ? "clear-block" : "clear";
    setBusy(path);
    setError(null);
    try {
      const result = await apiFetch<{ cleared: boolean; message: string }>(`/cache/${path}`, {
        method: "POST",
        body: JSON.stringify({ releaseId }),
      });
      await live.reload();
      if (result.cleared) toast.success(result.message);
      else setError(new Error(result.message));
      return result.cleared;
    } catch (e) {
      setError(e);
      return false;
    } finally {
      setBusy(null);
    }
  }

  if (!draft)
    return (
      <Workspace title={t("Cache")}>
        {config.error ? (
          <ErrorPanel error={config.error} onRetry={() => void config.reload()} />
        ) : (
          <div className="grid flex-1 place-items-center">
            <Spinner label={t("Loading cache")} />
          </div>
        )}
      </Workspace>
    );

  const snap = live.data;
  const storage = health.find((h) => h.id === "storage");
  const used = snap?.used ?? 0;
  const saved = config.data!.cache;
  const matched = cachePresetKey(saved);
  const savedPreset = CACHE_PRESETS.find((p) => p.key === matched);
  const policyName =
    saved.mode === "off" || matched === "pass-through"
      ? t("Pass-through")
      : savedPreset?.name || t("Custom policy");
  const activePolicyName = snap?.policy
    ? snap.policy.mode === "off"
      ? t("Pass-through")
      : (CACHE_PRESETS.find((p) => p.key === cachePresetKey(snap.policy!))?.name ??
        t("Custom policy"))
    : policyName;
  const enabled = draft.mode !== "off",
    speculate = enabled && !draft.disableSpeculation,
    shelfEnabled = enabled && Boolean(draft.preloadEnabled);

  return (
    <Workspace
      title={t("Cache")}
      actions={
        <>
          {dirty && <Badge variant="warning">{t("Unsaved policy")}</Badge>}
          <Badge variant="secondary">
            {snap?.shared
              ? snap.clustered
                ? t("Shared cluster pool")
                : t("NAS storage")
              : t("This node")}
          </Badge>
          <Button
            variant="outline"
            size="sm"
            disabled={busy !== null}
            onClick={() => void clearAll()}
          >
            {t("Clear cache")}
          </Button>
          <Button size="sm" onClick={() => setEditor("policy")}>
            <Settings2 size={14} /> {t("Edit policy")}
          </Button>
        </>
      }
    >
      <div className="@container flex min-h-0 flex-1 flex-col">
        {storage && (
          <div
            role="status"
            title={storage.details.join("\n")}
            className="flex shrink-0 flex-col gap-0.5 border-b border-warning/20 bg-warning/10 px-5 py-2 text-xs text-warning"
          >
            <span className="font-medium">{storage.summary}</span>
            <span>
              {storage.details[0]} {storage.note}
            </span>
          </div>
        )}
        {snap?.restartRequired && (
          <div
            role="status"
            className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b border-warning/20 bg-warning/10 px-5 py-2 text-xs text-warning"
          >
            <span>
              {t(
                "Storage change saved. Restart this node to use the new backend or folder. The overview shows the currently active cache.",
              )}
            </span>
            <RestartButton
              context={t("Restarting puts your new cache storage to work.")}
              label={t("Restart now")}
            />
          </div>
        )}
        {snap?.storageError && (
          <div
            role="alert"
            className="shrink-0 border-b border-danger/20 bg-danger/10 px-5 py-2 text-xs text-danger"
          >
            {t("{error}. Cache writes are suspended until the mount and database are available.", {
              error: snap.storageError,
            })}
          </div>
        )}
        <div className="shrink-0 border-b border-border bg-surface px-4 sm:px-6">
          <OverflowTabs
            ariaLabel={t("Cache sections")}
            active={tab}
            onSelect={(key) => navigate(`/cache/${key}`)}
            tabs={CACHE_TABS.map((tabDef) =>
              tabDef.key === "explorer" && snap?.releases.length
                ? { ...tabDef, count: snap.releases.length }
                : tabDef,
            )}
          />
        </div>
        <main
          aria-label={
            tab === "overview"
              ? t("Cache overview")
              : tab === "explorer"
                ? t("Cache explorer")
                : tab === "library"
                  ? t("Release library")
                  : tab === "libraries"
                    ? t("Library file caches")
                    : t("Cache preloading")
          }
          className={cn(
            "app-scroll flex min-h-0 flex-1 flex-col bg-bg",
            tab === "explorer" || tab === "library"
              ? "overflow-hidden"
              : tab === "overview"
                ? "overflow-y-auto cockpit:overflow-hidden"
                : "overflow-y-auto",
          )}
        >
          {error && editor === null ? (
            <div className="shrink-0 px-4 pt-3">
              <ErrorPanel error={error} />
            </div>
          ) : null}
          {live.error ? (
            <div className="shrink-0 px-4 pt-3">
              <ErrorPanel error={live.error} onRetry={() => void live.reload()} />
            </div>
          ) : null}
          {tab === "overview" ? (
            <div className="mx-auto w-full max-w-[1600px] p-4 sm:p-5 cockpit:min-h-0 cockpit:flex-1">
              <CacheOverview
                snapshot={snap}
                policy={snap?.policy ?? saved}
                name={activePolicyName}
                freeBytes={disk.data?.freeBytes}
                busy={busy !== null}
                onProfile={() => {
                  setProfileChoice(null);
                  setEditor("profiles");
                }}
                onExplore={() => navigate("/cache/explorer")}
                onPreloading={() => navigate("/cache/preloading")}
                onWarm={() => void action("warm")}
              />
            </div>
          ) : tab === "explorer" ? (
            <div className="mx-auto flex min-h-0 w-full max-w-[1600px] flex-1 flex-col p-4 sm:p-5">
              <CacheExplorer
                snapshot={snap ? { ...snap, policy: snap.policy ?? saved } : undefined}
                policyName={activePolicyName}
                busy={busy !== null}
                onPin={(releaseId, pinned) => void action("pin", { releaseId, pinned })}
                onClearFile={(libraryKey, ref, name) => {
                  if (window.confirm(t("Clear {name}?", { name })))
                    void action("clear-file", { libraryKey, ref });
                }}
                onClear={(id, title, block) => clearRelease(id, title, block)}
              />
            </div>
          ) : tab === "library" ? (
            <div className="mx-auto flex min-h-0 w-full max-w-[1600px] flex-1 flex-col p-4 sm:p-5">
              <CacheLibrary />
            </div>
          ) : tab === "libraries" ? (
            <div className="mx-auto w-full max-w-[1600px] p-4 sm:p-5">
              <LibraryCaches />
            </div>
          ) : (
            <div className="mx-auto w-full max-w-[1600px] space-y-5 p-4 sm:p-5">
              {!enabled && (
                <div
                  role="status"
                  className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-warning/25 bg-warning/10 px-4 py-3 text-xs text-warning"
                >
                  <span>
                    {t(
                      "Persistent media caching is turned off in your storage policy. Turn retention back on in Edit policy to enable warming and preloading.",
                    )}
                  </span>
                  <Button variant="secondary" size="sm" onClick={() => setEditor("policy")}>
                    <Settings2 size={14} /> {t("Edit policy")}
                  </Button>
                </div>
              )}
              <SettingsSection
                title={t("Opening & playback warming")}
                description={t(
                  "Prepare small windows before and during playback so streams start and seek without waiting on providers.",
                )}
              >
                <SettingsRow
                  setting="allow-opening-and-resume-warming"
                  title={
                    <label htmlFor="cache-allow-warming">
                      {t("Allow opening and resume warming")}
                    </label>
                  }
                  description={t(
                    "Let Ether fetch small windows before Play. Turn off for strictly on-demand media.",
                  )}
                  control={
                    <Switch
                      id="cache-allow-warming"
                      aria-label={t("Allow opening and resume warming")}
                      checked={speculate}
                      disabled={!enabled}
                      onChange={(e) => patch({ disableSpeculation: !e.currentTarget.checked })}
                    />
                  }
                />
                <SettingsRow
                  setting="warm-detail-pages"
                  title={<label htmlFor="cache-detail-warmup">{t("Warm detail pages")}</label>}
                  description={t(
                    "Opening a movie or episode prepares its source and opening bytes. Shows warm their On Deck episode when supported.",
                  )}
                  control={
                    <Switch
                      id="cache-detail-warmup"
                      aria-label={t("Warm detail pages")}
                      checked={draft.detailWarmup !== "off"}
                      disabled={!speculate}
                      onChange={(e) =>
                        patch({ detailWarmup: e.currentTarget.checked ? "on" : "off" })
                      }
                    />
                  }
                />
                <SettingsRow
                  setting="prepare-the-next-episode"
                  title={
                    <label htmlFor="cache-next-episode">{t("Prepare the next episode")}</label>
                  }
                  description={t("Warm the opening near the end of the current episode.")}
                  control={
                    <Switch
                      id="cache-next-episode"
                      aria-label={t("Prepare the next episode")}
                      checked={draft.prefetchNextEpisode}
                      disabled={!speculate}
                      onChange={(e) => patch({ prefetchNextEpisode: e.currentTarget.checked })}
                    />
                  }
                />
                <SettingsRow
                  setting="keep-resume-points-warm"
                  title={<label htmlFor="cache-resume-warm">{t("Keep resume points warm")}</label>}
                  description={t("Periodically refresh a small window around Continue Watching.")}
                  control={
                    <Switch
                      id="cache-resume-warm"
                      aria-label={t("Keep resume points warm")}
                      checked={draft.keepInProgressWarm}
                      disabled={!speculate}
                      onChange={(e) => patch({ keepInProgressWarm: e.currentTarget.checked })}
                    />
                  }
                />
                {discovery.doc.error ? (
                  <div className="px-5 py-3">
                    <ErrorPanel
                      error={discovery.doc.error}
                      onRetry={() => void discovery.doc.reload()}
                    />
                  </div>
                ) : (
                  <SettingsRow
                    setting="warm-up-new-arrivals"
                    title={<label htmlFor="watchlist-prewarm">{t("Warm up new arrivals")}</label>}
                    description={t(
                      "Fetch the opening minutes as soon as a title becomes available, so it starts quickly. Needs watchlist monitoring in Viewers.",
                    )}
                    control={
                      discovery.draft ? (
                        <Switch
                          id="watchlist-prewarm"
                          aria-label={t("Warm up new arrivals")}
                          checked={discovery.draft.prewarm}
                          disabled={!discovery.draft.enabled || discovery.saving}
                          onChange={(e) =>
                            discovery.setDraft({
                              ...discovery.draft!,
                              prewarm: e.currentTarget.checked,
                            })
                          }
                        />
                      ) : (
                        <Spinner size={16} label={t("Loading watchlist settings")} />
                      )
                    }
                  />
                )}
                <SettingsRow
                  setting="read-ahead-while-playing"
                  title={
                    <label htmlFor="cache-playback-read-ahead">
                      {t("Read ahead while playing")}
                    </label>
                  }
                  description={t(
                    "Keeps the next minutes of whatever is playing downloaded to this cache, so a slow Usenet provider does not make viewers buffer. Uses spare connections; playback always comes first.",
                  )}
                  control={
                    <ToolbarPicker
                      id="cache-playback-read-ahead"
                      ariaLabel={t("Read ahead while playing")}
                      disabled={!enabled}
                      value={String(draft.playbackReadAheadMinutes ?? 5)}
                      onChange={(v) => patch({ playbackReadAheadMinutes: Number(v) })}
                      searchable={false}
                      className="h-9 min-w-48 justify-between px-3"
                      options={[...new Set([0, 2, 5, 10, draft.playbackReadAheadMinutes ?? 5])]
                        .sort((a, b) => a - b)
                        .map((minutes) => ({
                          value: String(minutes),
                          label:
                            minutes === 0
                              ? t("Off")
                              : minutes === 5
                                ? t("5 minutes (recommended)")
                                : tn(minutes, "{count} minute", "{count} minutes"),
                        }))}
                    />
                  }
                />
                <MoreOptions
                  setting="playback-warming"
                  label={t("More options: Opening & playback warming")}
                >
                  <SettingsRow
                    setting="opening-window"
                    title={<label htmlFor="cache-opening">{t("Opening window")}</label>}
                    description={t(
                      "2–64 MiB for detail pages. Larger windows use more bandwidth; startup still depends on the source and client.",
                    )}
                    control={
                      <span className="flex items-center gap-2">
                        <NumberInput
                          id="cache-opening"
                          aria-label={t("Opening window")}
                          className="w-28"
                          min={2}
                          max={64}
                          disabled={!speculate}
                          value={(draft.detailBytes || 2 * MiB) / MiB}
                          onChange={(n) => patch({ detailBytes: Math.round(n * MiB) })}
                        />
                        <span className="text-sm text-fg-muted">MiB</span>
                      </span>
                    }
                  />
                  <SettingsRow
                    setting="next-episode-resume-window"
                    title={
                      <label htmlFor="cache-prefetch">{t("Next episode / resume window")}</label>
                    }
                    description={t(
                      "Detail warming is rate limited and pauses during active playback. Trailer and post-play warming also require background warming here and their own feature settings.",
                    )}
                    control={
                      <span className="flex items-center gap-2">
                        <NumberInput
                          id="cache-prefetch"
                          aria-label={t("Next episode / resume window")}
                          className="w-28"
                          min={1}
                          disabled={!speculate}
                          value={draft.prefetchBytes / MiB}
                          onChange={(n) => patch({ prefetchBytes: Math.round(n * MiB) })}
                        />
                        <span className="text-sm text-fg-muted">MiB</span>
                      </span>
                    }
                  />
                  <SettingsRow
                    setting="prepare-next-episode-at"
                    title={<label htmlFor="cache-trigger">{t("Prepare next episode at")}</label>}
                    description={t(
                      "Percentage of the current episode watched before warming the next one.",
                    )}
                    control={
                      <span className="flex items-center gap-2">
                        <NumberInput
                          id="cache-trigger"
                          aria-label={t("Prepare next episode at")}
                          className="w-28"
                          min={1}
                          max={99}
                          disabled={!speculate || !draft.prefetchNextEpisode}
                          value={Math.round(draft.nextEpisodeAt * 100)}
                          onChange={(n) => patch({ nextEpisodeAt: n / 100 })}
                        />
                        <span className="text-sm text-fg-muted">%</span>
                      </span>
                    }
                  />
                </MoreOptions>
              </SettingsSection>

              <SettingsSection
                title={t("Managed local shelf")}
                description={t(
                  "Proactively fill a dedicated part of your cache from the local catalogue using your sources and release rules.",
                )}
              >
                <SettingsRow
                  setting="preload-movies-and-tv"
                  title={<label htmlFor="cache-preload-enabled">{t("Preload movies & TV")}</label>}
                  description={t(
                    "Downloads complete movie and episode sources so chosen titles play from local disk.",
                  )}
                  control={
                    <Switch
                      id="cache-preload-enabled"
                      aria-label={t("Preload movies & TV")}
                      checked={draft.preloadEnabled ?? false}
                      disabled={!enabled}
                      onChange={(e) => {
                        const v = e.currentTarget.checked;
                        patch({
                          preloadEnabled: v,
                          ...(v && !draft.preloadBudgetBytes
                            ? {
                                preloadBudgetBytes: Math.min(
                                  draft.budgetBytes || 100 * GiB,
                                  100 * GiB,
                                ),
                              }
                            : {}),
                        });
                      }}
                    />
                  }
                />
                <WarmingPolicy
                  keys={draft.keepWarmTitles ?? []}
                  speculative={draft.speculativeWarming !== "off"}
                  disabled={!enabled || saving}
                  onRemove={(key) =>
                    patch({ keepWarmTitles: (draft.keepWarmTitles ?? []).filter((k) => k !== key) })
                  }
                  onSpeculativeChange={(value) =>
                    patch({ speculativeWarming: value ? "on" : "off" })
                  }
                />
                <SettingsRow
                  setting="preload-allocation"
                  title={<label htmlFor="preload-budget">{t("Preload allocation")}</label>}
                  description={
                    <>
                      {t("Part of the total budget, not additional storage.")}{" "}
                      {draft.preloadBudgetBytes
                        ? tn(
                            Math.floor((draft.preloadBudgetBytes ?? 0) / (8 * GiB)),
                            "Room for roughly {count} movie-sized source at 8 GiB each. Episode sizes vary; both share this allocation.",
                            "Room for roughly {count} movie-sized sources at 8 GiB each. Episode sizes vary; both share this allocation.",
                          )
                        : null}
                    </>
                  }
                  control={
                    <span className="flex items-center gap-2">
                      <NumberInput
                        id="preload-budget"
                        aria-label={t("Preload allocation")}
                        className="w-28"
                        min={1}
                        disabled={!shelfEnabled}
                        value={Math.round(((draft.preloadBudgetBytes ?? 0) / GiB) * 100) / 100}
                        onChange={(n) => patch({ preloadBudgetBytes: Math.round(n * GiB) })}
                      />
                      <span className="text-sm text-fg-muted">GiB</span>
                    </span>
                  }
                />
                <SettingsRow
                  setting="include-in-your-shelf"
                  title={<label htmlFor="preload-content">{t("Include in your shelf")}</label>}
                  control={
                    <ToolbarPicker
                      id="preload-content"
                      ariaLabel={t("Include in your shelf")}
                      disabled={!shelfEnabled}
                      value={draft.preloadContent || "mixed"}
                      onChange={(v) => patch({ preloadContent: v })}
                      searchable={false}
                      className="h-9 min-w-48 justify-between px-3"
                      options={[
                        { value: "mixed", label: t("Movies & TV episodes") },
                        { value: "movies", label: t("Movies only") },
                        { value: "tv", label: t("TV episodes only") },
                      ]}
                    />
                  }
                />
                <SettingsRow
                  setting="selection-strategy"
                  title={<label htmlFor="preload-source">{t("Selection strategy")}</label>}
                  control={
                    <ToolbarPicker
                      id="preload-source"
                      ariaLabel={t("Selection strategy")}
                      disabled={!shelfEnabled}
                      value={draft.preloadSource || "smart"}
                      onChange={(v) => patch({ preloadSource: v })}
                      searchable={false}
                      className="h-9 min-w-48 justify-between px-3"
                      options={[
                        { value: "smart", label: t("Smart household blend") },
                        { value: "popular", label: t("Catalogue popularity") },
                        { value: "recent", label: t("Newest releases") },
                      ]}
                    />
                  }
                />
                {(!draft.preloadSource || draft.preloadSource === "smart") && (
                  <SettingsRow
                    setting="household-influence"
                    title={<label htmlFor="household-weight">{t("Household influence")}</label>}
                    description={t(
                      "Recent repeat viewing, shared favourites and genre interests guide the shelf. Older viewing fades; no history falls back to catalogue popularity.",
                    )}
                    control={
                      <span className="flex items-center gap-2">
                        <NumberInput
                          id="household-weight"
                          aria-label={t("Household influence")}
                          className="w-28"
                          min={0}
                          max={100}
                          step={5}
                          disabled={!shelfEnabled}
                          value={draft.preloadHouseholdWeight ?? 65}
                          onChange={(n) =>
                            patch({ preloadHouseholdWeight: Math.max(0, Math.min(100, n)) })
                          }
                        />
                        <span className="text-sm text-fg-muted">%</span>
                      </span>
                    }
                  />
                )}
                <SettingsRow
                  setting="learn-from"
                  title={<label htmlFor="household-scope">{t("Learn from")}</label>}
                  description={t("Also determines whose TV progress guides episode selection.")}
                  control={
                    <ToolbarPicker
                      id="household-scope"
                      ariaLabel={t("Learn from")}
                      disabled={!shelfEnabled}
                      value={draft.preloadHouseholdScope || "home"}
                      onChange={(v) => patch({ preloadHouseholdScope: v })}
                      searchable={false}
                      className="h-9 min-w-48 justify-between px-3"
                      options={[
                        { value: "home", label: t("Owner & Plex Home members") },
                        { value: "all", label: t("All active shared viewers") },
                      ]}
                    />
                  }
                />
                {draft.preloadContent !== "movies" && (
                  <SettingsRow
                    setting="episode-window-per-show"
                    title={<label htmlFor="episode-window">{t("Episode window per show")}</label>}
                    description={t(
                      "A rolling shelf around current viewing positions. New shows start at episode one; different viewers share this limit.",
                    )}
                    control={
                      <span className="flex items-center gap-2">
                        <NumberInput
                          id="episode-window"
                          aria-label={t("Episode window per show")}
                          className="w-28"
                          min={1}
                          max={24}
                          disabled={!shelfEnabled}
                          value={draft.preloadEpisodesAhead || 3}
                          onChange={(n) => patch({ preloadEpisodesAhead: n })}
                        />
                        <span className="text-sm text-fg-muted">{t("episodes")}</span>
                      </span>
                    }
                  />
                )}
                <MoreOptions setting="local-shelf" label={t("More options: Managed local shelf")}>
                  <SettingsRow
                    setting="household-history-window"
                    title={<label htmlFor="history-window">{t("Household history window")}</label>}
                    description={t(
                      "Recent views count more; frequency is capped per viewer. Completed watches and reviewed imports contribute, never preload activity.",
                    )}
                    control={
                      <span className="flex items-center gap-2">
                        <NumberInput
                          id="history-window"
                          aria-label={t("Household history window")}
                          className="w-28"
                          min={1}
                          max={730}
                          disabled={!shelfEnabled}
                          value={draft.preloadHistoryDays || 90}
                          onChange={(n) => patch({ preloadHistoryDays: n })}
                        />
                        <span className="text-sm text-fg-muted">{t("days")}</span>
                      </span>
                    }
                  />
                  <SettingsRow
                    setting="maximum-preloaded-titles"
                    title={<label htmlFor="preload-count">{t("Maximum preloaded titles")}</label>}
                    description={t("Up to {max}. The allocation still limits how many fit.", {
                      max: formatNumber(5000),
                    })}
                    control={
                      <NumberInput
                        id="preload-count"
                        aria-label={t("Maximum preloaded titles")}
                        className="w-28"
                        min={1}
                        max={5000}
                        disabled={!shelfEnabled}
                        value={draft.preloadMaxTitles || 200}
                        onChange={(n) => patch({ preloadMaxTitles: n })}
                      />
                    }
                  />
                  <SettingsRow
                    setting="preload-speed"
                    title={<label htmlFor="preload-rate">{t("Preload speed")}</label>}
                    description={t(
                      "Average payload cap, with short bursts for each 16 MiB window. Zero removes the cap.",
                    )}
                    control={
                      <span className="flex items-center gap-2">
                        <NumberInput
                          id="preload-rate"
                          aria-label={t("Preload speed")}
                          className="w-28"
                          min={0}
                          max={10000}
                          disabled={!shelfEnabled}
                          value={draft.preloadRateMiB ?? 0}
                          onChange={(n) => patch({ preloadRateMiB: n })}
                        />
                        <span className="text-sm text-fg-muted">MiB/s</span>
                      </span>
                    }
                  />
                  <SettingsRow
                    setting="keep-disk-space-free"
                    title={<label htmlFor="preload-reserve">{t("Keep disk space free")}</label>}
                    description={t(
                      "Preloading waits below this reserve. Zero uses the 5 GiB default.",
                    )}
                    control={
                      <span className="flex items-center gap-2">
                        <NumberInput
                          id="preload-reserve"
                          aria-label={t("Keep disk space free")}
                          className="w-28"
                          min={0}
                          disabled={!shelfEnabled}
                          value={
                            Math.round(((draft.preloadMinFreeBytes ?? 5 * GiB) / GiB) * 100) / 100
                          }
                          onChange={(n) => patch({ preloadMinFreeBytes: Math.round(n * GiB) })}
                        />
                        <span className="text-sm text-fg-muted">GiB</span>
                      </span>
                    }
                  />
                  <SettingsRow
                    setting="preload-during-playback"
                    title={
                      <label htmlFor="preload-during-playback">
                        {t("Preload during playback")}
                      </label>
                    }
                    description={t(
                      "Otherwise, complete-title filling waits while viewers are streaming. Opening and next-episode warming use their separate controls.",
                    )}
                    control={
                      <Switch
                        id="preload-during-playback"
                        aria-label={t("Preload during playback")}
                        checked={draft.preloadDuringPlayback ?? false}
                        disabled={!shelfEnabled}
                        onChange={(e) => patch({ preloadDuringPlayback: e.currentTarget.checked })}
                      />
                    }
                  />
                </MoreOptions>
              </SettingsSection>

              <div className="grid items-start gap-4 lg:grid-cols-12">
                <section className="rounded-xl border border-border bg-surface lg:col-span-5">
                  <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-5 py-4">
                    <div>
                      <h2 className="text-sm font-semibold">{t("Preload activity")}</h2>
                      <p className="mt-0.5 text-xs text-fg-muted">{t("Managed local library")}</p>
                    </div>
                    <Button
                      variant="secondary"
                      size="sm"
                      disabled={busy !== null || !saved.preloadEnabled}
                      onClick={() => void action("preload", { paused: !snap?.preload.paused })}
                    >
                      {snap?.preload.paused ? t("Resume preloading") : t("Pause preloading")}
                    </Button>
                  </div>
                  <div className="space-y-4 p-5">
                    <div className="grid grid-cols-3 gap-3">
                      <div>
                        <p className="text-2xl font-semibold tabular-nums">
                          {snap?.preload.ready ?? 0}
                        </p>
                        <p className="text-xs text-fg-muted">{t("Sources preloaded")}</p>
                      </div>
                      <div>
                        <p className="text-2xl font-semibold tabular-nums">
                          {formatBytes(snap?.preload.allocated ?? 0)}
                        </p>
                        <p className="text-xs text-fg-muted">{t("Reserved, including overhead")}</p>
                      </div>
                      <div>
                        <p className="text-2xl font-semibold tabular-nums">
                          {formatBytes(saved.preloadBudgetBytes ?? 0)}
                        </p>
                        <p className="text-xs text-fg-muted">{t("Preload allocation")}</p>
                      </div>
                    </div>
                    <div className="rounded-lg bg-bg-subtle p-3">
                      <p className="text-sm font-medium">
                        {snap?.preload.paused
                          ? t("Paused")
                          : snap?.preload.state === "filling"
                            ? snap.preload.title
                            : snap?.preload.state === "resolving"
                              ? t("Finding a source for {title}", { title: snap.preload.title })
                              : !saved.preloadEnabled
                                ? t("Preloading disabled")
                                : snap?.preload.state === "selecting"
                                  ? t("Selecting titles")
                                  : snap?.preload.state === "ready"
                                    ? t("Selection pass complete")
                                    : t("Waiting for the next pass")}
                      </p>
                      <p className="mt-1 text-xs text-fg-muted">
                        {snap?.preload.message ||
                          t("Enable movie and TV preloading to build a local shelf.")}
                      </p>
                      {snap?.preload.state === "filling" && (
                        <>
                          <div
                            role="progressbar"
                            aria-label={t("Current preload")}
                            aria-valuenow={snap.preload.fetched}
                            aria-valuemax={snap.preload.size}
                            className="mt-3 h-1.5 overflow-hidden rounded bg-primary/10"
                          >
                            <div
                              className="h-full bg-primary"
                              style={{
                                width: `${snap.preload.size ? Math.min(100, (snap.preload.fetched / snap.preload.size) * 100) : 0}%`,
                              }}
                            />
                          </div>
                          <p className="mt-1 text-[10px] text-fg-subtle">
                            {formatBytes(snap.preload.fetched)} / {formatBytes(snap.preload.size)}
                          </p>
                        </>
                      )}
                    </div>
                    <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border pt-3">
                      <p className="text-xs text-fg-muted">
                        {saved.preloadDuringPlayback
                          ? t("May run during playback")
                          : t("Waits during playback")}{" "}
                        ·{" "}
                        {saved.preloadRateMiB
                          ? t("{rate} MiB/s limit", { rate: saved.preloadRateMiB })
                          : t("No bandwidth cap")}
                      </p>
                    </div>
                    {(snap?.preload.failed ?? 0) > 0 && (
                      <p className="text-xs text-fg-muted">
                        {tn(
                          snap?.preload.failed ?? 0,
                          "{count} attempt skipped or interrupted this run. Unavailable or oversized titles do not stop the shelf.",
                          "{count} attempts skipped or interrupted this run. Unavailable or oversized titles do not stop the shelf.",
                        )}
                      </p>
                    )}
                    {!!snap?.preload.items?.length && (
                      <div className="border-t border-border pt-3">
                        <h3 className="text-sm font-medium">{t("Shelf manifest")}</h3>
                        <ul className="mt-2 space-y-2">
                          {snap.preload.items.slice(0, 8).map((it) => (
                            <li
                              key={it.key}
                              className="flex items-center justify-between gap-2 text-xs"
                            >
                              <span className="truncate">{it.title}</span>
                              <Badge
                                variant={
                                  it.state === "failed"
                                    ? "destructive"
                                    : it.state === "cached"
                                      ? "success"
                                      : "secondary"
                                }
                              >
                                {shelfStateLabel(it.state)}
                              </Badge>
                            </li>
                          ))}
                        </ul>
                      </div>
                    )}
                  </div>
                </section>
                <section className="rounded-xl border border-border bg-surface lg:col-span-7">
                  <div className="flex items-start justify-between gap-3 border-b border-border px-5 py-4">
                    <div>
                      <h2 className="flex items-center gap-2 text-sm font-semibold">
                        <Sparkles size={16} className="text-primary" /> {t("Preload selection")}
                      </h2>
                      <p className="mt-1 text-xs text-fg-muted">
                        {tn(
                          snap?.preload.viewers ?? 0,
                          "Titles chosen for the local shelf from the last pass · {count} active viewer considered",
                          "Titles chosen for the local shelf from the last pass · {count} active viewers considered",
                        )}
                      </p>
                    </div>
                    <Badge>{t("Local shelf choices")}</Badge>
                  </div>
                  {snap?.preload.selection?.length ? (
                    <div className="divide-y divide-border">
                      {snap.preload.selection.slice(0, 6).map((item, i) => (
                        <div key={item.key} className="flex items-start gap-3 px-5 py-4">
                          <span className="mt-0.5 w-5 shrink-0 font-mono text-xs text-fg-subtle">
                            {String(i + 1).padStart(2, "0")}
                          </span>
                          <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-2">
                              <span className="truncate text-sm font-medium">{item.title}</span>
                              <span className="shrink-0 text-[10px] uppercase text-fg-subtle">
                                {item.type === "episode" ? t("TV") : t("Film")}
                              </span>
                              {(() => {
                                const state =
                                  snap.preload.items.find((it) => it.key === item.key)?.state ??
                                  item.state;
                                return (
                                  <Badge
                                    variant={
                                      state === "failed"
                                        ? "destructive"
                                        : state === "cached"
                                          ? "success"
                                          : "secondary"
                                    }
                                  >
                                    {shelfStateLabel(state)}
                                  </Badge>
                                );
                              })()}
                            </div>
                            <p className="mt-1 text-xs text-fg-muted">
                              {selectionReason(item.reasonCodes, item.reasonArgs)}
                            </p>
                            <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-[10px] text-fg-subtle">
                              <span>
                                {t("Catalogue {score}/100", { score: Math.round(item.catalogue) })}
                              </span>
                              <span>
                                {t("Household {score}/100", { score: Math.round(item.household) })}
                              </span>
                            </div>
                          </div>
                          <span
                            className="shrink-0 rounded-md bg-primary/10 px-2 py-1 font-mono text-xs text-primary"
                            aria-label={t("Selection score {score} out of 100", {
                              score: Math.round(item.score),
                            })}
                          >
                            {Math.round(item.score)}
                          </span>
                        </div>
                      ))}
                    </div>
                  ) : (
                    <p className="px-5 py-4 text-sm leading-relaxed text-fg-muted">
                      {t(
                        "Enable preloading and save to build your first selection. Movies blend discovery with household favourites; TV keeps a rolling window of episodes ready. Scores appear after the next selection pass.",
                      )}
                    </p>
                  )}
                  <p className="border-t border-border px-5 py-3 text-[10px] text-fg-subtle">
                    {t(
                      "Scores rank cache candidates, not predicted viewing probabilities. TV windows taper by episode distance. Selection does not guarantee source availability.",
                    )}
                  </p>
                </section>
              </div>
            </div>
          )}
        </main>
      </div>
      <SaveBar
        dirty={dirty && editor === null}
        saving={saving}
        error={editor === null ? error : null}
        onSave={save}
        onDiscard={() => {
          reset();
          setProfileChoice(null);
          setError(null);
        }}
      />
      <SaveBar
        dirty={discovery.dirty && editor === null}
        saving={discovery.saving}
        error={editor === null ? discovery.error : null}
        onSave={() => discovery.save(t("Watchlist settings saved"))}
        onDiscard={discovery.discard}
      />
      <Drawer
        open={editor !== null}
        onClose={() => setEditor(null)}
        title={
          editor === "profiles"
            ? t("Choose a cache profile")
            : profileChoice
              ? t("Review your cache profile")
              : t("Edit cache policy")
        }
        size="lg"
        footer={
          editor === "profiles" ? (
            <div className="flex items-center justify-between gap-3">
              <span className="text-xs text-fg-muted">{t("1 / Choose a starting point")}</span>
              <Button
                disabled={!profileChoice}
                onClick={() => {
                  if (profileChoice) {
                    patch(profileChoice.values);
                    setEditor("policy");
                  }
                }}
              >
                {t("Review profile")} <ArrowUpRight size={14} />
              </Button>
            </div>
          ) : (
            <div className="flex flex-wrap items-center justify-between gap-2">
              <Button
                variant="ghost"
                onClick={() => {
                  reset();
                  setEditor(null);
                  setProfileChoice(null);
                }}
                disabled={saving}
              >
                {t("Discard changes")}
              </Button>
              <Button onClick={() => void save()} disabled={!dirty || saving}>
                {saving ? t("Saving…") : t("Save policy")}
              </Button>
            </div>
          )
        }
      >
        {editor === "profiles" ? (
          <div className="space-y-4">
            <p className="text-sm leading-relaxed text-fg-muted">
              {t(
                "Pick a starting point, then review the budget and behavior. Nothing changes until you save.",
              )}
            </p>
            <p className="rounded-lg border border-primary/20 bg-primary/5 p-3 text-xs text-fg-muted">
              {t(
                "Cinema node and Edge library enable full movie and episode preloading. All sizes and limits can be customized in the next step.",
              )}
            </p>
            <CachePresetCards selected={profileChoice?.key ?? null} onSelect={setProfileChoice} />
          </div>
        ) : (
          <div className="space-y-6">
            <div className="rounded-xl border border-primary/20 bg-primary/5 p-4">
              <div className="flex items-center justify-between gap-3">
                <div>
                  <p className="text-[10px] font-semibold uppercase tracking-[.16em] text-primary">
                    {profileChoice ? t("Review your preset") : t("Your starting point")}
                  </p>
                  <p className="mt-1 text-sm font-semibold">{profileChoice?.name ?? policyName}</p>
                </div>
                <Button variant="secondary" size="sm" onClick={() => setEditor("profiles")}>
                  <ArrowLeft size={14} /> {t("Back to presets")}
                </Button>
              </div>
              <p className="mt-2 text-xs leading-relaxed text-fg-muted">
                {t(
                  "Choose a different preset at any time, then customize storage and retention here. Preloading options live on the Preloading tab.",
                )}
              </p>
            </div>
            {error ? <ErrorPanel error={error} /> : null}
            <section aria-label={t("Cache storage")} className="space-y-3">
              <div className="flex items-center justify-between">
                <h3 className="text-sm font-semibold">{t("Where your cache lives")}</h3>
                <Badge variant="secondary">
                  {draft.storage === "shared" && snap?.clustered
                    ? t("Cluster policy")
                    : t("Server policy")}
                </Badge>
              </div>
              <div
                className="grid grid-cols-2 gap-3"
                role="radiogroup"
                aria-label={t("Storage backend")}
              >
                <button
                  type="button"
                  role="radio"
                  aria-checked={draft.storage !== "shared"}
                  onClick={() => patch({ storage: "local" })}
                  className={cn(
                    "rounded-xl border p-3 text-left transition",
                    draft.storage !== "shared"
                      ? "border-primary bg-primary/5"
                      : "border-border hover:border-primary/40",
                  )}
                >
                  <HardDrive size={18} className="mb-2 text-primary" />
                  <span className="block text-sm font-medium">{t("Local disk")}</span>
                  <span className="mt-1 block text-xs text-fg-muted">
                    {t("An independent cache on this node.")}
                  </span>
                </button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={draft.storage === "shared"}
                  onClick={() =>
                    patch({ ...config.data?.sharedCache, storage: "shared", dir: draft.dir })
                  }
                  className={cn(
                    "rounded-xl border p-3 text-left transition disabled:opacity-50",
                    draft.storage === "shared"
                      ? "border-primary bg-primary/5"
                      : "border-border hover:border-primary/40",
                  )}
                >
                  <Network size={18} className="mb-2 text-primary" />
                  <span className="block text-sm font-medium">{t("Shared NAS")}</span>
                  <span className="mt-1 block text-xs text-fg-muted">
                    {snap?.clustered
                      ? t("One library and budget across your cluster.")
                      : t("Keep your cache on a mounted network share.")}
                  </span>
                </button>
              </div>
              {draft.storage === "shared" && (
                <p className="rounded-lg bg-primary/5 p-3 text-xs leading-relaxed text-fg-muted">
                  {snap?.clustered
                    ? t(
                        "Mount the same NAS share on every participating node. Each node may use a different mount path. Budgets, retention, pins, and preloading are shared; saving this policy updates all members of the pool.",
                      )
                    : t(
                        "Mount your NAS share on this server and choose a dedicated folder. Your media cache lives on the NAS; its database stays on this server. No cluster is required. To let multiple Ether servers use the same cache folder, connect them in a cluster first.",
                      )}
                </p>
              )}
              <Field
                label={
                  draft.storage === "shared"
                    ? t("NAS mount path on this node")
                    : t("Cache directory")
                }
                htmlFor="cache-dir"
                hint={t(
                  "Changing storage or its folder requires a node restart. Existing cache bytes stay in their original folder.",
                )}
              >
                <Input
                  id="cache-dir"
                  value={draft.dir}
                  onChange={(e) => patch({ dir: e.target.value })}
                  placeholder={draft.storage === "shared" ? "/mnt/media-cache" : undefined}
                />
              </Field>
            </section>

            <SettingsSection title={t("Budget & retention")}>
              <SettingsRow
                title={<label htmlFor="cache-budget">{t("Media disk budget")}</label>}
                description={
                  <>
                    {t(
                      "Zero means unlimited, not disabled. Use Back to presets and choose Pass-through to turn disk caching off.",
                    )}
                    {disk.data && draft.dir === activeDirectory && (
                      <span className="mt-1.5 flex items-center gap-1.5 text-xs text-fg-muted">
                        <HardDrive size={12} aria-hidden />
                        {t("{size} currently free", { size: formatBytes(disk.data.freeBytes) })}
                      </span>
                    )}
                    {disk.data &&
                      draft.dir === activeDirectory &&
                      enabled &&
                      draft.budgetBytes > 0 &&
                      draft.budgetBytes - used > disk.data.freeBytes && (
                        <span className="mt-1.5 block rounded-md bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
                          {t(
                            "This budget needs more space than is currently free. Choose a smaller budget.",
                          )}
                        </span>
                      )}
                  </>
                }
                control={
                  <span className="flex items-center gap-2">
                    <NumberInput
                      id="cache-budget"
                      aria-label={t("Media disk budget")}
                      className="w-28"
                      min={0}
                      step={0.25}
                      value={
                        Math.round(
                          (draft.budgetBytes / (unit === "TiB" ? 1024 * GiB : GiB)) * 1000,
                        ) / 1000
                      }
                      onChange={(n) =>
                        patch({
                          budgetBytes: Math.round(n * (unit === "TiB" ? 1024 * GiB : GiB)),
                        })
                      }
                    />
                    <ToolbarPicker
                      ariaLabel={t("Budget unit")}
                      value={unit}
                      onChange={(v) => setUnit(v)}
                      searchable={false}
                      className="h-9 shrink-0 px-3 text-xs"
                      menuClassName="w-28"
                      options={[
                        { value: "GiB", label: "GiB" },
                        { value: "TiB", label: "TiB" },
                      ]}
                    />
                  </span>
                }
              />
              <SettingsRow
                title={<label htmlFor="cache-library-budget">{t("Release library limit")}</label>}
                description={t(
                  "Kept NZBs and torrent records, so a release is never fetched from an indexer twice. A release takes well under 1 MB. Zero means no limit; when set, releases nobody replayed leave first.",
                )}
                control={
                  <span className="flex items-center gap-2">
                    <NumberInput
                      id="cache-library-budget"
                      aria-label={t("Release library limit")}
                      className="w-28"
                      min={0}
                      step={1}
                      value={Math.round(((draft.libraryBudgetBytes ?? 0) / GiB) * 100) / 100}
                      onChange={(n) => patch({ libraryBudgetBytes: Math.round(n * GiB) })}
                    />
                    <span className="text-xs text-fg-muted">GiB</span>
                  </span>
                }
              />
              <SettingsRow
                title={<label htmlFor="cache-mode">{t("Retention strategy")}</label>}
                description={
                  !enabled
                    ? t(
                        "New media bytes bypass disk. Unprotected existing bytes are released; playback and pinned bytes remain protected.",
                      )
                    : draft.mode === "adaptive"
                      ? t(
                          "Repeat plays earn extra time before eviction. Popularity is learned from playback, never from background reads.",
                        )
                      : t(
                          "Keep recently used bytes. The oldest unprotected segments make room first.",
                        )
                }
                control={
                  <ToolbarPicker
                    id="cache-mode"
                    ariaLabel={t("Retention strategy")}
                    value={draft.mode || "demand"}
                    onChange={(next) =>
                      patch({
                        mode: next,
                        ...(next === "off" ? { preloadEnabled: false } : {}),
                      })
                    }
                    searchable={false}
                    className="h-9 min-w-48 justify-between px-3"
                    options={[
                      { value: "off", label: t("No persistent media") },
                      { value: "demand", label: t("Keep what is streamed") },
                      { value: "adaptive", label: t("Favour repeat watches") },
                    ]}
                  />
                }
              />
              <SettingsRow
                title={<label htmlFor="cache-retention">{t("Expire after inactivity")}</label>}
                description={t(
                  "Days since last cache use. Zero keeps bytes until space is needed.",
                )}
                control={
                  <span className="flex items-center gap-2">
                    <NumberInput
                      id="cache-retention"
                      aria-label={t("Expire after inactivity")}
                      className="w-28"
                      min={0}
                      max={3650}
                      disabled={!enabled}
                      value={draft.retentionDays ?? 0}
                      onChange={(n) => patch({ retentionDays: n })}
                    />
                    <span className="text-sm text-fg-muted">{t("days")}</span>
                  </span>
                }
              />
              <MoreOptions label={t("More options: Budget & retention")}>
                <SettingsRow
                  title={<label htmlFor="cache-boost">{t("Replay credit")}</label>}
                  description={t(
                    "Adaptive mode gives up to eight repeat plays a recency credit. It never overrides expiry or pins. Credit fades out after 30 days without a play.",
                  )}
                  control={
                    <span className="flex items-center gap-2">
                      <NumberInput
                        id="cache-boost"
                        aria-label={t("Replay credit")}
                        className="w-28"
                        min={0}
                        max={720}
                        disabled={!enabled || draft.mode !== "adaptive"}
                        value={draft.replayBoostHours ?? 0}
                        onChange={(n) => patch({ replayBoostHours: n })}
                      />
                      <span className="text-sm text-fg-muted">{t("hours")}</span>
                    </span>
                  }
                />
                <SettingsRow
                  title={<label htmlFor="cache-share">{t("Resume protection share")}</label>}
                  description={t(
                    "Maximum share of the budget assigned to in-progress releases. The most recently watched are considered first.",
                  )}
                  control={
                    <span className="flex items-center gap-2">
                      <NumberInput
                        id="cache-share"
                        aria-label={t("Resume protection share")}
                        className="w-28"
                        min={1}
                        max={100}
                        disabled={!enabled}
                        value={draft.inProgressPinPercent || 50}
                        onChange={(n) => patch({ inProgressPinPercent: n })}
                      />
                      <span className="text-sm text-fg-muted">%</span>
                    </span>
                  }
                />
                <SettingsRow
                  title={<label htmlFor="cache-age">{t("Resume protection age")}</label>}
                  description={t("Abandoned titles lose their resume pin after this interval.")}
                  control={
                    <DurationPicker
                      id="cache-age"
                      label={t("Resume protection age")}
                      disabled={!enabled}
                      value={draft.inProgressPinMaxAge || "720h"}
                      onChange={(v) => patch({ inProgressPinMaxAge: v })}
                    />
                  }
                />
              </MoreOptions>
            </SettingsSection>

            <p className="text-[11px] leading-relaxed text-fg-subtle">
              {t(
                "Live streams and pinned releases can exceed the budget. Expiry never removes protected bytes. This budget covers playback segments and files downloaded by plugin libraries.",
              )}
            </p>
          </div>
        )}
      </Drawer>
    </Workspace>
  );
}

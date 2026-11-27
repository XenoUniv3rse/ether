import { ViewerCatalogRules } from "./ViewerCatalogRules";
import {
  Alert,
  AlertDescription,
  Badge,
  Button,
  SearchSelect,
  SettingsSection,
  Switch,
  toast,
} from "@braedonsaunders/appkit-ui";
import { useState } from "react";
import { Link } from "react-router-dom";
import { ChipGroup, NumberInput, Segmented } from "../../components/fields";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import {
  RatingLimitRow,
  StreamLimitRow,
  userLimits,
  type UserLimits,
} from "../../components/UserLimits";
import { viewerName } from "../../components/pickers";
import { t, tx } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { api, apiFetch, errorMessage } from "../../lib/api";
import type { Viewer } from "../../lib/contract";
import { useAsync } from "../../lib/hooks";
import { timeZones } from "../livetv/model";
import { PlexWatchSync } from "./sections/PlexWatchSync";
import { FromPlexDefaults } from "./FromPlexDefaults";
import { ViewerLiveTVAccess } from "./sections/ViewerLiveTV";
import { ViewerSimkl } from "./sections/ViewerSimkl";
import { ViewerMDBList } from "./sections/ViewerMDBList";
import { ViewerTrakt } from "./sections/ViewerTrakt";
import { KID_PRESETS, ListText, normalizePolicy, type SafetyPolicy } from "./shared";
import { librariesApi } from "../libraries/api";

/** A searchable IANA timezone picker; a stored zone the browser lacks stays listed. */
function TimeZoneSelect({
  value,
  onChange,
  serverDefault = false,
}: {
  value: string;
  onChange: (zone: string) => void;
  serverDefault?: boolean;
}) {
  const options = (() => {
    const zones = timeZones();
    const list = value && !zones.includes(value) ? [value, ...zones] : zones;
    return [
      ...(serverDefault ? [{ value: "", label: t("Server time zone") }] : []),
      ...list.map((zone) => ({ value: zone, label: zone.replace(/_/g, " ") })),
    ];
  })();
  return (
    <SearchSelect
      value={value}
      onChange={onChange}
      options={options}
      ariaLabel={t("Playback timezone")}
      searchable
      {...searchSelectLabels()}
    />
  );
}

/** Libraries this viewer can open (empty restriction = every library, §1.4). */
function ViewerLibraries({
  userId,
  allowed,
  isOwner,
  onSaved,
}: {
  userId: number;
  allowed?: number[] | null;
  isOwner?: boolean;
  onSaved: () => void;
}) {
  const libs = useAsync((signal) => librariesApi.list(signal), []);
  const [draft, setDraft] = useState<number[] | null>(null);
  const [saving, setSaving] = useState(false);
  const all = libs.data ?? [];
  const current = draft ?? allowed ?? [];
  const unrestricted = current.length === 0;
  const dirty = draft !== null && JSON.stringify(draft) !== JSON.stringify(allowed ?? []);
  const toggleLibrary = (id: number, on: boolean) =>
    setDraft((prev) => {
      const base = prev ?? allowed ?? [];
      return on ? [...base, id].sort((a, b) => a - b) : base.filter((x) => x !== id);
    });
  return (
    <SettingsSection
      title={t("Libraries this viewer can open")}
      description={t(
        "With every library allowed, new libraries open for this viewer too. Pick libraries to limit them to just those.",
      )}
    >
      {libs.loading ? (
        <LoadingRows rows={2} />
      ) : (
        <>
          <SettingsRow
            title={t("All libraries")}
            control={
              <Switch
                aria-label={t("All libraries")}
                checked={unrestricted || Boolean(isOwner)}
                disabled={Boolean(isOwner)}
                onChange={(e) => setDraft(e.target.checked ? [] : all.map((l) => l.id))}
              />
            }
          />
          {!unrestricted && !isOwner
            ? all.map((l) => (
                <SettingsRow
                  key={l.id}
                  title={l.name}
                  control={
                    <Switch
                      aria-label={l.name}
                      checked={current.includes(l.id)}
                      onChange={(e) => toggleLibrary(l.id, e.target.checked)}
                    />
                  }
                />
              ))
            : null}
          {isOwner ? (
            <p className="px-5 py-4 text-sm text-fg-muted">
              {t("The owner can always open every library.")}
            </p>
          ) : (
            <div className="pt-2">
              <Button
                size="sm"
                disabled={!dirty || saving}
                onClick={() => {
                  setSaving(true);
                  api
                    .updateUser(userId, { allowedLibraries: current })
                    .then(() => {
                      toast.success(t("Saved"));
                      setDraft(null);
                      onSaved();
                    })
                    .catch((e: unknown) => toast.error(errorMessage(e)))
                    .finally(() => setSaving(false));
                }}
              >
                {t("Save")}
              </Button>
            </div>
          )}
        </>
      )}
    </SettingsSection>
  );
}

/**
 * Connected watch-history accounts for one viewer (Sign-in & devices › Connected accounts):
 * Plex account sync, Plex Media Server mappings, Trakt, Simkl and MDBList.
 */
export function ViewerAccountSync({ viewer }: { viewer: Viewer }) {
  const id = viewer.id;
  const config = useAsync(() => api.getConfig(), []);
  const trakt = config.data?.trakt;
  const simkl = config.data?.simkl;
  const mdblist = config.data?.mdblist;
  return (
    <div className="space-y-6">
      <PlexWatchSync viewerId={id} plexId={viewer.plexId} />
      {config.data ? (
        <>
          <ViewerTrakt viewerId={id} ready={Boolean(trakt?.enabled && trakt.clientId.trim())} />
          <ViewerSimkl viewerId={id} ready={Boolean(simkl?.enabled && simkl.clientId.trim())} />
          <ViewerMDBList viewerId={id} ready={Boolean(mdblist?.enabled)} />
        </>
      ) : null}
    </div>
  );
}

type PlexDownloadDefaults = {
  allowSync?: boolean | null;
};

function overrideLabel(o: {
  id?: string;
  product?: string;
  productMatch?: string;
  device?: string;
  platform?: string;
}): string {
  const parts = [o.product || o.productMatch, o.device, o.platform].filter(Boolean);
  return parts.join(" · ") || o.id || t("App override");
}

/**
 * Everything that decides what one viewer may watch and when: streams at
 * once, the content rating limit, parental controls, allowed times and Live TV.
 * One in-place form; the save bar writes whichever parts changed.
 */
export function ViewerAccess({ viewer }: { viewer: Viewer }) {
  const id = viewer.id;
  const name = viewerName(viewer);
  const users = useAsync(() => api.users(), []);
  const config = useAsync(() => api.getConfig(), []);
  const policy = useAsync(
    async (signal) =>
      normalizePolicy(await apiFetch<SafetyPolicy>(`/safety/policies/${id}`, { signal })),
    [id],
  );
  const plexDefaults = useAsync(
    async (signal) =>
      viewer.isOwner
        ? null
        : await apiFetch<PlexDownloadDefaults>(`/viewer-plex-defaults/${id}`, { signal }).catch(
            () => null,
          ),
    [id, viewer.isOwner],
  );
  const user = users.data?.find((u) => u.id === id);
  const limits = useDraft<UserLimits>(user ? userLimits(user) : null);
  const rules = useDraft<SafetyPolicy>(policy.data);
  const [saving, setSaving] = useState(false);
  const [presetBusy, setPresetBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const patchLimits = (p: Partial<UserLimits>) => limits.setDraft((d) => (d ? { ...d, ...p } : d));
  const patchPolicy = (p: Partial<SafetyPolicy>) =>
    rules.setDraft((d) => {
      if (!d) return d;
      const next: SafetyPolicy = { ...d, ...p };
      if ("allowDownloads" in p && p.allowDownloads == null) {
        delete next.allowDownloads;
      }
      return next;
    });

  const applyPreset = async (age: string) => {
    setPresetBusy(true);
    try {
      const { maxContentRating, ...preset } = await apiFetch<
        Partial<SafetyPolicy> & { maxContentRating?: string }
      >(`/safety/policies/presets/kids?age=${age}`);
      rules.setDraft(normalizePolicy(preset));
      if (maxContentRating !== undefined) patchLimits({ maxContentRating });
    } catch (e) {
      toast.error(t("Couldn't load the preset"), { description: errorMessage(e) });
    } finally {
      setPresetBusy(false);
    }
  };

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      if (limits.dirty && limits.draft && user) {
        const saved = await api.updateUser(id, limits.draft);
        const next = saved ?? { ...user, ...limits.draft };
        users.setData((list) => (list ?? []).map((u) => (u.id === id ? next : u)));
      }
      if (rules.dirty && rules.draft) {
        await apiFetch(`/safety/policies/${id}`, {
          method: "PUT",
          body: JSON.stringify(rules.draft),
        });
        policy.setData(rules.draft);
      }
      toast.success(t("Preferences saved"), {
        description: t("{name}'s Ether preferences are up to date.", { name }),
      });
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  const l = limits.draft;
  const p = rules.draft;
  const noUser = users.data && (!user || !l);
  const isOwner = Boolean(user?.isOwner || viewer.isOwner);
  const serverVideoOff = config.data?.transcode?.videoTranscoding === "off";
  const alwaysEncodeOverrides = (config.data?.clients?.overrides ?? []).filter(
    (o) => o.alwaysEncodeVideo,
  );
  const alwaysEncodeNames = alwaysEncodeOverrides.map(overrideLabel).join(", ");
  const hasAlwaysEncodeConflict =
    alwaysEncodeOverrides.length > 0 && (Boolean(p?.videoTranscodingDisabled) || serverVideoOff);

  const serverDownloadsOn = Boolean(config.data?.downloads?.enabled);
  const serverFriendsDownloads = Boolean(config.data?.downloads?.allowFriends);
  const plexAllowSync = plexDefaults.data?.allowSync;
  const downloadMode: "default" | "allow" | "block" =
    p?.allowDownloads === true ? "allow" : p?.allowDownloads === false ? "block" : "default";
  const effectiveDownloads = !serverDownloadsOn
    ? false
    : p?.allowDownloads !== undefined && p?.allowDownloads !== null
      ? p.allowDownloads
      : plexAllowSync === false
        ? false
        : serverFriendsDownloads;
  return (
    <div className="space-y-6">
      <ViewerCatalogRules key={id} id={id} region={String(config.data?.catalog?.region || "US")} />

      {users.error ? (
        <ErrorPanel error={users.error} onRetry={() => void users.reload()} />
      ) : policy.error ? (
        <ErrorPanel error={policy.error} onRetry={() => void policy.reload()} />
      ) : !users.data || !p ? (
        <LoadingRows rows={3} />
      ) : (
        <>
          {noUser ? (
            <p className="text-sm text-fg-muted">
              {t(
                "This viewer has no user record on the server yet, so there are no limits to set.",
              )}
            </p>
          ) : null}
          {!isOwner ? (
            <SettingsSection
              title={t("Parental controls")}
              description={t(
                "Adult titles are always excluded. Unrated titles are blocked when a rating limit applies.",
              )}
            >
              <SettingsRow
                title={t("Start from a preset")}
                description={t(
                  "Replaces the parental controls, playback restrictions and allowed times below.",
                )}
                control={
                  <div className="flex flex-wrap gap-2">
                    {KID_PRESETS.map((preset) => (
                      <Button
                        key={preset.value}
                        variant="outline"
                        size="sm"
                        disabled={presetBusy}
                        onClick={() => void applyPreset(preset.value)}
                      >
                        {preset.label}
                      </Button>
                    ))}
                  </div>
                }
              />
              {user && l ? (
                <RatingLimitRow
                  value={l.maxContentRating}
                  onChange={(maxContentRating) => patchLimits({ maxContentRating })}
                  isOwner={user.isOwner}
                />
              ) : null}
              <SettingsRow
                title={t("Blocked genres")}
                description={t("Comma-separated, e.g. Horror, Thriller.")}
                control={
                  <div className="w-64">
                    <ListText
                      ariaLabel={t("Blocked genres")}
                      value={p.blockedGenres}
                      onChange={(v) => patchPolicy({ blockedGenres: v })}
                    />
                  </div>
                }
              />
              <SettingsRow
                title={t("Blocked keywords")}
                description={t("Comma-separated TMDB keywords.")}
                control={
                  <div className="w-64">
                    <ListText
                      ariaLabel={t("Blocked keywords")}
                      value={p.blockedKeywords}
                      onChange={(v) => patchPolicy({ blockedKeywords: v })}
                    />
                  </div>
                }
              />
            </SettingsSection>
          ) : null}

          <ViewerLibraries
            userId={id}
            allowed={user?.allowedLibraries}
            isOwner={isOwner}
            onSaved={() => void users.reload()}
          />

          <SettingsSection title={t("Playback restrictions")}>
            {user && l ? (
              <StreamLimitRow
                value={l.streamLimit}
                onChange={(streamLimit) => patchLimits({ streamLimit })}
                defaultStreamLimit={config.data?.server.defaultStreamLimit}
                hint={
                  <Link
                    to="/playback/transcoding"
                    className="font-medium text-primary hover:underline"
                  >
                    {t("Change the server default")}
                  </Link>
                }
              />
            ) : null}
            <SettingsRow
              title={t("Local network only")}
              description={t("Block playback from outside your home network.")}
              control={
                <Switch
                  aria-label={t("Local network only")}
                  checked={p.remoteDisabled}
                  onChange={(e) => patchPolicy({ remoteDisabled: e.target.checked })}
                />
              }
            />
            <SettingsRow
              setting="no-video-transcoding"
              title={t("No video transcoding")}
              description={
                <span className="space-y-1">
                  <span className="block">
                    {t(
                      "Play this viewer's video only as it is. Ether picks a version their device can play and never converts the picture for them. Audio can still be converted. Live TV is unaffected.",
                    )}
                  </span>
                  {serverVideoOff ? (
                    <span className="block">
                      {t("Video transcoding is already turned off for the whole server.")}{" "}
                      <Link
                        to="/playback/transcoding"
                        className="font-medium text-primary hover:underline"
                      >
                        {t("Review Transcoding")}
                      </Link>
                    </span>
                  ) : null}
                </span>
              }
              control={
                <Switch
                  aria-label={t("No video transcoding")}
                  checked={Boolean(p.videoTranscodingDisabled)}
                  onChange={(e) => patchPolicy({ videoTranscodingDisabled: e.target.checked })}
                />
              }
            />
            {hasAlwaysEncodeConflict ? (
              <div className="px-5 py-3">
                <Alert variant="warning" role="status">
                  <AlertDescription>
                    {t(
                      "“Always convert video” is turned on in App compatibility ({apps}). “No video transcoding” wins for this viewer on movies and shows, so Ether still copies video instead of re-encoding it (Live TV is unaffected).",
                      { apps: alwaysEncodeNames },
                    )}{" "}
                    <Link to="/playback/devices" className="font-medium underline">
                      {t("Review App compatibility")}
                    </Link>
                  </AlertDescription>
                </Alert>
              </div>
            ) : null}
            {!isOwner ? (
              <>
                <SettingsRow
                  setting="downloads"
                  title={t("Downloads")}
                  description={
                    <span className="space-y-1">
                      <span className="block">
                        {t(
                          "Allow or block offline downloads for this viewer, or follow the server setting.",
                        )}
                      </span>
                      {config.data ? (
                        !serverDownloadsOn ? (
                          <span className="block">
                            {t(
                              "Offline downloads are turned off for the whole server, so this setting has no effect right now.",
                            )}{" "}
                            <Link
                              to="/apps/plex/downloads"
                              className="font-medium text-primary hover:underline"
                            >
                              {t("Open Plex downloads")}
                            </Link>
                          </span>
                        ) : downloadMode === "default" ? (
                          <span className="block">
                            {plexAllowSync === false
                              ? t(
                                  "Following the server setting, but this viewer's plex.tv share disables downloads. Pick Allow to override it.",
                                )
                              : serverFriendsDownloads
                                ? t("Following the server setting: friends can download.")
                                : t("Following the server setting: friends cannot download.")}
                          </span>
                        ) : downloadMode === "allow" ? (
                          <span className="block">
                            {t(
                              "Overrides the server and plex.tv share settings while server-wide downloads are on.",
                            )}
                          </span>
                        ) : (
                          <span className="block">
                            {t(
                              "Blocks offline downloads for this viewer even if friends are allowed.",
                            )}
                          </span>
                        )
                      ) : null}
                    </span>
                  }
                  control={
                    <div className="flex flex-wrap items-center justify-end gap-2">
                      <Segmented
                        size="sm"
                        ariaLabel={t("Downloads")}
                        value={downloadMode}
                        onChange={(next) =>
                          patchPolicy({
                            allowDownloads: next === "default" ? undefined : next === "allow",
                          })
                        }
                        options={[
                          { value: "default", label: t("Server default") },
                          { value: "allow", label: t("Allow") },
                          { value: "block", label: t("Block") },
                        ]}
                      />
                      {config.data ? (
                        <Badge
                          variant={effectiveDownloads ? "success" : "secondary"}
                          className="whitespace-nowrap"
                        >
                          {effectiveDownloads ? t("Effective: Allowed") : t("Effective: Blocked")}
                        </Badge>
                      ) : null}
                    </div>
                  }
                />
                <SettingsRow
                  setting="no-download-transcoding"
                  title={t("No converting for downloads")}
                  description={t(
                    "Downloads for this viewer use only a version that needs no video conversion at the chosen quality. Separate from playback: either switch can be on without the other.",
                  )}
                  control={
                    <Switch
                      aria-label={t("No converting for downloads")}
                      checked={Boolean(p.downloadTranscodingDisabled)}
                      onChange={(e) =>
                        patchPolicy({ downloadTranscodingDisabled: e.target.checked })
                      }
                    />
                  }
                />
                <SettingsRow
                  setting="download-limit"
                  title={t("Download limit")}
                  description={t(
                    "How much this viewer can download: a number of titles or an amount of data, per day or per month. Server default follows the Downloads settings under Apps.",
                  )}
                  control={
                    <div className="flex flex-wrap items-center justify-end gap-2">
                      <SearchSelect
                        ariaLabel={t("Download limit counts")}
                        value={p.downloadLimitUnit || "default"}
                        options={[
                          { value: "default", label: t("Server default") },
                          { value: "none", label: t("No limit") },
                          { value: "titles", label: t("Titles") },
                          { value: "gb", label: t("GB") },
                        ]}
                        onChange={(choice) =>
                          patchPolicy({
                            downloadLimitUnit:
                              choice === "none" || choice === "titles" || choice === "gb"
                                ? choice
                                : "",
                          })
                        }
                        {...searchSelectLabels()}
                      />
                      {p.downloadLimitUnit === "titles" || p.downloadLimitUnit === "gb" ? (
                        <>
                          <NumberInput
                            aria-label={t("Download limit amount")}
                            className="w-24"
                            min={0}
                            value={p.downloadLimitAmount ?? 0}
                            onChange={(n) =>
                              patchPolicy({
                                downloadLimitAmount: Math.max(0, Math.min(100000, Math.round(n))),
                              })
                            }
                          />
                          <SearchSelect
                            ariaLabel={t("Download limit period")}
                            value={p.downloadLimitPeriod === "month" ? "month" : "day"}
                            options={[
                              { value: "day", label: t("per day") },
                              { value: "month", label: t("per month") },
                            ]}
                            onChange={(choice) =>
                              patchPolicy({
                                downloadLimitPeriod: choice === "month" ? "month" : "day",
                              })
                            }
                            {...searchSelectLabels()}
                          />
                        </>
                      ) : null}
                    </div>
                  }
                />
                <SettingsRow
                  setting="download-max-height"
                  title={t("Highest download quality")}
                  description={tx(
                    "Applies only to downloads, up to the server’s download limit. To limit playback quality, set a {link} for this viewer.",
                    {
                      link: (
                        <Link
                          className="text-primary underline underline-offset-2"
                          to="/releases/rules"
                        >
                          {t("quality rule")}
                        </Link>
                      ),
                    },
                  )}
                  control={
                    <SearchSelect
                      ariaLabel={t("Highest download quality")}
                      value={String(p.downloadMaxHeight ?? 0)}
                      options={[
                        { value: "0", label: t("Server default") },
                        { value: "480", label: "480p" },
                        { value: "720", label: "720p" },
                        { value: "1080", label: "1080p" },
                        { value: "2160", label: "4K" },
                      ]}
                      onChange={(choice) => patchPolicy({ downloadMaxHeight: Number(choice) || 0 })}
                      {...searchSelectLabels()}
                    />
                  }
                />
              </>
            ) : null}
            <SettingsRow
              title={t("Maximum bitrate")}
              description={t("In kbps. 0 uses the server default.")}
              control={
                <NumberInput
                  aria-label={t("Maximum bitrate")}
                  className="w-28"
                  min={0}
                  value={p.maxBitrateKbps}
                  onChange={(n) => patchPolicy({ maxBitrateKbps: Math.max(0, Math.round(n)) })}
                />
              }
            />
          </SettingsSection>

          {user && !isOwner ? (
            <SettingsSection
              title={t("Allowed times")}
              description={t("Let the current item finish, then pause playback until wake time.")}
            >
              <SettingsRow
                title={t("Bedtime")}
                control={
                  <Switch
                    checked={!!p.bedtime}
                    onChange={(e) =>
                      patchPolicy({
                        allowedHours: [],
                        bedtime: e.target.checked
                          ? { time: "20:00", wakeTime: "06:00", timezone: p.timezone }
                          : null,
                      })
                    }
                    aria-label={t("Bedtime")}
                  />
                }
              />
              {!p.bedtime && p.allowedHours.length > 0 ? (
                <SettingsRow
                  title={t("Allowed times")}
                  control={
                    <span>
                      {p.allowedHours
                        .map((hour) => `${String(hour).padStart(2, "0")}:00`)
                        .join(", ")}
                    </span>
                  }
                />
              ) : null}
              {p.bedtime ? (
                <>
                  <SettingsRow
                    title={t("Bedtime starts")}
                    control={
                      <input
                        type="time"
                        className="rounded border border-border bg-background px-3 py-2"
                        aria-label={t("Bedtime starts")}
                        required
                        value={p.bedtime.time}
                        onChange={(e) =>
                          patchPolicy({ bedtime: { ...p.bedtime!, time: e.target.value } })
                        }
                      />
                    }
                  />
                  <SettingsRow
                    title={t("Wake time")}
                    control={
                      <input
                        type="time"
                        className="rounded border border-border bg-background px-3 py-2"
                        aria-label={t("Wake time")}
                        required
                        value={p.bedtime.wakeTime || "06:00"}
                        onChange={(e) =>
                          patchPolicy({ bedtime: { ...p.bedtime!, wakeTime: e.target.value } })
                        }
                      />
                    }
                  />
                  <SettingsRow
                    title={t("Days")}
                    description={t(
                      "Choose the days bedtime starts. With no days selected, it applies every day.",
                    )}
                    control={
                      <ChipGroup
                        ariaLabel={t("Days")}
                        options={[
                          { value: 0, label: t("Sunday") },
                          { value: 1, label: t("Monday") },
                          { value: 2, label: t("Tuesday") },
                          { value: 3, label: t("Wednesday") },
                          { value: 4, label: t("Thursday") },
                          { value: 5, label: t("Friday") },
                          { value: 6, label: t("Saturday") },
                        ]}
                        value={p.bedtime.days ?? []}
                        onChange={(days) => patchPolicy({ bedtime: { ...p.bedtime!, days } })}
                      />
                    }
                  />
                </>
              ) : null}
              <SettingsRow
                title={t("Time zone")}
                description={t("Leave empty to use the server’s time zone.")}
                control={
                  <div className="w-64">
                    <TimeZoneSelect
                      serverDefault={!!p.bedtime || p.allowedHours.length === 0}
                      value={p.timezone}
                      onChange={(timezone) =>
                        patchPolicy({
                          timezone,
                          ...(p.bedtime ? { bedtime: { ...p.bedtime, timezone } } : {}),
                        })
                      }
                    />
                  </div>
                }
              />
            </SettingsSection>
          ) : null}
        </>
      )}

      <FromPlexDefaults viewerId={id} show="share" />

      <ViewerLiveTVAccess viewerId={id} />

      <SaveBar
        dirty={limits.dirty || rules.dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          limits.reset();
          rules.reset();
          setError(null);
        }}
      />
    </div>
  );
}

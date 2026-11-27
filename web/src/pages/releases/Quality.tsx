import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  EmptyState,
  SearchSelect,
  SettingsSection,
  Switch,
  confirmDialog,
  toast,
} from "@braedonsaunders/appkit-ui";
import { ChevronRight, Plus, RefreshCw, SlidersHorizontal } from "lucide-react";
import { useState } from "react";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { NumberInput } from "../../components/fields";
import { Section, SubtabNav } from "../../components/layout";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { localized, t, tn } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import { ApiError, api } from "../../lib/api";
import { relativeTime } from "../../lib/format";
import { itemName } from "../../lib/qualityRanking";
import { profileUses, tidyLimits, withProfileRenamed, type RuleRef } from "../../lib/qualityRules";
import { releaseConsole, type QualitySyncReport } from "../../lib/releases";
import type { CustomFormatDefinition, QualityProfile } from "../../lib/types";
import { parseDuration } from "../settings/duration";
import { LocalFilesFirstSection, SourceOrderSection } from "../sources/EqualQuality";
import { ConfigSection } from "../../components/ConfigSection";
import { PLAY_VERSIONS } from "../settings/schema";
import {
  BoolRow,
  RecordDrawer,
  RecordItem,
  TextRow,
  recordErrors,
  type Errors,
} from "../sources/form";
import { FilterRulesEditor, releaseRuleConflict, useRankingRegistry } from "./ReleasePreferences";
import { CustomFormatsSection, LanguageSection, QualityOrderSection } from "./QualityRanking";
import {
  formatsJSON,
  mergeCustomFormats,
  parseFormats,
  sameFormats,
  type ResolverPolicy,
} from "./resolverDraft";
import { TrashImport } from "./TrashImport";
import { RuleEditor, localizedRule, ruleTarget, useRuleNames, type RuleTarget } from "./RuleDrawer";
import { useQuality, type Quality } from "./useQuality";
const RESOLUTIONS = localized(() => [
  { value: "0", label: t("Any") },
  { value: "480", label: "480p" },
  { value: "720", label: "720p" },
  { value: "1080", label: "1080p" },
  { value: "2160", label: t("4K (2160p)") },
]);

const resolutionLabel = (h: number) =>
  RESOLUTIONS.find((x) => x.value === String(h))?.label ?? `${h}p`;

function profileSummary(p: QualityProfile) {
  const range =
    p.minHeight === p.maxHeight
      ? resolutionLabel(p.maxHeight)
      : t("{min} to {max}", {
          min: resolutionLabel(p.minHeight),
          max: resolutionLabel(p.maxHeight),
        });
  const first = p.qualityOrder?.[0];
  const formats = Object.values(p.customFormatScores ?? {}).filter((v) => v !== 0).length;
  return [
    range,
    p.allowDolbyVision ? t("HDR and Dolby Vision") : p.allowHdr ? t("HDR allowed") : t("SDR only"),
    first ? t("{quality} first", { quality: itemName(first) }) : "",
    formats ? tn(formats, "{count} custom format", "{count} custom formats") : "",
  ]
    .filter(Boolean)
    .join(" · ");
}

/** Where an imported profile came from: "TRaSH · Radarr", "Profilarr · Dictionarry". */
function importSource(p: QualityProfile): string {
  if (!p.import) return "";
  if (p.import.source === "dictionarry") return "Profilarr · Dictionarry";
  return `TRaSH · ${p.import.app === "sonarr" ? "Sonarr" : "Radarr"}`;
}

/**
 * The server names a refused profile by its position in the list sent. Name
 * the profile instead, so a change blocked by another profile (removing one
 * while two others clash) says which one to fix.
 */
function withProfileNames(e: unknown, sent: QualityProfile[]): unknown {
  if (!(e instanceof ApiError) || !e.fields?.length) return e;
  const fields = e.fields.map((f) => {
    const at = /^quality\.profiles\[(\d+)\]/.exec(f.field);
    const p = at ? sent[Number(at[1])] : undefined;
    if (!p) return f;
    return { ...f, field: p.name };
  });
  return new ApiError(e.status, e.code, e.message, fields);
}

const SYNC_INTERVALS = localized(() => [
  { value: "6", label: t("Every 6 hours") },
  { value: "12", label: t("Every 12 hours") },
  { value: "24", label: t("Every 24 hours") },
  { value: "72", label: t("Every 3 days") },
  { value: "168", label: t("Every 7 days") },
]);

/** Hours in a Go duration string, defaulting to 24. */
function syncHours(raw: string | undefined): string {
  if (!raw) return "24";
  const { amount, unit } = parseDuration(raw);
  const n = Number(amount);
  if (!Number.isFinite(n)) return "24";
  const hours = unit === "d" ? n * 24 : unit === "h" ? n : unit === "m" ? n / 60 : n / 3600;
  return hours > 0 ? String(hours) : "24";
}

/**
 * Automatic update of imported profiles and their format definitions. "Sync
 * now" stays available while automatic sync is off: a manual pull is how an
 * owner takes guide changes only when they choose to.
 */
function SyncSection({ quality, onSaved }: { quality: Quality; onSaved: () => Promise<void> }) {
  const sync = quality.sync;
  const hasRun = Boolean(sync?.lastRun) && !sync!.lastRun!.startsWith("0001-");
  const { draft, setDraft, dirty, reset } = useDraft({
    enabled: sync?.enabled ?? false,
    hours: syncHours(sync?.interval),
  });
  const [saving, setSaving] = useState(false);
  const [syncing, setSyncing] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [syncError, setSyncError] = useState<unknown>(null);
  const [report, setReport] = useState<QualitySyncReport | null>(null);
  if (!draft) return null;
  const imported = (quality.profiles ?? []).some((p) => p.import);
  const hours = draft.hours;
  const options = SYNC_INTERVALS.some((o) => o.value === hours)
    ? SYNC_INTERVALS
    : [...SYNC_INTERVALS, { value: hours, label: t("Every {hours} hours", { hours }) }];

  async function save() {
    if (!draft) return;
    setSaving(true);
    setSaveError(null);
    try {
      // The admin endpoint patches the quality section, so omitted run
      // state stays whatever the server recorded while this form was open.
      await api.putSection("quality", {
        sync: { enabled: draft.enabled, interval: `${draft.hours}h` },
      });
      toast.success(t("Sync settings saved"));
      await onSaved();
    } catch (e) {
      setSaveError(e);
    } finally {
      setSaving(false);
    }
  }

  async function syncNow() {
    setSyncing(true);
    setSyncError(null);
    setReport(null);
    try {
      setReport(await releaseConsole.syncQuality());
      toast.success(t("Guide sync finished"));
      await onSaved();
    } catch (e) {
      setSyncError(e);
    } finally {
      setSyncing(false);
    }
  }

  const title = t("Keep imported profiles in sync");
  if (!imported) {
    return (
      <Section title={title}>
        <p className="text-sm text-fg-muted">
          {t("Applies once you import a profile from TRaSH Guides or Profilarr.")}
        </p>
      </Section>
    );
  }

  return (
    <Section
      title={title}
      description={t("Only profiles imported from TRaSH Guides or Profilarr are updated.")}
      actions={
        <Switch
          aria-label={title}
          checked={draft.enabled}
          onChange={(e) => setDraft({ ...draft, enabled: e.currentTarget.checked })}
        />
      }
    >
      {draft.enabled ? (
        <div className="flex flex-wrap items-center justify-between gap-3">
          <span className="text-sm font-medium text-fg">{t("Check for changes")}</span>
          <div className="w-52">
            <SearchSelect
              {...searchSelectLabels()}
              ariaLabel={t("Check for changes")}
              value={hours}
              onChange={(value) => setDraft({ ...draft, hours: value })}
              options={options}
              searchable={false}
            />
          </div>
        </div>
      ) : null}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="text-sm text-fg-muted">
          {sync?.lastError ? (
            <span className="text-danger">{sync.lastError}</span>
          ) : hasRun ? (
            t("Last synced {when}", { when: relativeTime(sync!.lastRun!) })
          ) : (
            t("Never synced")
          )}
        </span>
        <Button variant="outline" size="sm" disabled={syncing} onClick={() => void syncNow()}>
          <RefreshCw size={14} className={syncing ? "animate-spin" : undefined} />
          {syncing ? t("Syncing…") : t("Sync now")}
        </Button>
      </div>
      {syncError ? <ErrorPanel error={syncError} /> : null}
      {report ? (
        <Alert variant="success" role="status">
          <AlertTitle>{t("Guide sync finished")}</AlertTitle>
          <AlertDescription>
            <ul className="list-disc space-y-1 pl-5">
              <li>
                {tn(report.profilesUpdated, "{count} profile updated", "{count} profiles updated")}
              </li>
              <li>{tn(report.formatsAdded, "{count} format added", "{count} formats added")}</li>
              <li>
                {tn(report.formatsUpdated, "{count} format updated", "{count} formats updated")}
              </li>
              <li>
                {tn(report.formatsRemoved, "{count} format removed", "{count} formats removed")}
              </li>
              {(report.warnings ?? []).map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      ) : null}
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={saveError}
        onSave={() => void save()}
        onDiscard={() => {
          reset();
          setSaveError(null);
        }}
      />
    </Section>
  );
}

export function QualityTab() {
  const { config, quality, commit, refresh } = useQuality();
  if (config.error) return <ErrorPanel error={config.error} onRetry={() => void config.reload()} />;
  if (!quality) return <LoadingRows rows={4} />;
  return (
    <>
      <ProfileList
        quality={quality}
        resolver={config.data?.resolver ?? null}
        commit={commit}
        refresh={refresh}
      />
      <SyncSection quality={quality} onSaved={refresh} />
      <SourceOrderSection />
      <LocalFilesFirstSection />
      <ConfigSection
        configKey="resolver"
        endpoint="resolver"
        groups={PLAY_VERSIONS}
        label={t("Version menu")}
      />
    </>
  );
}

type Commit = (next: Quality, message: string) => Promise<void>;
type ProfileDrawerTab = "tiers" | "limits" | "scoring";

function ProfileList({
  quality,
  resolver,
  commit,
  refresh,
}: {
  quality: Quality;
  resolver: ResolverPolicy | null;
  commit: Commit;
  refresh: () => Promise<void>;
}) {
  const { fields } = useRankingRegistry();
  const names = useRuleNames();
  const profiles = quality.profiles ?? [];
  /** Where a profile is used, with device kinds in the owner's language. */
  const usesOf = (name: string) =>
    profileUses({ ...quality, rules: (quality.rules ?? []).map(localizedRule) }, name, names);
  const [importing, setImporting] = useState(false);
  const [editing, setEditing] = useState<{ index: number; draft: QualityProfile } | null>(null);
  /** Set while removing a profile that is in use: the profile its rules switch to. */
  const [replacement, setReplacement] = useState<string | null>(null);
  /** The shared custom format definitions editor (JSON). */
  const [formatsText, setFormatsText] = useState("[]");
  /** The definitions editor text when the drawer opened; unchanged means no PUT. */
  const [formatBaseline, setFormatBaseline] = useState("");
  /** The shared definitions as they were when the drawer opened (for merging). */
  const [formatSnapshot, setFormatSnapshot] = useState<CustomFormatDefinition[]>([]);
  const [errors, setErrors] = useState<Errors>({});
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const [drawerTab, setDrawerTab] = useState<ProfileDrawerTab>("tiers");
  /** A rule opened from "Used by": over the profile drawer when one is open. */
  const [rule, setRule] = useState<RuleTarget | null>(null);
  const openRule = (ref: RuleRef) => setRule(ruleTarget(quality, ref));
  const ruleEditor = (
    <RuleEditor
      quality={quality}
      commit={commit}
      target={rule}
      onClose={() => setRule(null)}
      onOpenRule={(index) => {
        const id = (quality.rules ?? [])[index]?.id;
        if (id) openRule({ rule: id });
      }}
    />
  );

  const open = (index: number, draft: QualityProfile) => {
    setErrors({});
    setError(null);
    setReplacement(null);
    setDrawerTab("tiers");
    setEditing({ index, draft: structuredClone(draft) });
    const text = formatsJSON(resolver);
    setFormatsText(text);
    setFormatBaseline(text);
    setFormatSnapshot(structuredClone(resolver?.customFormats ?? []));
  };

  async function save() {
    if (!editing) return;
    const { index, draft } = editing;
    let parsed: CustomFormatDefinition[] | null = null;
    if (formatsText !== formatBaseline) {
      try {
        parsed = parseFormats(formatsText);
      } catch (e) {
        setError(e);
        setDrawerTab("scoring");
        return;
      }
    }
    const old = profiles[index];
    const list =
      index < profiles.length
        ? profiles.map((p, j) => (j === index ? draft : p))
        : [...profiles, draft];

    let next: Quality = { ...quality, profiles: list };
    // Rules and defaults follow a renamed profile.
    if (old && old.name !== draft.name) next = withProfileRenamed(next, old.name, draft.name);
    if (!next.default) next.default = draft.name;

    const conflict = releaseRuleConflict(draft.releaseRules ?? [], fields);
    if (conflict) {
      setErrors({ releaseRules: conflict });
      setDrawerTab("limits");
      return;
    }
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await commit(next, index < profiles.length ? t("Profile saved") : t("Profile added"));
      // Only touch the shared definitions when this drawer actually edited
      // them; otherwise a stale snapshot must not be sent back.
      if (parsed) {
        const latestConfig = await api.getConfig();
        const latest = latestConfig.resolver.customFormats ?? [];
        const app = draft.import?.app;
        const merged = mergeCustomFormats({
          original: formatSnapshot,
          edited: parsed,
          latest,
          app: app === "radarr" || app === "sonarr" ? app : null,
        });
        if (!sameFormats(merged, latest)) {
          await api.putSection("resolver", { customFormats: merged });
          await refresh();
        }
      }
      setEditing(null);
    } catch (e) {
      const nextErrors = recordErrors(e, `quality.profiles[${index}].`);
      setError(withProfileNames(e, list));
      setErrors(nextErrors);
      if (nextErrors.name) {
        setDrawerTab("tiers");
      } else if (
        nextErrors.minHeight ||
        nextErrors.maxHeight ||
        nextErrors.minSizeMbPerMin ||
        nextErrors.maxSizeMbPerMin ||
        nextErrors.episodeMinSizeMbPerMin ||
        nextErrors.episodeMaxSizeMbPerMin ||
        nextErrors.releaseRules
      ) {
        setDrawerTab("limits");
      }
    } finally {
      setSaving(false);
    }
  }

  /**
   * Remove the profile being edited. One that rules or defaults use asks
   * which profile they switch to first, so nothing changes silently.
   */
  async function remove(switchTo?: string) {
    if (!editing) return;
    const { index } = editing;
    const name = profiles[index]?.name ?? editing.draft.name;
    const rest = profiles.filter((_, j) => j !== index);
    if (usesOf(name).length && !switchTo) {
      setReplacement(rest[0]?.name ?? "");
      return;
    }
    if (!switchTo) {
      const ok = await confirmDialog({
        title: t("Remove profile?"),
        message: name ? t("{name} will be removed.", { name }) : t("This profile will be removed."),
        confirmLabel: t("Remove"),
        tone: "danger",
      });
      if (!ok) return;
    }
    let next: Quality = { ...quality, profiles: rest };
    if (switchTo) {
      next = withProfileRenamed(next, name, switchTo);
      // Away on the same profile and limits as at home is "Same as at home".
      if (
        next.defaultAway === next.default &&
        JSON.stringify(tidyLimits(next.defaultAwayLimits)) ===
          JSON.stringify(tidyLimits(next.defaultLimits))
      ) {
        next.defaultAway = "";
        next.defaultAwayLimits = {};
      }
    }
    setSaving(true);
    setError(null);
    try {
      await commit(next, t("Profile removed"));
      setEditing(null);
    } catch (e) {
      setError(withProfileNames(e, rest));
    } finally {
      setSaving(false);
    }
  }

  const draft = editing?.draft;
  const isNew = editing ? editing.index >= profiles.length : false;
  const patch = (p: Partial<QualityProfile>) =>
    setEditing((e) => (e ? { ...e, draft: { ...e.draft, ...p } } : e));
  const editedName = editing ? (profiles[editing.index]?.name ?? "") : "";
  const draftUses = editedName ? usesOf(editedName) : [];

  return (
    <>
      <Section
        title={t("Profiles")}
        description={t(
          "A profile says which releases to prefer: resolution, HDR, release type and language. Rules decide who gets which profile.",
        )}
        actions={
          <Button size="sm" onClick={() => setImporting(true)}>
            <Plus size={14} /> {t("Add profile")}
          </Button>
        }
        bodyClassName=""
      >
        {profiles.length === 0 ? (
          <EmptyState
            icon={<SlidersHorizontal />}
            title={t("No profiles")}
            description={t("Add a profile so Ether knows which releases to prefer.")}
          />
        ) : (
          <ul className="divide-y divide-border">
            {profiles.map((p, i) => {
              const uses = usesOf(p.name);
              const source = importSource(p);
              return (
                <RecordItem
                  key={`${p.name}-${i}`}
                  title={p.name || t("Profile {n}", { n: i + 1 })}
                  description={profileSummary(p)}
                  meta={
                    <>
                      {source ? <Badge variant="outline">{source}</Badge> : null}
                      {uses.length ? (
                        <span className="flex flex-wrap items-center gap-1.5">
                          <span className="text-xs text-fg-muted">{t("Used by")}</span>
                          {uses.map((u) => (
                            <button
                              key={u.key}
                              type="button"
                              className="rounded-full"
                              aria-label={t("Open {name}", { name: u.label })}
                              onClick={() => openRule(u.open)}
                            >
                              <Badge variant="info">{u.label}</Badge>
                            </button>
                          ))}
                        </span>
                      ) : (
                        <Badge variant="secondary">{t("Not used by any rule")}</Badge>
                      )}
                    </>
                  }
                  editLabel={t("Edit {name}", { name: p.name || t("profile {n}", { n: i + 1 }) })}
                  onEdit={() => open(i, p)}
                />
              );
            })}
          </ul>
        )}
      </Section>

      <TrashImport open={importing} onClose={() => setImporting(false)} onImported={refresh} />

      {/* From the list the rule drawer opens on its own. */}
      {editing === null ? ruleEditor : null}

      <RecordDrawer
        open={editing !== null}
        onClose={() => !saving && setEditing(null)}
        title={isNew ? t("New profile") : draft?.name || t("Profile")}
        description={t("How releases are filtered and ranked for plays that use this profile.")}
        subtabs={
          draft ? (
            <div className="[&>div]:mb-0">
              <SubtabNav
                ariaLabel={t("Profile sections")}
                active={drawerTab}
                onSelect={(k) => setDrawerTab(k as ProfileDrawerTab)}
                tabs={[
                  { key: "tiers", label: t("Tiers") },
                  { key: "limits", label: t("Limits") },
                  { key: "scoring", label: t("Scoring") },
                ]}
              />
            </div>
          ) : undefined
        }
        onSave={() => void save()}
        saving={saving}
        saveLabel={t("Save profile")}
        onRemove={isNew ? undefined : () => void remove()}
        removeLabel={t("Remove profile")}
        removeDisabled={profiles.length <= 1}
        error={error}
        summary={
          replacement !== null ? (
            <Alert variant="warning">
              <AlertTitle>{t("{name} is in use", { name: editedName })}</AlertTitle>
              <AlertDescription>
                <p>
                  {t("Used by: {uses}. Choose the profile they use instead.", {
                    uses: draftUses.map((u) => u.label).join(", "),
                  })}
                </p>
                <div className="mt-3 flex flex-wrap items-center gap-2">
                  <div className="w-64 max-w-full">
                    <SearchSelect
                      {...searchSelectLabels()}
                      value={replacement}
                      onChange={setReplacement}
                      options={profiles
                        .filter((p) => p.name !== editedName)
                        .map((p) => ({ value: p.name, label: p.name }))}
                      ariaLabel={t("Use instead")}
                    />
                  </div>
                  <Button
                    size="sm"
                    variant="outline"
                    className="text-danger"
                    disabled={saving || !replacement}
                    onClick={() => void remove(replacement)}
                  >
                    {t("Remove and switch")}
                  </Button>
                  <Button size="sm" variant="ghost" onClick={() => setReplacement(null)}>
                    {t("Cancel")}
                  </Button>
                </div>
              </AlertDescription>
            </Alert>
          ) : undefined
        }
      >
        {draft ? (
          drawerTab === "tiers" ? (
            <>
              <SettingsSection title={t("Profile")}>
                <TextRow
                  label={t("Name")}
                  value={draft.name}
                  error={errors.name}
                  onChange={(v) => patch({ name: v })}
                />
                {draft.import ? (
                  <SettingsRow
                    title={t("Imported from")}
                    control={<Badge variant="outline">{importSource(draft)}</Badge>}
                  />
                ) : null}
              </SettingsSection>

              {isNew ? null : (
                <SettingsSection
                  title={t("Used by")}
                  description={
                    draftUses.length
                      ? t("Plays that match one of these get this profile.")
                      : t("No rule uses this profile, so nothing plays with it yet.")
                  }
                >
                  {draftUses.length ? (
                    <ul className="divide-y divide-border">
                      {draftUses.map((u) => (
                        <li key={u.key}>
                          <button
                            type="button"
                            className="flex w-full items-center justify-between gap-3 px-5 py-3 text-left text-sm font-medium text-fg hover:bg-bg-subtle/60"
                            onClick={() => openRule(u.open)}
                          >
                            <span className="min-w-0">{u.label}</span>
                            <ChevronRight
                              aria-hidden
                              size={16}
                              className="shrink-0 text-fg-muted"
                            />
                          </button>
                        </li>
                      ))}
                    </ul>
                  ) : null}
                  <div className="px-5 py-3">
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => openRule({ create: { profile: editedName } })}
                    >
                      <Plus size={14} /> {t("Use this profile in a new rule")}
                    </Button>
                  </div>
                </SettingsSection>
              )}

              <QualityOrderSection draft={draft} patch={patch} />

              <LanguageSection draft={draft} patch={patch} />
            </>
          ) : drawerTab === "limits" ? (
            <SettingsSection
              title={t("Never play")}
              description={t(
                "Maximum limits and blocked releases always apply. Minimum resolution is a preference.",
              )}
            >
              <SettingsRow
                title={t("Resolution")}
                description={
                  errors.minHeight || errors.maxHeight ? (
                    <span className="text-danger">{errors.minHeight || errors.maxHeight}</span>
                  ) : (
                    t("Lowest preferred and highest allowed resolution.")
                  )
                }
                control={
                  <span className="flex items-center gap-2">
                    <span className="w-32">
                      <SearchSelect
                        {...searchSelectLabels()}
                        value={String(draft.minHeight)}
                        onChange={(v) => patch({ minHeight: Number(v) })}
                        options={RESOLUTIONS}
                        ariaLabel={t("Minimum resolution")}
                        searchable={false}
                      />
                    </span>
                    <span className="text-sm text-fg-muted">{t("to")}</span>
                    <span className="w-32">
                      <SearchSelect
                        {...searchSelectLabels()}
                        value={String(draft.maxHeight)}
                        onChange={(v) => patch({ maxHeight: Number(v) })}
                        options={RESOLUTIONS}
                        ariaLabel={t("Maximum resolution")}
                        searchable={false}
                      />
                    </span>
                  </span>
                }
              />
              <div className="grid gap-6 px-5 py-4 sm:grid-cols-2">
                <fieldset className="min-w-0 space-y-2">
                  <legend className="text-sm font-medium text-fg">{t("Movies")}</legend>
                  {errors.minSizeMbPerMin || errors.maxSizeMbPerMin ? (
                    <p className="text-xs text-danger">
                      {errors.minSizeMbPerMin || errors.maxSizeMbPerMin}
                    </p>
                  ) : (
                    <p className="text-xs text-fg-muted">
                      {t("Lowest and highest MB per minute for a film. 0 means no limit.")}
                    </p>
                  )}
                  <span className="flex flex-wrap items-center gap-2">
                    <NumberInput
                      aria-label={t("Minimum MB per minute")}
                      value={draft.minSizeMbPerMin}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ minSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">{t("to")}</span>
                    <NumberInput
                      aria-label={t("Maximum MB per minute")}
                      value={draft.maxSizeMbPerMin}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ maxSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">MB</span>
                  </span>
                </fieldset>
                <fieldset className="min-w-0 space-y-2">
                  <legend className="text-sm font-medium text-fg">{t("TV episodes")}</legend>
                  {errors.episodeMinSizeMbPerMin || errors.episodeMaxSizeMbPerMin ? (
                    <p className="text-xs text-danger">
                      {errors.episodeMinSizeMbPerMin || errors.episodeMaxSizeMbPerMin}
                    </p>
                  ) : (
                    <p className="text-xs text-fg-muted">
                      {t("Optional override for TV episodes. 0 uses the movie limits.")}
                    </p>
                  )}
                  <span className="flex flex-wrap items-center gap-2">
                    <NumberInput
                      aria-label={t("Episode minimum MB per minute")}
                      value={draft.episodeMinSizeMbPerMin ?? 0}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ episodeMinSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">{t("to")}</span>
                    <NumberInput
                      aria-label={t("Episode maximum MB per minute")}
                      value={draft.episodeMaxSizeMbPerMin ?? 0}
                      min={0}
                      className="w-20"
                      onChange={(n) => patch({ episodeMaxSizeMbPerMin: n })}
                    />
                    <span className="text-sm text-fg-muted">MB</span>
                  </span>
                </fieldset>
              </div>
              <BoolRow
                label={t("Allow HDR")}
                value={draft.allowHdr}
                help={t("Only used when the app supports it.")}
                onChange={(v) => patch({ allowHdr: v })}
              />
              <BoolRow
                label={t("Allow Dolby Vision")}
                value={draft.allowDolbyVision}
                onChange={(v) => patch({ allowDolbyVision: v })}
              />
              <BoolRow
                label={t("Allow CAM and screener releases")}
                value={draft.allowCam}
                onChange={(v) => patch({ allowCam: v })}
              />
              <FilterRulesEditor
                profile={draft}
                onChange={(next) => patch(next)}
                error={errors.releaseRules}
              />
            </SettingsSection>
          ) : (
            <CustomFormatsSection
              draft={draft}
              patch={patch}
              resolver={resolver}
              formatsText={formatsText}
              onFormatsText={setFormatsText}
              revealed={false}
            />
          )
        ) : null}
        {/* Inside the profile drawer, so the rule drawer stacks over it and returns here. */}
        {editing !== null ? ruleEditor : null}
      </RecordDrawer>
    </>
  );
}

import { useEffect, useState } from "react";
import { Input, confirmDialog } from "@braedonsaunders/appkit-ui";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { formatBytes, formatNumber } from "../../lib/format";
import { ApiError, apiFetch } from "../../lib/api";
import { t } from "../../i18n";
import type { DurableQuota, ListItemsGrant, RuntimeLimits } from "./types";
import { formatLimit, limitNumber, runtimeLimitFields } from "./runtime-limit-fields";

type Policy = {
  runtimeLimits?: Partial<RuntimeLimits>;
  runtimeOverrides?: Record<string, Partial<RuntimeLimits>>;
  durableStorage?: {
    diskAvailable: boolean;
    capacity: DurableQuota & { freeBytes: number; freePercent: number };
    usage?: DurableQuota & { bytes: number; files: number };
  };
};
type Draft = Record<keyof RuntimeLimits, string>;
function isIntegerLimit(key: keyof RuntimeLimits): boolean {
  return (
    key === "durableFiles" ||
    key === "durableFreePercent" ||
    key === "unitBudget" ||
    key === "poolSize" ||
    key === "archiveMembers" ||
    key === "maxItems" ||
    key === "listItems"
  );
}
export function RuntimeLimitsEditor({
  plugin = "",
  limits,
  requested,
  storedItems,
  maxItems,
  listItems,
  listsOnly = false,
  hasMediaKinds = false,
  hasUploadRoutes = false,
  hasDurableStorage = false,
  flush = false,
  onSaved,
}: {
  plugin?: string;
  limits?: RuntimeLimits;
  requested?: Partial<RuntimeLimits>;
  storedItems?: number;
  maxItems?: number;
  /** The plugin's list sync size; without it the "Titles per list" row is hidden. */
  listItems?: ListItemsGrant;
  /** A plain add-on: only its list size applies. */
  listsOnly?: boolean;
  hasMediaKinds?: boolean;
  hasUploadRoutes?: boolean;
  hasDurableStorage?: boolean;
  flush?: boolean;
  onSaved?: () => void;
}) {
  const [policy, setPolicy] = useState<Policy | null>(null);
  const [saved, setSaved] = useState<Draft | null>(null);
  const { draft, setDraft, dirty, reset } = useDraft(saved);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const allFields = runtimeLimitFields();
  const uploadField = allFields.find((f) => f.key === "uploadBytes");
  const durableFields = allFields.filter(
    (f) => f.key === "durableBytes" || f.key === "durableFiles",
  );
  const shareField = allFields.find((f) => f.key === "durableFreePercent");
  const standardFields = allFields.filter(
    (f) => f.key !== "uploadBytes" && !f.key.startsWith("durable"),
  );
  const fields = plugin
    ? [
        ...standardFields,
        ...(uploadField ? [uploadField] : []),
        ...(hasDurableStorage ? durableFields : []),
      ]
    : [...standardFields, ...(shareField ? [shareField] : [])];
  const storage = policy?.durableStorage;
  const usage = storage?.usage;
  const capacity = storage?.capacity;
  const durableLimit = (key: keyof RuntimeLimits) =>
    key === "durableBytes"
      ? capacity?.quotaBytes
      : key === "durableFiles"
        ? capacity?.quotaFiles
        : undefined;
  const aboveCeiling =
    draft &&
    durableFields.some((f) => {
      const ceiling = durableLimit(f.key);
      const proposed = Math.round(Number(draft[f.key]) * f.scale);
      return (
        ceiling !== undefined &&
        proposed > ceiling &&
        proposed > Math.round(Number(saved?.[f.key]) * f.scale)
      );
    });
  const belowUsage =
    usage &&
    durableFields.some((f) => {
      const configured = Math.round(Number(draft?.[f.key]) * f.scale) || f.fallback * f.scale;
      const proposed = Math.min(configured, durableLimit(f.key) ?? configured);
      return proposed < (f.key === "durableBytes" ? usage.bytes : usage.files);
    });
  const rows = plugin
    ? standardFields.filter((f) => (f.key === "listItems" ? Boolean(listItems) : !listsOnly))
    : standardFields;
  useEffect(() => {
    let live = true;
    setPolicy(null);
    setSaved(null);
    setError("");
    const path = hasDurableStorage
      ? `/plugins/runtime-limits?plugin=${encodeURIComponent(plugin)}`
      : "/plugins/runtime-limits";
    void apiFetch<Policy>(path)
      .then((next) => {
        if (!live) return;
        setPolicy(next);
        const configured = (plugin ? next.runtimeOverrides?.[plugin] : next.runtimeLimits) ?? {};
        setSaved(
          Object.fromEntries(
            runtimeLimitFields().map((f) => {
              const n = limitNumber(f.key, configured) / f.scale;
              return [f.key, n > 0 ? String(n) : ""];
            }),
          ) as Draft,
        );
      })
      .catch((e) => {
        if (live) setError(String(e));
      });
    return () => {
      live = false;
    };
  }, [plugin, hasDurableStorage]);
  const invalid =
    aboveCeiling ||
    (draft &&
      fields.some((f) => {
        const value = Number(draft[f.key]);
        return (
          !Number.isFinite(value) ||
          (f.key === "durableBytes" && !Number.isSafeInteger(Math.round(value * f.scale))) ||
          value < 0 ||
          (f.max !== undefined && value > f.max) ||
          (isIntegerLimit(f.key) && !Number.isSafeInteger(value))
        );
      }));
  const input = (f: (typeof fields)[number]) => {
    const ceiling = durableLimit(f.key);
    let global = limitNumber(f.key, policy?.runtimeLimits) / f.scale || f.fallback;
    // A list's default is what the plugin asks for, within the global limit.
    if (plugin && f.key === "listItems" && listItems)
      global = Math.min(listItems.requested, global);
    return (
      <Input
        aria-label={plugin ? t("Override: {limit}", { limit: f.label }) : f.label}
        type="number"
        min={0}
        max={
          f.max ??
          (ceiling !== undefined
            ? Math.max(ceiling / f.scale, Number(saved?.[f.key] ?? 0))
            : undefined)
        }
        step={isIntegerLimit(f.key) ? 1 : "any"}
        disabled={!draft || busy}
        value={draft?.[f.key] ?? ""}
        placeholder={t("{value} (default)", {
          value: formatLimit(f.key, plugin ? global : f.fallback),
        })}
        onChange={(e) => setDraft((old) => old && { ...old, [f.key]: e.target.value })}
      />
    );
  };
  async function save() {
    if (!draft || invalid) return;
    setBusy(true);
    setError("");
    try {
      if (fields.some((f) => !f.key.startsWith("durable") && draft[f.key] !== saved?.[f.key])) {
        // Read only when saving: idle settings pages do not poll job APIs.
        const ids = plugin
          ? [plugin]
          : ((await apiFetch<{ plugins?: { id: string }[] }>("/plugins")).plugins?.map(
              (p) => p.id,
            ) ?? []);
        const jobs = await Promise.all(
          ids.map((id) => apiFetch<{ state: string }[]>(`/plugins/${encodeURIComponent(id)}/jobs`)),
        );
        if (
          jobs.some((list) => list.some((job) => job.state === "running")) &&
          !(await confirmDialog({
            title: t("Save changes"),
            message: t("Saving stops current jobs."),
            confirmLabel: t("Save changes"),
          }))
        )
          return;
      }
      const next = Object.fromEntries(
        fields.map((f) => [
          f.key,
          f.key === "deadline"
            ? Number(draft[f.key]) > 0
              ? `${Number(draft[f.key])}h`
              : ""
            : Math.round(Number(draft[f.key]) * f.scale),
        ]),
      );
      const updated = await apiFetch<Policy>("/plugins/runtime-limits", {
        method: "PUT",
        body: JSON.stringify({ plugin, limits: next }),
      });
      setPolicy(updated);
      setSaved({ ...draft });
      onSaved?.();
    } catch (e) {
      setError(
        e instanceof ApiError && e.code === "durable_quota_ceiling"
          ? t("The quota exceeds the current disk limit.")
          : String(e),
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <div
        className={
          flush ? "space-y-4 px-5 py-4" : "space-y-4 rounded-xl border border-border bg-surface p-4"
        }
      >
        {plugin ? (
          <>
            <h3 className="text-sm font-medium">{t("Limits")}</h3>
            <p className="text-xs text-fg-muted">
              {t(
                "Leave overrides empty to use the global setting. Browsing and playback calls always stay fast.",
              )}
            </p>
            {hasUploadRoutes && uploadField ? (
              <label className="block max-w-sm space-y-1 text-xs">
                <span className="font-medium">{uploadField.label}</span>
                {input(uploadField)}
                <span className="block text-fg-muted">
                  {t("Up to {max} per upload. Routes can set a smaller limit.", {
                    max: formatLimit("uploadBytes", uploadField.max ?? 512),
                  })}
                </span>
              </label>
            ) : null}
            {hasDurableStorage ? (
              <section className="max-w-lg space-y-2">
                <h4 className="text-sm font-medium">{t("Durable storage")}</h4>
                <div className="grid gap-3 sm:grid-cols-2">
                  {durableFields.map((f) => (
                    <label key={f.key} className="block space-y-1 text-xs">
                      <span className="font-medium">{f.label}</span>
                      {input(f)}
                    </label>
                  ))}
                </div>
                {usage ? (
                  <p className="text-xs text-fg-muted">
                    {t("{used} of {size} used · {count} of {files} files", {
                      used: formatBytes(usage.bytes),
                      size: formatBytes(usage.quotaBytes),
                      count: formatNumber(usage.files),
                      files: formatNumber(usage.quotaFiles),
                    })}
                  </p>
                ) : null}
                {capacity ? (
                  <p className="text-xs text-fg-muted">
                    {storage.diskAvailable
                      ? t("{free} free on this drive. Maximum quota: {size} and {files} files.", {
                          free: formatBytes(capacity.freeBytes),
                          size: formatBytes(capacity.quotaBytes),
                          files: formatNumber(capacity.quotaFiles),
                        })
                      : t("Free disk space is unavailable.")}
                  </p>
                ) : null}
                {aboveCeiling ? (
                  <p className="text-xs" role="status">
                    {t("The quota exceeds the current disk limit.")}
                  </p>
                ) : null}
                {belowUsage ? (
                  <p className="text-xs" role="status">
                    {t(
                      "Saved data is kept. New writes are refused until there is room within both limits.",
                    )}
                  </p>
                ) : null}
              </section>
            ) : null}
            <div className="overflow-x-auto" hidden={rows.length === 0}>
              <table className="w-full min-w-[600px] text-xs">
                <thead>
                  <tr className="text-left text-fg-muted">
                    <th className="pb-2" scope="col">
                      {t("Limit")}
                    </th>
                    <th className="pb-2 pr-3" scope="col">
                      {t("Asked for")}
                    </th>
                    <th className="pb-2 pr-3" scope="col">
                      {t("Allowed")}
                    </th>
                    <th className="w-44 pb-2" scope="col">
                      {t("Override")}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((f) => {
                    const list = f.key === "listItems" ? listItems : undefined;
                    const asked = list ? list.requested : limitNumber(f.key, requested) / f.scale;
                    const allowed = list
                      ? list.granted
                      : f.key === "maxItems" && !limits && maxItems
                        ? maxItems / f.scale
                        : limitNumber(f.key, limits) / f.scale;
                    const hasAllowed = Boolean(
                      list || limits || (f.key === "maxItems" && maxItems),
                    );
                    const global =
                      limitNumber(f.key, policy?.runtimeLimits) / f.scale || f.fallback;
                    return (
                      <tr key={f.key} className="border-t border-border">
                        <th scope="row" className="py-3 pr-3 text-left font-medium">
                          {f.label}
                        </th>
                        <td className="py-3 pr-3">
                          {asked > 0 ? formatLimit(f.key, asked) : t("Default")}
                        </td>
                        <td className="py-3 pr-3">
                          {hasAllowed ? formatLimit(f.key, allowed) : t("Not available")}
                          {f.key === "maxItems" &&
                          allowed > 0 &&
                          (hasMediaKinds || (storedItems ?? 0) > 0) ? (
                            <p className="mt-1 max-w-44 text-fg-muted">
                              {t("{used} stored (limit {limit})", {
                                used: formatLimit("maxItems", storedItems ?? 0),
                                limit: formatLimit("maxItems", allowed),
                              })}
                            </p>
                          ) : null}
                          {list ? (
                            <p className="mt-1 max-w-44 text-fg-muted">
                              {t("Each list syncs up to this many titles.")}
                            </p>
                          ) : null}
                          {limits && !list && !asked && allowed > 0 && allowed < global ? (
                            <p className="mt-1 max-w-44 text-fg-muted">
                              {t("The plugin didn't ask for more.")}
                            </p>
                          ) : null}
                        </td>
                        <td className="py-2">{input(f)}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </>
        ) : (
          <>
            <section className="space-y-3">
              <h3 className="text-sm font-medium">{t("Running plugins")}</h3>
              <p className="text-xs text-fg-muted">
                {t(
                  "Memory, time and work per background job. Browsing and playback calls always stay fast.",
                )}
              </p>
              <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
                {fields.slice(0, 4).map((f) => (
                  <label key={f.key} className="space-y-1 text-xs">
                    {f.label}
                    {input(f)}
                  </label>
                ))}
              </div>
            </section>
            <section className="space-y-3 border-t border-border pt-4">
              <h3 className="text-sm font-medium">{t("Downloads and archives")}</h3>
              <p className="text-xs text-fg-muted">
                {t(
                  "Size and file counts per download or archive. Browsing and playback calls always stay fast. Clear a field to restore its default.",
                )}
              </p>
              <div className="grid gap-3 sm:grid-cols-2">
                {fields.slice(4, 8).map((f) => (
                  <label key={f.key} className="space-y-1 text-xs">
                    {f.label}
                    {input(f)}
                  </label>
                ))}
              </div>
            </section>
            <section className="space-y-3 border-t border-border pt-4">
              <h3 className="text-sm font-medium">{t("Plugin libraries")}</h3>
              <p className="text-xs text-fg-muted">
                {t(
                  "Total items a plugin can store across its libraries. Clear the field to restore its default.",
                )}
              </p>
              <div className="grid gap-3 sm:grid-cols-2">
                {fields.slice(8, 9).map((f) => (
                  <label key={f.key} className="space-y-1 text-xs">
                    {f.label}
                    {input(f)}
                  </label>
                ))}
              </div>
            </section>
            <section className="space-y-3 border-t border-border pt-4">
              <h3 className="text-sm font-medium">{t("Plugin lists")}</h3>
              <p className="text-xs text-fg-muted">
                {t(
                  "The most titles each plugin list syncs. Plugins show the size they ask for when you install them. Clear the field to restore the default.",
                )}
              </p>
              <div className="grid gap-3 sm:grid-cols-2">
                {standardFields.slice(9).map((f) => (
                  <label key={f.key} className="space-y-1 text-xs">
                    {f.label}
                    {input(f)}
                  </label>
                ))}
              </div>
            </section>
            {shareField ? (
              <section className="space-y-3 border-t border-border pt-4">
                <h3 className="text-sm font-medium">{t("Durable storage")}</h3>
                <p className="text-xs text-fg-muted">
                  {t(
                    "Sets the share of free disk space used to cap each plugin’s saved data and file limits.",
                  )}
                </p>
                <label className="block max-w-sm space-y-1 text-xs">
                  <span>{shareField.label}</span>
                  {input(shareField)}
                </label>
              </section>
            ) : null}
          </>
        )}
      </div>
      <SaveBar
        dirty={dirty}
        saving={busy}
        disabled={!draft || !!invalid}
        onSave={save}
        onDiscard={() => {
          reset();
          setError("");
        }}
        error={error || undefined}
      />
    </>
  );
}

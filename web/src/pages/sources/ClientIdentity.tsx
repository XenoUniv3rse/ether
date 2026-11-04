import { SearchSelect, SettingsSection, Spinner, toast } from "@braedonsaunders/appkit-ui";
import { useMemo, useState } from "react";
import { MoreOptions } from "../../components/ConfigSection";
import { ErrorPanel } from "../../components/feedback";
import { MapInput } from "../../components/fields";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { t, tn } from "../../i18n";
import { api, ApiError } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import type { ClientIdentityConfig, Indexer } from "../../lib/types";
import { identityOptions, searchIdentityOptions } from "../settings/schema";
import { TextRow } from "./form";

type IdentityKey = "tvSearch" | "movieSearch" | "tvGrab" | "movieGrab";

interface Cell {
  key: IdentityKey;
  label: string;
  media: string;
  options: { value: string; label: string }[];
}

/**
 * The owner's client-identity policy: one selector per purpose and media, the
 * user agent each selector really sends, and how many indexers inherit it.
 */
export function ClientIdentity({
  indexers,
  onSaved,
}: {
  /** The page's live indexer list, so the inherit count follows edits. */
  indexers: Indexer[];
  onSaved?: () => void;
}) {
  const section = useAsync(() => api.getSection<ClientIdentityConfig | null>("clientIdentity"), []);
  const saved = useMemo(() => section.data?.value ?? {}, [section.data]);
  const { draft, setDraft, dirty, reset } = useDraft<ClientIdentityConfig>(saved);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});

  if (section.error) {
    return <ErrorPanel error={section.error} onRetry={() => void section.reload()} />;
  }
  if (!section.data || !draft) {
    return (
      <div className="py-10 text-center">
        <Spinner size={20} label={t("Loading settings")} />
      </div>
    );
  }

  const policy = draft;
  const presets = section.data.presets ?? [];
  const presetUa = new Map(presets.map((p) => [p.key, p.userAgent]));
  const etherUa = presetUa.get("ether") ?? "";
  const identityUa = (key: IdentityKey) => {
    // Resolve trims and lowercases, so the display does too.
    const name = String(policy[key] ?? "")
      .trim()
      .toLowerCase();
    if (name === "custom") return String(policy.userAgent ?? "").trim() || etherUa;
    return presetUa.get(name) ?? etherUa;
  };

  const searchOptions = [{ value: "", label: "Ether" }, ...searchIdentityOptions(true, false)];
  const grabOptions = [{ value: "", label: "Ether" }, ...identityOptions(true, false)];
  const groups: { title: string; cells: Cell[] }[] = [
    {
      title: t("Searches"),
      cells: [
        { key: "tvSearch", label: t("TV searches"), media: t("TV"), options: searchOptions },
        {
          key: "movieSearch",
          label: t("Movie searches"),
          media: t("Movies"),
          options: searchOptions,
        },
      ],
    },
    {
      title: t("Downloads"),
      cells: [
        { key: "tvGrab", label: t("TV downloads"), media: t("TV"), options: grabOptions },
        { key: "movieGrab", label: t("Movie downloads"), media: t("Movies"), options: grabOptions },
      ],
    },
  ];

  // Only the Newznab/Torznab client resolves this policy; built-in sites and
  // Stremio addons have their own request paths.
  const capable = indexers.filter((ix) => !ix.kind || ix.kind === "newznab");
  const inherited = capable.filter((ix) => !ix.clientIdentity).length;
  const overrides = capable.length - inherited;
  const backend = indexers.length - capable.length;

  const patch = (part: Partial<ClientIdentityConfig>) => setDraft({ ...policy, ...part });
  const save = async () => {
    setSaving(true);
    setError(null);
    setErrors({});
    try {
      await api.putSection("clientIdentity", {
        tvSearch: policy.tvSearch ?? "",
        movieSearch: policy.movieSearch ?? "",
        tvGrab: policy.tvGrab ?? "",
        movieGrab: policy.movieGrab ?? "",
        userAgent: policy.userAgent ?? "",
        headers: policy.headers ?? {},
      });
      toast.success(t("Settings saved"), { description: t("Client identity") });
      await section.reload();
      onSaved?.();
    } catch (e) {
      setError(e);
      if (e instanceof ApiError && e.fields) {
        setErrors(
          Object.fromEntries(
            e.fields.map((f) => [f.field.replace(/^clientIdentity\./, ""), f.message]),
          ),
        );
      }
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <SettingsSection
        title={t("Client identity")}
        description={t(
          "What Newznab and Torznab indexers see when Ether searches or downloads. Defaults to Ether; choose a real client for sites that treat unknown clients badly. Indexers set to Inherit use this policy; an indexer's own identity wins.",
        )}
      >
        <div className="space-y-5 px-5 py-4">
          {groups.map((group) => (
            <div key={group.title}>
              <h3 className="text-xs font-semibold tracking-wider text-fg-subtle uppercase">
                {group.title}
              </h3>
              <div className="mt-2 grid gap-4 sm:grid-cols-2">
                {group.cells.map((cell) => {
                  const ua = identityUa(cell.key);
                  return (
                    <div key={cell.key} className="min-w-0">
                      <div className="text-sm font-medium text-fg">{cell.media}</div>
                      <div className="mt-1.5">
                        <SearchSelect
                          value={String(policy[cell.key] ?? "")}
                          onChange={(v) => patch({ [cell.key]: v })}
                          options={cell.options}
                          ariaLabel={cell.label}
                          searchable={cell.options.length > 7}
                          placeholder={t("Select…")}
                          searchPlaceholder={t("Search…")}
                          noMatchesLabel={t("No matches")}
                          selectLabel={t("Select")}
                          closeLabel={t("Close")}
                          invalid={Boolean(errors[cell.key])}
                        />
                      </div>
                      <p className="mt-1 truncate font-mono text-xs text-fg-muted" title={ua}>
                        {ua || "—"}
                      </p>
                      {errors[cell.key] ? (
                        <p role="alert" className="mt-0.5 text-xs text-danger">
                          {errors[cell.key]}
                        </p>
                      ) : null}
                    </div>
                  );
                })}
              </div>
            </div>
          ))}
        </div>

        {capable.length > 0 ? (
          <div className="px-5 py-3">
            <p className="text-sm text-fg-muted">
              {overrides === 0
                ? tn(
                    capable.length,
                    "{count} indexer inherits this policy",
                    "All {count} indexers inherit this policy",
                  )
                : tn(
                    overrides,
                    "{inherited} of {total} indexers inherit this policy; {count} has its own identity",
                    "{inherited} of {total} indexers inherit this policy; {count} have their own identity",
                    { inherited, total: capable.length },
                  )}
            </p>
            {backend > 0 ? (
              <p className="mt-0.5 text-xs text-fg-subtle">
                {t("Built-in sites and Stremio addons use their own identity.")}
              </p>
            ) : null}
          </div>
        ) : null}

        <MoreOptions label={t("More options: {title}", { title: t("Client identity") })}>
          <TextRow
            label={t("Custom user agent")}
            value={String(policy.userAgent ?? "")}
            onChange={(v) => patch({ userAgent: v })}
            placeholder={etherUa}
            help={t("Sent when an identity above is Custom.")}
            error={errors.userAgent}
          />
          <SettingsRow
            stacked
            title={t("Custom headers")}
            description={
              <>
                {t(
                  "Sent to every indexer whose identity is Custom, so don't put one site's login here. Values are sealed at rest and shown as •••• after saving.",
                )}
                {errors.headers ? (
                  <span className="mt-1 block text-xs text-danger" role="alert">
                    {errors.headers}
                  </span>
                ) : null}
              </>
            }
          >
            <MapInput
              ariaLabel={t("Custom headers")}
              value={(policy.headers as Record<string, string> | null) ?? {}}
              onChange={(v) => patch({ headers: v })}
              namePlaceholder={t("Header name")}
              valuePlaceholder={t("Value")}
              invalid={Boolean(errors.headers)}
            />
          </SettingsRow>
        </MoreOptions>
      </SettingsSection>
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setErrors({});
          setError(null);
        }}
      />
    </>
  );
}

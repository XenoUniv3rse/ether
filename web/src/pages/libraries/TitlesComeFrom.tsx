import { sourceName, sourceLabel, tvdbSourceError } from "./CatalogueSources";
import {
  Badge,
  Button,
  Checkbox,
  SettingsSection,
  buttonVariants,
} from "@braedonsaunders/appkit-ui";
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { SettingsRow } from "../../components/SettingsRow";
import { ErrorPanel } from "../../components/feedback";
import { t } from "../../i18n";
import { useAsync } from "../../lib/hooks";
import { librariesApi, type Library } from "./api";
import { SourceIcon, catalogueCapNote, catalogueCountLabel, sourceOwner } from "./CatalogueSources";
export function TitlesComeFrom({
  library,
  sources,
  onChange,
  children,
}: {
  library: Library;
  sources: string[];
  onChange: (sources: string[]) => void;
  children?: ReactNode;
}) {
  const registry = useAsync((signal) => librariesApi.catalogueSources(signal), []);
  const [draft, setDraft] = useState<string[] | null>(null);
  const busy = false;
  const ids = sources;
  const attached = registry.data?.filter((source) => ids.includes(source.id)) ?? [];
  const failedAttached = attached.filter((source) => Boolean(source.lastError));
  // A list cut off at its "Titles per list" limit is never silent.
  const cappedAttached = attached.filter(
    (source) => !source.lastError && Boolean(catalogueCapNote(source)),
  );
  const plugin = ids.length === 1 ? attached[0] : undefined;
  const manageHref = plugin?.pluginId
    ? `/plugins/installed?plugin=${encodeURIComponent(plugin.pluginId)}&tab=setup`
    : ids.length > 0 && ids.every((id) => id === "tmdb-movies" || id === "tmdb-tv")
      ? "/services/tmdb"
      : "/services/overview";
  const manageLabel = plugin?.pluginId
    ? t("Manage {app}", { app: plugin.owner })
    : t("Manage in Integrations");
  const mediaType = String(library.mediaType ?? "movie");
  const save = () => {
    if (draft) {
      onChange(draft);
      setDraft(null);
    }
  };
  if (library.source === "collection")
    return <SettingsSection title={t("Titles come from")}>{children}</SettingsSection>;
  return (
    <>
      <SettingsSection title={t("Titles come from")}>
        <SettingsRow
          title={t("Catalogue sources")}
          description={
            library.source === "builtin"
              ? attached[0]
                ? t("Always {source}.", { source: sourceLabel(attached[0]) })
                : undefined
              : library.source === "plugin"
                ? t("Comes from the {plugin} plugin.", {
                    plugin: attached[0]?.owner ?? library.pluginName ?? t("Plugin"),
                  })
                : t("Choose sources for this library.")
          }
          control={
            <div className="flex flex-wrap items-center justify-end gap-2">
              {ids.map((id) => {
                const source = registry.data?.find((item) => item.id === id);
                return (
                  <Badge key={id} variant={source?.lastError ? "warning" : "secondary"}>
                    {source ? sourceLabel(source) : t("Unavailable")}
                  </Badge>
                );
              })}
              {library.source !== "builtin" && library.source !== "plugin" ? (
                <Button size="sm" variant="outline" onClick={() => setDraft([...ids])}>
                  {t("Change")}
                </Button>
              ) : null}
              <Link className={buttonVariants({ variant: "outline", size: "sm" })} to={manageHref}>
                {manageLabel}
              </Link>
            </div>
          }
        />
        {failedAttached.length ? (
          <div className="space-y-1 px-5 pb-4 text-sm text-danger">
            {failedAttached.map((source) => (
              <p key={source.id}>
                {sourceLabel(source)}:{" "}
                {source.id === "tvdb" ? tvdbSourceError(source.lastError) : source.lastError}
              </p>
            ))}
          </div>
        ) : null}
        {cappedAttached.length ? (
          <div
            className="space-y-1 px-5 pb-4 text-sm text-fg-muted"
            data-testid="catalogue-cap-notes"
          >
            {cappedAttached.map((source) => (
              <p key={source.id}>
                {sourceLabel(source)}: {catalogueCountLabel(source)}. {catalogueCapNote(source)}{" "}
                {source.pluginId ? (
                  <Link
                    className="text-primary underline-offset-2 hover:underline"
                    to={`/plugins/installed?plugin=${encodeURIComponent(source.pluginId)}&tab=setup`}
                  >
                    {t("Open {plugin}", { plugin: source.owner })}
                  </Link>
                ) : null}
              </p>
            ))}
          </div>
        ) : null}
        {children}
      </SettingsSection>
      {draft ? (
        <Drawer
          open
          onClose={() => setDraft(null)}
          title={t("Titles come from")}
          footer={
            <div className="flex justify-end gap-2">
              <Button variant="ghost" onClick={() => setDraft(null)}>
                {t("Cancel")}
              </Button>
              <Button disabled={busy || !draft.length} onClick={() => void save()}>
                {t("Apply")}
              </Button>
            </div>
          }
        >
          <div className="space-y-3">
            {registry.error ? (
              <ErrorPanel error={registry.error} onRetry={() => void registry.reload()} />
            ) : null}
            {registry.data
              ?.filter(
                (source) => source.group !== "plugin" && source.mediaTypes.includes(mediaType),
              )
              .map((source) => (
                <label
                  key={source.id}
                  className="flex cursor-pointer items-center gap-3 rounded-xl border border-border p-4 hover:bg-surface-hover"
                >
                  <SourceIcon source={source} />
                  <Checkbox
                    disabled={busy}
                    checked={draft.includes(source.id)}
                    onChange={(e) =>
                      setDraft(
                        e.target.checked
                          ? [...draft, source.id]
                          : draft.filter((id) => id !== source.id),
                      )
                    }
                  />
                  <span>
                    <span className="block font-medium">{sourceName(source)}</span>
                    <span className="block text-sm text-fg-muted">{sourceOwner(source)}</span>
                    {source.lastError ? (
                      <span className="block text-sm text-danger">
                        {source.id === "tvdb"
                          ? tvdbSourceError(source.lastError)
                          : source.lastError}
                      </span>
                    ) : null}
                  </span>
                </label>
              ))}
            {registry.data
              ? draft
                  .filter((id) => !registry.data?.some((source) => source.id === id))
                  .map((id) => (
                    <label
                      key={id}
                      className="flex items-center gap-3 rounded-xl border border-border p-4"
                    >
                      <Checkbox
                        checked
                        disabled={busy}
                        onChange={() => setDraft(draft.filter((value) => value !== id))}
                      />
                      {t("Unavailable")}
                    </label>
                  ))
              : null}
          </div>
        </Drawer>
      ) : null}
    </>
  );
}

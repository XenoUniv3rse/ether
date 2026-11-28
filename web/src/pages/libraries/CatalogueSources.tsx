import { Film, ListFilter, Tv, Plug, Link2 } from "lucide-react";
import type { CatalogueSource, Library } from "./api";
import { useState } from "react";
import { Link } from "react-router-dom";
import { AddAddonDrawer } from "../../components/addons/AddAddonDrawer";
import { Drawer } from "../../components/Drawer";
import { getLocale, t, tn } from "../../i18n";

export function AddCatalogueSource({
  onClose,
  onAdded,
}: {
  onClose: () => void;
  onAdded: () => void;
}) {
  const [addon, setAddon] = useState(false);
  if (addon)
    return (
      <AddAddonDrawer
        open
        role="list_source"
        title={t("Add a Stremio add-on")}
        submitLabel={t("Add add-on")}
        onClose={onClose}
        onAdded={() => {
          onAdded();
          onClose();
        }}
      />
    );
  return (
    <Drawer open onClose={onClose} title={t("Add catalogue source")}>
      <div className="flex flex-col gap-3">
        <button
          type="button"
          className="flex items-center gap-4 rounded-xl border border-border p-4 text-left hover:bg-surface-hover"
          onClick={() => setAddon(true)}
        >
          <Link2 size={22} />
          <span>
            <span className="block font-medium">{t("Stremio add-on")}</span>
            <span className="block text-sm text-fg-muted">
              {t("Paste a manifest link to add its catalogues.")}
            </span>
          </span>
        </button>
        <Link
          className="flex items-center gap-4 rounded-xl border border-border p-4 text-left hover:bg-surface-hover"
          to="/plugins/marketplace"
          onClick={onClose}
        >
          <Plug size={22} />
          <span>
            <span className="block font-medium">{t("Plugin")}</span>
            <span className="block text-sm text-fg-muted">
              {t("Install from the plugin directory or build one in Studio.")}
            </span>
          </span>
        </Link>
      </div>
    </Drawer>
  );
}
export function SourceIcon({ source, size = 18 }: { source: CatalogueSource; size?: number }) {
  if (source.logo) return <SourceLogo url={source.logo} size={size} />;
  const Icon = source.group === "plugin" ? Plug : source.mediaTypes[0] === "show" ? Tv : Film;
  return <Icon size={18} aria-hidden />;
}

export function LibrarySourceIcon({ library, size = 18 }: { library: Library; size?: number }) {
  if (library.logo) return <SourceLogo url={library.logo} size={size} />;
  const Icon =
    library.source === "plugin"
      ? Plug
      : library.source === "catalogue"
        ? ListFilter
        : library.mediaType === "show"
          ? Tv
          : Film;
  return <Icon size={size} aria-hidden />;
}

function SourceLogo({ url, size }: { url: string; size: number }) {
  return <img src={url} width={size} height={size} alt="" className="shrink-0 object-contain" />;
}
export function sourceOwner(source: CatalogueSource) {
  return source.group === "builtin"
    ? t("Included with Ether")
    : t("From {plugin}", { plugin: source.owner });
}

export function sourceName(source: CatalogueSource) {
  if (source.group !== "stremio") return source.name;
  return source.mediaTypes[0] === "show"
    ? t("{name} TV shows", { name: source.name })
    : t("{name} movies", { name: source.name });
}
export function sourceLabel(source: CatalogueSource) {
  const name = sourceName(source);
  return source.group === "builtin"
    ? name
    : source.group === "plugin" && source.owner.toLowerCase().includes(name.toLowerCase())
      ? source.owner
      : `${source.owner} · ${name}`;
}

export function catalogueGroups(sources: CatalogueSource[]) {
  const groups = new Map<string, CatalogueSource[]>();
  for (const source of sources) {
    const key = source.pluginId ?? source.id;
    groups.set(key, [...(groups.get(key) ?? []), source]);
  }
  return [...groups.values()].map((entries) => {
    const first = entries[0]!;
    const failed = entries.filter(
      (s) => s.health === "error" || s.health === "attention" || Boolean(s.lastError),
    );
    const perListError =
      entries.length > 1 && failed.length > 0
        ? failed.map((s) => `${sourceName(s)}: ${s.lastError || t("Needs attention")}`).join("; ")
        : failed.find((s) => s.lastError)?.lastError;
    return {
      ...first,
      logo: entries.find((entry) => entry.logo)?.logo,
      id: first.pluginId ?? first.id,
      name: first.owner,
      entries,
      health:
        failed.length > 0
          ? failed.length < entries.length
            ? "attention"
            : "error"
          : entries.every((s) => s.health === "unknown")
            ? "unknown"
            : "ok",
      lastError: perListError,
      libraries: [
        ...new Map(entries.flatMap((s) => s.libraries).map((lib) => [lib.id, lib])).values(),
      ],
    };
  });
}

type SyncCounts = { itemCount?: number; listed?: number; limit?: number };

const formatCount = (n: number) => new Intl.NumberFormat(getLocale()).format(n);

/** The last sync stopped at the plugin's "Titles per list" limit. */
export function catalogueCapped(source: SyncCounts): boolean {
  return Boolean(source.limit && (source.listed ?? 0) >= source.limit);
}

/** "248 of 2,000 titles synced", or "248 titles" when every listed title synced. */
export function catalogueCountLabel(source: SyncCounts): string {
  const count = source.itemCount ?? 0;
  const listed = source.listed ?? 0;
  return listed > count
    ? t("{count} of {listed} titles synced", {
        count: formatCount(count),
        listed: formatCount(listed),
      })
    : tn(count, "{count} title", "{count} titles");
}

/** Says a sync was cut off at its limit and where to raise it; empty otherwise. */
export function catalogueCapNote(source: SyncCounts & { id?: string }): string {
  if (source.id === "tvdb") {
    return catalogueCapped(source)
      ? t("TheTVDB lists the first {limit} shows.", { limit: formatCount(source.limit ?? 0) })
      : "";
  }
  return catalogueCapped(source)
    ? t(
        "Only the first {limit} titles in this list sync. Raise Titles per list in the plugin's limits to sync more.",
        {
          limit: formatCount(source.limit ?? 0),
        },
      )
    : "";
}

export function tvdbSourceError(message?: string) {
  if (message === "Enter a TheTVDB API key.") return t("Enter a TheTVDB API key.");
  if (message === "TheTVDB rejected the API key.") return t("TheTVDB rejected the API key.");
  return t("Could not load TheTVDB.");
}

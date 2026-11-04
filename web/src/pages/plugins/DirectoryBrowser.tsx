import { Badge, Button, Input, cn } from "@braedonsaunders/appkit-ui";
import { Search } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { Drawer } from "../../components/Drawer";
import { t } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { apiFetch, errorMessage } from "../../lib/api";
import { textOf, useRegistry } from "../../lib/extend";

type Entry = {
  id: string;
  name: string;
  description?: string;
  version?: string;
  manifestURL?: string;
  unsupported?: string;
};

/**
 * Add-ons can publish lists of other add-ons (Cinemeta publishes Stremio's
 * official and community catalogues). Pick a list, see what is in it, and
 * review any entry before it installs.
 */
export function DirectoryBrowser({
  onClose,
  onImport,
}: {
  onClose: () => void;
  onImport: (url: string) => void;
}) {
  const registry = useRegistry<{ key: string }>("pluginDirectory");
  const lists = registry.descriptors;
  const { locale } = useLocale();
  const [source, setSource] = useState("");
  const [entries, setEntries] = useState<Entry[] | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [filter, setFilter] = useState("");
  const generation = useRef(0);

  // Open the broadest list first ("· all"), so there is something to see.
  useEffect(() => {
    if (source || !lists.length) return;
    const all = lists.find((d) => /·\s*all$/i.test(textOf(d.label))) ?? lists[0]!;
    setSource(all.id);
  }, [source, lists]);

  const selected = lists.find((d) => d.id === source);
  useEffect(() => {
    if (!selected?.meta?.key) return;
    const current = ++generation.current;
    setBusy(true);
    setError("");
    setEntries(null);
    apiFetch<{ entries: Entry[] }>(
      `/plugins/${encodeURIComponent(selected.owner)}/directories/${encodeURIComponent(selected.meta.key)}`,
    )
      .then((r) => current === generation.current && setEntries(r.entries))
      .catch((e) => current === generation.current && setError(errorMessage(e)))
      .finally(() => current === generation.current && setBusy(false));
  }, [selected]);

  // Group "Official · movie" style labels by their first part.
  const groups = useMemo(() => {
    const out = new Map<string, { id: string; label: string }[]>();
    for (const d of lists) {
      const full = textOf(d.label);
      const [group, rest] = full.includes("·") ? full.split("·").map((s) => s.trim()) : ["", full];
      const key = `${d.group || d.owner}${group ? " · " + group : ""}`;
      out.set(key, [...(out.get(key) ?? []), { id: d.id, label: rest || full }]);
    }
    return [...out.entries()];
  }, [lists, locale]);

  const shown = (entries ?? []).filter((e) =>
    `${e.name} ${e.description ?? ""}`.toLowerCase().includes(filter.trim().toLowerCase()),
  );

  return (
    <Drawer open onClose={onClose} title={t("Browse plugin directories")} size="lg">
      <div className="space-y-5">
        <p className="text-sm text-fg-muted">
          {t(
            "Some installed plugins publish a list of other plugins, such as Cinemeta's community catalogue. Pick a list to see what is in it, then review any plugin before installing.",
          )}
        </p>
        {!registry.loading && !lists.length ? (
          <p className="rounded-xl border border-dashed border-border p-4 text-sm text-fg-muted">
            {t(
              "No installed plugin publishes a directory yet. Cinemeta does; install it from a link to browse.",
            )}
          </p>
        ) : null}
        {groups.map(([group, items]) => (
          <div key={group} className="space-y-2">
            <p className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">{group}</p>
            <div className="flex flex-wrap gap-1.5">
              {items.map((it) => (
                <button
                  key={it.id}
                  type="button"
                  onClick={() => setSource(it.id)}
                  aria-pressed={source === it.id}
                  className={cn(
                    "rounded-full border px-3 py-1 text-sm capitalize transition",
                    source === it.id
                      ? "border-primary/50 bg-primary/10 text-fg"
                      : "border-border text-fg-muted hover:text-fg",
                  )}
                >
                  {it.label}
                </button>
              ))}
            </div>
          </div>
        ))}
        {entries?.length ? (
          <div className="relative">
            <Search
              size={14}
              className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
              aria-hidden
            />
            <Input
              aria-label={t("Filter plugins")}
              placeholder={t("Filter {count} plugins", { count: entries.length })}
              value={filter}
              onChange={(e) => setFilter(e.currentTarget.value)}
              className="pl-8"
            />
          </div>
        ) : null}
        {busy ? <p className="text-sm text-fg-muted">{t("Loading…")}</p> : null}
        {error || registry.error ? (
          <p role="alert" className="text-sm text-danger">
            {error || registry.error?.message}
          </p>
        ) : null}
        {entries?.length === 0 ? (
          <p className="text-sm text-fg-muted">{t("This list is empty.")}</p>
        ) : null}
        <ul className="grid gap-3 sm:grid-cols-2">
          {shown.map((e, i) => (
            <li
              key={`${e.id}:${i}`}
              className="flex flex-col rounded-xl border border-border bg-surface p-4"
            >
              <div className="flex items-start justify-between gap-2">
                <h3 className="font-medium text-fg">{e.name || e.id}</h3>
                {e.version ? <Badge variant="secondary">v{e.version}</Badge> : null}
              </div>
              <p className="mt-1.5 line-clamp-3 text-sm text-fg-muted">{e.description}</p>
              <div className="mt-auto pt-3">
                {e.unsupported ? (
                  <p className="text-xs text-warning">{e.unsupported}</p>
                ) : (
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={!e.manifestURL}
                    onClick={() => onImport(e.manifestURL!)}
                  >
                    {t("Review and install")}
                  </Button>
                )}
              </div>
            </li>
          ))}
        </ul>
      </div>
    </Drawer>
  );
}

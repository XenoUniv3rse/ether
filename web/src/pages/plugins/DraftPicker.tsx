import { Badge, Button, Popover, cn } from "@braedonsaunders/appkit-ui";
import { ChevronDown } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import type { StudioProjectSummary } from "./develop/types";

interface InstalledPlugin {
  id: string;
  name: string;
  enabled: boolean;
  stremioOnly?: boolean;
}

type Row = {
  key: string;
  name: string;
  /** The draft to open, or the installed plugin to open as a draft. */
  projectId?: string;
  pluginId: string;
  state: "draft" | "installed" | "disabled";
  addon: boolean;
  updated: number;
};

/**
 * What the owner can edit right now, once each: their drafts, and every
 * installed plugin (Stremio add-ons included), newest first.
 */
export function DraftPicker({
  projects,
  currentId,
  disabled,
  onPickProject,
  onPickPlugin,
}: {
  projects: (StudioProjectSummary & { addonOf?: string; installed?: boolean })[];
  currentId?: string;
  disabled?: boolean;
  onPickProject: (projectId: string) => void;
  onPickPlugin: (pluginId: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [plugins, setPlugins] = useState<InstalledPlugin[]>([]);
  useEffect(() => {
    let live = true;
    apiFetch<{ plugins: InstalledPlugin[] }>("/plugins")
      .then((r) => live && setPlugins(r.plugins ?? []))
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, [open]);

  const { drafts, installed } = useMemo(() => {
    const byPlugin = new Map(plugins.map((p) => [p.id, p]));
    // The newest draft per plugin; drafts of removed plugins are gone.
    const newest = new Map<string, (typeof projects)[number]>();
    for (const p of projects) {
      if (p.installed && !byPlugin.has(p.pluginId)) continue;
      const have = newest.get(p.pluginId);
      if (!have || Date.parse(p.updatedAt) > Date.parse(have.updatedAt)) newest.set(p.pluginId, p);
    }
    const rows: Row[] = [];
    for (const p of newest.values()) {
      const plug = byPlugin.get(p.pluginId);
      rows.push({
        key: p.id,
        name: plug?.name ?? p.name,
        projectId: p.id,
        pluginId: p.pluginId,
        state: plug ? (plug.enabled ? "installed" : "disabled") : "draft",
        addon: Boolean(p.addonOf) || Boolean(plug?.stremioOnly),
        updated: Date.parse(p.updatedAt),
      });
    }
    for (const plug of plugins) {
      if (newest.has(plug.id)) continue;
      rows.push({
        key: plug.id,
        name: plug.name,
        pluginId: plug.id,
        state: plug.enabled ? "installed" : "disabled",
        addon: Boolean(plug.stremioOnly),
        updated: 0,
      });
    }
    rows.sort((a, b) => b.updated - a.updated || a.name.localeCompare(b.name));
    return {
      drafts: rows.filter((r) => r.state === "draft"),
      installed: rows.filter((r) => r.state !== "draft"),
    };
  }, [projects, plugins]);

  const current = currentId
    ? [...drafts, ...installed].find((r) => r.projectId === currentId)
    : undefined;
  const badge = (r: Row) => (
    <span className="flex shrink-0 items-center gap-1">
      {r.addon ? <Badge variant="secondary">{t("Add-on")}</Badge> : null}
      <Badge
        variant={r.state === "draft" ? "info" : r.state === "installed" ? "success" : "secondary"}
      >
        {r.state === "draft"
          ? t("Draft")
          : r.state === "installed"
            ? t("Installed")
            : t("Disabled")}
      </Badge>
    </span>
  );
  const group = (title: string, rows: Row[]) =>
    rows.length ? (
      <div className="py-1">
        <p className="px-2.5 pb-1 pt-1.5 text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
          {title}
        </p>
        {rows.map((r) => (
          <button
            key={r.key}
            type="button"
            role="menuitem"
            onClick={() => {
              setOpen(false);
              if (r.projectId) onPickProject(r.projectId);
              else onPickPlugin(r.pluginId);
            }}
            className={cn(
              "flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-left text-sm hover:bg-surface-hover",
              Boolean(currentId) && r.projectId === currentId && "bg-primary/10",
            )}
          >
            <span className="min-w-0 flex-1 truncate text-fg">{r.name}</span>
            {badge(r)}
          </button>
        ))}
      </div>
    ) : null;

  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      align="end"
      className="max-h-[70vh] w-80 overflow-y-auto p-1"
      trigger={
        <Button
          size="sm"
          variant="outline"
          disabled={disabled}
          onClick={() => setOpen((o) => !o)}
          aria-haspopup="menu"
          aria-expanded={open}
        >
          <span className="max-w-48 truncate">{current?.name ?? t("Open a plugin")}</span>
          <ChevronDown size={14} />
        </Button>
      }
    >
      <div role="menu" aria-label={t("Plugins you can edit")}>
        {group(t("Drafts"), drafts)}
        {group(t("Installed"), installed)}
      </div>
    </Popover>
  );
}

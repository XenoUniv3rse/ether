import { LibrarySourceIcon, sourceLabel } from "./CatalogueSources";
import {
  Badge,
  Button,
  EmptyState,
  Switch,
  confirmDialog,
  toast,
  type ContextMenuEntry,
} from "@braedonsaunders/appkit-ui";
import {
  ArrowLeft,
  ArrowRight,
  Clapperboard,
  EyeOff,
  Pencil,
  Plus,
  RefreshCw,
  Trash2,
} from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { Link, Navigate, Route, Routes, useNavigate } from "react-router-dom";
import { useApp } from "../../app-context";
import { ActionMenu } from "../../components/ActionMenu";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { DomainPage } from "../../components/layout";
import { DragHandle, SortableList, arrayMove } from "../../components/Sortable";
import { LevelBadge } from "../../components/status";
import { t, tn, tx } from "../../i18n";
import { errorMessage } from "../../lib/api";
import { formatNumber } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { librariesApi, publishedTo, type Library, type LibrarySync } from "./api";
import { NewLibraryDrawer } from "./LibraryEditor";
import { LibraryPage } from "./LibraryPage";
import {
  formatDeleteGuardSummary,
  formatItemStorageSummary,
  resolveDeleteGuard,
} from "./syncStatus";
import "./libraries.css";

export default function Libraries() {
  return (
    <Routes>
      <Route index element={<LibrariesList />} />
      <Route path=":id/:tab?" element={<LibraryPage />} />
      <Route path="*" element={<Navigate to="/libraries" replace />} />
    </Routes>
  );
}

function sourceBadge(lib: Library): string {
  if (lib.source === "builtin") return lib.mediaType === "show" ? t("TV shows") : t("Movies");
  if (lib.source === "catalogue") return t("Catalogue sources");
  if (lib.source === "collection") return t("Collection");
  return lib.pluginName || lib.pluginId || t("Plugin");
}

function protocolLabel(key: string): string {
  switch (key) {
    case "vela":
      return "Vela";
    case "plex":
      return "Plex";
    case "mediaBrowser":
      return t("Jellyfin & Emby");
    case "stremio":
      return "Stremio";
    case "xtream":
      return "Xtream";
    case "silo":
      return "Silo";
    default:
      return t("Its own app");
  }
}

export function isDisabledPluginError(raw?: string): boolean {
  if (!raw) return false;
  return raw.toLowerCase().includes("plugin is disabled");
}

export function formatLibrarySyncError(raw: string, sync?: LibrarySync | null): ReactNode {
  const guard = resolveDeleteGuard(sync ?? (raw ? ({ lastError: raw } as LibrarySync) : undefined));
  if (guard) {
    return formatDeleteGuardSummary(guard);
  }
  const lower = raw.trim().toLowerCase();
  if (lower.includes("plugin is disabled")) {
    return tx("Its plugin is turned off. Turn it on in {link}.", {
      link: (
        <Link to="/plugins/installed" className="font-medium underline hover:text-fg">
          {t("Plugins")}
        </Link>
      ),
    });
  }
  if (lower === "plugins: not found") {
    return tx("Its plugin is not installed. Check {link}.", {
      link: (
        <Link to="/plugins/installed" className="font-medium underline hover:text-fg">
          {t("Plugins")}
        </Link>
      ),
    });
  }
  if (lower === "plugins: plugin was revoked") {
    return t("Its plugin was disabled for safety.");
  }
  if (lower === "plugins: no runtime registered for this plugin") {
    return t("Its plugin runtime is unavailable right now.");
  }
  if (lower === "plugins: host service unavailable") {
    return t("A service this plugin needs is unavailable right now.");
  }
  if (lower === "plugins: storage quota exceeded") {
    return t("Its plugin ran out of storage space.");
  }
  if (lower === "plugins: permission denied") {
    return t("Its plugin is missing a required permission.");
  }
  return raw.replace(/^plugins:\s*/i, "");
}

function LibrariesList() {
  const navigate = useNavigate();
  const { health } = useApp();
  const list = useAsync((signal) => librariesApi.list(signal), []);
  const registry = useAsync((signal) => librariesApi.catalogueSources(signal), []);
  const [creating, setCreating] = useState(false);
  const [renaming, setRenaming] = useState<Library | null>(null);
  const [nameDraft, setNameDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const catalogHealth = health.find((h) => h.id === "catalog");
  const libs = list.data ?? [];
  const anyPluginSyncing = libs.some((l) => l.source === "plugin" && l.sync?.running);

  useEffect(() => {
    if (!anyPluginSyncing) return;
    const timer = window.setInterval(() => {
      void list.reload();
    }, 2500);
    return () => window.clearInterval(timer);
  }, [anyPluginSyncing, list.reload]);

  const act = async (run: () => Promise<unknown>, done?: string) => {
    setBusy(true);
    try {
      await run();
      await list.reload();
      if (done) toast.success(done);
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const applyOrder = (next: Library[]) => {
    const rawSlots = libs.map((l) => l.sort).sort((a, b) => a - b);
    const hasDuplicates = rawSlots.some((s, idx) => idx > 0 && s === rawSlots[idx - 1]);
    const targetSort = (idx: number) => (hasDuplicates ? idx + 1 : (rawSlots[idx] ?? idx + 1));
    void act(async () => {
      for (let idx = 0; idx < next.length; idx++) {
        const lib = next[idx]!;
        const want = targetSort(idx);
        if (lib.sort !== want) {
          await librariesApi.patch(lib.id, { sort: want });
        }
      }
    });
  };

  const move = (index: number, dir: -1 | 1) => {
    const to = index + dir;
    if (to < 0 || to >= libs.length) return;
    applyOrder(arrayMove(libs, index, to));
  };

  const confirmDelete = async (lib: Library) => {
    const ok = await confirmDialog({
      title: t("Delete {name}?", { name: lib.name }),
      message: t("Delete the library “{name}”? Media and watch history are kept.", {
        name: lib.name,
      }),
      confirmLabel: t("Delete"),
      tone: "danger",
    });
    if (ok) void act(() => librariesApi.remove(lib.id), t("Library deleted"));
  };

  return (
    <DomainPage
      title={t("Libraries")}
      description={t("Everything your viewers can open, in one place.")}
      base="/libraries"
      actions={
        <Button size="sm" onClick={() => setCreating(true)}>
          <Plus size={14} /> {t("New library")}
        </Button>
      }
    >
      {list.error ? <ErrorPanel error={list.error} onRetry={() => void list.reload()} /> : null}
      {list.loading && !list.data ? <LoadingRows /> : null}
      {!list.loading && libs.length === 0 && !list.error ? (
        <EmptyState
          icon={<Clapperboard size={22} aria-hidden />}
          title={t("No libraries yet")}
          description={t("Your movies and TV shows appear here once the catalog is ready.")}
        />
      ) : (
        <SortableList
          items={libs}
          getId={(lib) => String(lib.id)}
          strategy="grid"
          className="libraries-grid"
          onReorder={(next) => applyOrder(next)}
          renderItem={(lib, handle, i) => {
            const shown = publishedTo(lib);
            const sources =
              lib.source === "collection"
                ? t("Collection")
                : registry.data
                    ?.filter(
                      (source) =>
                        lib.sources?.includes(source.id) ||
                        source.libraries.some((item) => item.id === lib.id),
                    )
                    .map(sourceLabel)
                    .join(", ");
            const syncBad = lib.sync?.lastError;
            const syncGuard = lib.sync?.running ? null : resolveDeleteGuard(lib.sync);
            const pluginDisabled = isDisabledPluginError(syncBad);
            const removable =
              lib.source === "catalogue" || lib.source === "collection" || lib.source === "plugin";
            const menuItems: ContextMenuEntry[] = [
              {
                key: "rename",
                label: t("Rename"),
                icon: Pencil,
                disabled: busy,
                onSelect: () => {
                  setRenaming(lib);
                  setNameDraft(lib.name);
                },
              },
              ...(libs.length > 1
                ? ([
                    {
                      key: "move-left",
                      label: t("Move left"),
                      icon: ArrowLeft,
                      disabled: busy || i === 0,
                      onSelect: () => move(i, -1),
                    },
                    {
                      key: "move-right",
                      label: t("Move right"),
                      icon: ArrowRight,
                      disabled: busy || i === libs.length - 1,
                      onSelect: () => move(i, 1),
                    },
                  ] satisfies ContextMenuEntry[])
                : []),
              ...(removable
                ? ([
                    { key: "sep-delete", separator: true },
                    {
                      key: "delete",
                      label: t("Delete"),
                      icon: Trash2,
                      danger: true,
                      disabled: busy,
                      onSelect: () => void confirmDelete(lib),
                    },
                  ] satisfies ContextMenuEntry[])
                : []),
            ];
            return (
              <section className="library-card" aria-label={lib.name}>
                <div className="library-card-head">
                  <span className="library-card-icon">
                    <LibrarySourceIcon library={lib} />
                  </span>
                  <div className="min-w-0 flex-1">
                    <Link to={`/libraries/${lib.id}`} className="library-card-name">
                      {lib.name}
                    </Link>
                    <p className="library-card-meta">
                      {sources || sourceBadge(lib)}
                      {sources && lib.source === "catalogue" && Boolean(lib.rules?.length)
                        ? ` · ${t("Rules")}`
                        : ""}
                      {lib.source === "plugin" &&
                      typeof (lib.storedItems ?? lib.itemCount) === "number"
                        ? ` · ${formatItemStorageSummary(lib)}`
                        : typeof lib.itemCount === "number"
                          ? ` · ${tn(lib.itemCount, "{count} item", "{count} items", {
                              count: formatNumber(lib.itemCount, 0),
                            })}`
                          : ""}
                    </p>
                  </div>
                  {lib.source === "builtin" && catalogHealth ? (
                    <LevelBadge
                      level={catalogHealth.level}
                      label={catalogHealth.level === "ok" ? t("Healthy") : t("Needs attention")}
                    />
                  ) : lib.source === "plugin" && lib.sync?.running ? (
                    <Badge variant="secondary" className="gap-1">
                      <RefreshCw size={11} className="animate-spin" aria-hidden />
                      {lib.sync.phase === "reindexing" ? t("Reindexing") : t("Syncing")}
                    </Badge>
                  ) : syncGuard ? (
                    <LevelBadge level="warn" label={t("Needs review")} />
                  ) : syncBad ? (
                    <LevelBadge
                      level={pluginDisabled ? "warn" : "error"}
                      label={pluginDisabled ? t("Plugin off") : t("Sync failed")}
                    />
                  ) : lib.source === "plugin" && lib.sync?.lastOk ? (
                    <LevelBadge level="ok" label={t("Healthy")} />
                  ) : null}
                </div>
                <p className="library-card-apps">
                  {lib.hidden ? (
                    <>
                      <EyeOff size={13} aria-hidden /> {t("Hidden from apps")}
                    </>
                  ) : shown.length ? (
                    t("In apps: {apps}", { apps: shown.map(protocolLabel).join(", ") })
                  ) : (
                    t("Not shown in any app yet")
                  )}
                </p>
                {syncBad ||
                syncGuard ||
                (lib.source === "builtin" && catalogHealth && catalogHealth.level !== "ok") ? (
                  <p className="library-card-error" role="alert">
                    {syncBad || syncGuard
                      ? formatLibrarySyncError(syncBad ?? "", lib.sync)
                      : catalogHealth?.summary}
                  </p>
                ) : null}
                <div className="library-card-actions">
                  <Button size="sm" onClick={() => navigate(`/libraries/${lib.id}`)}>
                    {t("Open")}
                  </Button>
                  <label className="library-card-hide">
                    <Switch
                      aria-label={t("Hide {name} from apps", { name: lib.name })}
                      checked={lib.hidden}
                      disabled={busy}
                      onChange={(e) =>
                        void act(() => librariesApi.patch(lib.id, { hidden: e.target.checked }))
                      }
                    />
                    <span>{t("Hidden")}</span>
                  </label>
                  <span className="flex-1" />
                  {libs.length > 1 ? (
                    <DragHandle handle={handle} label={t("Reorder {name}", { name: lib.name })} />
                  ) : null}
                  <ActionMenu
                    label={t("More actions for {name}", { name: lib.name })}
                    items={menuItems}
                  />
                </div>
              </section>
            );
          }}
        />
      )}
      {creating ? (
        <NewLibraryDrawer
          onClose={() => setCreating(false)}
          onCreated={(lib) => {
            setCreating(false);
            void list.reload();
            navigate(`/libraries/${lib.id}`);
          }}
          onPluginOpened={(libId) => {
            setCreating(false);
            void list.reload();
            if (libId) navigate(`/libraries/${libId}`);
          }}
        />
      ) : null}
      <Drawer
        open={renaming !== null}
        onClose={() => setRenaming(null)}
        title={t("Rename library")}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setRenaming(null)}>
              {t("Cancel")}
            </Button>
            <Button
              disabled={busy || !nameDraft.trim()}
              onClick={() => {
                if (!renaming) return;
                void act(async () => {
                  await librariesApi.patch(renaming.id, { name: nameDraft.trim() });
                  setRenaming(null);
                }, t("Library renamed"));
              }}
            >
              {t("Save")}
            </Button>
          </div>
        }
      >
        <label className="library-field">
          <span>{t("Library name")}</span>
          <input
            value={nameDraft}
            onChange={(e) => setNameDraft(e.target.value)}
            aria-label={t("Library name")}
          />
        </label>
        <p className="text-sm text-fg-muted">
          {t("This is the name viewers see in their apps. You can change it any time.")}
        </p>
      </Drawer>
    </DomainPage>
  );
}

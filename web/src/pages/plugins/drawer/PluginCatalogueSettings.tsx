import { PluginFailureDetails } from "../../../components/PluginFailure";
import {
  catalogueCapNote,
  catalogueCapped,
  catalogueCountLabel,
  sourceName,
} from "../../libraries/CatalogueSources";
import { Badge, Button, SettingsSection, cn, toast } from "@braedonsaunders/appkit-ui";
import { RefreshCw } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { SettingsRow } from "../../../components/SettingsRow";
import { t } from "../../../i18n";
import { errorMessage } from "../../../lib/api";
import { useAsync } from "../../../lib/hooks";
import { librariesApi, type LibrarySyncRequest } from "../../libraries/api";
import {
  DeleteGuardChoice,
  formatItemStorageSummary,
  formatSyncStatusLine,
  notifySyncError,
  notifySyncResponse,
  resolveDeleteGuard,
} from "../../libraries/syncStatus";

type SyncAction = "sync" | "full" | "keep" | "force";

export function PluginCatalogueSettings({
  pluginId,
  onSetup,
  onSyncActivity,
}: {
  pluginId: string;
  onSetup: () => void;
  onSyncActivity?: () => void;
}) {
  const libraries = useAsync(
    (signal) => librariesApi.setup(pluginId, signal).then((setup) => setup.libraries),
    [pluginId],
  );
  const sources = useAsync((signal) => librariesApi.catalogueSources(signal), [pluginId]);
  const [busyLib, setBusyLib] = useState<{ id: number; action: SyncAction } | null>(null);
  const [busySource, setBusySource] = useState<string | null>(null);

  const owned = libraries.data?.filter((lib) => lib.pluginId === pluginId) ?? [];
  const catalogues =
    sources.data?.filter((source) => source.pluginId === pluginId && source.group === "stremio") ??
    [];

  const anyRunning = owned.some((lib) => Boolean(lib.sync?.running));
  const wasRunning = useRef(false);

  useEffect(() => {
    if (wasRunning.current && !anyRunning) {
      onSyncActivity?.();
      void sources.reload();
    }
    wasRunning.current = anyRunning;
  }, [anyRunning, onSyncActivity, sources.reload]);

  useEffect(() => {
    if (!anyRunning) return;
    const timer = window.setInterval(() => {
      void libraries.reload();
    }, 1500);
    return () => window.clearInterval(timer);
  }, [anyRunning, libraries.reload]);

  const runSync = async (id: number, action: SyncAction, opts: LibrarySyncRequest) => {
    setBusyLib({ id, action });
    try {
      const res = await librariesApi.sync(id, opts);
      notifySyncResponse(res);
    } catch (e) {
      notifySyncError(e);
    } finally {
      await libraries.reload();
      void sources.reload();
      onSyncActivity?.();
      setBusyLib(null);
    }
  };

  if (!owned.length && !catalogues.length) return null;
  return (
    <SettingsSection title={t("Keeping it up to date")}>
      {owned.map((lib) => {
        const capSummary = formatItemStorageSummary(lib);
        const running = Boolean(lib.sync?.running);
        const guard = running ? null : resolveDeleteGuard(lib.sync);
        const libAction = busyLib?.id === lib.id ? busyLib.action : null;
        const isBusy = Boolean(libAction) || running;
        const status = guard
          ? ""
          : formatSyncStatusLine(lib.sync, {
              count: lib.storedItems ?? lib.itemCount ?? 0,
            });
        const summaryLine =
          capSummary && status ? `${capSummary} · ${status}` : capSummary || status;
        const description = (
          <div className="space-y-1">
            <div className="flex flex-wrap items-center gap-2">
              {summaryLine ? <span>{summaryLine}</span> : null}
              {running ? (
                <Badge variant="secondary" className="gap-1">
                  <RefreshCw size={11} className="animate-spin" aria-hidden />
                  {lib.sync?.phase === "reindexing" ? t("Reindexing") : t("Syncing")}
                </Badge>
              ) : guard ? (
                <Badge variant="outline" className="border-warning/50 text-warning">
                  {t("Needs review")}
                </Badge>
              ) : null}
            </div>
            {!running && !guard && lib.sync?.failure ? (
              <PluginFailureDetails failure={lib.sync.failure} />
            ) : null}
            {guard ? (
              <DeleteGuardChoice
                guard={guard}
                busyAction={libAction}
                onKeep={() => void runSync(lib.id, "keep", { keepExisting: true })}
                onRemoveMissing={() =>
                  void runSync(lib.id, "force", {
                    force: true,
                    fullWalk: guard.source === "walk",
                  })
                }
              />
            ) : null}
          </div>
        );
        return (
          <SettingsRow
            key={lib.id}
            title={lib.kind?.label ?? lib.name}
            description={description}
            control={
              <div className="flex flex-wrap gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  disabled={isBusy}
                  onClick={() => void runSync(lib.id, "full", { fullWalk: true })}
                >
                  {libAction === "full" ? (
                    <RefreshCw size={13} className="animate-spin" aria-hidden />
                  ) : null}
                  {t("Full re-sync")}
                </Button>
                <Button
                  size="sm"
                  disabled={isBusy}
                  onClick={() => void runSync(lib.id, "sync", { fullWalk: false })}
                >
                  <RefreshCw
                    size={13}
                    className={cn((libAction === "sync" || running) && "animate-spin")}
                    aria-hidden
                  />{" "}
                  {running ? t("Syncing…") : t("Sync now")}
                </Button>
              </div>
            }
          />
        );
      })}
      {catalogues.map((source) => {
        const countLabel = typeof source.itemCount === "number" ? catalogueCountLabel(source) : "";
        const capNote = source.lastError ? "" : catalogueCapNote(source);
        const statusLabel = source.lastError
          ? source.lastError
          : source.health === "unknown"
            ? t("Not loaded yet")
            : source.health === "ok"
              ? t("Healthy")
              : t("Needs attention");
        return (
          <SettingsRow
            key={source.id}
            title={sourceName(source)}
            description={
              <>
                {countLabel ? `${countLabel} · ${statusLabel}` : statusLabel}
                {capNote ? (
                  <span className="mt-1 block text-warning" data-testid="catalogue-cap-note">
                    {capNote}
                  </span>
                ) : null}
              </>
            }
            control={
              <Button
                size="sm"
                disabled={busySource === source.id}
                onClick={async () => {
                  setBusySource(source.id);
                  try {
                    const res = await librariesApi.syncCatalogueSource(source.id);
                    if (catalogueCapped(res)) {
                      toast.warning(`${catalogueCountLabel(res)}. ${catalogueCapNote(res)}`);
                    } else {
                      toast.success(
                        t("Catalogue refreshed: {count}", { count: catalogueCountLabel(res) }),
                      );
                    }
                  } catch (e) {
                    toast.error(errorMessage(e));
                  } finally {
                    void sources.reload();
                    onSyncActivity?.();
                    setBusySource(null);
                  }
                }}
              >
                {t("Sync now")}
              </Button>
            }
          />
        );
      })}
      {owned.length ? (
        <SettingsRow
          title={t("Setup")}
          description={t("Re-run the guided setup or check a step any time.")}
          control={
            <Button size="sm" variant="outline" onClick={onSetup}>
              {t("Open setup")}
            </Button>
          }
        />
      ) : null}
    </SettingsSection>
  );
}

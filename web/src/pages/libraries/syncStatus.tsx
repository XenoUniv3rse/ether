import { pluginFailureSentence } from "../../components/PluginFailure";
import { Button, toast } from "@braedonsaunders/appkit-ui";
import { AlertTriangle, RefreshCw } from "lucide-react";
import { t, tn } from "../../i18n";
import { ApiError, errorMessage } from "../../lib/api";
import { formatDateTime, formatNumber, relativeTime } from "../../lib/format";
import type { Library, LibrarySync, LibrarySyncDeleteGuard, LibrarySyncResponse } from "./api";

const LEGACY_WALK_GUARD = /50% delete guard:\s*(\d+)\s+of\s+(\d+)\s+items missing in walk/i;
const LEGACY_CHANGES_GUARD =
  /50% delete guard:\s*attempting to delete\s+(\d+)\s+of\s+(\d+)\s+items/i;

/**
 * Returns structured delete-guard details when a library sync paused on the
 * 50% delete guard, falling back to parsing legacy persisted error strings so
 * existing installs upgrade cleanly.
 */
export function resolveDeleteGuard(sync?: LibrarySync | null): LibrarySyncDeleteGuard | null {
  if (!sync) return null;
  if (sync.deleteGuard && sync.deleteGuard.missing > 0 && sync.deleteGuard.total > 0) {
    return sync.deleteGuard;
  }
  const raw = sync.lastError ?? "";
  if (!raw) return null;
  const walk = LEGACY_WALK_GUARD.exec(raw);
  if (walk) {
    const missing = Number(walk[1]);
    const total = Number(walk[2]);
    if (missing > 0 && total > 0) return { source: "walk", missing, total };
  }
  const changes = LEGACY_CHANGES_GUARD.exec(raw);
  if (changes) {
    const missing = Number(changes[1]);
    const total = Number(changes[2]);
    if (missing > 0 && total > 0) return { source: "changes", missing, total };
  }
  return null;
}

/**
 * Formats a plugin library's stored count alongside the plugin's storage cap
 * without looking like a download/sync progress ratio ("526 of 2,000,000").
 */
export function formatItemStorageSummary(
  lib: Pick<Library, "itemCount" | "storedItems" | "maxPluginItems">,
): string {
  const used = lib.storedItems ?? lib.itemCount;
  if (typeof used !== "number") return "";
  const hasCap = typeof lib.maxPluginItems === "number" && lib.maxPluginItems > 0;
  const cap = hasCap ? formatNumber(lib.maxPluginItems!, 0) : "";

  if (
    typeof lib.itemCount === "number" &&
    typeof lib.storedItems === "number" &&
    lib.storedItems !== lib.itemCount
  ) {
    const itemsPart = tn(lib.itemCount, "{count} item", "{count} items", {
      count: formatNumber(lib.itemCount, 0),
    });
    if (hasCap) {
      return `${itemsPart} · ${t("{used} stored (storage limit {limit})", {
        used: formatNumber(lib.storedItems, 0),
        limit: cap,
      })}`;
    }
    return `${itemsPart} · ${t("{used} stored", { used: formatNumber(lib.storedItems, 0) })}`;
  }

  const countLabel = tn(used, "{count} item", "{count} items", {
    count: formatNumber(used, 0),
  });
  return hasCap
    ? t("{items} (storage limit {limit})", { items: countLabel, limit: cap })
    : countLabel;
}

export function formatStoredCapSummary(used: number, limit: number): string {
  return t("{used} stored (storage limit {limit})", {
    used: formatNumber(used, 0),
    limit: formatNumber(limit, 0),
  });
}

export function formatSyncProgress(sync?: LibrarySync | null, count = 0): string {
  if (!sync) return t("Starting sync…");
  const checked = Math.max(sync.syncedItems ?? 0, count);
  if (sync.phase === "reindexing") {
    return checked > 0
      ? t("Updating search index… · {count} items", { count: formatNumber(checked, 0) })
      : t("Updating search index…");
  }
  const pages = sync.pages ?? 0;
  const synced = sync.syncedItems ?? 0;
  if (pages > 0 && synced > 0) {
    return t("Syncing… · Page {pages} · {count} items checked", {
      pages: formatNumber(pages, 0),
      count: formatNumber(synced, 0),
    });
  }
  if (synced > 0) {
    return t("Syncing… · {count} items checked", { count: formatNumber(synced, 0) });
  }
  if (pages > 0) {
    return t("Syncing… · Page {pages}", { pages: formatNumber(pages, 0) });
  }
  return t("Starting sync…");
}

export function formatDeleteGuardSummary(guard: LibrarySyncDeleteGuard): string {
  const missing = formatNumber(guard.missing, 0);
  const total = formatNumber(guard.total, 0);
  if (guard.source === "changes") {
    return t(
      "Sync paused — plugin asked to remove {missing} of {total} items. Existing items were kept.",
      {
        missing,
        total,
      },
    );
  }
  return t(
    "Sync paused — {missing} of {total} items were not returned in the walk. Existing items were kept.",
    {
      missing,
      total,
    },
  );
}

export function formatSyncStatusLine(
  sync?: LibrarySync | null,
  opts?: { count?: number; preferRelative?: boolean },
): string {
  const count = opts?.count ?? sync?.items ?? 0;
  if (sync?.running) {
    return formatSyncProgress(sync, count);
  }
  const guard = resolveDeleteGuard(sync);
  if (guard) {
    return formatDeleteGuardSummary(guard);
  }
  if (sync?.failure) return pluginFailureSentence(sync.failure);
  if (sync?.lastError) {
    return sync.lastError;
  }
  if (sync?.lastOutcome?.summary) {
    const when = sync.lastOutcome.finishedAt ?? sync.lastOk;
    if (when) {
      return `${sync.lastOutcome.summary} · ${
        opts?.preferRelative ? relativeTime(when) : formatDateTime(when)
      }`;
    }
    return sync.lastOutcome.summary;
  }
  if (sync?.lastOk) {
    return opts?.preferRelative
      ? t("Last synced {when}", { when: relativeTime(sync.lastOk) })
      : t("Last synced {when}", { when: formatDateTime(sync.lastOk) });
  }
  return t("Not synced yet");
}

export function formatSyncActionOutcome(res: LibrarySyncResponse): string {
  if (res.kept) return t("Kept existing items.");
  if (res.alreadyRunning) return t("Sync is already running in the background.");
  if (res.started || res.running) return t("Sync started in the background.");
  if (res.sync?.lastOutcome?.summary) return res.sync.lastOutcome.summary;
  const upserted = res.upserted ?? 0;
  const deleted = res.deleted ?? 0;
  if (deleted > 0) {
    return t("Synced ({updated} updated, {removed} removed)", {
      updated: formatNumber(upserted, 0),
      removed: formatNumber(deleted, 0),
    });
  }
  return t("Sync complete ({updated} updated)", { updated: formatNumber(upserted, 0) });
}

export function notifySyncResponse(res: LibrarySyncResponse) {
  const msg = formatSyncActionOutcome(res);
  if (res.started || res.alreadyRunning || res.running) {
    toast.info(msg);
  } else {
    toast.success(msg);
  }
}

export function notifySyncError(err: unknown) {
  if (err instanceof ApiError && err.code === "delete_guard") {
    toast.warning(err.message);
    return;
  }
  const failure =
    err instanceof ApiError
      ? (err.body as { sync?: LibrarySync } | undefined)?.sync?.failure
      : undefined;
  toast.error(failure ? pluginFailureSentence(failure) : errorMessage(err));
}

export function DeleteGuardChoice({
  guard,
  busyAction,
  onKeep,
  onRemoveMissing,
}: {
  guard: LibrarySyncDeleteGuard;
  busyAction?: "keep" | "force" | "sync" | "full" | null;
  onKeep: () => void;
  onRemoveMissing: () => void;
}) {
  const missing = formatNumber(guard.missing, 0);
  const total = formatNumber(guard.total, 0);
  const busy = Boolean(busyAction);
  const isChanges = guard.source === "changes";

  return (
    <div
      role="status"
      className="mt-2.5 rounded-lg border border-warning/40 bg-warning/5 p-3.5 text-xs"
    >
      <div className="flex items-start gap-2.5">
        <AlertTriangle size={15} className="mt-0.5 shrink-0 text-warning" aria-hidden />
        <div className="min-w-0 flex-1 space-y-2">
          <p className="font-medium text-fg">
            {isChanges
              ? t("Sync paused — plugin asked to remove {missing} of {total} items", {
                  missing,
                  total,
                })
              : t("Sync paused — {missing} of {total} items were not returned in the walk", {
                  missing,
                  total,
                })}
          </p>
          <p className="leading-relaxed text-fg-muted">
            {isChanges
              ? t(
                  "The plugin's change feed asked to delete {missing} of {total} existing items. Ether paused before deleting them and kept your existing items by default.",
                  { missing, total },
                )
              : t(
                  "The latest walk did not return {missing} of {total} existing items. This can happen when a plugin pulls from a search or rotating API, or hits a temporary error. Ether kept your existing items by default.",
                  { missing, total },
                )}
            {guard.protected && guard.protected > 0
              ? ` ${tn(
                  guard.protected,
                  "{count} opened, played, favourited, or searched item is protected and will stay.",
                  "{count} opened, played, favourited, or searched items are protected and will stay.",
                  { count: formatNumber(guard.protected, 0) },
                )}`
              : ""}
          </p>
          <div className="flex flex-wrap items-center gap-2 pt-0.5">
            <Button size="sm" variant="outline" disabled={busy} onClick={onKeep}>
              {busyAction === "keep" ? t("Keeping…") : t("Keep existing items")}
            </Button>
            <Button size="sm" variant="destructive" disabled={busy} onClick={onRemoveMissing}>
              {busyAction === "force" ? (
                <>
                  <RefreshCw size={12} className="animate-spin" aria-hidden />{" "}
                  {t("Removing missing items…")}
                </>
              ) : (
                tn(
                  guard.missing,
                  "Remove the {count} missing item",
                  "Remove the {count} missing items",
                  { count: missing },
                )
              )}
            </Button>
          </div>
        </div>
      </div>
    </div>
  );
}

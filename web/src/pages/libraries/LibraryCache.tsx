import { Button, SettingsSection, Spinner, toast } from "@braedonsaunders/appkit-ui";
import { useId, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { ErrorPanel } from "../../components/feedback";
import { NumberInput } from "../../components/fields";
import { SaveBar, useDraft } from "../../components/SaveBar";
import { SettingsRow } from "../../components/SettingsRow";
import { t, tn } from "../../i18n";
import { errorMessage } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import { useAsync } from "../../lib/hooks";
import { librariesApi, type Library, type LibraryCache } from "./api";

const GIB = 1024 ** 3;

/** Quota and TTL are keyed by library ID, so these stay with the library. */
export function LibraryCacheCard({
  library,
  data,
  maxBytes,
  ttlSeconds,
  title,
  onChange,
  onReload,
}: {
  library: Library;
  data: LibraryCache;
  maxBytes: number;
  ttlSeconds: number;
  title?: string;
  onChange: (bytes: number, seconds: number) => void;
  onReload: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const keepId = useId();
  const forgetId = useId();
  const run = async (purge: boolean) => {
    if (
      purge &&
      !window.confirm(t("Delete every downloaded file for {name}?", { name: library.name }))
    )
      return;
    setBusy(true);
    try {
      await (purge ? librariesApi.purgeCache(library.id) : librariesApi.sweepCache(library.id));
      onReload();
      toast.success(t("Old files cleared"));
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <SettingsSection title={title ?? t("Downloaded files")}>
      <SettingsRow
        title={t("Space used")}
        description={tn(
          data.blobs,
          "{count} file kept for quick opening",
          "{count} files kept for quick opening",
        )}
        control={
          <span className="text-sm font-medium tabular-nums text-fg">{formatBytes(data.used)}</span>
        }
      />
      <SettingsRow
        title={<label htmlFor={keepId}>{t("Keep at most")}</label>}
        description={t("Oldest files go first when the library passes this size.")}
        control={
          <span className="flex items-center gap-2">
            <NumberInput
              id={keepId}
              aria-label={t("Keep at most")}
              className="w-28"
              min={0}
              step={0.5}
              value={Math.round((maxBytes / GIB) * 100) / 100}
              onChange={(n) => onChange(Math.round(Math.max(0, n) * GIB), ttlSeconds)}
            />
            <span className="text-sm text-fg-muted">GiB</span>
          </span>
        }
      />
      {data.sharedCap !== undefined ? (
        <SettingsRow
          title={t("Shared library cache")}
          description={t("All plugin libraries can currently use up to {size} together.", {
            size: formatBytes(data.sharedCap),
          })}
          control={
            <Link to="/plugins/settings" className="text-sm text-accent hover:underline">
              {t("Edit plugin limits")}
            </Link>
          }
        />
      ) : null}
      <SettingsRow
        title={<label htmlFor={forgetId}>{t("Forget files after")}</label>}
        description={t(
          "Days before unused files are removed. Zero keeps files until space is needed.",
        )}
        control={
          <span className="flex items-center gap-2">
            <NumberInput
              id={forgetId}
              aria-label={t("Forget files after")}
              className="w-28"
              min={0}
              step={1}
              value={Math.round((ttlSeconds / 86400) * 100) / 100}
              onChange={(n) => onChange(maxBytes, Math.round(Math.max(0, n) * 86400))}
            />
            <span className="text-sm text-fg-muted">{t("days")}</span>
          </span>
        }
      />
      <SettingsRow
        title={t("Cached files")}
        control={
          <div className="flex flex-wrap gap-2">
            <Button size="sm" variant="outline" disabled={busy} onClick={() => void run(false)}>
              {t("Clear old files")}
            </Button>
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => void run(true)}>
              {t("Delete all files")}
            </Button>
          </div>
        }
      />
    </SettingsSection>
  );
}

export function LibraryCaches() {
  const list = useAsync((signal) => librariesApi.list(signal), []);
  if (list.error) return <ErrorPanel error={list.error} onRetry={() => void list.reload()} />;
  if (!list.data)
    return (
      <div className="py-10 text-center">
        <Spinner size={20} label={t("Loading library caches")} />
      </div>
    );
  return (
    <div className="space-y-5">
      {list.data
        .filter((lib) => lib.source === "plugin")
        .map((library) => (
          <StandaloneCache key={library.id} library={library} />
        ))}
    </div>
  );
}

function StandaloneCache({ library }: { library: Library }) {
  const cache = useAsync((signal) => librariesApi.cache(library.id, signal), [library.id]);
  const saved = useMemo(
    () =>
      cache.data ? { maxBytes: cache.data.maxBytes, ttlSeconds: cache.data.ttlSeconds } : null,
    [cache.data],
  );
  const { draft, setDraft, dirty, reset } = useDraft(saved);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);

  if (cache.error) return <ErrorPanel error={cache.error} onRetry={() => void cache.reload()} />;
  if (!cache.data || !draft) return null;

  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    try {
      await librariesApi.setCache(library.id, draft.maxBytes, draft.ttlSeconds);
      await cache.reload();
      toast.success(t("Settings saved"), { description: library.name });
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  return (
    <>
      <LibraryCacheCard
        library={library}
        title={library.name}
        data={cache.data}
        maxBytes={draft.maxBytes}
        ttlSeconds={draft.ttlSeconds}
        onChange={(maxBytes, ttlSeconds) => setDraft({ maxBytes, ttlSeconds })}
        onReload={() => void cache.reload()}
      />
      <SaveBar
        dirty={dirty}
        saving={saving}
        error={error}
        onSave={save}
        onDiscard={() => {
          reset();
          setError(null);
        }}
      />
    </>
  );
}

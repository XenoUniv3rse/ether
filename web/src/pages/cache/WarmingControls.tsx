import { Button, Switch } from "@braedonsaunders/appkit-ui";
import { Flame } from "lucide-react";
import { useState } from "react";
import { ErrorPanel } from "../../components/feedback";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync } from "../../lib/hooks";

type Warming = { titles: { key: number; title: string }[]; speculative: boolean };
const path = "/cache/warming";

export function WarmingControls({ titleKey }: { titleKey: string }) {
  const state = useAsync(() => apiFetch<Warming>(path), [titleKey]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  async function save(body: { key?: number; pinned?: boolean; speculative?: boolean }) {
    setBusy(true);
    setError(null);
    try {
      await apiFetch(path, { method: "POST", body: JSON.stringify(body) });
      await state.reload();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }
  const data = state.data;
  const pinned = data?.titles.some((x) => x.key === Number(titleKey)) ?? false;
  const problem =
    state.error || error ? (
      <ErrorPanel error={error ?? state.error} onRetry={() => void state.reload()} />
    ) : null;
  return (
    <>
      <Button
        size="sm"
        variant={pinned ? "default" : "outline"}
        aria-pressed={pinned}
        disabled={!data || busy}
        onClick={() => void save({ key: Number(titleKey), pinned: !pinned })}
      >
        <Flame size={14} fill={pinned ? "currentColor" : "none"} />
        {t("Keep warm")}
      </Button>
      {problem}
    </>
  );
}

/** Rows inside the Managed local shelf section; changes join the cache policy draft. */
export function WarmingPolicy({
  keys,
  speculative,
  disabled,
  onRemove,
  onSpeculativeChange,
}: {
  keys: number[];
  speculative: boolean;
  disabled: boolean;
  onRemove: (key: number) => void;
  onSpeculativeChange: (value: boolean) => void;
}) {
  const state = useAsync((signal) => apiFetch<Warming>(path, { signal }), []);
  return (
    <>
      {keys.length ? (
        <SettingsRow
          setting="keep-warm"
          stacked
          title={t("Keep warm")}
          description={t(
            "People's next episodes come first, then your pins. Uses your cache budget.",
          )}
        >
          <ul className="max-h-40 divide-y divide-border overflow-y-auto rounded-lg border border-border bg-bg-subtle/40 px-3">
            {keys.map((key) => (
              <li key={key} className="flex items-center justify-between gap-3 py-2 text-sm">
                <span className="min-w-0 truncate text-fg">
                  {state.data?.titles.find((title) => title.key === key)?.title ?? key}
                </span>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={disabled}
                  onClick={() => onRemove(key)}
                >
                  {t("Remove")}
                </Button>
              </li>
            ))}
          </ul>
        </SettingsRow>
      ) : (
        <SettingsRow
          setting="keep-warm"
          title={t("Keep warm")}
          description={t(
            "People's next episodes come first, then your pins. Uses your cache budget.",
          )}
          control={
            <span className="text-xs text-fg-muted">
              {t("Choose Keep warm on a movie or show.")}
            </span>
          }
        />
      )}
      <SettingsRow
        setting="speculative-warming"
        title={<label htmlFor="speculative-warming">{t("Speculative warming")}</label>}
        description={
          speculative
            ? t("Warms other likely plays when cache space is available.")
            : t("Off: only pins, watchlists and what people are watching.")
        }
        control={
          <Switch
            id="speculative-warming"
            aria-label={t("Speculative warming")}
            checked={speculative}
            disabled={disabled}
            onChange={(e) => onSpeculativeChange(e.currentTarget.checked)}
          />
        }
      />
      {state.error ? (
        <div className="px-5 py-3">
          <ErrorPanel error={state.error} onRetry={() => void state.reload()} />
        </div>
      ) : null}
    </>
  );
}

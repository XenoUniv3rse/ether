import { Button, Input, Select } from "@braedonsaunders/appkit-ui";
import { useEffect, useRef, useState } from "react";
import { SettingsRow } from "../../components/SettingsRow";
import { t } from "../../i18n";
import { errorMessage } from "../../lib/api";

import type { SearchOptions } from "./SchemaForm";

type Option = { value: string; label: string };
/** Runtime-backed choices, with explicit search and no provider-specific UI. */
export function SearchSetting({
  searchOptions,
  description,
  disabled,
  field,
  value,
  onChange,
}: {
  searchOptions: SearchOptions;
  description?: React.ReactNode;
  disabled?: boolean;
  field: { key: string; label: string; placeholder?: string };
  value: unknown;
  onChange: (value: string) => void;
}) {
  const [query, setQuery] = useState("");
  const [options, setOptions] = useState<Option[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  useEffect(() => {
    setOptions([]);
    setBusy(false);
    setError("");
    return () => request.current?.abort();
  }, [searchOptions, field.key]);
  // JSON option values may carry structured provider coordinates and a label.
  let label = String(value ?? "");
  try {
    const name: unknown = JSON.parse(label)?.name;
    if (typeof name === "string") label = name;
  } catch {
    /* opaque option */
  }
  const search = async () => {
    request.current?.abort();
    const current = new AbortController();
    request.current = current;
    setBusy(true);
    setError("");
    try {
      const result = await searchOptions(field.key, query.trim(), current.signal);
      if (current.signal.aborted) return;
      setOptions(result);
      if (!result.length) setError(t("No results"));
    } catch (e) {
      if (!current.signal.aborted) setError(errorMessage(e));
    } finally {
      if (!current.signal.aborted) setBusy(false);
    }
  };
  return (
    <SettingsRow title={field.label} description={description}>
      <div className="space-y-2">
        <form
          className="flex gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            void search();
          }}
        >
          <Input
            aria-label={field.label}
            disabled={disabled}
            placeholder={field.placeholder}
            value={query}
            onChange={(event) => {
              setQuery(event.target.value);
              request.current?.abort();
              setBusy(false);
              setOptions([]);
              setError("");
            }}
          />
          <Button type="submit" size="sm" disabled={disabled || busy || query.trim().length < 2}>
            {busy ? t("Searching…") : t("Search")}
          </Button>
        </form>
        {options.length > 0 ? (
          <Select
            aria-label={t("Search results")}
            disabled={disabled}
            value={String(value ?? "")}
            onChange={(event) => onChange(event.target.value)}
          >
            <option value="">{t("Choose a result")}</option>
            {options.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </Select>
        ) : null}
        {label && !options.some((option) => option.value === String(value)) ? (
          <p className="text-sm text-fg-muted">{label}</p>
        ) : null}
        {error ? (
          <p role="status" className="text-sm text-danger">
            {error}
          </p>
        ) : null}
      </div>
    </SettingsRow>
  );
}

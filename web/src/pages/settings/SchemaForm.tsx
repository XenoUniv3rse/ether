import { SearchSetting } from "./SearchSetting";
import { SubtabNav } from "../../components/layout";
import { parseDuration, formatDuration, type DurationUnit } from "./duration";
import { SettingAnchor } from "../../components/SettingFocus";
import { SettingsRow } from "../../components/SettingsRow";
import {
  Badge,
  Button,
  Input,
  Select,
  MultiSelect,
  SearchSelect,
  SettingsSection,
  Switch,
  Textarea,
} from "@braedonsaunders/appkit-ui";
import { ChevronDown, Plus, Search, X } from "lucide-react";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { ListInput, MapInput, NumberInput, SecretInput } from "../../components/fields";
import { TitleSearch, viewerName } from "../../components/pickers";
import { DragHandle, SortableList } from "../../components/Sortable";
import { t, tn } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { api, errorMessage, isAbort } from "../../lib/api";
import { textOf, useRegistry, type ExtendText } from "../../lib/extend";
import { humanizeKey, isPlainObject } from "../../lib/format";
import { useAsync, useDebouncedValue } from "../../lib/hooks";
import { REDACTED } from "../../lib/types";
import { LANGUAGE_TAGS } from "../libraries/browser/CatalogPickers";

type Obj = Record<string, unknown>;

export type FieldDef =
  | { key: string; label: string; help?: ReactNode; kind: "group"; fields: FieldDef[] }
  | {
      key: string;
      label: string;
      help?: ReactNode;
      kind: "text" | "secret" | "duration" | "readonly";
      placeholder?: string;
      wide?: boolean;
    }
  | {
      key: string;
      label: string;
      help?: ReactNode;
      kind: "number";
      min?: number;
      max?: number;
      step?: number;
      unit?: string;
    }
  | {
      key: string;
      label: string;
      help?: ReactNode;
      kind: "bool";
      disabled?: boolean;
      disabledValue?: boolean;
    }
  | {
      key: string;
      label: string;
      help?: ReactNode;
      kind: "select";
      options: { value: string; label: string }[];
      /** Runtime option source when options is empty (plugin /ether/options). */
      optionsFrom?: string;
      numeric?: boolean;
      defaultValue?: string;
      search?: boolean;
      placeholder?: string;
    }
  | {
      key: string;
      label: string;
      help?: ReactNode;
      kind: "list";
      placeholder?: string;
      numeric?: boolean;
    }
  /** Name/value rows whose values are secrets (custom headers). */
  | { key: string; label: string; help?: ReactNode; kind: "map" }
  | { key: string; label: string; help?: ReactNode; kind: "bytes"; unit: "GiB" | "MiB" }
  /** Nested values the UI has no form for, edited as JSON. */
  | { key: string; label: string; help?: ReactNode; kind: "json" }
  /** Known to this section but edited on another page; never rendered here. */
  | { key: string; label: string; help?: ReactNode; kind: "hidden" }
  | {
      key: string;
      label: string;
      help?: ReactNode;
      kind: "multiSelect" | "orderedList";
      options: { value: string; label: string; description?: string }[];
      /** Runtime option source when options is empty (plugin /ether/options). */
      optionsFrom?: string;
      /** Search runtime options as the owner types (plugin /ether/options/{key}?q=…). */
      search?: boolean;
      placeholder?: string;
      numeric?: boolean;
    }
  | { key: string; label: string; help?: ReactNode; kind: "language" }
  | { key: string; label: string; help?: ReactNode; kind: "viewer" }
  | { key: string; label: string; help?: ReactNode; kind: "collection" }
  | {
      key: string;
      label: string;
      help?: ReactNode;
      kind: "folder" | "url";
      placeholder?: string;
      wide?: boolean;
    }
  | { key: string; label: string; help?: ReactNode; kind: "title" };

export interface FieldGroup {
  /** Stable identity for code that picks groups; titles are translated copy. */
  id?: string;
  title?: string;
  description?: string;
  fields: FieldDef[];
  /**
   * Same section, shown only after "More options". Use for controls a
   * household can leave at their defaults.
   */
  more?: FieldDef[];
  /** Collapsed by default as a disclosure section (plugin SettingsSchema.groups[].advanced). */
  advanced?: boolean;
}

/** Loads runtime options for a field whose schema names an optionsFrom source. */
export type LoadOptions = (field: string) => Promise<{ value: string; label: string }[]>;
/** Searches runtime options as the owner types for a `search: true` field. */
export type SearchOptions = (
  field: string,
  query: string,
  signal: AbortSignal,
) => Promise<{ value: string; label: string; description?: string }[]>;

// ---- Served schemas ---------------------------------------------------------
// Mirror of internal/config/schema (SPEC section 5.5). Built-in forms and
// plugin settings arrive in this shape; fieldGroupsFromServed localises them
// into the FieldDef the renderers below take.

type ServedFieldKind =
  | "group"
  | "text"
  | "secret"
  | "duration"
  | "readonly"
  | "number"
  | "bool"
  | "select"
  | "list"
  | "bytes"
  | "json"
  | "hidden"
  | "multiSelect"
  | "orderedList"
  | "language"
  | "viewer"
  | "collection"
  | "folder"
  | "url"
  | "title";

interface ServedField {
  key: string;
  label: ExtendText;
  help?: ExtendText;
  kind: ServedFieldKind;
  placeholder?: string;
  wide?: boolean;
  min?: number;
  max?: number;
  step?: number;
  unit?: ExtendText;
  numeric?: boolean;
  required?: boolean;
  options?: { value: string; label: ExtendText }[];
  optionsFrom?: string;
  fields?: ServedField[];
}

interface ServedGroup {
  id?: string;
  title?: ExtendText;
  description?: ExtendText;
  fields: ServedField[];
  more?: ServedField[];
}

export interface ServedSchema {
  groups: ServedGroup[];
}

/** Meta of a settingsSchema descriptor (internal/extend/meta_settings.go). */
export interface SettingsSchemaMeta {
  configKey?: string;
  schema: ServedSchema;
}

function servedOptions(f: ServedField): { value: string; label: string }[] {
  return (f.options ?? []).map((o) => ({ value: o.value, label: textOf(o.label) }));
}

function fieldDefFromServed(f: ServedField): FieldDef {
  const label = textOf(f.label);
  const help = f.help ? textOf(f.help) : undefined;
  const base = { key: f.key, label, help };
  switch (f.kind) {
    case "group":
      return { ...base, kind: "group", fields: (f.fields ?? []).map(fieldDefFromServed) };
    case "number":
      return {
        ...base,
        kind: "number",
        min: f.min,
        max: f.max,
        step: f.step,
        unit: f.unit ? textOf(f.unit) : undefined,
      };
    case "bytes": {
      const unit = f.unit ? textOf(f.unit) : "GiB";
      return { ...base, kind: "bytes", unit: unit === "MiB" ? "MiB" : "GiB" };
    }
    case "select":
      return {
        ...base,
        kind: "select",
        options: servedOptions(f),
        optionsFrom: f.optionsFrom,
        numeric: f.numeric,
      };
    case "multiSelect":
    case "orderedList":
      return {
        ...base,
        kind: f.kind,
        options: servedOptions(f),
        optionsFrom: f.optionsFrom,
        numeric: f.numeric,
      };
    case "list":
      return { ...base, kind: "list", placeholder: f.placeholder, numeric: f.numeric };
    case "text":
    case "secret":
    case "duration":
    case "readonly":
      return { ...base, kind: f.kind, placeholder: f.placeholder, wide: f.wide };
    case "folder":
    case "url":
      return { ...base, kind: f.kind, placeholder: f.placeholder, wide: f.wide };
    default:
      return { ...base, kind: f.kind };
  }
}

export function fieldGroupsFromServed(schema: ServedSchema): FieldGroup[] {
  return schema.groups.map((g) => ({
    id: g.id,
    title: g.title ? textOf(g.title) : undefined,
    description: g.description ? textOf(g.description) : undefined,
    fields: g.fields.map(fieldDefFromServed),
    more: g.more?.length ? g.more.map(fieldDefFromServed) : undefined,
  }));
}

/**
 * One served settings schema by descriptor id, localised for rendering.
 * groups is undefined until the registry answers; pages keep their drafts
 * mounted and show a loading state meanwhile (see ConfigSection).
 */
export function useSettingsSchema(id: string): {
  groups: FieldGroup[] | undefined;
  meta: SettingsSchemaMeta | undefined;
  loading: boolean;
  error: Error | null;
} {
  const { locale } = useLocale();
  const { descriptors, loading, error } = useRegistry<SettingsSchemaMeta>("settingsSchema");
  const found = descriptors.find((d) => d.id === id);
  const meta = found?.meta;
  const groups = useMemo(
    () => (meta ? fieldGroupsFromServed(meta.schema) : undefined),
    // textOf reads the active catalogue; locale is the dependency that
    // changes when the language does.

    [meta, locale],
  );
  return { groups, meta, loading, error };
}

/**
 * Several served schemas concatenated in the given order, for pages that
 * edit one config section through more than one served form. groups is
 * undefined until every named schema has arrived.
 */
export function useSettingsSchemas(ids: readonly string[]): {
  groups: FieldGroup[] | undefined;
  loading: boolean;
  error: Error | null;
} {
  const { locale } = useLocale();
  const { descriptors, loading, error } = useRegistry<SettingsSchemaMeta>("settingsSchema");
  const idsKey = ids.join(",");
  const groups = useMemo(() => {
    const wanted = idsKey ? idsKey.split(",") : [];
    const out: FieldGroup[] = [];
    for (const id of wanted) {
      const meta = descriptors.find((d) => d.id === id)?.meta;
      if (!meta) return undefined;
      out.push(...fieldGroupsFromServed(meta.schema));
    }
    return out;
    // textOf reads the active catalogue; locale changes with the language.
  }, [descriptors, idsKey, locale]);
  return { groups, loading, error };
}

/**
 * A whole served schema as one form. This is the entry point other surfaces
 * (plugin settings, channel kinds, integration cards) render with.
 */
function pointerKey(seg: string): string {
  return seg.replace(/~/g, "~0").replace(/\//g, "~1");
}

/**
 * Looks up a field error by full dotted path (`server.port`), relative dotted
 * path (`port`, `rtxVideo.mode`), or JSON pointer (`/port`, `/rtxVideo/mode`).
 */
function errorForPath(
  errors: Record<string, string> | undefined,
  path: string,
): string | undefined {
  if (!errors) return undefined;
  if (errors[path]) return errors[path];
  const fullPtr = "/" + path.split(".").map(pointerKey).join("/");
  if (errors[fullPtr]) return errors[fullPtr];
  const dot = path.indexOf(".");
  if (dot > 0) {
    const rel = path.slice(dot + 1);
    if (errors[rel]) return errors[rel];
    const relPtr = "/" + rel.split(".").map(pointerKey).join("/");
    if (errors[relPtr]) return errors[relPtr];
  }
  return undefined;
}

export function hasFieldError(
  f: FieldDef,
  errors: Record<string, string>,
  parentPath: string,
): boolean {
  const path = parentPath ? `${parentPath}.${f.key}` : f.key;
  if (errorForPath(errors, path)) return true;
  if (f.kind === "group") {
    return f.fields.some((child) => hasFieldError(child, errors, path));
  }
  return false;
}

const UNIT_BYTES = { GiB: 1024 ** 3, MiB: 1024 ** 2 } as const;

/**
 * Renders one field as an AppKit SettingsRow. `errors` is keyed by the
 * field's dotted path; the row carries `data-setting` so ⌘K can jump to it.
 */
export function SchemaField(props: {
  def: FieldDef;
  value: unknown;
  onChange: (v: unknown) => void;
  error?: string;
  errors?: Record<string, string>;
  path: string;
  loadOptions?: LoadOptions;
  searchOptions?: SearchOptions;
  disabled?: boolean;
}) {
  if (props.def.kind === "hidden") return null;
  return (
    <SettingAnchor id={props.def.key}>
      <SchemaFieldBody {...props} />
    </SettingAnchor>
  );
}

function SchemaFieldBody({
  def,
  value,
  onChange,
  error,
  errors,
  path,
  loadOptions,
  searchOptions,
  disabled,
}: {
  def: FieldDef;
  value: unknown;
  onChange: (v: unknown) => void;
  error?: string;
  errors?: Record<string, string>;
  path: string;
  loadOptions?: LoadOptions;
  searchOptions?: SearchOptions;
  disabled?: boolean;
}) {
  const isDisabled = Boolean(disabled || (def.kind === "bool" && def.disabled));
  const rowClassName = isDisabled ? "opacity-50" : undefined;
  const resolvedError = error ?? errorForPath(errors, path);
  const id = `f-${path.replace(/[^a-z0-9]+/gi, "-")}`;
  const description = (
    <>
      {def.help}
      {resolvedError ? (
        <span className="mt-1 block text-xs text-danger" role="alert">
          {resolvedError}
        </span>
      ) : null}
    </>
  );
  switch (def.kind) {
    case "group": {
      const nested = asObj(value);
      return (
        <div role="group" aria-label={def.label}>
          {def.fields.map((field) => (
            <SchemaField
              key={field.key}
              def={field}
              value={nested[field.key]}
              path={`${path}.${field.key}`}
              errors={errors}
              error={errorForPath(errors, `${path}.${field.key}`)}
              loadOptions={loadOptions}
              searchOptions={searchOptions}
              disabled={isDisabled}
              onChange={(v) => onChange({ ...nested, [field.key]: v })}
            />
          ))}
          {resolvedError ? (
            <p role="alert" className="text-sm text-danger">
              {resolvedError}
            </p>
          ) : null}
        </div>
      );
    }
    case "hidden":
      return null;
    case "json":
      return <JsonRow label={def.label} value={value} onChange={onChange} error={resolvedError} />;
    case "bool":
      return (
        <SettingsRow
          className={rowClassName}
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
          control={
            <Switch
              id={id}
              disabled={isDisabled}
              checked={
                isDisabled && def.disabledValue !== undefined ? def.disabledValue : Boolean(value)
              }
              onChange={(e) => onChange(e.currentTarget.checked)}
              aria-label={def.label}
            />
          }
        />
      );
    case "number":
      return (
        <SettingsRow
          className={rowClassName}
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
          control={
            <span className="flex items-center gap-2">
              <NumberInput
                id={id}
                aria-label={def.label}
                aria-invalid={resolvedError ? true : undefined}
                className="w-28"
                value={Number(value ?? 0)}
                min={def.min}
                max={def.max}
                step={def.step ?? "any"}
                disabled={isDisabled}
                onChange={onChange}
              />
              {def.unit ? <span className="text-sm text-fg-muted">{def.unit}</span> : null}
            </span>
          }
        />
      );
    case "bytes": {
      const unit = UNIT_BYTES[def.unit];
      return (
        <SettingsRow
          className={rowClassName}
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
          control={
            <span className="flex items-center gap-2">
              <NumberInput
                id={id}
                aria-label={t("{label} in {unit}", { label: def.label, unit: def.unit })}
                className="w-28"
                value={Math.round((Number(value ?? 0) / unit) * 10) / 10}
                min={0}
                step={1}
                disabled={isDisabled}
                onChange={(n) => onChange(Math.round(n * unit))}
              />
              <span className="text-sm text-fg-muted">{def.unit}</span>
            </span>
          }
        />
      );
    }
    case "select":
      if (def.search && searchOptions) {
        return (
          <SearchSetting
            field={def}
            value={value}
            onChange={(v) => onChange(def.numeric ? Number(v) : v)}
            description={description}
            searchOptions={searchOptions}
            disabled={isDisabled}
          />
        );
      }
      return (
        <SelectRow
          def={def}
          value={value}
          onChange={onChange}
          description={description}
          loadOptions={loadOptions}
          disabled={isDisabled}
        />
      );
    case "multiSelect":
      return (
        <MultiSelectRow
          def={def}
          value={value}
          onChange={onChange}
          description={description}
          loadOptions={loadOptions}
          searchOptions={searchOptions}
        />
      );
    case "orderedList":
      return (
        <OrderedListRow
          def={def}
          value={value}
          onChange={onChange}
          description={description}
          loadOptions={loadOptions}
        />
      );
    case "language":
      return <LanguageRow def={def} value={value} onChange={onChange} description={description} />;
    case "viewer":
      return <ViewerRow def={def} value={value} onChange={onChange} description={description} />;
    case "collection":
      return (
        <CollectionRow def={def} value={value} onChange={onChange} description={description} />
      );
    case "title":
      return <TitleRow def={def} value={value} onChange={onChange} description={description} />;
    case "folder":
    case "url": {
      const input = (
        <Input
          id={id}
          aria-label={def.label}
          aria-invalid={resolvedError ? true : undefined}
          type={def.kind === "url" ? "url" : "text"}
          inputMode={def.kind === "url" ? "url" : undefined}
          value={String(value ?? "")}
          placeholder={def.placeholder}
          onChange={(e) => onChange(e.currentTarget.value)}
          className={def.wide ? undefined : "sm:w-72"}
          spellCheck={false}
        />
      );
      return def.wide ? (
        <SettingsRow
          stacked
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
        >
          {input}
        </SettingsRow>
      ) : (
        <SettingsRow
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
          control={input}
        />
      );
    }
    case "list":
      return (
        <SettingsRow
          stacked
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
        >
          <ListInput
            id={id}
            ariaLabel={def.label}
            invalid={Boolean(resolvedError)}
            value={(value as (string | number)[] | null) ?? []}
            onChange={onChange}
            placeholder={def.placeholder}
            numeric={def.numeric}
          />
        </SettingsRow>
      );
    case "map":
      return (
        <SettingsRow
          stacked
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
        >
          <MapInput
            id={id}
            ariaLabel={def.label}
            invalid={Boolean(error)}
            value={(value as Record<string, string> | null) ?? {}}
            onChange={onChange}
            namePlaceholder={t("Header name")}
            valuePlaceholder={t("Value")}
          />
        </SettingsRow>
      );
    case "secret":
      return (
        <SettingsRow
          stacked
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
        >
          <SecretInput
            id={id}
            ariaLabel={def.label}
            value={String(value ?? "")}
            onChange={onChange}
            placeholder={def.placeholder}
            invalid={Boolean(resolvedError)}
          />
        </SettingsRow>
      );
    case "duration":
      return (
        <SettingsRow
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
          control={
            <DurationControl
              id={id}
              label={def.label}
              value={String(value ?? "")}
              onChange={onChange}
            />
          }
        />
      );
    case "readonly":
      return (
        <SettingsRow
          title={def.label}
          description={description}
          control={
            <code
              className="max-w-64 truncate rounded bg-bg-subtle px-2 py-1 text-xs text-fg-muted"
              title={String(value ?? "")}
            >
              {String(value ?? "") || "—"}
            </code>
          }
        />
      );
    default: {
      const input = (
        <Input
          id={id}
          aria-label={def.label}
          aria-invalid={resolvedError ? true : undefined}
          value={String(value ?? "")}
          placeholder={def.placeholder}
          disabled={isDisabled}
          onChange={(e) => onChange(e.currentTarget.value)}
          className={def.wide ? undefined : "sm:w-72"}
          spellCheck={false}
        />
      );
      return def.wide ? (
        <SettingsRow
          stacked
          className={rowClassName}
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
        >
          {input}
        </SettingsRow>
      ) : (
        <SettingsRow
          className={rowClassName}
          title={<label htmlFor={id}>{def.label}</label>}
          description={description}
          control={input}
        />
      );
    }
  }
}

type RowProps<D> = {
  def: D;
  value: unknown;
  onChange: (v: unknown) => void;
  description: ReactNode;
  loadOptions?: LoadOptions;
  searchOptions?: SearchOptions;
  disabled?: boolean;
};

type OptionDef = Extract<FieldDef, { kind: "select" | "multiSelect" | "orderedList" }>;

/** Static options, or runtime options loaded once for an optionsFrom field. */
function useFieldOptions(def: OptionDef, loadOptions?: LoadOptions) {
  const [loaded, setLoaded] = useState<
    { value: string; label: string; description?: string }[] | null
  >(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const from = def.options.length ? undefined : def.optionsFrom;
  useEffect(() => {
    if (!from || !loadOptions) return;
    let live = true;
    loadOptions(from)
      .then((opts) => {
        if (live) setLoaded(opts);
      })
      .catch((e: unknown) => {
        if (live) setLoadError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      live = false;
    };
  }, [from, loadOptions]);
  return {
    options: loaded ?? def.options,
    loading: Boolean(from && loadOptions && !loaded && !loadError),
    loadError,
  };
}

function optionNote(loading: boolean, loadError: string | null): ReactNode {
  if (loading) return <span className="text-xs text-fg-muted">{t("Loading…")}</span>;
  if (loadError)
    return (
      <span className="text-xs text-danger" role="alert">
        {t("Couldn't load the choices: {error}", { error: loadError })}
      </span>
    );
  return null;
}

function SelectRow({
  def,
  value,
  onChange,
  description,
  loadOptions,
  disabled,
}: RowProps<Extract<FieldDef, { kind: "select" }>>) {
  const { options, loading, loadError } = useFieldOptions(def, loadOptions);
  const raw =
    value === undefined || value === null || value === "" ? (def.defaultValue ?? "") : value;
  return (
    <SettingsRow
      className={disabled ? "opacity-50" : undefined}
      title={def.label}
      description={
        <>
          {description}
          {optionNote(loading, loadError)}
        </>
      }
      control={
        <div className="w-48">
          <SearchSelect
            value={String(raw)}
            onChange={(v) => onChange(def.numeric ? Number(v) : v)}
            options={options}
            disabled={disabled}
            ariaLabel={def.label}
            searchable={options.length > 7}
            placeholder={t("Select…")}
            searchPlaceholder={t("Search…")}
            noMatchesLabel={t("No matches")}
            selectLabel={t("Select")}
            closeLabel={t("Close")}
          />
        </div>
      }
    />
  );
}

function stringList(value: unknown): string[] {
  return Array.isArray(value) ? value.map((v) => String(v)) : [];
}

/** Displays a readable label for a saved value, supporting opaque JSON `{ name }` values. */
function savedOptionLabel(raw: string): string {
  if (raw.startsWith("{")) {
    try {
      const name: unknown = JSON.parse(raw)?.name;
      if (typeof name === "string" && name) return name;
    } catch {
      /* opaque option */
    }
  }
  return raw;
}

function MultiSelectRow({
  def,
  value,
  onChange,
  description,
  loadOptions,
  searchOptions,
}: RowProps<Extract<FieldDef, { kind: "multiSelect" | "orderedList" }>>) {
  const selected = useMemo(() => stringList(value), [value]);
  const emit = (next: string[]) => onChange(def.numeric ? next.map(Number) : next);
  if (def.kind === "multiSelect" && def.search && searchOptions) {
    return (
      <SearchMultiSelectRow
        def={def}
        selected={selected}
        onChange={emit}
        description={description}
        searchOptions={searchOptions}
      />
    );
  }
  return (
    <StaticMultiSelectRow
      def={def}
      selected={selected}
      onChange={emit}
      description={description}
      loadOptions={loadOptions}
    />
  );
}

function StaticMultiSelectRow({
  def,
  selected,
  onChange,
  description,
  loadOptions,
}: {
  def: Extract<FieldDef, { kind: "multiSelect" | "orderedList" }>;
  selected: string[];
  onChange: (next: string[]) => void;
  description: ReactNode;
  loadOptions?: LoadOptions;
}) {
  const { options, loading, loadError } = useFieldOptions(def, loadOptions);
  // Keep saved values that aren't in the option list visible as chips.
  const mergedOptions = useMemo(() => {
    if (!selected.length) return options;
    const known = new Set(options.map((o) => o.value));
    const extra: { value: string; label: string; description?: string }[] = [];
    for (const v of selected) {
      if (v && !known.has(v)) {
        known.add(v);
        extra.push({ value: v, label: savedOptionLabel(v) });
      }
    }
    return extra.length ? [...options, ...extra] : options;
  }, [options, selected]);
  return (
    <SettingsRow
      stacked
      title={def.label}
      description={
        <>
          {description}
          {optionNote(loading, loadError)}
        </>
      }
    >
      <MultiSelect
        value={selected}
        onChange={onChange}
        options={mergedOptions}
        ariaLabel={def.label}
        placeholder={loading ? t("Loading…") : (def.placeholder ?? t("Choose…"))}
        searchPlaceholder={t("Search…")}
        noMatchesLabel={t("No matches")}
        clearLabel={t("Clear")}
        selectAllLabel={t("Select all")}
        clearable
        maxVisibleValues={4}
      />
    </SettingsRow>
  );
}

function SearchMultiSelectRow({
  def,
  selected,
  onChange,
  description,
  searchOptions,
}: {
  def: Extract<FieldDef, { kind: "multiSelect" | "orderedList" }>;
  selected: string[];
  onChange: (next: string[]) => void;
  description: ReactNode;
  searchOptions: SearchOptions;
}) {
  const [query, setQuery] = useState("");
  const debounced = useDebouncedValue(query, 200);
  const [open, setOpen] = useState(false);
  const [results, setResults] = useState<{ value: string; label: string; description?: string }[]>(
    [],
  );
  const [searchedQuery, setSearchedQuery] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [knownLabels, setKnownLabels] = useState<Record<string, string>>({});

  const uniqueSelected = useMemo(() => {
    const seen = new Set<string>();
    const out: string[] = [];
    for (const v of selected) {
      if (v && !seen.has(v)) {
        seen.add(v);
        out.push(v);
      }
    }
    return out;
  }, [selected]);
  const selectedSet = useMemo(() => new Set(uniqueSelected), [uniqueSelected]);

  useEffect(() => {
    const needle = debounced.trim();
    if (!needle) {
      setResults([]);
      setError(null);
      setLoading(false);
      setSearchedQuery("");
      return;
    }
    const ctl = new AbortController();
    setLoading(true);
    setError(null);
    searchOptions(def.key, needle, ctl.signal)
      .then((opts) => {
        if (ctl.signal.aborted) return;
        const seen = new Set<string>();
        const deduped = opts.filter((o) => !seen.has(o.value) && seen.add(o.value));
        setResults(deduped);
        setSearchedQuery(needle);
        if (deduped.length) {
          setKnownLabels((prev) => {
            let next: Record<string, string> | null = null;
            for (const o of deduped) {
              if (prev[o.value] !== o.label) {
                if (!next) next = { ...prev };
                next[o.value] = o.label;
              }
            }
            return next ?? prev;
          });
        }
      })
      .catch((e: unknown) => {
        if (ctl.signal.aborted || isAbort(e)) return;
        setResults([]);
        setSearchedQuery(needle);
        setError(errorMessage(e));
      })
      .finally(() => {
        if (!ctl.signal.aborted) setLoading(false);
      });
    return () => ctl.abort();
  }, [debounced, def.key, searchOptions]);

  const labelOf = (v: string) =>
    def.options.find((o) => o.value === v)?.label ?? knownLabels[v] ?? savedOptionLabel(v);

  const add = (opt: { value: string; label: string }) => {
    setKnownLabels((prev) =>
      prev[opt.value] === opt.label ? prev : { ...prev, [opt.value]: opt.label },
    );
    if (!selectedSet.has(opt.value)) {
      onChange([...uniqueSelected, opt.value]);
    }
  };

  const remove = (val: string) => {
    onChange(uniqueSelected.filter((v) => v !== val));
  };

  const clearQuery = () => {
    setQuery("");
    setResults([]);
    setError(null);
    setSearchedQuery("");
    setOpen(false);
  };

  const trimmed = query.trim();
  const showEmpty =
    open &&
    !loading &&
    !error &&
    Boolean(trimmed) &&
    searchedQuery === trimmed &&
    results.length === 0;

  return (
    <SettingsRow stacked title={def.label} description={description}>
      <div className="space-y-2">
        {uniqueSelected.length > 0 ? (
          <div className="flex flex-wrap gap-1.5">
            {uniqueSelected.map((v) => {
              const label = labelOf(v);
              return (
                <Badge
                  key={v}
                  variant="secondary"
                  className="gap-1 py-1 pl-2 pr-1 text-xs font-medium text-fg"
                >
                  <span className="max-w-56 truncate">{label}</span>
                  <button
                    type="button"
                    aria-label={t("Remove {name}", { name: label })}
                    onClick={() => remove(v)}
                    className="ml-0.5 rounded-full p-0.5 text-fg-subtle hover:bg-surface-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                  >
                    <X size={12} aria-hidden />
                  </button>
                </Badge>
              );
            })}
          </div>
        ) : null}
        <div className="relative">
          <Search
            size={14}
            className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
            aria-hidden
          />
          <Input
            aria-label={def.label}
            className={query ? "pl-8 pr-8" : "pl-8"}
            value={query}
            placeholder={def.placeholder ?? t("Search…")}
            onFocus={() => {
              if (query.trim()) setOpen(true);
            }}
            onChange={(e) => {
              const next = e.currentTarget.value;
              setQuery(next);
              if (!next.trim()) {
                setResults([]);
                setError(null);
                setSearchedQuery("");
                setOpen(false);
              } else {
                setOpen(true);
              }
            }}
            onKeyDown={(e) => {
              if (e.key === "Escape") setOpen(false);
            }}
          />
          {query ? (
            <button
              type="button"
              aria-label={t("Clear")}
              onClick={clearQuery}
              className="absolute inset-y-0 right-0 flex w-8 items-center justify-center rounded-r-md text-fg-subtle hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
            >
              <X size={14} aria-hidden />
            </button>
          ) : null}
        </div>
        {open && loading ? <p className="text-xs text-fg-muted">{t("Searching…")}</p> : null}
        {open && error ? (
          <p role="status" className="text-xs text-danger">
            {error}
          </p>
        ) : null}
        {showEmpty ? <p className="text-xs text-fg-muted">{t("No matches")}</p> : null}
        {open && results.length > 0 ? (
          <ul
            role="listbox"
            aria-label={t("Search results")}
            className="max-h-60 space-y-0.5 overflow-auto rounded-lg border border-border bg-bg-subtle p-1"
          >
            {results.map((opt) => {
              const picked = selectedSet.has(opt.value);
              return (
                <li key={opt.value} role="presentation">
                  <button
                    type="button"
                    role="option"
                    aria-selected={picked}
                    disabled={picked}
                    onClick={() => add(opt)}
                    className="flex w-full items-center justify-between gap-2 rounded-md px-2.5 py-1.5 text-left text-sm transition-colors hover:bg-surface-hover disabled:opacity-60"
                  >
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-fg">{opt.label}</span>
                      {opt.description ? (
                        <span className="block truncate text-xs text-fg-muted">
                          {opt.description}
                        </span>
                      ) : null}
                    </span>
                    {picked ? (
                      <Badge variant="secondary">{t("Added")}</Badge>
                    ) : (
                      <span className="inline-flex shrink-0 items-center gap-1 text-xs font-medium text-fg-muted">
                        {t("Add")}
                        <Plus size={14} aria-hidden />
                      </span>
                    )}
                  </button>
                </li>
              );
            })}
          </ul>
        ) : null}
      </div>
    </SettingsRow>
  );
}

function OrderedListRow({
  def,
  value,
  onChange,
  description,
  loadOptions,
}: RowProps<Extract<FieldDef, { kind: "multiSelect" | "orderedList" }>>) {
  const { options, loading, loadError } = useFieldOptions(def, loadOptions);
  const current = stringList(value);
  const labelOf = (v: string) => options.find((o) => o.value === v)?.label ?? v;
  const emit = (next: string[]) => onChange(def.numeric ? next.map(Number) : next);
  const available = options.filter((o) => !current.includes(o.value));
  return (
    <SettingsRow
      stacked
      title={def.label}
      description={
        <>
          {description}
          {optionNote(loading, loadError)}
        </>
      }
    >
      <div className="space-y-2">
        {current.length ? (
          <SortableList
            items={current}
            getId={(v) => v}
            onReorder={(next) => emit(next)}
            className="space-y-1"
            renderItem={(v, handle) => (
              <div className="flex items-center gap-1 rounded-md border border-border px-1 py-0.5">
                <DragHandle handle={handle} label={t("Move {name}", { name: labelOf(v) })} />
                <span className="min-w-0 flex-1 truncate text-sm text-fg">{labelOf(v)}</span>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  aria-label={t("Remove {name}", { name: labelOf(v) })}
                  onClick={() => emit(current.filter((x) => x !== v))}
                >
                  <X size={14} />
                </Button>
              </div>
            )}
          />
        ) : (
          <p className="text-sm text-fg-muted">{t("Nothing chosen yet.")}</p>
        )}
        {available.length ? (
          <div className="w-64">
            <SearchSelect
              value=""
              onChange={(v) => {
                if (v) emit([...current, v]);
              }}
              options={available}
              ariaLabel={t("Add to {label}", { label: def.label })}
              placeholder={t("Add…")}
              searchPlaceholder={t("Search…")}
              noMatchesLabel={t("No matches")}
              selectLabel={t("Select")}
              closeLabel={t("Close")}
              searchable={available.length > 7}
            />
          </div>
        ) : null}
      </div>
    </SettingsRow>
  );
}

function LanguageRow({
  def,
  value,
  onChange,
  description,
}: RowProps<Extract<FieldDef, { kind: "language" }>>) {
  const { locale } = useLocale();
  const current = String(value ?? "");
  const options = useMemo(() => {
    let names: Intl.DisplayNames | null = null;
    try {
      names = new Intl.DisplayNames([locale], { type: "language" });
    } catch {
      names = null;
    }
    const label = (tag: string) => {
      try {
        return names?.of(tag) ?? tag;
      } catch {
        return tag;
      }
    };
    const opts = [...LANGUAGE_TAGS].map((tag) => ({ value: tag, label: `${label(tag)} (${tag})` }));
    // Bare ISO codes (en, fr) the tag list spells regionally stay selectable.
    for (const base of new Set(opts.map((o) => o.value.split("-")[0] ?? o.value))) {
      opts.push({ value: base, label: `${label(base)} (${base})` });
    }
    if (current && !opts.some((o) => o.value === current)) {
      opts.push({ value: current, label: `${label(current)} (${current})` });
    }
    return opts.sort((a, b) => a.label.localeCompare(b.label, locale));
  }, [locale, current]);
  return (
    <SettingsRow
      title={def.label}
      description={description}
      control={
        <div className="w-56">
          <SearchSelect
            value={current}
            onChange={onChange}
            options={options}
            ariaLabel={def.label}
            searchable
            placeholder={t("Select…")}
            searchPlaceholder={t("Search…")}
            noMatchesLabel={t("No matches")}
            selectLabel={t("Select")}
            closeLabel={t("Close")}
          />
        </div>
      }
    />
  );
}

function ViewerRow({
  def,
  value,
  onChange,
  description,
}: RowProps<Extract<FieldDef, { kind: "viewer" }>>) {
  const viewers = useAsync(() => api.viewers(), []);
  const options = (viewers.data ?? []).map((v) => ({
    value: String(v.id),
    label: viewerName(v),
  }));
  return (
    <SettingsRow
      title={def.label}
      description={
        <>
          {description}
          {viewers.error ? (
            <span className="text-xs text-danger" role="alert">
              {t("Couldn't load the viewers.")}
            </span>
          ) : null}
        </>
      }
      control={
        <div className="w-56">
          <SearchSelect
            value={value === null || value === undefined ? "" : String(value)}
            onChange={(v) => onChange(v === "" ? "" : Number(v))}
            options={options}
            ariaLabel={def.label}
            searchable={options.length > 7}
            placeholder={viewers.data ? t("Select…") : t("Loading…")}
            searchPlaceholder={t("Search…")}
            noMatchesLabel={t("No matches")}
            selectLabel={t("Select")}
            closeLabel={t("Close")}
          />
        </div>
      }
    />
  );
}

function CollectionRow({
  def,
  value,
  onChange,
  description,
}: RowProps<Extract<FieldDef, { kind: "collection" }>>) {
  const collections = useAsync(() => api.collections(), []);
  const options = (collections.data ?? [])
    .filter((c) => c.id)
    .map((c) => ({ value: c.id as string, label: c.name }));
  return (
    <SettingsRow
      title={def.label}
      description={
        <>
          {description}
          {collections.error ? (
            <span className="text-xs text-danger" role="alert">
              {t("Couldn't load the collections.")}
            </span>
          ) : null}
        </>
      }
      control={
        <div className="w-56">
          <SearchSelect
            value={String(value ?? "")}
            onChange={onChange}
            options={options}
            ariaLabel={def.label}
            searchable
            placeholder={collections.data ? t("Select…") : t("Loading…")}
            searchPlaceholder={t("Search…")}
            noMatchesLabel={t("No matches")}
            selectLabel={t("Select")}
            closeLabel={t("Close")}
          />
        </div>
      }
    />
  );
}

function TitleRow({
  def,
  value,
  onChange,
  description,
}: RowProps<Extract<FieldDef, { kind: "title" }>>) {
  const current = String(value ?? "");
  const [pickedName, setPickedName] = useState("");
  return (
    <SettingsRow stacked title={def.label} description={description}>
      <div className="space-y-2">
        {current ? (
          <div className="flex items-center gap-2 text-sm">
            <span className="min-w-0 flex-1 truncate text-fg">{pickedName || current}</span>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              aria-label={t("Clear {label}", { label: def.label })}
              onClick={() => {
                setPickedName("");
                onChange("");
              }}
            >
              <X size={14} />
            </Button>
          </div>
        ) : null}
        <TitleSearch
          label={def.label}
          onPick={(title) => {
            setPickedName(title.title);
            onChange(title.key);
          }}
        />
      </div>
    </SettingsRow>
  );
}

/** A group's advanced (more) fields, collapsed until asked or in error. */
function MoreFields({
  title,
  setting,
  children,
  hasError,
}: {
  title?: string;
  setting?: string;
  children: ReactNode;
  hasError: boolean;
}) {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    if (hasError) setOpen(true);
  }, [hasError]);
  return (
    <div className="border-t border-border">
      <button
        type="button"
        className="w-full px-4 py-3 text-left text-sm font-medium text-primary"
        aria-expanded={open}
        aria-label={title ? t("More options: {title}", { title }) : t("More options")}
        data-more={setting}
        onClick={() => setOpen((v) => !v)}
      >
        {t("More options")}
      </button>
      {open ? children : null}
    </div>
  );
}

/** A collapsible advanced group (plugin SettingsSchema.groups[].advanced). */
function CollapsibleGroup({
  group,
  value,
  onChange,
  errors,
  prefix,
  loadOptions,
  searchOptions,
}: {
  group: FieldGroup;
  value: Obj;
  onChange: (v: Obj) => void;
  errors: Record<string, string>;
  prefix: string;
  loadOptions?: LoadOptions;
  searchOptions?: SearchOptions;
}) {
  const hasError = [...group.fields, ...(group.more ?? [])].some((f) =>
    hasFieldError(f, errors, prefix),
  );
  const [open, setOpen] = useState(hasError);
  useEffect(() => {
    if (hasError) setOpen(true);
  }, [hasError]);
  return (
    <section className="overflow-hidden rounded-xl border border-border bg-surface">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center justify-between gap-3 px-5 py-3.5 text-left text-sm font-medium text-fg hover:bg-surface-hover"
      >
        <span className="min-w-0">
          <span className="block">{group.title || t("Advanced")}</span>
          {group.description ? (
            <span className="mt-0.5 block text-xs font-normal text-fg-muted">
              {group.description}
            </span>
          ) : null}
        </span>
        <ChevronDown
          size={16}
          className={
            open ? "shrink-0 rotate-180 transition-transform" : "shrink-0 transition-transform"
          }
          aria-hidden
        />
      </button>
      {open ? (
        <div className="divide-y divide-border border-t border-border">
          {group.fields.map((f) => (
            <SchemaField
              key={f.key}
              def={f}
              value={value[f.key]}
              path={`${prefix}.${f.key}`}
              error={errorForPath(errors, `${prefix}.${f.key}`)}
              errors={errors}
              loadOptions={loadOptions}
              searchOptions={searchOptions}
              onChange={(v) => onChange({ ...value, [f.key]: v })}
            />
          ))}
        </div>
      ) : null}
    </section>
  );
}

export function SchemaGroups({
  groups,
  value,
  onChange,
  errors,
  prefix,
  footer,
  loadOptions,
  searchOptions,
  showUnknownFields = true,
}: {
  groups: FieldGroup[];
  value: Obj;
  onChange: (v: Obj) => void;
  errors: Record<string, string>;
  prefix: string;
  footer?: ReactNode;
  loadOptions?: LoadOptions;
  searchOptions?: SearchOptions;
  showUnknownFields?: boolean;
}) {
  const primaryGroups = useMemo(() => groups.filter((g) => !g.advanced), [groups]);
  const advancedGroups = useMemo(() => groups.filter((g) => g.advanced), [groups]);
  const [active, setActive] = useState("0");
  useEffect(() => {
    const at = primaryGroups.findIndex((g) =>
      [...g.fields, ...(g.more ?? [])].some((f) => hasFieldError(f, errors, prefix)),
    );
    if (at >= 0) setActive(String(at));
  }, [errors, primaryGroups, prefix]);
  const known = new Set(groups.flatMap((g) => [...g.fields, ...(g.more ?? [])].map((f) => f.key)));
  const extra = showUnknownFields ? Object.keys(value).filter((k) => !known.has(k)) : [];
  const hasAdvanced = advancedGroups.length > 0;
  const sectionFooter = hasAdvanced ? undefined : footer;
  const body = (
    <>
      {primaryGroups.length + (extra.length ? 1 : 0) > 1 && (
        <SubtabNav
          ariaLabel={t("Settings categories")}
          active={active}
          onSelect={setActive}
          tabs={[
            ...primaryGroups.map((g, i) => ({ key: String(i), label: g.title || t("General") })),
            ...(extra.length ? [{ key: "advanced", label: t("Advanced") }] : []),
          ]}
        />
      )}
      {primaryGroups
        .filter((_, i) => String(i) === active)
        .map((g) => (
          <SettingsSection
            key={g.id ?? g.title ?? "general"}
            title={g.title}
            description={g.description}
            footer={sectionFooter}
          >
            {g.fields.map((f) => (
              <SchemaField
                key={f.key}
                def={f}
                value={value[f.key]}
                path={`${prefix}.${f.key}`}
                error={errorForPath(errors, `${prefix}.${f.key}`)}
                errors={errors}
                loadOptions={loadOptions}
                searchOptions={searchOptions}
                onChange={(v) => onChange({ ...value, [f.key]: v })}
              />
            ))}
            {g.more?.length ? (
              <MoreFields
                title={g.title}
                setting={g.id}
                hasError={g.more.some((f) => hasFieldError(f, errors, prefix))}
              >
                {g.more.map((f) => (
                  <SchemaField
                    key={`more-${f.key}`}
                    def={f}
                    value={value[f.key]}
                    path={`${prefix}.${f.key}`}
                    error={errors[`${prefix}.${f.key}`]}
                    errors={errors}
                    loadOptions={loadOptions}
                    searchOptions={searchOptions}
                    onChange={(v) => onChange({ ...value, [f.key]: v })}
                  />
                ))}
              </MoreFields>
            ) : null}
          </SettingsSection>
        ))}
      {extra.length && (active === "advanced" || primaryGroups.length === 0) ? (
        <SettingsSection
          title={t("Advanced")}
          description={t("Additional options this server supports.")}
          footer={sectionFooter}
        >
          <GenericFields
            value={value}
            keys={extra}
            onChange={onChange}
            errors={errors}
            prefix={prefix}
          />
        </SettingsSection>
      ) : null}
      {advancedGroups.map((g, idx) => (
        <CollapsibleGroup
          key={g.id ?? g.title ?? `advanced-${idx}`}
          group={g}
          value={value}
          onChange={onChange}
          errors={errors}
          prefix={prefix}
          loadOptions={loadOptions}
          searchOptions={searchOptions}
        />
      ))}
      {hasAdvanced && footer ? (
        <div className="flex items-center justify-end gap-2 rounded-xl border border-border bg-bg-subtle px-5 py-3">
          {footer}
        </div>
      ) : null}
    </>
  );
  return hasAdvanced ? <div className="space-y-4">{body}</div> : body;
}

/** Labels/help for Advanced keys that would otherwise read as raw camelCase. */
function advancedFieldMeta(key: string): { label: string; help?: string } | undefined {
  switch (key) {
    case "abrLadder":
      return {
        label: t("Adaptive bitrate ladder · Transcode"),
        help: t(
          "Advertise lower encode qualities during transcodes so clients can switch quality with bandwidth.",
        ),
      };
    case "abrLadderUnderCopy":
      return {
        label: t("Adaptive bitrate ladder · Direct Stream"),
        help: t(
          "On by default with the transcode ladder. When off, Direct Stream sessions advertise only the copy primary with no soft encode fallbacks.",
        ),
      };
    default:
      return undefined;
  }
}

/** A field definition guessed from a value, for options the UI has no schema for. */
function inferField(key: string, value: unknown): FieldDef {
  const meta = advancedFieldMeta(key);
  const label = meta?.label ?? humanizeKey(key);
  if (typeof value === "boolean") return { key, label, kind: "bool", help: meta?.help };
  if (typeof value === "number") return { key, label, kind: "number" };
  if (typeof value === "string")
    return {
      key,
      label,
      kind:
        value === REDACTED || /password|secret|token|apikey|key$/i.test(key) ? "secret" : "text",
    };
  if (
    value === null ||
    (Array.isArray(value) && value.every((x) => typeof x === "string" || typeof x === "number"))
  )
    return {
      key,
      label,
      kind: "list",
      numeric: Array.isArray(value) && typeof value[0] === "number",
    };
  return { key, label, kind: "json" };
}

/** Renders keys the UI has no schema for, so new server options are never unreachable. */
export function GenericFields({
  value,
  keys,
  onChange,
  errors,
  prefix,
}: {
  value: Obj;
  keys: string[];
  onChange: (v: Obj) => void;
  errors: Record<string, string>;
  prefix: string;
}) {
  return (
    <>
      {keys.map((k) => {
        const path = `${prefix}.${k}`;
        return (
          <SchemaField
            key={k}
            def={inferField(k, value[k])}
            value={value[k]}
            onChange={(nv) => onChange({ ...value, [k]: nv })}
            path={path}
            error={errors[path]}
          />
        );
      })}
    </>
  );
}

function JsonRow({
  label,
  value,
  onChange,
  error,
}: {
  label: string;
  value: unknown;
  onChange: (v: unknown) => void;
  error?: string;
}) {
  const text = JSON.stringify(value, null, 2);
  return (
    <SettingsRow
      stacked
      title={label}
      description={error ? <span className="text-danger">{error}</span> : t("Edited as JSON.")}
    >
      <Textarea
        key={text}
        defaultValue={text}
        rows={Math.min(12, text.split("\n").length + 1)}
        className="font-mono text-xs"
        aria-label={label}
        onBlur={(e) => {
          try {
            onChange(JSON.parse(e.currentTarget.value));
          } catch {
            /* keep the last valid value */
          }
        }}
      />
    </SettingsRow>
  );
}

export function asObj(v: unknown): Obj {
  return isPlainObject(v) ? v : {};
}

function DurationControl({
  id,
  label,
  value,
  onChange,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (v: string) => void;
}) {
  const [draft, setDraft] = useState(() => ({ ...parseDuration(value), raw: value }));
  const current = draft.raw === value ? draft : { ...parseDuration(value), raw: value };
  function update(amount: string, unit: DurationUnit) {
    const raw = formatDuration(amount, unit);
    setDraft({ amount, unit, raw });
    onChange(raw);
  }
  const count = Number(current.amount) || 0;
  return (
    <span className="flex items-center gap-2">
      <Input
        id={id}
        aria-label={label}
        type="number"
        min="0"
        step="any"
        value={current.amount}
        className="w-24"
        onChange={(e) => update(e.currentTarget.value, current.unit)}
      />
      <Select
        aria-label={t("{label} unit", { label })}
        value={current.unit}
        onChange={(e) => update(current.amount, e.currentTarget.value as DurationUnit)}
      >
        <option value="s">{tn(count, "Second", "Seconds")}</option>
        <option value="m">{tn(count, "Minute", "Minutes")}</option>
        <option value="h">{tn(count, "Hour", "Hours")}</option>
        <option value="d">{tn(count, "Day", "Days")}</option>
      </Select>
    </span>
  );
}

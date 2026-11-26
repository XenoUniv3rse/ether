// The one row drawer (design §2.6): "+" opens it at "What fills this row?",
// picking a source turns it into that row's settings, and clicking a row
// opens the same drawer to edit it. Every change goes straight into the page
// draft, so the page behind it previews it as you go.
import { Badge, Button, Input, Spinner, Switch, cn } from "@braedonsaunders/appkit-ui";
import {
  ArrowDown,
  ArrowUp,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  GripVertical,
  Pencil,
  Plus,
  Puzzle,
  Search,
  X,
} from "lucide-react";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { editCollectionHref, newCollectionHref, pluginCollectionHref } from "../collections/links";
import { ChipGroup, Segmented } from "../../components/fields";
import { SubtabNav } from "../../components/layout";
import { ToolbarPicker } from "../../components/ToolbarPicker";
import { viewerName } from "../../components/pickers";
import { t, tn } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { api, errorMessage, isAbort } from "../../lib/api";
import type { Device, Viewer } from "../../lib/contract";
import {
  MAX_ROWS_PER_LIBRARY_ROW,
  SOURCE_GROUP_ORDER,
  daypartLabel,
  deviceLabel,
  fillLibrary,
  hasCollectionCardSelection,
  hideWatchedLabel,
  holidayGroupLabel,
  holidays,
  monthNames,
  presetOf,
  presetSchedule,
  sourceGroupLabel,
  withMode,
  type Daypart,
  type Experience,
  type FacetValueOption,
  type PageLibrary,
  type PageRow,
  type PageSource,
  type PlacementMode,
  type PreviewRow,
  type RowCard,
  type RowLook,
  type RowParam,
  type RowPlacement,
  type RowStyle,
  type Schedule,
  type SchedulePreset,
} from "../../lib/pages";
import { KindChip, RowGrid, VelaOnlyChip } from "./RowVisuals";

export type DrawerState =
  | {
      kind: "add";
      at: number;
      picking: boolean;
      tab?: "settings" | "content";
      preset?: { source: string; params: Record<string, string> };
    }
  | { kind: "edit"; rowId: string; tab?: "settings" | "content" };

export interface DrawerContext {
  pageKey: string;
  curated: boolean;
  /** The library of a library page; null on Home. */
  library: PageLibrary | null;
  /** Every library a Home row can draw from. */
  libraries: PageLibrary[];
  sources: PageSource[] | null;
  sourcesError: unknown;
  viewers: Viewer[];
  /** Editing for one viewer: only Look and Placement change, just for them. */
  editingFor: Viewer | null;
  preview: { viewer?: number; device: Device; at: string };
  /** The draft's rows, so the picker says what's already on the page. */
  rows?: PageRow[];
}

/** "New collection…": Collection Studio, which comes back here with it as a new row. */
function newCollectionLink(pageKey: string): string {
  const back = new URLSearchParams({ page: pageKey });
  return newCollectionHref({ returnTo: `/recommendations?${back}&add=collection:` });
}

// ---------------------------------------------------------------- layout bits

function Section({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <section className="space-y-2" aria-label={title}>
      <div>
        <h3 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">{title}</h3>
        {hint ? <p className="mt-0.5 text-sm text-fg-muted">{hint}</p> : null}
      </div>
      <div className="space-y-3.5 rounded-xl border border-border bg-surface p-3.5 text-sm">
        {children}
      </div>
    </section>
  );
}

function Line({
  label,
  hint,
  children,
}: {
  label: ReactNode;
  hint?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className="space-y-1 border-t border-border-subtle pt-3 first:border-0 first:pt-0">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="font-medium text-fg">{label}</span>
        {children}
      </div>
      {hint ? <p className="text-xs text-fg-muted">{hint}</p> : null}
    </div>
  );
}

function NumberField({
  label,
  value,
  onChange,
  min = 1,
  max = 50,
  disabled,
}: {
  label: string;
  value: number | undefined;
  onChange: (v: number | undefined) => void;
  min?: number;
  max?: number;
  disabled?: boolean;
}) {
  return (
    <Input
      aria-label={label}
      type="number"
      inputMode="numeric"
      min={min}
      max={max}
      className="h-8 w-20"
      disabled={disabled}
      value={value ?? ""}
      onChange={(e) => {
        const raw = e.currentTarget.value;
        if (raw === "") return onChange(undefined);
        const n = Math.round(Number(raw));
        if (Number.isFinite(n)) onChange(Math.min(max, Math.max(min, n)));
      }}
    />
  );
}

// ---------------------------------------------------------------- source picker

/** A plugin's slug label ("popular-week") in words ("Popular week"). */
function humanize(label: string): string {
  if (!/^[a-z0-9]+([-_][a-z0-9]+)+$/.test(label)) return label;
  const words = label.replace(/[-_]/g, " ");
  return words.charAt(0).toUpperCase() + words.slice(1);
}

interface PickEntry {
  key: string;
  /** Plugin entries sit under their library or plugin ("Music", "Cinemeta"). */
  sub?: string;
  /** Extra words search matches: genres and other choices, library names. */
  terms: string;
  source: PageSource;
  title: string;
  description: string;
  params: Record<string, string>;
  /** Rows of exactly this entry (this collection, this shelf) in the draft. */
  used: number;
}

function entriesFor(
  sources: PageSource[],
  rows: PageRow[] | undefined,
  libraryNames: Map<number, string>,
): PickEntry[] {
  const out: PickEntry[] = [];
  const usedBy = (kind: string, key?: string, value?: string) =>
    rows
      ? rows.filter((r) => r.source.kind === kind && (!key || r.source.params?.[key] === value))
          .length
      : 0;
  for (const s of sources) {
    if (!s.available) continue;
    const ref = s.params?.find((p) => p.type === "ref" && p.required);
    const terms = [
      s.plugin ?? "",
      ...(s.params ?? []).flatMap((p) => (p.choices ?? []).map((c) => c.label)),
      ...(s.libraries ?? []).map((id) => libraryNames.get(id) ?? ""),
    ].join(" ");
    if (ref && s.refs?.length) {
      for (const r of s.refs) {
        // A shelf's label is "<Library> › <shelf>" on Home: the shelf is the
        // title, its library and plugin go under it.
        const [lib, shelf] = r.label.includes(" › ")
          ? (r.label.split(" › ", 2) as [string, string])
          : ["", r.label];
        const sub = s.group === "plugins" ? lib || s.plugin || undefined : undefined;
        out.push({
          key: `${s.kind}:${r.value}`,
          source: s,
          sub,
          terms: `${terms} ${lib}`,
          title: humanize(shelf),
          description:
            s.kind === "collection"
              ? t("Your collection")
              : [s.plugin, lib].filter(Boolean).join(" · ") || s.title,
          params: { [ref.key]: r.value },
          used: rows ? usedBy(s.kind, ref.key, r.value) : s.used,
        });
      }
      continue;
    }
    if (ref && s.kind === "collection") continue; // no collections yet: "New collection…" covers it
    out.push({
      key: s.kind,
      source: s,
      sub: s.group === "plugins" ? s.plugin || undefined : undefined,
      terms,
      title: humanize(s.title),
      description: s.description,
      params: {},
      used: rows ? usedBy(s.kind) : s.used,
    });
  }
  return out;
}

function PickButton({
  entry: e,
  heading,
  pluginGroup,
  onPick,
}: {
  entry: PickEntry;
  heading?: string;
  pluginGroup: boolean;
  onPick: (s: PageSource, params: Record<string, string>) => void;
}) {
  return (
    <>
      {heading ? (
        <p className="col-span-full mt-2 text-[11px] font-semibold text-fg-muted first:mt-0">
          {heading}
        </p>
      ) : null}
      <button
        type="button"
        aria-label={e.title}
        onClick={() => onPick(e.source, e.params)}
        className="block w-full rounded-lg border border-transparent px-2.5 py-1.5 text-left hover:border-border hover:bg-surface"
      >
        <span className="flex flex-wrap items-center gap-1.5">
          <span className="text-sm font-medium text-fg">{e.title}</span>
          <KindChip personal={e.source.personal} />
          {e.source.kind === "surface" || e.source.kind.startsWith("surface:") ? (
            <VelaOnlyChip />
          ) : null}
          {e.used > 0 ? <span className="text-[11px] text-fg-subtle">{t("used here")}</span> : null}
        </span>
        <span className="block text-xs text-fg-muted">
          {pluginGroup && e.source.plugin && !e.description.includes(e.source.plugin)
            ? `${e.source.plugin} · ${e.description}`
            : e.description}
        </span>
      </button>
    </>
  );
}

function SourcePicker({
  sources,
  error,
  onPick,
  rows,
  pageKey,
  libraries,
}: {
  sources: PageSource[] | null;
  error: unknown;
  onPick: (s: PageSource, params: Record<string, string>) => void;
  rows?: PageRow[];
  pageKey: string;
  libraries: PageLibrary[];
}) {
  const [q, setQ] = useState("");
  const { locale } = useLocale();
  const groups = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const names = new Map(libraries.map((l) => [l.id, l.name]));
    const entries = entriesFor(sources ?? [], rows, names).filter(
      (e) => !needle || `${e.title} ${e.description} ${e.terms}`.toLowerCase().includes(needle),
    );
    const byGroup = new Map<string, PickEntry[]>();
    for (const e of entries) {
      const g = e.source.group || "more";
      byGroup.set(g, [...(byGroup.get(g) ?? []), e]);
    }
    return [...byGroup.entries()]
      .sort(
        ([a], [b]) =>
          ((SOURCE_GROUP_ORDER.indexOf(a) + 100) % 100) -
          ((SOURCE_GROUP_ORDER.indexOf(b) + 100) % 100),
      )
      .map(([g, items]) => ({
        group: g,
        label: sourceGroupLabel(g),
        // Plugin rows sit together under their library or plugin.
        items:
          g === "plugins"
            ? [...items].sort((a, b) => (a.sub ?? "").localeCompare(b.sub ?? ""))
            : items,
      }));
  }, [sources, q, locale, rows, libraries]);
  const canCollect = (sources ?? []).some((s) => s.kind === "collection" && s.available);
  if (error) return <p className="text-sm text-danger">{errorMessage(error)}</p>;
  if (!sources) return <Spinner size={18} label={t("Loading what can fill a row")} />;
  return (
    <div className="space-y-4">
      <div>
        <h3 className="text-base font-semibold text-fg">{t("What fills this row?")}</h3>
        <p className="text-sm text-fg-muted">
          {t(
            "Built-in rows, your collections and plugin rows that fit this page. Pick one to set it up.",
          )}
        </p>
      </div>
      {sources.some((source) => source.unavailable === "metadata_source_unavailable") ? (
        <p className="text-sm text-fg-muted">
          {t("Some discovery rows need a metadata source that provides lists.")}
        </p>
      ) : null}
      <div className="relative">
        <Search
          size={14}
          className="absolute left-2.5 top-1/2 -translate-y-1/2 text-fg-subtle"
          aria-hidden
        />
        <Input
          aria-label={t("Search rows")}
          className="h-9 pl-8"
          placeholder={t("Search: trending, a genre, a library, playlists…")}
          value={q}
          onChange={(e) => setQ(e.currentTarget.value)}
        />
      </div>
      {groups.length === 0 ? (
        <p className="text-sm text-fg-muted">{t("Nothing matches that search.")}</p>
      ) : null}
      <div className="space-y-4">
        {groups.map((g) => (
          <div key={g.group} role="group" aria-label={g.label}>
            <h4 className="mb-1 text-xs font-semibold uppercase tracking-wider text-fg-subtle">
              {g.label}
            </h4>
            <div className="grid items-start gap-x-4 gap-y-0.5 sm:grid-cols-2">
              {g.group === "collections" && canCollect ? (
                <Link
                  to={newCollectionLink(pageKey)}
                  className="col-span-full flex w-full flex-wrap items-baseline gap-x-1.5 rounded-lg border border-transparent px-2.5 py-1.5 text-sm font-medium text-primary hover:border-border hover:bg-surface"
                >
                  <Plus size={13} /> {t("New collection…")}
                  <span className="text-xs font-normal text-fg-muted">
                    {t("Opens Collection Studio, then comes back here")}
                  </span>
                </Link>
              ) : null}
              {g.items.map((e, i) => (
                <PickButton
                  key={e.key}
                  entry={e}
                  heading={e.sub && e.sub !== g.items[i - 1]?.sub ? e.sub : undefined}
                  pluginGroup={g.group === "plugins"}
                  onPick={onPick}
                />
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------- parameters

function FacetPicker({
  param,
  value,
  library,
  onChange,
  disabled,
}: {
  param: RowParam;
  value: string;
  library: number | null;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  const [pinning, setPinning] = useState(Boolean(value));
  const [q, setQ] = useState("");
  const [values, setValues] = useState<FacetValueOption[] | null>(null);
  useEffect(() => {
    if (!pinning || library == null || !param.facet) return;
    const ctl = new AbortController();
    const timer = window.setTimeout(() => {
      api
        .facetValues(library, param.facet!, q, ctl.signal)
        .then((r) => setValues(r.values ?? []))
        .catch((e) => {
          if (!isAbort(e)) setValues([]);
        });
    }, 200);
    return () => {
      window.clearTimeout(timer);
      ctl.abort();
    };
  }, [pinning, library, param.facet, q]);
  return (
    <div className="space-y-2 border-t border-border-subtle pt-3 first:border-0 first:pt-0">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="font-medium text-fg">{param.label}</span>
        <Segmented
          ariaLabel={param.label}
          size="sm"
          value={pinning ? "pin" : "ether"}
          onChange={(v) => {
            setPinning(v === "pin");
            if (v === "ether") onChange("");
          }}
          options={[
            { value: "ether", label: t("Ether picks for each viewer"), disabled },
            { value: "pin", label: t("Pick one…"), disabled },
          ]}
        />
      </div>
      {pinning ? (
        <div className="space-y-2">
          {value ? (
            <p className="text-sm">
              {t("Pinned to:")}{" "}
              <Badge variant="info">{values?.find((v) => v.value === value)?.label ?? value}</Badge>
            </p>
          ) : null}
          <Input
            aria-label={t("Search {facet}", { facet: param.label })}
            className="h-8"
            placeholder={t("Search…")}
            value={q}
            disabled={disabled}
            onChange={(e) => setQ(e.currentTarget.value)}
          />
          {library == null ? (
            <p className="text-xs text-fg-muted">{t("Choose a library first.")}</p>
          ) : values === null ? (
            <Spinner size={14} />
          ) : (
            <div className="flex max-h-40 flex-wrap gap-1.5 overflow-y-auto">
              {values.map((v) => (
                <button
                  key={v.value}
                  type="button"
                  disabled={disabled}
                  onClick={() => onChange(v.value)}
                  className={cn(
                    "rounded-full border px-2.5 py-0.5 text-xs",
                    v.value === value
                      ? "border-primary bg-primary text-primary-fg"
                      : "border-border hover:border-primary",
                  )}
                >
                  {v.label}
                  {v.count ? <span className="ml-1 opacity-70">{v.count}</span> : null}
                </button>
              ))}
            </div>
          )}
        </div>
      ) : null}
    </div>
  );
}

function CollectionOrder({
  value,
  source,
  max,
  onChange,
  disabled,
}: {
  value: string;
  source: PageSource;
  max: number;
  onChange: (value: string) => void;
  disabled: boolean;
}) {
  const ids = value.split(",").filter(Boolean);
  const refs = source.refs ?? [];
  const names = new Map(refs.map((ref) => [ref.value, ref.label]));
  const selected = new Set(ids);
  const choices = refs.filter((ref) => !selected.has(ref.value));
  const move = (index: number, direction: number) => {
    const order = [...ids];
    [order[index], order[index + direction]] = [order[index + direction]!, order[index]!];
    onChange(order.join(","));
  };
  return (
    <div className="space-y-2">
      <ol className="space-y-1">
        {ids.map((id, index) => {
          const name = names.get(id) || t("Unavailable collection");
          return (
            <li
              key={id}
              className="flex items-center gap-1 rounded-lg border border-border px-2 py-1"
            >
              <span className="min-w-0 flex-1 truncate">{name}</span>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                aria-label={t("Move {name} up", { name })}
                disabled={disabled || index === 0}
                onClick={() => move(index, -1)}
              >
                <ArrowUp size={14} />
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                aria-label={t("Move {name} down", { name })}
                disabled={disabled || index === ids.length - 1}
                onClick={() => move(index, 1)}
              >
                <ArrowDown size={14} />
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                aria-label={t("Remove {name}", { name })}
                disabled={disabled}
                onClick={() => onChange(ids.filter((other) => other !== id).join(","))}
              >
                <X size={14} />
              </Button>
            </li>
          );
        })}
      </ol>
      {!disabled && choices.length > 0 && ids.length < max ? (
        <ToolbarPicker
          ariaLabel={t("Add a collection")}
          value=""
          onChange={(id) => id && onChange([...ids, id].join(","))}
          searchable
          options={[{ value: "", label: t("Add a collection") }, ...choices]}
        />
      ) : null}
    </div>
  );
}

function ParamControl({
  param,
  source,
  value,
  library,
  onChange,
  disabled,
}: {
  param: RowParam;
  source: PageSource;
  value: string;
  library: number | null;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  if (param.type === "bool") {
    return (
      <Line label={param.label}>
        <Switch
          aria-label={param.label}
          checked={(value || param.default) === "true"}
          disabled={disabled}
          onChange={(e) => onChange(String(e.currentTarget.checked))}
        />
      </Line>
    );
  }
  if (param.type === "collectionList") {
    return (
      <CollectionOrder
        value={value}
        source={source}
        max={param.max ?? 50}
        onChange={onChange}
        disabled={!!disabled}
      />
    );
  }
  if (param.type === "text") {
    return (
      <Line label={t("Collection name starts with")}>
        <Input
          aria-label={t("Collection name starts with")}
          value={value}
          maxLength={param.max}
          disabled={disabled}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
      </Line>
    );
  }
  if (param.type === "facetValue") {
    return (
      <FacetPicker
        param={param}
        value={value}
        library={library}
        onChange={onChange}
        disabled={disabled}
      />
    );
  }
  if (param.type === "count") {
    return (
      <Line label={param.label}>
        <NumberField
          label={param.label}
          value={value ? Number(value) : param.default ? Number(param.default) : undefined}
          min={param.min || 1}
          max={param.max || 10}
          disabled={disabled}
          onChange={(n) => onChange(n === undefined ? "" : String(n))}
        />
      </Line>
    );
  }
  const choices = param.type === "ref" ? (source.refs ?? []) : (param.choices ?? []);
  const current = value || param.default || "";
  if (param.type === "choice" && choices.length <= 4) {
    return (
      <Line label={param.label}>
        <Segmented
          ariaLabel={param.label}
          size="sm"
          value={current}
          onChange={onChange}
          options={choices.map((c) => ({ value: c.value, label: c.label, disabled }))}
        />
      </Line>
    );
  }
  return (
    <Line label={param.label}>
      <ToolbarPicker
        ariaLabel={param.label}
        value={current}
        onChange={onChange}
        disabled={disabled}
        searchable={choices.length > 8}
        options={[
          ...(param.required ? [] : [{ value: "", label: t("Any") }]),
          ...choices.map((c) => ({ value: c.value, label: c.label })),
        ]}
      />
    </Line>
  );
}

// ---------------------------------------------------------------- libraries

function LibrariesSection({
  row,
  source,
  libraries,
  onChange,
  disabled,
}: {
  row: PageRow;
  source: PageSource;
  libraries: PageLibrary[];
  onChange: (r: PageRow) => void;
  disabled?: boolean;
}) {
  const supported = libraries.filter((l) => !source.libraries || source.libraries.includes(l.id));
  const unsupported = libraries.filter((l) => !supported.includes(l));
  const chosen = row.scope.libraries ?? [];
  const these = chosen.length > 0;
  const per = Boolean(row.scope.perLibrary);
  const firstExp = libraries.find((l) => l.id === chosen[0])?.experience;
  const offered = supported.filter((l) => per || !these || !firstExp || l.experience === firstExp);
  const otherKind = supported.filter((l) => !offered.includes(l));
  const fanCount = per ? (these ? chosen.length : supported.length) : 1;
  const set = (scope: PageRow["scope"]) =>
    onChange({
      ...row,
      scope: {
        libraries: scope.libraries?.length ? scope.libraries : undefined,
        perLibrary: scope.perLibrary || undefined,
      },
    });
  return (
    <Section title={t("Libraries")} hint={t("Where the titles come from.")}>
      <Line label={t("Draw from")}>
        <Segmented
          ariaLabel={t("Draw from")}
          size="sm"
          value={these ? "these" : "any"}
          onChange={(v) =>
            set({
              ...row.scope,
              libraries: v === "these" ? supported.slice(0, 1).map((l) => l.id) : undefined,
            })
          }
          options={[
            { value: "any", label: t("Any library you can watch"), disabled },
            { value: "these", label: t("These libraries"), disabled },
          ]}
        />
      </Line>
      {these ? (
        <LibraryOrder
          chosen={chosen}
          offered={offered}
          libraries={libraries}
          per={per}
          disabled={disabled}
          onChange={(v) => set({ ...row.scope, libraries: v })}
        />
      ) : null}
      {these && otherKind.length ? (
        <p className="text-xs text-fg-muted">
          {t("{libraries}: a different kind of library. Use one row per library to include them.", {
            libraries: otherKind.map((l) => l.name).join(", "),
          })}
        </p>
      ) : null}
      {unsupported.length ? (
        <p className="text-xs text-fg-muted">
          {t("{libraries} can't fill this row.", {
            libraries: unsupported.map((l) => l.name).join(", "),
          })}
        </p>
      ) : null}
      {source.split ? (
        <Line
          label={t("Show as")}
          hint={
            per
              ? fanCount > MAX_ROWS_PER_LIBRARY_ROW
                ? t("Only the first {max} libraries get a row.", { max: MAX_ROWS_PER_LIBRARY_ROW })
                : tn(
                    fanCount,
                    "One row, kept together with the others. Up to {max} libraries.",
                    "{count} rows kept together, one per library. Up to {max} libraries.",
                    { max: MAX_ROWS_PER_LIBRARY_ROW },
                  )
              : t("One row mixing the libraries.")
          }
        >
          <Segmented
            ariaLabel={t("Show as")}
            size="sm"
            value={per ? "per" : "one"}
            onChange={(v) => set({ ...row.scope, perLibrary: v === "per" })}
            options={[
              { value: "one", label: t("One combined row"), disabled },
              { value: "per", label: t("One row per library"), disabled },
            ]}
          />
        </Line>
      ) : null}
    </Section>
  );
}

/**
 * The chosen libraries as an ordered list: drag a chip, or use its arrows,
 * to set the order (a one-row-per-library row shows its rows in this order).
 * The libraries not chosen sit below, to add.
 */
function LibraryOrder({
  chosen,
  offered,
  libraries,
  per,
  disabled,
  onChange,
}: {
  chosen: number[];
  offered: PageLibrary[];
  libraries: PageLibrary[];
  per: boolean;
  disabled?: boolean;
  onChange: (ids: number[]) => void;
}) {
  const dragFrom = useRef<number | null>(null);
  const name = (id: number) => libraries.find((l) => l.id === id)?.name ?? String(id);
  const move = (from: number, to: number) => {
    if (disabled || from === to || to < 0 || to >= chosen.length) return;
    const next = [...chosen];
    const [id] = next.splice(from, 1);
    next.splice(to, 0, id!);
    onChange(next);
  };
  const addable = offered.filter((l) => !chosen.includes(l.id));
  return (
    <div className="space-y-2">
      <ol className="flex flex-wrap gap-1.5" aria-label={t("Libraries, in order")}>
        {chosen.map((id, i) => (
          <li
            key={id}
            draggable={!disabled && chosen.length > 1}
            onDragStart={(e) => {
              e.dataTransfer.effectAllowed = "move";
              e.dataTransfer.setData("text/plain", String(id));
              dragFrom.current = i;
            }}
            onDragOver={(e) => e.preventDefault()}
            onDrop={(e) => {
              e.preventDefault();
              if (dragFrom.current != null) move(dragFrom.current, i);
              dragFrom.current = null;
            }}
            className="inline-flex items-center gap-1 rounded-full border border-primary bg-primary-subtle py-0.5 pl-1.5 pr-1 text-xs text-fg"
          >
            {chosen.length > 1 ? (
              <GripVertical size={11} className="cursor-grab text-fg-subtle" aria-hidden />
            ) : null}
            {per && chosen.length > 1 ? (
              <span className="font-semibold tabular-nums text-primary">{i + 1}.</span>
            ) : null}
            {name(id)}
            {chosen.length > 1 ? (
              <>
                <button
                  type="button"
                  disabled={disabled || i === 0}
                  onClick={() => move(i, i - 1)}
                  aria-label={t("Move {library} earlier", { library: name(id) })}
                  className="rounded p-0.5 hover:bg-surface disabled:opacity-30"
                >
                  <ChevronLeft size={11} />
                </button>
                <button
                  type="button"
                  disabled={disabled || i === chosen.length - 1}
                  onClick={() => move(i, i + 1)}
                  aria-label={t("Move {library} later", { library: name(id) })}
                  className="rounded p-0.5 hover:bg-surface disabled:opacity-30"
                >
                  <ChevronRight size={11} />
                </button>
              </>
            ) : null}
            <button
              type="button"
              disabled={disabled || chosen.length === 1}
              onClick={() => onChange(chosen.filter((x) => x !== id))}
              aria-label={t("Remove {library}", { library: name(id) })}
              className="rounded p-0.5 hover:bg-surface disabled:opacity-30"
            >
              <X size={11} />
            </button>
          </li>
        ))}
      </ol>
      {addable.length ? (
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="text-xs text-fg-muted">{t("Add")}</span>
          {addable.map((l) => (
            <button
              key={l.id}
              type="button"
              disabled={disabled}
              onClick={() => onChange([...chosen, l.id])}
              className="inline-flex items-center gap-1 rounded-full border border-dashed border-border px-2 py-0.5 text-xs text-fg-muted hover:border-primary hover:text-primary"
            >
              <Plus size={11} /> {l.name}
            </button>
          ))}
        </div>
      ) : null}
      {per && chosen.length > 1 ? (
        <p className="text-xs text-fg-muted">
          {t("The rows show in this order. Drag a library, or use its arrows, to change it.")}
        </p>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------- look

function triState(v: boolean | null | undefined): "source" | "on" | "off" {
  return v == null ? "source" : v ? "on" : "off";
}
function fromTri(v: string): boolean | null {
  return v === "source" ? null : v === "on";
}

/** One Look choice: the row's style and card shape together. */
type LookKind = "posters" | "landscape" | "hero" | "squares" | "round" | "tiles" | "tracks";
const WATCH_LOOKS: LookKind[] = ["posters", "landscape", "hero"];
const LISTEN_LOOKS: LookKind[] = ["squares", "round", "tiles", "tracks"];
const LOOK_VALUES: Record<LookKind, Pick<RowLook, "style" | "card">> = {
  posters: { style: "standard", card: "poster" },
  landscape: { style: "wide", card: "landscape" },
  hero: { style: "hero", card: "" },
  squares: { style: "standard", card: "square" },
  round: { style: "standard", card: "round" },
  tiles: { style: "tiles", card: "" },
  tracks: { style: "tracks", card: "" },
};

/** Which Look a style and card shape amount to. */
function lookOf(style: RowStyle, card: RowCard, listen: boolean): LookKind {
  if (style === "hero") return "hero";
  if (style === "tiles") return "tiles";
  if (style === "tracks") return "tracks";
  if (style === "wide" || card === "landscape") return "landscape";
  if (card === "round") return "round";
  if (card === "square") return "squares";
  if (card === "poster") return "posters";
  return listen ? "squares" : "posters";
}

function lookLabel(k: LookKind): string {
  switch (k) {
    case "posters":
      return t("Posters");
    case "landscape":
      return t("Landscape");
    case "hero":
      return t("Hero");
    case "squares":
      return t("Squares");
    case "round":
      return t("Round");
    case "tiles":
      return t("Tiles");
    case "tracks":
      return t("Track list");
  }
}

/** A mini sketch of a row in this look. */
function LookSketch({ look }: { look: LookKind }) {
  const box = "rounded-[3px] bg-fg-subtle/45";
  const frame = "flex h-12 items-center gap-1 overflow-hidden rounded-md bg-bg-subtle px-1.5";
  switch (look) {
    case "posters":
      return (
        <span className={frame} aria-hidden>
          {[0, 1, 2, 3, 4].map((i) => (
            <span key={i} className={cn(box, "aspect-[2/3] h-9 shrink-0")} />
          ))}
        </span>
      );
    case "landscape":
      return (
        <span className={frame} aria-hidden>
          {[0, 1, 2].map((i) => (
            <span key={i} className={cn(box, "aspect-video h-6 shrink-0")} />
          ))}
        </span>
      );
    case "hero":
      return (
        <span
          className={cn(frame, "flex-col items-stretch justify-center gap-0.5 py-1")}
          aria-hidden
        >
          <span className={cn(box, "h-6 w-full")} />
          <span className="flex gap-0.5">
            {[0, 1, 2, 3, 4].map((i) => (
              <span key={i} className={cn(box, "h-2.5 flex-1")} />
            ))}
          </span>
        </span>
      );
    case "squares":
    case "round":
      return (
        <span className={frame} aria-hidden>
          {[0, 1, 2, 3].map((i) => (
            <span
              key={i}
              className={cn(box, "aspect-square h-7 shrink-0", look === "round" && "rounded-full")}
            />
          ))}
        </span>
      );
    case "tiles":
      return (
        <span className={cn(frame, "grid grid-cols-3 gap-1 py-1.5")} aria-hidden>
          {[0, 1, 2, 3, 4, 5].map((i) => (
            <span key={i} className={cn(box, "h-3.5")} />
          ))}
        </span>
      );
    case "tracks":
      return (
        <span className={cn(frame, "flex-col items-stretch justify-center gap-1")} aria-hidden>
          {[0, 1, 2].map((i) => (
            <span key={i} className="flex items-center gap-1">
              <span className={cn(box, "size-2.5 shrink-0")} />
              <span className="h-1.5 flex-1 rounded-full bg-fg-subtle/30" />
            </span>
          ))}
        </span>
      );
  }
}

function LookSection({
  look,
  source,
  experience,
  onChange,
}: {
  look: RowLook;
  source: PageSource;
  experience: Experience;
  onChange: (l: RowLook) => void;
}) {
  const listen =
    experience === "listen" || source.look.style === "tiles" || source.look.style === "tracks";
  const usual = lookOf(source.look.style ?? "", source.look.card ?? "", listen);
  const current =
    look.style || look.card ? lookOf(look.style ?? "", look.card ?? "", listen) : usual;
  const collectionCards = source.kind === "collection-cards";
  const choices: LookKind[] = collectionCards
    ? ["posters", "landscape"]
    : [...(listen ? LISTEN_LOOKS : WATCH_LOOKS)];
  if (!choices.includes(current)) choices.push(current);
  const patch = (p: Partial<RowLook>) => {
    const next: RowLook = { ...look, ...p };
    for (const k of Object.keys(next) as (keyof RowLook)[]) {
      if (next[k] === null || next[k] === undefined || next[k] === "" || next[k] === false)
        delete next[k];
    }
    onChange(next);
  };
  return (
    <Section title={t("Look")} hint={t("Changes show in the page as you make them.")}>
      <div className="space-y-2">
        <div
          role="radiogroup"
          aria-label={t("Look")}
          className={cn(
            "grid gap-2",
            collectionCards
              ? "grid-cols-2"
              : choices.length > 3
                ? "grid-cols-2 sm:grid-cols-4"
                : "grid-cols-3",
          )}
        >
          {choices.map((k) => {
            const on = k === current;
            return (
              <button
                key={k}
                type="button"
                role="radio"
                aria-checked={on}
                onClick={() =>
                  // The row's usual look follows the source; the others are set.
                  k === usual ? patch({ style: "", card: "" }) : patch(LOOK_VALUES[k])
                }
                className={cn(
                  "flex flex-col items-stretch gap-1.5 rounded-lg border p-2 text-left transition-colors",
                  on
                    ? "border-primary bg-primary-subtle/40"
                    : "border-border hover:border-primary/60",
                )}
              >
                <LookSketch look={k} />
                <span className="text-xs font-medium text-fg">{lookLabel(k)}</span>
                {k === usual ? (
                  <span className="-mt-1 text-[11px] text-fg-muted">
                    {t("This row's usual look")}
                  </span>
                ) : null}
              </button>
            );
          })}
        </div>
        {!collectionCards ? (
          <p className="text-xs text-fg-muted">
            {listen
              ? t(
                  "Plex, Vela and Silo show these looks; Jellyfin and Emby keep their own. Tiles and Track list are Vela's listening layouts.",
                )
              : t("Plex, Vela and Silo show these looks; Jellyfin and Emby keep their own.")}
          </p>
        ) : null}
      </div>
      {!collectionCards ? (
        <>
          <Line
            label={hideWatchedLabel(experience)}
            hint={t("Leave out titles the viewer has finished. Every app honours it.")}
          >
            <Switch
              aria-label={hideWatchedLabel(experience)}
              checked={Boolean(look.hideWatched)}
              onChange={(e) => patch({ hideWatched: e.currentTarget.checked })}
            />
          </Line>
          <Line
            label={t("Numbered")}
            hint={t(
              "Number the first ten, Top 10 style. Plex, Vela, Silo, Jellyfin and Emby show it.",
            )}
          >
            <Segmented
              ariaLabel={t("Numbered")}
              size="sm"
              value={triState(look.numbered)}
              onChange={(v) => patch({ numbered: fromTri(v) })}
              options={[
                {
                  value: "source",
                  label: source.look.numbered ? t("Source (on)") : t("Source (off)"),
                },
                { value: "on", label: t("On") },
                { value: "off", label: t("Off") },
              ]}
            />
          </Line>
          {experience === "watch" || experience === "" ? (
            <Line
              label={t("Trailer on focus")}
              hint={t("Plays a trailer when the row is focused, where the app can.")}
            >
              <Segmented
                ariaLabel={t("Trailer on focus")}
                size="sm"
                value={triState(look.trailer)}
                onChange={(v) => patch({ trailer: fromTri(v) })}
                options={[
                  { value: "source", label: t("Follow the source") },
                  { value: "on", label: t("On") },
                  { value: "off", label: t("Off") },
                ]}
              />
            </Line>
          ) : null}
          <Line label={t("Titles other rows show")}>
            <Segmented
              ariaLabel={t("Titles other rows show")}
              size="sm"
              value={look.keepWhole ? "keep" : "skip"}
              onChange={(v) => patch({ keepWhole: v === "keep" })}
              options={[
                { value: "skip", label: t("Skip them") },
                { value: "keep", label: t("Keep this row whole") },
              ]}
            />
          </Line>
        </>
      ) : null}
    </Section>
  );
}

// ---------------------------------------------------------------- placement

const DEVICES: Device[] = ["tv", "mobile", "desktop", "web"];
const DAYPARTS: Daypart[] = ["morning", "afternoon", "evening", "late"];

function ScheduleEditor({
  when,
  onChange,
}: {
  when: Schedule | null | undefined;
  onChange: (s: Schedule | null) => void;
}) {
  const preset = presetOf(when);
  const [custom, setCustom] = useState(preset === "custom");
  const shown: SchedulePreset = custom ? "custom" : preset;
  const presets: { value: SchedulePreset; label: string }[] = [
    { value: "any", label: t("Any time") },
    { value: "evenings", label: t("Evenings") },
    { value: "weeknights", label: t("Weeknights") },
    { value: "weekends", label: t("Weekends") },
    { value: "weekend-nights", label: t("Weekend nights") },
    { value: "mornings", label: t("Mornings") },
    { value: "holidays", label: t("Holidays…") },
    { value: "custom", label: t("Custom…") },
  ];
  const s = when ?? {};
  const set = (p: Partial<Schedule>) => {
    const next: Schedule = { ...s, ...p };
    for (const k of Object.keys(next) as (keyof Schedule)[]) {
      const v = next[k];
      if (v === undefined || (Array.isArray(v) && v.length === 0 && k !== "holidays"))
        delete next[k];
    }
    onChange(next);
  };
  const days = [t("Sun"), t("Mon"), t("Tue"), t("Wed"), t("Thu"), t("Fri"), t("Sat")];
  const months = monthNames();
  const all = holidays();
  const groups = [...new Set(all.map((h) => h.group))];
  const range = s.dates?.[0];
  return (
    <div className="space-y-2 border-t border-border-subtle pt-3">
      <span className="font-medium text-fg">{t("When")}</span>
      <div className="flex flex-wrap gap-1.5" role="radiogroup" aria-label={t("When")}>
        {presets.map((p) => (
          <button
            key={p.value}
            type="button"
            role="radio"
            aria-checked={shown === p.value}
            onClick={() => {
              setCustom(p.value === "custom");
              if (p.value === "custom") onChange(when ?? {});
              else onChange(presetSchedule(p.value));
            }}
            className={cn(
              "rounded-full border px-2.5 py-0.5 text-xs",
              shown === p.value
                ? "border-primary bg-primary text-primary-fg"
                : "border-border text-fg hover:border-primary",
            )}
          >
            {p.label}
          </button>
        ))}
      </div>
      {shown === "holidays" ? (
        <div className="space-y-2">
          {groups.map((g) => (
            <div key={g}>
              <p className="text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
                {holidayGroupLabel(g)}
              </p>
              <ChipGroup
                ariaLabel={holidayGroupLabel(g)}
                size="sm"
                value={(s.holidays ?? []).filter((h) =>
                  all.some((x) => x.id === h && x.group === g),
                )}
                onChange={(v) =>
                  set({
                    holidays: [
                      ...(s.holidays ?? []).filter(
                        (h) => !all.some((x) => x.id === h && x.group === g),
                      ),
                      ...v,
                    ],
                  })
                }
                options={all
                  .filter((h) => h.group === g)
                  .map((h) => ({ value: h.id, label: h.label }))}
              />
            </div>
          ))}
          <p className="text-xs text-fg-muted">
            {t("Moving dates (Easter, Ramadan, Diwali…) are worked out every year.")}
          </p>
        </div>
      ) : null}
      {shown === "custom" ? (
        <div className="space-y-2">
          <ChipGroup
            ariaLabel={t("Days")}
            size="sm"
            value={s.days ?? []}
            onChange={(v) => set({ days: v })}
            options={days.map((d, i) => ({ value: i, label: d }))}
          />
          <ChipGroup
            ariaLabel={t("Times of day")}
            size="sm"
            value={s.dayparts ?? []}
            onChange={(v) => set({ dayparts: v })}
            options={DAYPARTS.map((d) => ({ value: d, label: daypartLabel(d) }))}
          />
          <p className="text-xs text-fg-muted">
            {t("Late is 10 PM to 5 AM and belongs to the day it started.")}
          </p>
          <ChipGroup
            ariaLabel={t("Months")}
            size="sm"
            value={s.months ?? []}
            onChange={(v) => set({ months: v })}
            options={months.map((m, i) => ({ value: i + 1, label: m }))}
          />
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-fg-muted">{t("Dates")}</span>
            <Input
              aria-label={t("From date")}
              type="date"
              className="h-8 w-40"
              value={range ? dateInput(range.from) : ""}
              onChange={(e) =>
                set({
                  dates: e.currentTarget.value
                    ? [
                        {
                          from: e.currentTarget.value,
                          to: range?.to ? dateInput(range.to) : e.currentTarget.value,
                          yearly: range?.yearly,
                        },
                      ].map(yearlyRange)
                    : [],
                })
              }
            />
            <span>–</span>
            <Input
              aria-label={t("To date")}
              type="date"
              className="h-8 w-40"
              value={range ? dateInput(range.to) : ""}
              onChange={(e) =>
                set({
                  dates: e.currentTarget.value
                    ? [
                        {
                          from: range?.from ? dateInput(range.from) : e.currentTarget.value,
                          to: e.currentTarget.value,
                          yearly: range?.yearly,
                        },
                      ].map(yearlyRange)
                    : [],
                })
              }
            />
            <label className="inline-flex items-center gap-1.5 text-xs">
              <Switch
                aria-label={t("Repeats every year")}
                checked={Boolean(range?.yearly)}
                disabled={!range}
                onChange={(e) =>
                  range &&
                  set({
                    dates: [
                      yearlyRange({
                        from: dateInput(range.from),
                        to: dateInput(range.to),
                        yearly: e.currentTarget.checked,
                      }),
                    ],
                  })
                }
              />
              {t("Repeats every year")}
            </label>
          </div>
        </div>
      ) : null}
    </div>
  );
}

/** A stored date ("MM-DD" yearly, "YYYY-MM-DD" once) as a date input value. */
function dateInput(v: string): string {
  return /^\d{2}-\d{2}$/.test(v) ? `${new Date().getFullYear()}-${v}` : v;
}
function yearlyRange(r: { from: string; to: string; yearly?: boolean }) {
  return r.yearly
    ? { from: r.from.slice(5), to: r.to.slice(5), yearly: true }
    : { from: r.from, to: r.to };
}

function PlacementSection({
  placement,
  curated,
  viewers,
  onChange,
  lockAudience,
}: {
  placement: RowPlacement;
  curated: boolean;
  viewers: Viewer[];
  onChange: (p: RowPlacement) => void;
  lockAudience?: boolean;
}) {
  const [more, setMore] = useState(
    Boolean(
      placement.notAbove || placement.notBelow || placement.daysPerWeek || placement.priority,
    ),
  );
  const modes: { value: PlacementMode; label: string }[] = curated
    ? [
        { value: "pinned", label: t("Pinned") },
        { value: "show", label: t("On the page") },
        { value: "hide", label: t("Hide") },
      ]
    : [
        { value: "pinned", label: t("Pinned") },
        { value: "show", label: t("Show") },
        { value: "auto", label: t("Auto") },
        { value: "hide", label: t("Hide") },
      ];
  const audience = placement.audience?.mode ?? "everyone";
  const set = (p: Partial<RowPlacement>) => onChange({ ...placement, ...p });
  return (
    <Section title={t("Placement")} hint={t("Where, when and for whom.")}>
      <Line
        label={t("On this page")}
        hint={
          curated
            ? t(
                "Curated: pinned rows sit at their number; the others follow in your order. Rows with nothing to show are skipped.",
              )
            : t(
                "Pinned: always at this position. Show: always on the page, Ether picks where. Auto: Ether decides by what each viewer plays.",
              )
        }
      >
        <Segmented
          ariaLabel={t("Placement")}
          size="sm"
          value={curated && placement.mode === "auto" ? "show" : placement.mode}
          onChange={(m) => {
            // On a Curated page an Auto row is "On the page"; it stays Auto
            // for when the page is Personalized again.
            if (curated && m === "show" && placement.mode === "auto") return;
            onChange(withMode(placement, m));
          }}
          options={modes}
        />
      </Line>
      {placement.mode === "pinned" ? (
        <Line label={t("Position")}>
          <NumberField
            label={t("Position")}
            value={placement.position}
            onChange={(n) => set({ position: n ?? 1 })}
          />
        </Line>
      ) : null}
      {placement.mode !== "hide" ? (
        <>
          <ScheduleEditor when={placement.when} onChange={(when) => set({ when })} />
          <div className="space-y-2 border-t border-border-subtle pt-3">
            <span className="font-medium text-fg">{t("Devices")}</span>
            <div className="flex flex-wrap items-center gap-2">
              <button
                type="button"
                aria-pressed={!placement.devices?.length}
                onClick={() => set({ devices: undefined })}
                className={cn(
                  "rounded-full border px-2.5 py-0.5 text-xs",
                  !placement.devices?.length
                    ? "border-primary bg-primary text-primary-fg"
                    : "border-border hover:border-primary",
                )}
              >
                {t("All devices")}
              </button>
              <ChipGroup
                ariaLabel={t("Devices")}
                size="sm"
                value={placement.devices ?? []}
                onChange={(v) => set({ devices: v.length ? v : undefined })}
                options={DEVICES.map((d) => ({ value: d, label: deviceLabel(d) }))}
              />
            </div>
          </div>
          <Line
            label={t("Who sees it")}
            hint={
              lockAudience
                ? t("A row you add while editing for one viewer is just for them.")
                : undefined
            }
          >
            <Segmented
              ariaLabel={t("Who sees it")}
              size="sm"
              value={audience}
              onChange={(m) =>
                set({
                  audience:
                    m === "everyone"
                      ? { mode: "everyone" }
                      : { mode: m, viewers: placement.audience?.viewers ?? [] },
                })
              }
              options={[
                { value: "everyone", label: t("Everyone"), disabled: lockAudience },
                { value: "only", label: t("Only…"), disabled: lockAudience },
                { value: "except", label: t("Everyone except…"), disabled: lockAudience },
              ]}
            />
          </Line>
          {audience !== "everyone" ? (
            <ChipGroup
              ariaLabel={t("Viewers")}
              size="sm"
              value={placement.audience?.viewers ?? []}
              onChange={(v) => !lockAudience && set({ audience: { mode: audience, viewers: v } })}
              options={viewers.map((v) => ({ value: v.id, label: viewerName(v) }))}
            />
          ) : null}
        </>
      ) : null}
      {placement.mode === "auto" && !curated ? (
        <>
          <button
            type="button"
            aria-expanded={more}
            onClick={() => setMore(!more)}
            className="flex w-full items-center justify-between border-t border-border-subtle pt-3 text-left text-fg-muted"
          >
            {t("More options: never above or below, days a week, priority")}
            {more ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
          </button>
          {more ? (
            <>
              <Line label={t("Never above #")}>
                <NumberField
                  label={t("Never above #")}
                  value={placement.notAbove || undefined}
                  onChange={(n) => set({ notAbove: n })}
                />
              </Line>
              <Line label={t("Never below #")}>
                <NumberField
                  label={t("Never below #")}
                  value={placement.notBelow || undefined}
                  onChange={(n) => set({ notBelow: n })}
                />
              </Line>
              <Line label={t("At most this many days a week")}>
                <NumberField
                  label={t("At most this many days a week")}
                  value={placement.daysPerWeek || undefined}
                  max={6}
                  onChange={(n) => set({ daysPerWeek: n })}
                />
              </Line>
              <Line label={t("Priority")}>
                <Segmented
                  ariaLabel={t("Priority")}
                  size="sm"
                  value={String(placement.priority ?? 0)}
                  onChange={(v) => set({ priority: Number(v) || undefined })}
                  options={[
                    { value: "-1", label: t("Lower") },
                    { value: "0", label: t("Normal") },
                    { value: "1", label: t("Higher") },
                  ]}
                />
              </Line>
            </>
          ) : null}
        </>
      ) : null}
    </Section>
  );
}

// ---------------------------------------------------------------- content tab

/** How many titles the Content tab asks for (the preview's cap). */
const CONTENT_LIMIT = 40;

function ContentTab({
  ctx,
  row,
  source,
}: {
  ctx: DrawerContext;
  row: PageRow;
  source?: PageSource;
}) {
  const [result, setResult] = useState<{ rows: PreviewRow[]; total: number } | null>(null);
  const [error, setError] = useState<unknown>(null);
  const key = JSON.stringify({ row, p: ctx.preview });
  useEffect(() => {
    const ctl = new AbortController();
    setError(null);
    api
      .previewPageRow(ctx.pageKey, { row, ...ctx.preview, count: CONTENT_LIMIT }, ctl.signal)
      .then((r) => setResult({ rows: r.rows ?? [], total: r.total }))
      .catch((e) => {
        if (!isAbort(e)) setError(e);
      });
    return () => ctl.abort();
    // The key captures the row and the preview context.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, ctx.pageKey]);
  if (error) return <p className="text-sm text-danger">{errorMessage(error)}</p>;
  if (!result) {
    return (
      <div className="grid min-h-48 place-items-center">
        <Spinner size={18} label={t("Loading the titles")} />
      </div>
    );
  }
  const fanOut = result.rows.length > 1;
  const capped = result.rows.some((r) => (r.items?.length ?? 0) >= CONTENT_LIMIT);
  const shown = result.rows.reduce((n, r) => n + (r.items?.length ?? 0), 0);
  return (
    <div className="space-y-5">
      <p className="text-sm text-fg-muted">
        {result.total > 0 && source?.kind === "collection-cards"
          ? tn(result.total, "{count} collection", "{count} collections")
          : result.total === 0
            ? t("Nothing for this viewer right now.")
            : fanOut
              ? capped
                ? tn(
                    result.total,
                    "{count} title across {rows} rows · showing the first {limit} of each",
                    "{count} titles across {rows} rows · showing the first {limit} of each",
                    { rows: result.rows.length, limit: CONTENT_LIMIT },
                  )
                : tn(
                    result.total,
                    "{count} title across {rows} rows",
                    "{count} titles across {rows} rows",
                    { rows: result.rows.length },
                  )
              : capped
                ? tn(
                    result.total,
                    "{count} title · showing the first {shown}",
                    "{count} titles · showing the first {shown}",
                    { shown },
                  )
                : tn(result.total, "{count} title", "{count} titles")}
      </p>
      {result.rows.map((r) => (
        <section key={r.hubId} className="space-y-2.5" aria-label={r.title}>
          {fanOut ? (
            <h4 className="sticky -top-5 z-10 -mx-1 flex items-baseline justify-between gap-2 bg-bg/95 px-1 py-1.5 text-sm font-semibold text-fg backdrop-blur">
              <span className="truncate">{r.title}</span>
              <span className="shrink-0 text-xs font-normal text-fg-muted">
                {tn(r.items?.length ?? 0, "{count} title", "{count} titles")}
              </span>
            </h4>
          ) : null}
          <RowGrid
            row={r}
            showRating={source?.kind !== "collection-cards"}
            style={row.look.style}
            card={row.look.card}
            numbered={row.look.numbered ?? undefined}
            pluginName={source?.plugin}
          />
        </section>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------- the drawer

export function RowDrawer({
  state,
  row,
  source,
  isNew,
  ctx,
  onClose,
  onPick,
  onChange,
  onAdd,
  onRemove,
  onChangeSource,
  onTab,
}: {
  state: DrawerState | null;
  /** The row being set up or edited (Everyone's, or the viewer's effective row). */
  row: PageRow | null;
  source: PageSource | undefined;
  isNew: boolean;
  ctx: DrawerContext;
  onClose: () => void;
  onPick: (s: PageSource, params: Record<string, string>) => void;
  onChange: (r: PageRow) => void;
  onAdd: () => void;
  onRemove: () => void;
  onChangeSource: () => void;
  onTab: (tab: "settings" | "content") => void;
}) {
  // A row already on the page (stored or not yet) edits like any other row.
  const adding = state?.kind === "add" && isNew;
  const location = useLocation();
  const picking = adding && (state.picking || !row);
  const viewerOnly = Boolean(ctx.editingFor) && !isNew;
  const experience: Experience =
    ctx.library?.experience ??
    ctx.libraries.find((l) => l.id === row?.scope.libraries?.[0])?.experience ??
    "watch";
  const libraryNames = (row?.scope.libraries ?? []).map(
    (id) => ctx.libraries.find((l) => l.id === id)?.name ?? "",
  );
  const refParam = source?.params?.find((p) => p.type === "ref" && p.required);
  const refLabel =
    refParam && row?.source.params?.[refParam.key]
      ? source?.refs?.find((r) => r.value === row.source.params?.[refParam.key])?.label
      : undefined;
  const sourceTitle = refLabel ?? source?.title ?? row?.source.kind ?? "";
  // A collection row links to its collection in Collection Studio; closing
  // or saving there comes back to this row, the page draft kept.
  const collectionId =
    source?.kind === "collection" && state?.kind === "edit" && refParam
      ? row?.source.params?.[refParam.key]
      : undefined;
  const editCollection = (() => {
    if (!collectionId || !row) return null;
    const back = new URLSearchParams(location.search);
    back.delete("add");
    back.set("row", row.id);
    return editCollectionHref(collectionId, `/recommendations?${back}`);
  })();
  const pinnedAt = row?.placement.mode === "pinned" ? row.placement.position : undefined;
  const missing = (source?.params ?? []).filter((p) => p.required && !row?.source.params?.[p.key]);
  const title = adding
    ? source && !picking
      ? pinnedAt
        ? t("New row at #{at} · {source}", { at: pinnedAt, source: sourceTitle })
        : t("New row · {source}", { source: sourceTitle })
      : t("New row at #{at}", { at: state.at })
    : row?.name
      ? fillLibrary(row.name, libraryNames[0] ?? "")
      : sourceTitle;
  /** The add button names its effect. */
  const addLabel = t("Add to page");
  const needsCollection = row != null && !hasCollectionCardSelection(row);
  const addNote = needsCollection
    ? t("Add a collection")
    : missing.length
      ? t("Choose {param} first", { param: missing[0]!.label.toLowerCase() })
      : row?.placement.mode === "pinned"
        ? undefined
        : row?.placement.mode === "hide"
          ? t("Adds it hidden")
          : row?.placement.mode === "auto"
            ? t("Adds one row; Ether decides whether and where")
            : t("Adds one row; Ether places it");
  const facetLibrary =
    ctx.library?.id ?? row?.scope.libraries?.[0] ?? source?.libraries?.[0] ?? null;
  const tab = (state && !picking ? state.tab : undefined) ?? "settings";
  const fanOut = row?.scope.perLibrary
    ? row.scope.libraries?.length
      ? row.scope.libraries.slice(0, MAX_ROWS_PER_LIBRARY_ROW)
      : (source?.libraries ?? []).slice(0, MAX_ROWS_PER_LIBRARY_ROW)
    : [];
  const addSummary =
    row && source && fanOut.length > 1
      ? tn(fanOut.length, "Adds {count} row at #{at}", "Adds {count} rows at #{at}: {names}", {
          at: adding ? (pinnedAt ?? state.at) : 0,
          names: fanOut
            .map((id) =>
              fillLibrary(
                row.name || `${sourceTitle} {library}`,
                ctx.libraries.find((l) => l.id === id)?.name ?? "",
              ),
            )
            .join(", "),
        })
      : t("Pick what fills the row");
  const setParams = (k: string, v: string) => {
    if (!row) return;
    const params = { ...(row.source.params ?? {}), [k]: v };
    if (!v) delete params[k];
    onChange({ ...row, source: { ...row.source, params } });
  };
  // A collection stays switchable (its picker shows the chosen one); other
  // picked refs (a shelf, a plugin row) are the source itself.
  const visibleParams = (source?.params ?? []).filter(
    (p) =>
      source?.kind === "collection" ||
      !(p.type === "ref" && p.required && source?.refs?.length && row?.source.params?.[p.key]),
  );
  return (
    <Drawer
      open={state !== null}
      onClose={onClose}
      title={title}
      description={
        adding
          ? pinnedAt
            ? t("It goes into the draft at this position and shows in the page as you set it up.")
            : t("It goes into the draft and shows in the page as you set it up.")
          : viewerOnly
            ? t("Changes here are just for {viewer}: only the look and placement change.", {
                viewer: viewerName(ctx.editingFor!),
              })
            : t(
                "Edits go straight into the draft and show in the page. Save the page when you're done.",
              )
      }
      size="md"
      footer={
        adding ? (
          <div className="flex w-full flex-wrap items-center justify-between gap-2">
            <span className="text-sm text-fg-muted">
              {row && source
                ? (addNote ??
                  (fanOut.length > 1
                    ? addSummary
                    : t("Adds one row at #{at}", { at: pinnedAt ?? state.at })))
                : addSummary}
            </span>
            <div className="flex gap-2">
              <Button variant="ghost" onClick={onClose}>
                {t("Cancel")}
              </Button>
              <Button
                disabled={!row || picking || needsCollection || missing.length > 0}
                onClick={onAdd}
              >
                {addLabel}
              </Button>
            </div>
          </div>
        ) : (
          <div className="flex w-full items-center justify-between">
            {viewerOnly ? (
              <span />
            ) : (
              <Button variant="ghost" className="text-destructive" onClick={onRemove}>
                {t("Remove from page")}
              </Button>
            )}
            <Button onClick={onClose}>{t("Close")}</Button>
          </div>
        )
      }
    >
      {picking ? (
        <SourcePicker
          sources={ctx.sources}
          error={ctx.sourcesError}
          onPick={onPick}
          rows={ctx.rows}
          pageKey={ctx.pageKey}
          libraries={ctx.library ? [ctx.library] : ctx.libraries}
        />
      ) : row ? (
        <>
          <SubtabNav
            ariaLabel={t("Row")}
            active={tab}
            onSelect={(k) => onTab(k as "settings" | "content")}
            tabs={[
              { key: "settings", label: t("Settings") },
              { key: "content", label: t("Content") },
            ]}
          />
          {tab === "content" ? (
            <ContentTab ctx={ctx} row={row} source={source} />
          ) : (
            <div className="space-y-5">
              <Section title={t("What fills it")}>
                <Line
                  label={
                    <span className="inline-flex items-center gap-2">
                      {sourceTitle}
                      {source?.plugin ? (
                        <Badge variant="outline" className="gap-1 font-normal">
                          <Puzzle size={11} /> {source.plugin}
                        </Badge>
                      ) : null}
                    </span>
                  }
                  hint={source?.description}
                >
                  <span className="flex items-center gap-2">
                    {source ? <KindChip personal={source.personal} /> : null}
                    {viewerOnly ? null : (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="h-7 px-2 text-xs"
                        onClick={onChangeSource}
                      >
                        {t("Change")}
                      </Button>
                    )}
                    {editCollection ? (
                      <Link
                        to={editCollection}
                        className="inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline"
                      >
                        <Pencil size={11} /> {t("Edit collection")}
                      </Link>
                    ) : null}
                    {source &&
                    source.group === "plugins" &&
                    !source.kind.startsWith("surface") &&
                    !viewerOnly ? (
                      <Link
                        to={pluginCollectionHref(source.kind, source.title)}
                        className="text-xs font-medium text-primary hover:underline"
                      >
                        {t("Save as a collection")}
                      </Link>
                    ) : null}
                  </span>
                </Line>
                {source
                  ? visibleParams
                      .filter(
                        (p) =>
                          !p.more &&
                          (p.type !== "collectionList" || !row.source.params?.match?.trim()),
                      )
                      .map((p) => (
                        <ParamControl
                          key={p.key}
                          param={p}
                          source={source}
                          value={row.source.params?.[p.key] ?? ""}
                          library={facetLibrary}
                          disabled={viewerOnly}
                          onChange={(v) => setParams(p.key, v)}
                        />
                      ))
                  : null}
                {source?.kind === "collection" && !viewerOnly ? (
                  <Link
                    to={newCollectionLink(ctx.pageKey)}
                    className="inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline"
                  >
                    <Plus size={12} /> {t("New collection…")}
                  </Link>
                ) : null}
                {source?.kind === "collection-cards" && row.source.params?.match?.trim() ? (
                  <p className="text-xs text-fg-muted">
                    {t("Collections with matching names appear in alphabetical order.")}
                  </p>
                ) : null}
                {source && visibleParams.some((p) => p.more) ? (
                  <details className="border-t border-border-subtle pt-3">
                    <summary className="cursor-pointer text-fg-muted">{t("More options")}</summary>
                    <div className="mt-3 space-y-3">
                      {visibleParams
                        .filter((p) => p.more)
                        .map((p) => (
                          <ParamControl
                            key={p.key}
                            param={p}
                            source={source}
                            value={row.source.params?.[p.key] ?? ""}
                            library={facetLibrary}
                            disabled={viewerOnly}
                            onChange={(v) => setParams(p.key, v)}
                          />
                        ))}
                    </div>
                  </details>
                ) : null}
                <Line
                  label={t("Name")}
                  hint={
                    row.scope.perLibrary
                      ? t("{library} becomes each library's name.")
                      : t("Leave it empty to use the source's name.")
                  }
                >
                  <Input
                    aria-label={t("Name")}
                    className="h-8 w-56"
                    maxLength={80}
                    disabled={viewerOnly}
                    placeholder={row.scope.perLibrary ? `${sourceTitle} {library}` : sourceTitle}
                    value={row.name ?? ""}
                    onChange={(e) => onChange({ ...row, name: e.currentTarget.value || undefined })}
                  />
                </Line>
              </Section>
              {ctx.library === null && source && source.kind !== "collection-cards" ? (
                <LibrariesSection
                  row={row}
                  source={source}
                  libraries={ctx.libraries}
                  onChange={onChange}
                  disabled={viewerOnly}
                />
              ) : null}
              {source ? (
                <LookSection
                  look={row.look}
                  source={source}
                  experience={experience}
                  onChange={(look) => onChange({ ...row, look })}
                />
              ) : null}
              <PlacementSection
                placement={row.placement}
                curated={ctx.curated}
                viewers={ctx.viewers}
                lockAudience={Boolean(ctx.editingFor) && isNew}
                onChange={(placement) => onChange({ ...row, placement })}
              />
            </div>
          )}
        </>
      ) : (
        <p className="text-sm text-fg-muted">{t("This row is no longer on the page.")}</p>
      )}
    </Drawer>
  );
}

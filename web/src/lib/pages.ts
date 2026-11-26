// Recommendations: pages of rows (design: rows-redesign §5, §7). A page is
// Home or one rows tab of a library; a row is a source, a scope (libraries),
// a look and a placement. These types mirror internal/core/pages.go.
import { t } from "../i18n";
import type { Device, TitleCard } from "./contract";

export type PageKey = string;
export type PageMode = "curated" | "personalized";
export type PlacementMode = "pinned" | "show" | "auto" | "hide";
export type Experience = "watch" | "listen" | "read" | "gallery" | "live" | "";
export type RowStyle = "" | "standard" | "hero" | "wide" | "tiles" | "tracks";
export type RowCard = "" | "poster" | "landscape" | "square" | "round";
export type Daypart = "morning" | "afternoon" | "evening" | "late";

export const PAGE_HOME: PageKey = "home";
/** At most this many rows come from one "one row per library" row. */
export const MAX_ROWS_PER_LIBRARY_ROW = 6;

export interface PageLength {
  rows?: number;
  byDevice?: Partial<Record<Device, number>>;
}
export interface PageTuning {
  discovery?: number;
  freshness?: number;
  variety?: number;
}
export type TuningDial = keyof PageTuning;

export interface RowSource {
  kind: string;
  params?: Record<string, string>;
}
export interface RowScope {
  libraries?: number[];
  perLibrary?: boolean;
}
export interface RowLook {
  style?: RowStyle;
  card?: RowCard;
  hideWatched?: boolean;
  numbered?: boolean | null;
  trailer?: boolean | null;
  keepWhole?: boolean;
}
export interface DateRange {
  from: string;
  to: string;
  yearly?: boolean;
}
export interface Schedule {
  days?: number[];
  dayparts?: Daypart[];
  fromHour?: number;
  toHour?: number;
  months?: number[];
  dates?: DateRange[];
  holidays?: string[];
}
export interface Audience {
  mode?: "everyone" | "only" | "except";
  viewers?: number[];
}
export interface RowPlacement {
  mode: PlacementMode;
  position?: number;
  when?: Schedule | null;
  devices?: Device[];
  audience: Audience;
  notAbove?: number;
  notBelow?: number;
  daysPerWeek?: number;
  priority?: number;
}
export interface PageRow {
  id: string;
  source: RowSource;
  scope: RowScope;
  name?: string;
  look: RowLook;
  placement: RowPlacement;
}
export interface RowOverride {
  row: string;
  look?: RowLook;
  placement?: RowPlacement;
}
export interface PageVariant {
  viewer: number;
  mode?: PageMode;
  length?: PageLength;
  rows: RowOverride[];
}
export interface RowPage {
  key: PageKey;
  mode: PageMode;
  length: PageLength;
  picks: boolean;
  tuning?: PageTuning;
  skipHome?: boolean | null;
  rows: PageRow[];
  variants?: PageVariant[];
  offered?: string[];
  revision: number;
  updatedAt?: string;
}

/** GET /pages: one entry of the page picker. */
export interface PageSummary {
  key: PageKey;
  name: string;
  library?: number;
  tab?: string;
  experience: Experience;
  mode: PageMode;
  rowCount: number;
  saved: boolean;
  apps?: string[];
}

export interface PageLibrary {
  id: number;
  name: string;
  experience: Experience;
  mediaType?: "movie" | "show";
  plugin?: string;
  caps?: string[];
  card?: RowCard;
  rowsTabs?: { key: string; label: string }[];
}

/** GET /pages/{key}. */
export interface PageDoc {
  page: RowPage;
  saved: boolean;
  library?: PageLibrary;
  libraries?: PageLibrary[];
  viewersWithChanges?: { id: number; name: string; changes: number }[] | null;
  defaultLength: number;
}

export interface ParamOption {
  value: string;
  label: string;
}
export interface RowParam {
  key: string;
  label: string;
  type: "bool" | "choice" | "count" | "facetValue" | "ref" | "collectionList" | "text";
  choices?: ParamOption[];
  facet?: string;
  default?: string;
  min?: number;
  max?: number;
  more?: boolean;
  required?: boolean;
}
export interface RowSourceInfo {
  kind: string;
  title: string;
  description: string;
  /** keep, trending, new, foryou, top, browse, collections, plugins or live. */
  group: string;
  personal: boolean;
  needs?: string[];
  homeOnly?: boolean;
  /** Can be one row per library. */
  split?: boolean;
  params?: RowParam[];
  purpose?: string;
  look: RowLook;
  plugin?: string;
  library?: number;
}
/** One entry of GET /pages/{key}/sources. */
export interface PageSource extends RowSourceInfo {
  available: boolean;
  unavailable?: string;
  /** The page's libraries (Home: every library) it can draw from. */
  libraries?: number[];
  used: number;
  /** Choices of a ref parameter (collections, shelves): never raw ids. */
  refs?: ParamOption[];
}
export interface SourcesDoc {
  sources: PageSource[];
  libraries: PageLibrary[];
  collections?: { id: string; name: string; count: number }[];
}

export interface RowPresentation {
  rowId?: string;
  layout?: RowStyle;
  card?: RowCard;
  role?: string;
  personal?: boolean;
  library?: number;
  experience?: Experience;
  numbered?: boolean;
  trailer?: boolean;
  hideWatched?: boolean;
  surface?: { plugin: string; pluginName?: string; surface: string };
}

/** A served row in a preview. */
export interface PreviewRow {
  hubId: string;
  /** The page row it comes from; absent for Ether's own picks. */
  rowId?: string;
  library?: number;
  /** Effective placement of a page row (pinned, show, auto). */
  mode?: PlacementMode;
  why?: string;
  note?: string;
  look: RowLook;
  personal: boolean;
  flags?: string[] | null;
  /** A scheduled-away row drawn faintly where it would sit. */
  ghost?: boolean;
  present: RowPresentation;
  title: string;
  kind: string;
  reason?: string;
  items: TitleCard[] | null;
  itemReasons?: Record<string, string> | null;
}
export interface NotShown {
  row?: string;
  title: string;
  reason: string;
  previewAs?: number;
  previewAt?: string;
}
export interface EtherPick {
  kind: string;
  title: string;
  personal: boolean;
  why: string;
}
export interface ViewerAccess {
  viewer: number;
  name: string;
  ageLimit?: string;
  unrated: "hidden" | "shown";
  librariesSeen: number;
  librariesTotal: number;
  hiddenLibraries?: string[] | null;
}
export interface PreviewSummary {
  rows: number;
  length: number;
  pinned: number;
  shown: number;
  /** Rows Ether chose (Auto rows and its picks). */
  auto: number;
  /** How many of Ether's picks the page could still use. */
  picks: number;
  /** Whether Ether adds its picks: the console keeps it on for Personalized pages, off for Curated. */
  picksOn?: boolean;
  /** The page's own rows on it (rows less Ether's picks). */
  chosen?: number;
  /** Rows only some apps draw (plugin rows in Vela): not in rows, not counted toward the length. */
  appOnly?: number;
}
export interface PagePreview {
  summary: PreviewSummary;
  rows: PreviewRow[] | null;
  picks: EtherPick[] | null;
  notShown: NotShown[] | null;
  access?: ViewerAccess | null;
}
export interface PreviewRequest {
  page?: RowPage;
  viewer?: number;
  device: Device;
  at: string;
  count?: number;
}
export interface RowPreview {
  rows: PreviewRow[] | null;
  total: number;
}
export interface PageImpact {
  viewers: number;
  withChanges: number;
}
export interface FacetValueOption {
  value: string;
  label: string;
  count?: number;
}

/** The API path of a page: "/" in a key is written "~" ("/pages/library:4~recommended"). */
export function pagePath(key: PageKey): string {
  return `/pages/${key.replaceAll("/", "~")}`;
}

// ---------------------------------------------------------------- row ids

const ID_CHARS = "abcdefghijklmnopqrstuvwxyz0123456789";

/**
 * A new row id in the server's shape ("r" + 8 chars). getRandomValues works
 * on the plain-HTTP LAN origin owners use; crypto.randomUUID does not.
 */
export function newRowId(taken: Iterable<string> = []): string {
  const used = new Set(taken);
  for (;;) {
    const bytes = new Uint8Array(8);
    crypto.getRandomValues(bytes);
    let id = "r";
    for (const b of bytes) id += ID_CHARS[b % ID_CHARS.length];
    if (!used.has(id)) return id;
  }
}

// ---------------------------------------------------------------- variants

/** The variant for a viewer, or undefined. */
export function variantFor(page: RowPage, viewer: number | null): PageVariant | undefined {
  if (viewer == null) return undefined;
  return page.variants?.find((v) => v.viewer === viewer);
}

/** A row as one viewer gets it: their Look and Placement overrides applied. */
export function effectiveRow(page: RowPage, row: PageRow, viewer: number | null): PageRow {
  const o = variantFor(page, viewer)?.rows.find((r) => r.row === row.id);
  if (!o) return row;
  return { ...row, look: o.look ?? row.look, placement: o.placement ?? row.placement };
}

export function effectiveMode(page: RowPage, viewer: number | null): PageMode {
  return variantFor(page, viewer)?.mode ?? page.mode;
}

export function effectiveLength(page: RowPage, viewer: number | null): PageLength {
  return variantFor(page, viewer)?.length ?? page.length;
}

/** Writes a Look or Placement change for one viewer ("Changes just for Luca"). */
export function setOverride(
  page: RowPage,
  viewer: number,
  rowId: string,
  patch: { look?: RowLook; placement?: RowPlacement },
): RowPage {
  const variants = [...(page.variants ?? [])];
  let i = variants.findIndex((v) => v.viewer === viewer);
  if (i < 0) {
    variants.push({ viewer, rows: [] });
    i = variants.length - 1;
  }
  const v = variants[i]!;
  const rows = [...v.rows];
  const j = rows.findIndex((r) => r.row === rowId);
  const next: RowOverride = { ...(j >= 0 ? rows[j] : undefined), ...patch, row: rowId };
  if (j >= 0) rows[j] = next;
  else rows.push(next);
  variants[i] = { ...v, rows };
  return { ...page, variants };
}

/** Drops one viewer's changes to a row ("Reset to Everyone"). */
export function clearOverride(page: RowPage, viewer: number, rowId: string): RowPage {
  const variants = (page.variants ?? [])
    .map((v) => (v.viewer === viewer ? { ...v, rows: v.rows.filter((r) => r.row !== rowId) } : v))
    .filter((v) => v.rows.length > 0 || v.mode || v.length);
  return { ...page, variants };
}

export function hasOverride(page: RowPage, viewer: number | null, rowId: string): boolean {
  return Boolean(variantFor(page, viewer)?.rows.some((r) => r.row === rowId));
}

export function changeCount(page: RowPage, viewer: number): number {
  const v = variantFor(page, viewer);
  if (!v) return 0;
  return v.rows.length + (v.mode ? 1 : 0) + (v.length ? 1 : 0);
}

// ---------------------------------------------------------------- draft helpers

/** Stable JSON for comparing a draft with the saved page. */
export function pageFingerprint(page: RowPage): string {
  return JSON.stringify(page, (_k, v: unknown) =>
    v === null || v === undefined || (Array.isArray(v) && v.length === 0) ? undefined : v,
  );
}

export function samePage(a: RowPage, b: RowPage): boolean {
  return pageFingerprint(a) === pageFingerprint(b);
}

const DRAFT_PREFIX = "ether.recommendations.draft:";

function draftKey(key: PageKey, revision: number): string {
  return `${DRAFT_PREFIX}${key}@${revision}`;
}

/** The kept draft of a page at this revision, if any. Older revisions are stale and dropped. */
export function loadDraft(key: PageKey, revision: number): RowPage | null {
  try {
    const mine = draftKey(key, revision);
    const prefix = `${DRAFT_PREFIX}${key}@`;
    for (let i = localStorage.length - 1; i >= 0; i--) {
      const k = localStorage.key(i);
      if (k && k.startsWith(prefix) && k !== mine) localStorage.removeItem(k);
    }
    const raw = localStorage.getItem(mine);
    if (!raw) return null;
    const page = JSON.parse(raw) as RowPage;
    return page && page.key === key && Array.isArray(page.rows) ? page : null;
  } catch {
    return null;
  }
}

/** Any kept draft of a page (whatever its revision), for the page picker's counts. */
export function peekDraft(key: PageKey): RowPage | null {
  try {
    const prefix = `${DRAFT_PREFIX}${key}@`;
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i);
      if (!k || !k.startsWith(prefix)) continue;
      const page = JSON.parse(localStorage.getItem(k) ?? "null") as RowPage | null;
      if (page && page.key === key && Array.isArray(page.rows)) return page;
    }
  } catch {
    // Unreadable storage: no draft.
  }
  return null;
}

export function storeDraft(page: RowPage | null, key: PageKey, revision: number): void {
  try {
    if (page) localStorage.setItem(draftKey(key, revision), JSON.stringify(page));
    else localStorage.removeItem(draftKey(key, revision));
  } catch {
    // Private mode or a full quota: the draft lives only in memory.
  }
}

// ---------------------------------------------------------------- page edits

/** Moves a page row (and so its whole fan-out block) to sit before another row (null = end). */
export function moveRow(page: RowPage, rowId: string, beforeId: string | null): RowPage {
  const row = page.rows.find((r) => r.id === rowId);
  if (!row || rowId === beforeId) return page;
  const rest = page.rows.filter((r) => r.id !== rowId);
  const at = beforeId ? rest.findIndex((r) => r.id === beforeId) : -1;
  rest.splice(at < 0 ? rest.length : at, 0, row);
  return { ...page, rows: rest };
}

export function insertRow(page: RowPage, row: PageRow, beforeId: string | null): RowPage {
  const rows = page.rows.filter((r) => r.id !== row.id);
  const at = beforeId ? rows.findIndex((r) => r.id === beforeId) : -1;
  rows.splice(at < 0 ? rows.length : at, 0, row);
  return { ...page, rows };
}

/**
 * Keeps pinned positions unique, as the server does (core NormalizePins):
 * two rows that show together (same audience, schedule and devices) never
 * hold the same position. On a collision the row that just got its pin
 * (`prefer`) keeps it and the other moves down one, the rows after it
 * following; a pin pushed past 50 becomes Show. A viewer's overrides are
 * kept unique among themselves the same way.
 */
export function normalizePins(page: RowPage, prefer?: string): RowPage {
  const order = new Map(page.rows.map((r, i) => [r.id, i]));
  const settle = (pins: { id: string; pl: RowPlacement }[]): Map<string, RowPlacement> => {
    const groups = new Map<string, { id: string; pl: RowPlacement }[]>();
    for (const x of pins) {
      const a = x.pl.audience ?? { mode: "everyone" };
      const sig = JSON.stringify([
        a.mode || "everyone",
        a.viewers?.length ? a.viewers : null,
        x.pl.when ?? null,
        x.pl.devices?.length ? x.pl.devices : null,
      ]);
      groups.set(sig, [...(groups.get(sig) ?? []), x]);
    }
    const changed = new Map<string, RowPlacement>();
    for (const g of groups.values()) {
      g.sort(
        (a, b) =>
          a.pl.position! - b.pl.position! ||
          (a.id === prefer ? -1 : b.id === prefer ? 1 : 0) ||
          (order.get(a.id) ?? 0) - (order.get(b.id) ?? 0),
      );
      let last = 0;
      for (const x of g) {
        if (x.pl.position! > last) {
          last = x.pl.position!;
          continue;
        }
        if (last >= 50) changed.set(x.id, withMode(x.pl, "show"));
        else changed.set(x.id, { ...x.pl, position: ++last });
      }
    }
    return changed;
  };
  const pinned = (pl: RowPlacement | undefined) =>
    Boolean(pl && pl.mode === "pinned" && (pl.position ?? 0) > 0);
  const base = settle(
    page.rows.filter((r) => pinned(r.placement)).map((r) => ({ id: r.id, pl: r.placement })),
  );
  let changed = base.size > 0;
  const rows = base.size
    ? page.rows.map((r) => (base.has(r.id) ? { ...r, placement: base.get(r.id)! } : r))
    : page.rows;
  const variants = page.variants?.map((v) => {
    const own = settle(
      v.rows.filter((o) => pinned(o.placement)).map((o) => ({ id: o.row, pl: o.placement! })),
    );
    if (!own.size) return v;
    changed = true;
    return {
      ...v,
      rows: v.rows.map((o) => (own.has(o.row) ? { ...o, placement: own.get(o.row)! } : o)),
    };
  });
  return changed ? { ...page, rows, variants } : page;
}

export function replaceRow(page: RowPage, row: PageRow): RowPage {
  return { ...page, rows: page.rows.map((r) => (r.id === row.id ? row : r)) };
}

export function removeRow(page: RowPage, rowId: string): RowPage {
  return {
    ...page,
    rows: page.rows.filter((r) => r.id !== rowId),
    variants: page.variants
      ?.map((v) => ({ ...v, rows: v.rows.filter((o) => o.row !== rowId) }))
      .filter((v) => v.rows.length > 0 || v.mode || v.length),
  };
}

export function hasCollectionCardSelection(row: PageRow): boolean {
  return (
    row.source.kind !== "collection-cards" ||
    Boolean(row.source.params?.collections || row.source.params?.match?.trim())
  );
}

/** A fresh row for a source, placed for the page's mode. */
export function rowForSource(
  source: PageSource,
  params: Record<string, string>,
  opts: {
    page: RowPage;
    position: number;
    editingFor: number | null;
    libraries?: number[];
    perLibrary?: boolean;
  },
): PageRow {
  const curated = opts.page.mode === "curated";
  const defaults: Record<string, string> = {};
  for (const p of source.params ?? []) if (p.default) defaults[p.key] = p.default;
  return {
    id: newRowId(opts.page.rows.map((r) => r.id)),
    source: { kind: source.kind, params: { ...defaults, ...params } },
    scope: { libraries: opts.libraries, perLibrary: opts.perLibrary },
    look: {},
    placement: {
      mode: curated ? "show" : "pinned",
      position: curated ? undefined : opts.position,
      audience:
        opts.editingFor != null
          ? { mode: "only", viewers: [opts.editingFor] }
          : { mode: "everyone" },
    },
  };
}

/** Placement with a mode change applied (positions only on pinned rows). */
export function withMode(p: RowPlacement, mode: PlacementMode, position?: number): RowPlacement {
  const next: RowPlacement = { ...p, mode };
  if (mode === "pinned") next.position = position ?? p.position ?? 1;
  else delete next.position;
  if (mode !== "auto") {
    delete next.notAbove;
    delete next.notBelow;
    delete next.daysPerWeek;
    delete next.priority;
  }
  return next;
}

// ---------------------------------------------------------------- schedule presets

export type SchedulePreset =
  | "any"
  | "evenings"
  | "weeknights"
  | "weekends"
  | "weekend-nights"
  | "mornings"
  | "holidays"
  | "custom";

const PRESETS: Record<Exclude<SchedulePreset, "any" | "holidays" | "custom">, Schedule> = {
  evenings: { dayparts: ["evening"] },
  weeknights: { days: [1, 2, 3, 4], dayparts: ["evening"] },
  weekends: { days: [0, 6] },
  "weekend-nights": { days: [5, 6], dayparts: ["evening", "late"] },
  mornings: { dayparts: ["morning"] },
};

export function isEmptySchedule(s: Schedule | null | undefined): boolean {
  if (!s) return true;
  return !(
    s.days?.length ||
    s.dayparts?.length ||
    s.months?.length ||
    s.dates?.length ||
    s.holidays?.length ||
    s.fromHour != null ||
    s.toHour != null
  );
}

export function presetSchedule(p: SchedulePreset): Schedule | null {
  if (p === "any") return null;
  if (p === "holidays") return { holidays: [] };
  if (p === "custom") return {};
  return structuredClone(PRESETS[p]);
}

export function presetOf(s: Schedule | null | undefined): SchedulePreset {
  if (!s) return "any";
  if (s.holidays) {
    const other = { ...s, holidays: undefined };
    if (isEmptySchedule(other)) return "holidays";
  }
  if (isEmptySchedule(s)) return Object.keys(s).length ? "custom" : "any";
  const norm = (x: Schedule) =>
    JSON.stringify({
      days: [...(x.days ?? [])].sort(),
      dayparts: [...(x.dayparts ?? [])].sort(),
      rest: [x.months, x.dates, x.fromHour, x.toHour, x.holidays?.length ? x.holidays : undefined],
    });
  for (const [k, v] of Object.entries(PRESETS)) {
    if (norm(v) === norm(s)) return k as SchedulePreset;
  }
  return "custom";
}

export interface HolidayInfo {
  id: string;
  label: string;
  group: "seasons" | "christian" | "jewish" | "islamic" | "asian" | "more";
  when: string;
}

/** The global holiday set (owner decision §12.4); ids match internal/core/holidays.go. */
export function holidays(): HolidayInfo[] {
  return [
    { id: "christmas", label: t("Christmas season"), group: "seasons", when: t("Dec 1–26") },
    { id: "new-year", label: t("New Year"), group: "seasons", when: t("Dec 26–Jan 1") },
    { id: "halloween", label: t("Halloween"), group: "seasons", when: t("October") },
    { id: "valentines", label: t("Valentine's Day"), group: "seasons", when: t("Feb 7–14") },
    {
      id: "thanksgiving-us",
      label: t("Thanksgiving (US)"),
      group: "seasons",
      when: t("The week of the fourth Thursday of November"),
    },
    {
      id: "thanksgiving-ca",
      label: t("Thanksgiving (Canada)"),
      group: "seasons",
      when: t("The week up to the second Monday of October"),
    },
    {
      id: "easter",
      label: t("Easter"),
      group: "christian",
      when: t("Palm Sunday to Easter Monday"),
    },
    {
      id: "easter-orthodox",
      label: t("Orthodox Easter"),
      group: "christian",
      when: t("Palm Sunday to Easter Monday"),
    },
    {
      id: "carnival",
      label: t("Carnival"),
      group: "christian",
      when: t("The five days to Shrove Tuesday"),
    },
    { id: "hanukkah", label: t("Hanukkah"), group: "jewish", when: t("The eight days") },
    { id: "passover", label: t("Passover"), group: "jewish", when: t("The eight days") },
    {
      id: "rosh-hashanah",
      label: t("Rosh Hashanah"),
      group: "jewish",
      when: t("The eve and both days"),
    },
    { id: "ramadan", label: t("Ramadan"), group: "islamic", when: t("The whole month") },
    {
      id: "eid-al-fitr",
      label: t("Eid al-Fitr"),
      group: "islamic",
      when: t("The eve and three days"),
    },
    {
      id: "eid-al-adha",
      label: t("Eid al-Adha"),
      group: "islamic",
      when: t("The eve and four days"),
    },
    {
      id: "lunar-new-year",
      label: t("Lunar New Year"),
      group: "asian",
      when: t("Two days before to a week after"),
    },
    {
      id: "mid-autumn",
      label: t("Mid-Autumn Festival"),
      group: "asian",
      when: t("The festival and a day either side"),
    },
    { id: "diwali", label: t("Diwali"), group: "asian", when: t("Five days around Lakshmi Puja") },
    { id: "holi", label: t("Holi"), group: "asian", when: t("Holika Dahan and Holi") },
    {
      id: "vesak",
      label: t("Vesak"),
      group: "asian",
      when: t("The full moon and a day either side"),
    },
    {
      id: "dia-de-muertos",
      label: t("Día de los Muertos"),
      group: "more",
      when: t("Oct 31–Nov 2"),
    },
    { id: "nowruz", label: t("Nowruz"), group: "more", when: t("Mar 20–Apr 2") },
  ];
}

export function holidayGroupLabel(g: HolidayInfo["group"]): string {
  switch (g) {
    case "seasons":
      return t("Seasons & days");
    case "christian":
      return t("Christian");
    case "jewish":
      return t("Jewish");
    case "islamic":
      return t("Islamic");
    case "asian":
      return t("Asian");
    default:
      return t("More");
  }
}

function dayNames(): string[] {
  return [t("Sun"), t("Mon"), t("Tue"), t("Wed"), t("Thu"), t("Fri"), t("Sat")];
}
export function monthNames(): string[] {
  return [
    t("Jan"),
    t("Feb"),
    t("Mar"),
    t("Apr"),
    t("May"),
    t("Jun"),
    t("Jul"),
    t("Aug"),
    t("Sep"),
    t("Oct"),
    t("Nov"),
    t("Dec"),
  ];
}
export function daypartLabel(d: Daypart): string {
  switch (d) {
    case "morning":
      return t("Mornings");
    case "afternoon":
      return t("Afternoons");
    case "evening":
      return t("Evenings");
    default:
      return t("Late (10 PM–5 AM)");
  }
}
export function deviceLabel(d: Device): string {
  switch (d) {
    case "tv":
      return t("TV");
    case "mobile":
      return t("Phone & tablet");
    case "desktop":
      return t("Computer");
    default:
      return t("Web");
  }
}

function dayRun(days: number[]): string {
  const names = dayNames();
  const sorted = [...days].sort((a, b) => a - b);
  const contiguous = sorted.every((d, i) => i === 0 || d === sorted[i - 1]! + 1);
  if (contiguous && sorted.length > 2) return `${names[sorted[0]!]}–${names[sorted.at(-1)!]}`;
  return sorted.map((d) => names[d]).join(", ");
}

/** A short chip for a row's schedule and devices: "Fri–Sat evenings · TV". */
export function scheduleSummary(s: Schedule | null | undefined, devices?: Device[]): string {
  const parts: string[] = [];
  if (s && !isEmptySchedule(s)) {
    const preset = presetOf(s);
    if (preset === "evenings") parts.push(t("Evenings"));
    else if (preset === "weeknights") parts.push(t("Weeknights"));
    else if (preset === "weekends") parts.push(t("Weekends"));
    else if (preset === "weekend-nights") parts.push(t("Fri–Sat evenings"));
    else if (preset === "mornings") parts.push(t("Mornings"));
    else {
      if (s.holidays?.length) {
        const byId = new Map(holidays().map((h) => [h.id, h.label]));
        parts.push(s.holidays.map((h) => byId.get(h) ?? h).join(", "));
      }
      if (s.days?.length) parts.push(dayRun(s.days));
      if (s.dayparts?.length) parts.push(s.dayparts.map(daypartLabel).join(", ").toLowerCase());
      if (s.months?.length) parts.push(s.months.map((m) => monthNames()[m - 1]).join(", "));
      if (s.dates?.length)
        parts.push(s.dates.map((d) => `${d.from}–${d.to}${d.yearly ? "" : ""}`).join(", "));
    }
  }
  if (devices?.length) parts.push(devices.map(deviceLabel).join(", "));
  return parts.join(" · ");
}

export function experienceLabel(e: Experience): string {
  switch (e) {
    case "watch":
      return t("Watch");
    case "listen":
      return t("Listen");
    case "read":
      return t("Read");
    case "gallery":
      return t("Gallery");
    case "live":
      return t("Live");
    default:
      return t("Everywhere");
  }
}

export function hideWatchedLabel(e: Experience): string {
  if (e === "listen") return t("Hide played");
  if (e === "read") return t("Hide finished");
  return t("Hide watched");
}

export function sourceGroupLabel(g: string): string {
  switch (g) {
    case "keep":
      return t("Keep watching");
    case "foryou":
      return t("For you");
    case "trending":
      return t("Trending & popular");
    case "new":
      return t("New");
    case "top":
      return t("Top rated");
    case "browse":
      return t("Browse");
    case "collections":
      return t("Your collections");
    case "plugins":
      return t("From plugins");
    case "live":
      return t("Live");
    default:
      return t("More");
  }
}

export const SOURCE_GROUP_ORDER = [
  "keep",
  "trending",
  "new",
  "foryou",
  "top",
  "browse",
  "collections",
  "plugins",
  "live",
];

/** "Trending {library}" → "Trending Movies". */
export function fillLibrary(name: string, library: string): string {
  return name.replaceAll("{library}", library);
}

/** "Fri 28 Nov · 9 PM" for the preview's date and time. */
export function formatPreviewAt(at: Date): string {
  return at.toLocaleString(undefined, {
    weekday: "short",
    day: "numeric",
    month: "short",
    hour: "numeric",
    minute: at.getMinutes() ? "2-digit" : undefined,
  });
}

/** A Date as the value of an <input type="datetime-local">. */
export function toLocalInput(d: Date): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

import { cn, useElementSize } from "@braedonsaunders/appkit-ui";
import { useId, useMemo, useState } from "react";
import { getLocale, t } from "../i18n";
import { fixed, formatHour, formatPercent } from "../lib/format";

/**
 * Small token-driven charts. Each is single-series (identity is named by the
 * chart title), uses thin marks with rounded data ends anchored to the
 * baseline, recessive gridlines, text in text tokens, and a hover/focus
 * tooltip. Colors come from semantic tokens so light and dark both work.
 */

function roundedTopBar(x: number, y: number, w: number, h: number, r: number): string {
  if (h <= 0) return "";
  const rr = Math.min(r, w / 2, h);
  return `M${x},${y + h}V${y + rr}Q${x},${y} ${x + rr},${y}H${x + w - rr}Q${x + w},${y} ${x + w},${y + rr}V${y + h}Z`;
}

function niceMax(v: number): number {
  if (v <= 0) return 1;
  const p = 10 ** Math.floor(Math.log10(v));
  const n = v / p;
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return step * p;
}

export interface BarDatum {
  key: string;
  label: string;
  value: number;
  detail?: string;
}

/** Vertical bars over time (e.g. plays per day). */
export function TimeBars({
  data,
  height: fixedHeight = 160,
  fill = false,
  ariaLabel,
  valueFormat = (v) => String(v),
  className,
}: {
  data: BarDatum[];
  height?: number;
  /** Take the container's height instead of `height` (give it a definite height). */
  fill?: boolean;
  ariaLabel: string;
  valueFormat?: (v: number) => string;
  className?: string;
}) {
  const [hover, setHover] = useState<number | null>(null);
  const [ref, size] = useElementSize<HTMLDivElement>();
  const width = Math.max(240, Math.round(size.width || 600));
  const height = fill ? Math.max(96, Math.round(size.height || fixedHeight)) : fixedHeight;
  const padL = 32;
  const padB = 22;
  const padT = 8;
  const max = niceMax(Math.max(0, ...data.map((d) => d.value)));
  const plotH = height - padB - padT;
  const slot = data.length ? (width - padL) / data.length : 0;
  const gap = Math.min(2, slot * 0.25);
  const barW = Math.max(1, slot - gap);
  const ticks = [0, max / 2, max];
  const labelIdx = data.length ? [0, Math.floor((data.length - 1) / 2), data.length - 1] : [];
  const h = hover !== null ? data[hover] : undefined;
  return (
    <div ref={ref} className={cn("relative", fill && "h-full min-h-0", className)}>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        width="100%"
        height={height}
        className={cn("block overflow-visible", fill && "absolute inset-0")}
        role="img"
        aria-label={ariaLabel}
        onMouseLeave={() => setHover(null)}
      >
        {ticks.map((tick) => {
          const y = padT + plotH - (tick / max) * plotH;
          return (
            <g key={tick}>
              <line
                x1={padL}
                x2={width}
                y1={y}
                y2={y}
                className="stroke-border-subtle"
                strokeWidth={1}
                vectorEffect="non-scaling-stroke"
              />
              <text
                x={padL - 6}
                y={y + 3}
                textAnchor="end"
                className="fill-fg-subtle text-[10px] tabular-nums"
              >
                {valueFormat(tick)}
              </text>
            </g>
          );
        })}
        {data.map((d, i) => {
          const bh = (d.value / max) * plotH;
          const x = padL + i * slot + gap / 2;
          return (
            <g key={d.key}>
              <rect
                x={padL + i * slot}
                y={padT}
                width={slot}
                height={plotH}
                fill="transparent"
                onMouseEnter={() => setHover(i)}
                onFocus={() => setHover(i)}
                onBlur={() => setHover(null)}
                tabIndex={0}
                aria-label={`${d.label}: ${valueFormat(d.value)}${d.detail ? `, ${d.detail}` : ""}`}
                className="focus:outline-none"
              />
              <path
                d={roundedTopBar(x, padT + plotH - bh, barW, bh, 4)}
                className={cn(
                  "pointer-events-none transition-opacity",
                  hover === null || hover === i ? "fill-primary" : "fill-primary opacity-40",
                )}
              />
            </g>
          );
        })}
        <line
          x1={padL}
          x2={width}
          y1={padT + plotH}
          y2={padT + plotH}
          className="stroke-border"
          strokeWidth={1}
          vectorEffect="non-scaling-stroke"
        />
        {labelIdx.map((i, n) => {
          const d = data[i];
          if (!d || (n > 0 && i === labelIdx[n - 1])) return null;
          const x = padL + i * slot + slot / 2;
          return (
            <text
              key={`l${i}`}
              x={x}
              y={height - 6}
              textAnchor={n === 0 ? "start" : n === 2 ? "end" : "middle"}
              className="fill-fg-subtle text-[10px]"
            >
              {d.label}
            </text>
          );
        })}
      </svg>
      {h && hover !== null ? (
        <div
          className="pointer-events-none absolute top-0 z-10 -translate-x-1/2 whitespace-nowrap rounded-md border border-border bg-elevated px-2.5 py-1.5 text-xs shadow-md"
          style={{ left: `${((padL + hover * slot + slot / 2) / width) * 100}%` }}
        >
          <div className="font-medium text-fg">{h.label}</div>
          <div className="tabular-nums text-fg-muted">
            {valueFormat(h.value)}
            {h.detail ? ` · ${h.detail}` : ""}
          </div>
        </div>
      ) : null}
    </div>
  );
}

export interface WeightDatum {
  key: string;
  label: string;
  weight: number;
  hint?: string;
}

/**
 * Horizontal diverging bars for weights in -1..1: likes extend right in the
 * brand hue, dislikes extend left in a contrasting categorical hue, with a
 * neutral zero line. Values are printed in text tokens.
 */
export function DivergingBars({
  data,
  ariaLabel,
  onSelect,
  selectedKey,
}: {
  data: WeightDatum[];
  ariaLabel: string;
  onSelect?: (d: WeightDatum) => void;
  selectedKey?: string;
}) {
  return (
    <ul className="space-y-1.5" aria-label={ariaLabel}>
      {data.map((d) => {
        const w = Math.max(-1, Math.min(1, d.weight));
        const pct = Math.abs(w) * 50;
        const Row = onSelect ? "button" : "div";
        return (
          <li key={d.key}>
            <Row
              {...(onSelect ? { type: "button" as const, onClick: () => onSelect(d) } : {})}
              title={d.hint}
              className={cn(
                "grid w-full grid-cols-[minmax(5rem,9rem)_1fr_2.75rem] items-center gap-2 rounded-md px-1 py-0.5 text-left",
                onSelect &&
                  "hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
                selectedKey === d.key && "bg-primary-subtle",
              )}
            >
              <span className="truncate text-sm text-fg">{d.label}</span>
              <span className="relative h-3" aria-hidden>
                <span className="absolute inset-y-0 left-1/2 w-px bg-border-strong" />
                <span
                  className={cn(
                    "absolute inset-y-0.5",
                    w >= 0 ? "left-1/2 rounded-r bg-primary" : "right-1/2 rounded-l bg-accent-rose",
                  )}
                  style={{ width: `${pct}%` }}
                />
              </span>
              <span className="text-right text-xs tabular-nums text-fg-muted">
                {w > 0 ? "+" : ""}
                {fixed(w, 2)}
              </span>
            </Row>
          </li>
        );
      })}
    </ul>
  );
}

/** Sequential single-hue columns (e.g. decades), magnitude by height. */
export function Columns({
  data,
  ariaLabel,
  height = 96,
}: {
  data: { key: string; label: string; value: number; hint?: string }[];
  ariaLabel: string;
  height?: number;
}) {
  const max = Math.max(0.0001, ...data.map((d) => Math.abs(d.value)));
  const [hover, setHover] = useState<string | null>(null);
  return (
    <div role="img" aria-label={ariaLabel} className="flex items-end gap-1.5" style={{ height }}>
      {data.map((d) => {
        const v = Math.max(0, d.value);
        const pct = (v / max) * 100;
        return (
          <div
            key={d.key}
            className="flex h-full min-w-0 flex-1 flex-col items-center gap-1"
            onMouseEnter={() => setHover(d.key)}
            onMouseLeave={() => setHover(null)}
            title={d.hint}
          >
            <div className="relative flex w-full flex-1 items-end">
              {hover === d.key ? (
                <span className="absolute -top-5 left-1/2 -translate-x-1/2 text-[10px] tabular-nums text-fg-muted">
                  {fixed(d.value, 2)}
                </span>
              ) : null}
              <div
                className={cn(
                  "w-full rounded-t bg-primary transition-opacity",
                  d.value <= 0 && "bg-border",
                )}
                style={{
                  height: `${Math.max(pct, d.value <= 0 ? 4 : 2)}%`,
                  opacity: d.value <= 0 ? 1 : 0.35 + 0.65 * (v / max),
                }}
              />
            </div>
            <span className="truncate text-[10px] text-fg-subtle">{d.label}</span>
          </div>
        );
      })}
    </div>
  );
}

/** 24 cells, one per hour; peak hours filled. */
export function HourStrip({ peaks, ariaLabel }: { peaks: number[]; ariaLabel: string }) {
  const set = useMemo(() => new Set(peaks.map((h) => ((h % 24) + 24) % 24)), [peaks]);
  return (
    <div
      role="img"
      aria-label={`${ariaLabel}: ${peaks.length ? peaks.map(formatHour).join(", ") : t("no clear peak")}`}
    >
      <div className="grid grid-cols-24 gap-0.5">
        {Array.from({ length: 24 }, (_, h) => (
          <div
            key={h}
            title={formatHour(h)}
            className={cn(
              "h-5 rounded-sm",
              set.has(h) ? "bg-primary" : "bg-bg-subtle ring-1 ring-inset ring-border-subtle",
            )}
          />
        ))}
      </div>
      <div className="mt-1 flex justify-between text-[10px] text-fg-subtle">
        <span>{formatHour(0)}</span>
        <span>{formatHour(6)}</span>
        <span>{formatHour(12)}</span>
        <span>{formatHour(18)}</span>
        <span>{formatHour(24)}</span>
      </div>
    </div>
  );
}

/** Thin horizontal meter (0..1) with a label and value. */
export function Meter({
  value,
  buffered,
  label,
  valueLabel,
  tone = "primary",
  onScrim = false,
}: {
  value: number;
  /** 0..1 read-ahead already downloaded beyond the playhead. */
  buffered?: number;
  label?: string;
  valueLabel?: string;
  tone?: "primary" | "warning" | "danger";
  /** Drawn over artwork: text and track use the on-scrim colour. */
  onScrim?: boolean;
}) {
  const id = useId();
  const v = Math.max(0, Math.min(1, Number.isFinite(value) ? value : 0));
  const b = Math.max(v, Math.min(1, Number.isFinite(buffered ?? NaN) ? (buffered as number) : v));
  return (
    <div>
      {label || valueLabel ? (
        <div className="mb-1 flex items-baseline justify-between gap-2 text-xs">
          <span id={id} className={onScrim ? "text-on-scrim/80" : "text-fg-muted"}>
            {label}
          </span>
          <span className={cn("tabular-nums", onScrim ? "text-on-scrim" : "text-fg")}>
            {valueLabel ?? formatPercent(v)}
          </span>
        </div>
      ) : null}
      <div
        className={cn(
          "relative h-1.5 overflow-hidden rounded-full",
          onScrim ? "bg-on-scrim/25" : "bg-bg-subtle ring-1 ring-inset ring-border-subtle",
        )}
        role="meter"
        aria-labelledby={label ? id : undefined}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(v * 100)}
        aria-valuetext={valueLabel}
      >
        {b > v && (
          <div
            className={cn(
              "absolute inset-y-0 left-0 rounded-full transition-[width] duration-500",
              onScrim ? "bg-on-scrim/45" : "bg-primary/30",
            )}
            style={{ width: `${b * 100}%` }}
          />
        )}
        <div
          className={cn(
            "relative h-full rounded-full transition-[width] duration-500",
            tone === "primary" ? "bg-primary" : tone === "warning" ? "bg-warning" : "bg-danger",
          )}
          style={{ width: `${v * 100}%` }}
        />
      </div>
    </div>
  );
}

export interface RankDatum {
  key: string;
  label: string;
  value: number;
  /** Shown after the value, e.g. "12 plays". */
  detail?: string;
  /** Optional leading visual (poster, avatar). */
  leading?: React.ReactNode;
}

/**
 * Ranked horizontal bars, largest first, one hue: for "top" lists where the
 * reader compares parts with each other. Bars share one baseline.
 */
export function RankedBars({
  data,
  valueFormat = (v) => String(v),
  empty,
  ariaLabel,
}: {
  data: RankDatum[];
  valueFormat?: (v: number) => string;
  empty?: React.ReactNode;
  ariaLabel: string;
}) {
  const rows = [...data].sort((a, b) => b.value - a.value);
  const max = Math.max(0, ...rows.map((r) => r.value));
  if (!rows.length) return <>{empty ?? null}</>;
  return (
    <ol className="space-y-2.5" aria-label={ariaLabel}>
      {rows.map((r) => (
        <li key={r.key} className="flex items-center gap-3">
          {r.leading ? <span className="shrink-0">{r.leading}</span> : null}
          <div className="min-w-0 flex-1">
            <div className="flex items-baseline justify-between gap-3 text-sm">
              <span className="truncate text-fg" title={r.label}>
                {r.label}
              </span>
              <span className="shrink-0 text-xs tabular-nums text-fg-muted">
                <span className="font-medium text-fg">{valueFormat(r.value)}</span>
                {r.detail ? ` · ${r.detail}` : ""}
              </span>
            </div>
            <div className="mt-1 h-1.5 overflow-hidden rounded-full bg-bg-subtle">
              <div
                className="h-full rounded-full bg-primary"
                style={{ width: `${max > 0 ? Math.max(2, (r.value / max) * 100) : 0}%` }}
              />
            </div>
          </div>
        </li>
      ))}
    </ol>
  );
}

export interface SplitDatum {
  key: string;
  label: string;
  value: number;
}

/** One hue in decreasing strength, so up to six parts read as one whole. */
const SPLIT_STEPS = [
  "bg-primary",
  "bg-primary/70",
  "bg-primary/45",
  "bg-primary/30",
  "bg-primary/20",
  "bg-primary/12",
];

/**
 * A tall segmented bar for one whole split into at most six parts: each
 * part's share printed on it where it fits, and a legend of names and values.
 * Parts beyond six fold into "Other".
 */
export function SplitBar({
  data,
  valueFormat = (v) => String(v),
  ariaLabel,
}: {
  data: SplitDatum[];
  valueFormat?: (v: number) => string;
  ariaLabel: string;
}) {
  const sorted = [...data].filter((d) => d.value > 0).sort((a, b) => b.value - a.value);
  const parts =
    sorted.length > 6
      ? [
          ...sorted.slice(0, 5),
          {
            key: "other",
            label: t("Other"),
            value: sorted.slice(5).reduce((n, d) => n + d.value, 0),
          },
        ]
      : sorted;
  const total = parts.reduce((n, d) => n + d.value, 0);
  if (!total) return <p className="text-sm text-fg-muted">{t("Nothing in this period.")}</p>;
  return (
    <div>
      <div
        className="flex h-9 gap-0.5 overflow-hidden rounded-lg"
        role="img"
        aria-label={`${ariaLabel}: ${parts
          .map((p) => `${p.label} ${formatPercent(p.value / total)}`)
          .join(", ")}`}
      >
        {parts.map((p, i) => {
          const share = p.value / total;
          return (
            <div
              key={p.key}
              title={`${p.label}: ${valueFormat(p.value)} (${formatPercent(share)})`}
              className={cn(
                "flex min-w-0 items-center justify-center first:rounded-l-lg last:rounded-r-lg",
                SPLIT_STEPS[i] ?? SPLIT_STEPS[5],
              )}
              style={{ width: `${share * 100}%` }}
            >
              {share >= 0.08 ? (
                <span
                  className={cn(
                    "truncate px-1 text-xs font-semibold tabular-nums",
                    i < 2 ? "text-primary-fg" : "text-fg",
                  )}
                >
                  {formatPercent(share)}
                </span>
              ) : null}
            </div>
          );
        })}
      </div>
      <ul className="mt-3 flex flex-wrap gap-x-5 gap-y-1.5 text-xs">
        {parts.map((p, i) => (
          <li key={p.key} className="flex items-center gap-1.5">
            <span className={cn("size-2.5 rounded-sm", SPLIT_STEPS[i] ?? SPLIT_STEPS[5])} />
            <span className="text-fg">{p.label}</span>
            <span className="tabular-nums text-fg-muted">{valueFormat(p.value)}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

export interface HeatCell {
  weekday: number;
  hour: number;
  value: number;
}

const HEAT_STEPS = [
  "bg-primary/15",
  "bg-primary/35",
  "bg-primary/60",
  "bg-primary/85",
  "bg-primary",
];

/**
 * When something happens across the week: weekdays down, hours across, one
 * hue that deepens with the value. Hover a cell for its figure.
 */
export function WeekHeatmap({
  cells,
  valueFormat = (v) => String(v),
  ariaLabel,
  weekStartsOn = 1,
}: {
  cells: HeatCell[];
  valueFormat?: (v: number) => string;
  ariaLabel: string;
  /** 0 = Sunday, 1 = Monday. */
  weekStartsOn?: 0 | 1;
}) {
  const [hover, setHover] = useState<HeatCell | null>(null);
  const grid = useMemo(() => {
    const m = new Map<string, number>();
    for (const c of cells)
      m.set(`${c.weekday}:${c.hour}`, (m.get(`${c.weekday}:${c.hour}`) ?? 0) + c.value);
    return m;
  }, [cells]);
  const max = Math.max(0, ...grid.values());
  const days = Array.from({ length: 7 }, (_, i) => (i + weekStartsOn) % 7);
  // 2023-01-01 was a Sunday: weekday n is that date plus n days.
  const dayName = (n: number) =>
    new Date(Date.UTC(2023, 0, 1 + n)).toLocaleDateString(getLocale(), {
      weekday: "short",
      timeZone: "UTC",
    });
  const step = (v: number) =>
    v <= 0 || max <= 0
      ? -1
      : Math.min(HEAT_STEPS.length - 1, Math.floor((v / max) * HEAT_STEPS.length - 1e-9));
  return (
    <div role="img" aria-label={ariaLabel} onMouseLeave={() => setHover(null)}>
      <div className="grid grid-cols-[2.5rem_1fr] gap-x-2 gap-y-1">
        {days.map((d) => (
          <div key={d} className="contents">
            <div className="self-center text-right text-[11px] text-fg-subtle">{dayName(d)}</div>
            <div className="grid grid-cols-24 gap-0.5">
              {Array.from({ length: 24 }, (_, h) => {
                const v = grid.get(`${d}:${h}`) ?? 0;
                const s = step(v);
                return (
                  <div
                    key={h}
                    onMouseEnter={() => setHover({ weekday: d, hour: h, value: v })}
                    title={`${dayName(d)} ${formatHour(h)}: ${valueFormat(v)}`}
                    className={cn(
                      "h-5 rounded-[3px]",
                      s < 0 ? "bg-bg-subtle ring-1 ring-inset ring-border-subtle" : HEAT_STEPS[s],
                      hover && hover.weekday === d && hover.hour === h && "ring-2 ring-fg/60",
                    )}
                  />
                );
              })}
            </div>
          </div>
        ))}
        <div />
        <div className="flex justify-between text-[10px] text-fg-subtle">
          <span>{formatHour(0)}</span>
          <span>{formatHour(6)}</span>
          <span>{formatHour(12)}</span>
          <span>{formatHour(18)}</span>
          <span>{formatHour(24)}</span>
        </div>
      </div>
      <div className="mt-3 flex items-center justify-between gap-3 text-xs">
        <span className="tabular-nums text-fg-muted">
          {hover
            ? `${dayName(hover.weekday)} ${formatHour(hover.hour)} · ${valueFormat(hover.value)}`
            : " "}
        </span>
        <span className="flex items-center gap-1.5 text-fg-subtle">
          {t("Less")}
          {HEAT_STEPS.map((c) => (
            <span key={c} className={cn("size-3 rounded-[3px]", c)} />
          ))}
          {t("More")}
        </span>
      </div>
    </div>
  );
}

export interface StackDatum {
  key: string;
  label: string;
  a: number;
  b: number;
}

/**
 * Columns over time, each split into two series (e.g. direct vs transcoded),
 * stacked from the baseline with a 2px surface gap and a legend.
 */
export function StackedColumns({
  data,
  aLabel,
  bLabel,
  height = 180,
  ariaLabel,
  valueFormat = (v) => String(v),
}: {
  data: StackDatum[];
  aLabel: string;
  bLabel: string;
  height?: number;
  ariaLabel: string;
  valueFormat?: (v: number) => string;
}) {
  const [hover, setHover] = useState<number | null>(null);
  const [ref, size] = useElementSize<HTMLDivElement>();
  const width = Math.max(240, Math.round(size.width || 600));
  const padL = 32;
  const padB = 22;
  const padT = 8;
  const max = niceMax(Math.max(0, ...data.map((d) => d.a + d.b)));
  const plotH = height - padB - padT;
  const slot = data.length ? (width - padL) / data.length : 0;
  const gap = Math.min(2, slot * 0.25);
  const barW = Math.max(1, slot - gap);
  const labelIdx = data.length ? [0, Math.floor((data.length - 1) / 2), data.length - 1] : [];
  const h = hover !== null ? data[hover] : undefined;
  return (
    <div>
      <div ref={ref} className="relative">
        <svg
          viewBox={`0 0 ${width} ${height}`}
          width="100%"
          height={height}
          className="block overflow-visible"
          role="img"
          aria-label={ariaLabel}
          onMouseLeave={() => setHover(null)}
        >
          {[0, max / 2, max].map((tick) => {
            const y = padT + plotH - (tick / max) * plotH;
            return (
              <g key={tick}>
                <line
                  x1={padL}
                  x2={width}
                  y1={y}
                  y2={y}
                  className="stroke-border-subtle"
                  strokeWidth={1}
                  vectorEffect="non-scaling-stroke"
                />
                <text
                  x={padL - 6}
                  y={y + 3}
                  textAnchor="end"
                  className="fill-fg-subtle text-[10px] tabular-nums"
                >
                  {valueFormat(tick)}
                </text>
              </g>
            );
          })}
          {data.map((d, i) => {
            const x = padL + i * slot + gap / 2;
            const ah = (d.a / max) * plotH;
            const bh = (d.b / max) * plotH;
            const dim = hover !== null && hover !== i;
            const base = padT + plotH;
            return (
              <g key={d.key} className={cn("transition-opacity", dim && "opacity-40")}>
                <rect
                  x={padL + i * slot}
                  y={padT}
                  width={slot}
                  height={plotH}
                  fill="transparent"
                  onMouseEnter={() => setHover(i)}
                  onFocus={() => setHover(i)}
                  onBlur={() => setHover(null)}
                  tabIndex={0}
                  aria-label={`${d.label}: ${aLabel} ${valueFormat(d.a)}, ${bLabel} ${valueFormat(d.b)}`}
                  className="focus:outline-none"
                />
                {ah > 0 ? (
                  <path
                    d={
                      bh > 0
                        ? `M${x},${base}V${base - ah}H${x + barW}V${base}Z`
                        : roundedTopBar(x, base - ah, barW, ah, 4)
                    }
                    className="pointer-events-none fill-primary"
                  />
                ) : null}
                {bh > 0 ? (
                  <path
                    d={roundedTopBar(x, base - ah - bh - (ah > 0 ? 2 : 0), barW, bh, 4)}
                    className="pointer-events-none fill-primary/40"
                  />
                ) : null}
              </g>
            );
          })}
          <line
            x1={padL}
            x2={width}
            y1={padT + plotH}
            y2={padT + plotH}
            className="stroke-border"
            strokeWidth={1}
            vectorEffect="non-scaling-stroke"
          />
          {labelIdx.map((i, n) => {
            const d = data[i];
            if (!d || (n > 0 && i === labelIdx[n - 1])) return null;
            return (
              <text
                key={`l${i}`}
                x={padL + i * slot + slot / 2}
                y={height - 6}
                textAnchor={n === 0 ? "start" : n === 2 ? "end" : "middle"}
                className="fill-fg-subtle text-[10px]"
              >
                {d.label}
              </text>
            );
          })}
        </svg>
        {h && hover !== null ? (
          <div
            className="pointer-events-none absolute top-0 z-10 -translate-x-1/2 whitespace-nowrap rounded-md border border-border bg-elevated px-2.5 py-1.5 text-xs shadow-md"
            style={{ left: `${((padL + hover * slot + slot / 2) / width) * 100}%` }}
          >
            <div className="font-medium text-fg">{h.label}</div>
            <div className="tabular-nums text-fg-muted">
              {aLabel} {valueFormat(h.a)} · {bLabel} {valueFormat(h.b)}
            </div>
          </div>
        ) : null}
      </div>
      <ul className="mt-2 flex gap-4 text-xs">
        <li className="flex items-center gap-1.5">
          <span className="size-2.5 rounded-sm bg-primary" />
          <span className="text-fg">{aLabel}</span>
        </li>
        <li className="flex items-center gap-1.5">
          <span className="size-2.5 rounded-sm bg-primary/40" />
          <span className="text-fg">{bLabel}</span>
        </li>
      </ul>
    </div>
  );
}

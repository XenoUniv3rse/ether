import { Badge, Button, Input, Popover, cn, toast } from "@braedonsaunders/appkit-ui";
import {
  AlertTriangle,
  ArrowRight,
  CalendarPlus,
  CalendarRange,
  CheckCircle2,
  ChevronRight,
  Link2,
  Link2Off,
  Search,
  Wand2,
} from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { errorMessage, isAbort } from "../../lib/api";
import { t, tn, tx } from "../../i18n";
import { formatNumber } from "../../lib/format";
import { useDebouncedValue } from "../../lib/hooks";
import { ChannelLogo } from "./ChannelLogo";
import {
  clock,
  dayClock,
  progress,
  tv,
  type Channel,
  type Coverage,
  type GuideChannel,
  type MatchChange,
  type Programme,
  type Source,
} from "./model";

/** Search every imported XMLTV guide for a channel to take listings from. */
export function GuideChannelSearch({
  initial,
  onPick,
  autoFocus,
}: {
  initial?: string;
  onPick: (g: GuideChannel) => void;
  autoFocus?: boolean;
}) {
  const [q, setQ] = useState(initial ?? "");
  const debounced = useDebouncedValue(q, 200);
  const [items, setItems] = useState<GuideChannel[] | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    const ctl = new AbortController();
    tv.guideChannels(debounced.trim(), ctl.signal, 40)
      .then((r) => {
        setItems(r ?? []);
        setError("");
      })
      .catch((e) => {
        if (!isAbort(e)) setError(errorMessage(e));
      });
    return () => ctl.abort();
  }, [debounced]);
  return (
    <div className="space-y-2">
      <div className="relative">
        <Search
          size={14}
          className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
          aria-hidden
        />
        <Input
          autoFocus={autoFocus}
          className="pl-8"
          value={q}
          onChange={(e) => setQ(e.currentTarget.value)}
          placeholder={t("Search guide channels by name or id…")}
          aria-label={t("Search guide channels")}
        />
      </div>
      {error ? <p className="text-xs text-danger">{error}</p> : null}
      {items && items.length === 0 ? (
        <p className="px-1 py-3 text-center text-xs text-fg-muted">
          {t("No guide channels match. Add an XMLTV guide source to bring in more listings.")}
        </p>
      ) : null}
      <ul className="max-h-72 space-y-0.5 overflow-y-auto" aria-label={t("Guide channels")}>
        {(items ?? []).map((g) => (
          <li key={`${g.sourceId}/${g.guideId}`}>
            <button
              type="button"
              onClick={() => onPick(g)}
              className="flex w-full items-center gap-2.5 rounded-md p-1.5 text-left hover:bg-surface-hover"
            >
              <ChannelLogo name={g.name} logo={g.logo} seed={g.guideId} size={28} />
              <span className="min-w-0 flex-1">
                <span className="block truncate text-sm text-fg">{g.name}</span>
                <span className="block truncate text-[11px] text-fg-muted">
                  {g.guideId} · {g.sourceName}
                </span>
              </span>
              <span className="shrink-0 text-right text-[11px] text-fg-subtle">
                {g.programmes
                  ? tn(g.programmes, "{count} listing", "{count} listings")
                  : t("No listings")}
                {g.until ? (
                  <span className="block">{t("to {time}", { time: dayClock(g.until) })}</span>
                ) : null}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}

/** The mapping popover used on each coverage row and the channel's Guide tab. */
export function MapGuideButton({
  channel,
  onMapped,
  label = t("Map guide"),
  variant = "outline",
}: {
  channel: Channel;
  onMapped: (c: Channel) => void | Promise<void>;
  label?: string;
  variant?: "outline" | "ghost" | "default";
}) {
  const [open, setOpen] = useState(false);
  return (
    <Popover
      open={open}
      onOpenChange={setOpen}
      align="end"
      className="w-[22rem] p-3"
      trigger={
        <Button
          size="sm"
          variant={variant}
          aria-label={t("Map guide for {channel}", { channel: channel.name })}
          onClick={() => setOpen((o) => !o)}
        >
          <Link2 size={13} /> {label}
        </Button>
      }
    >
      <p className="mb-2 text-xs text-fg-muted">
        {tx("Listings for {channel} come from the guide channel you pick.", {
          channel: <strong className="text-fg">{channel.name}</strong>,
        })}
      </p>
      <GuideChannelSearch
        autoFocus
        initial={channel.name}
        onPick={(g) => {
          setOpen(false);
          void onMapped({ ...channel, guideSourceId: g.sourceId, guideId: g.guideId });
        }}
      />
    </Popover>
  );
}

const GROUP_PAGE = 120;

/** A collapsible lineup section with a sticky header, as on the Channels tab. */
function GuideGroup({
  title,
  icon,
  count,
  tone,
  defaultOpen = true,
  children,
}: {
  title: string;
  icon: React.ReactNode;
  count: number;
  tone?: "warning";
  defaultOpen?: boolean;
  children: React.ReactNode;
}) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <section aria-label={title}>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        className={cn(
          "sticky top-0 z-10 flex w-full items-center gap-1.5 border-b border-border/60 bg-bg-subtle/95 px-4 py-1.5 text-left text-[11px] font-semibold uppercase tracking-wider backdrop-blur hover:text-fg",
          tone === "warning" ? "text-warning" : "text-fg-subtle",
        )}
      >
        <ChevronRight size={12} className={cn("transition-transform", open && "rotate-90")} />
        {icon}
        <span className="min-w-0 flex-1 truncate">{title}</span>
        <span className="tabular-nums">{formatNumber(count)}</span>
      </button>
      {open ? children : null}
    </section>
  );
}

function GuideChannelRow({
  channel: c,
  coverage,
  selected,
  onSelect,
}: {
  channel: Channel;
  coverage?: Coverage;
  selected: boolean;
  onSelect: (id: string) => void;
}) {
  const listed = (coverage?.programmes ?? 0) > 0;
  return (
    <li>
      <button
        type="button"
        onClick={() => onSelect(c.id)}
        aria-current={selected ? "true" : undefined}
        aria-label={t("Guide for {channel}", { channel: `${c.number} ${c.name}` })}
        className={cn(
          "flex w-full items-center gap-2.5 rounded-lg px-2 py-1.5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
          selected
            ? "bg-primary-subtle ring-1 ring-inset ring-primary/30"
            : "hover:bg-surface-hover",
          !c.enabled && "opacity-55",
        )}
      >
        <span
          className={cn(
            "w-9 shrink-0 text-right font-mono text-xs tabular-nums",
            selected ? "text-primary" : "text-fg-subtle",
          )}
        >
          {c.number}
        </span>
        <ChannelLogo name={c.name} logo={c.logo} seed={c.id} size={34} />
        <span className="min-w-0 flex-1">
          <span
            className={cn(
              "block truncate text-sm font-medium",
              selected ? "text-primary" : "text-fg",
            )}
          >
            {c.name}
          </span>
          <span className="block truncate text-xs text-fg-muted">
            {listed
              ? coverage?.until
                ? t("Listings to {time}", { time: dayClock(coverage.until) })
                : t("Listed")
              : c.guideId
                ? t("Its guide has no listings ahead")
                : t("Not matched to a guide")}
          </span>
        </span>
        {listed ? (
          <ChevronRight size={14} className="shrink-0 text-fg-subtle" />
        ) : (
          <AlertTriangle
            size={13}
            className="shrink-0 text-warning"
            aria-label={t("No guide data")}
          />
        )}
      </button>
    </li>
  );
}

function PagedRows({
  items,
  render,
}: {
  items: Channel[];
  render: (c: Channel) => React.ReactNode;
}) {
  const [limit, setLimit] = useState(GROUP_PAGE);
  return (
    <ul className="px-2 py-1">
      {items.slice(0, limit).map(render)}
      {items.length > limit ? (
        <li className="px-2 py-1">
          <Button
            variant="ghost"
            size="sm"
            className="w-full text-xs"
            onClick={() => setLimit((l) => l + GROUP_PAGE * 4)}
          >
            {t("Show {count} more", {
              count: formatNumber(Math.min(items.length - limit, GROUP_PAGE * 4)),
            })}
          </Button>
        </li>
      ) : null}
    </ul>
  );
}

/** A guide source is stale when it has failed, or its listings end within six hours. */
const STALE_MS = 6 * 60 * 60 * 1000;

/**
 * The Guide tab: how much of the lineup has listings, the channels that still
 * need a guide, every matched channel grouped by where its listings come
 * from, and the guide sources with how fresh they are. Each row opens a
 * drawer; the guide grid stays on the right.
 */
export function GuideList({
  channels,
  lineup,
  sources,
  coverage,
  selected,
  onSelect,
  onOpenSource,
  onAutoMatch,
  onAddGuideSource,
  now,
}: {
  /** Channels after the panel's search and filter. */
  channels: Channel[];
  /** The whole lineup, for the coverage totals. */
  lineup: Channel[];
  sources: Source[];
  coverage: Map<string, Coverage> | null;
  selected: string | null;
  onSelect: (id: string) => void;
  onOpenSource: (id: string) => void;
  onAutoMatch: () => void;
  onAddGuideSource: () => void;
  now: number;
}) {
  const has = (c: Channel) => (coverage?.get(c.id)?.programmes ?? 0) > 0;
  const live = lineup.filter((c) => c.enabled && c.kind === "iptv");
  const liveMissing = coverage === null ? null : live.filter((c) => !has(c)).length;
  const pct = liveMissing === null || !live.length ? 1 : (live.length - liveMissing) / live.length;

  const iptv = channels.filter((c) => c.kind === "iptv");
  const needs = iptv.filter((c) => !has(c));
  const sourceName = (id?: string) => sources.find((s) => s.id === id)?.name ?? t("Unknown source");
  const matched = new Map<string, Channel[]>();
  for (const c of iptv) {
    if (!has(c)) continue;
    const k = c.guideSourceId || c.sourceId || "";
    matched.set(k, [...(matched.get(k) ?? []), c]);
  }
  const guideSources = sources.filter(
    (s) => s.hasGuide || Boolean(s.guideUrl) || (s.guideChannelCount ?? 0) > 0,
  );
  const row = (c: Channel) => (
    <GuideChannelRow
      key={c.id}
      channel={c}
      coverage={coverage?.get(c.id)}
      selected={selected === c.id}
      onSelect={onSelect}
    />
  );

  return (
    <div className="pb-6">
      <div className="space-y-3 border-b border-border/60 p-3 sm:px-4">
        <div className="flex items-start gap-3">
          <div className="min-w-0 flex-1">
            <p className="text-sm font-semibold text-fg">
              {liveMissing === null
                ? t("Checking listings…")
                : liveMissing
                  ? t("{covered} of {total} on-air channels have listings", {
                      covered: formatNumber(live.length - liveMissing),
                      total: formatNumber(live.length),
                    })
                  : tn(
                      live.length,
                      "Your {count} on-air channel has listings",
                      "All {count} on-air channels have listings",
                    )}
            </p>
            <p className="text-xs text-fg-muted">{t("Virtual channels build their own guide.")}</p>
          </div>
          <span className="text-lg font-semibold tabular-nums text-fg">
            {liveMissing === null ? "—" : `${Math.round(pct * 100)}%`}
          </span>
        </div>
        <div className="h-1.5 overflow-hidden rounded-full bg-bg-subtle">
          <div
            className={cn(
              "h-full rounded-full transition-all",
              pct >= 0.999 ? "bg-success" : "bg-gradient-to-r from-primary to-info",
            )}
            style={{ width: `${pct * 100}%` }}
          />
        </div>
        <div className="flex gap-2">
          <Button
            size="sm"
            className="flex-1"
            onClick={onAutoMatch}
            disabled={coverage === null || !liveMissing}
          >
            <Wand2 size={13} /> {t("Auto-match")}
          </Button>
          <Button size="sm" variant="outline" className="flex-1" onClick={onAddGuideSource}>
            <CalendarPlus size={13} /> {t("Add guide")}
          </Button>
        </div>
      </div>

      {needs.length ? (
        <GuideGroup
          title={t("Needs a guide")}
          icon={<AlertTriangle size={12} />}
          count={needs.length}
          tone="warning"
        >
          <PagedRows items={needs} render={row} />
        </GuideGroup>
      ) : coverage !== null && iptv.length ? (
        <div className="flex items-center gap-2 px-4 py-3 text-xs text-fg-muted">
          <CheckCircle2 size={14} className="text-success" />
          {t("Every channel here is getting listings.")}
        </div>
      ) : null}

      {[...matched.entries()]
        .sort((a, b) => b[1].length - a[1].length)
        .map(([id, items]) => (
          <GuideGroup
            key={id}
            title={sourceName(id)}
            icon={<Link2 size={12} />}
            count={items.length}
          >
            <PagedRows items={items} render={row} />
          </GuideGroup>
        ))}

      {iptv.length === 0 && coverage !== null ? (
        <p className="px-4 py-8 text-center text-xs text-fg-muted">
          {channels.length
            ? t("No IPTV channels match.")
            : t("Add an M3U source to bring in IPTV channels.")}
        </p>
      ) : null}

      <GuideGroup
        title={t("Listings from")}
        icon={<CalendarRange size={12} />}
        count={guideSources.length}
      >
        {guideSources.length ? (
          <ul className="px-2 py-1">
            {guideSources.map((s) => {
              const until = s.guideUntil ? Date.parse(s.guideUntil) : 0;
              const stale = Boolean(s.lastError) || (until > 0 && until - now < STALE_MS);
              return (
                <li key={s.id}>
                  <button
                    type="button"
                    onClick={() => onOpenSource(s.id)}
                    className="flex w-full items-center gap-2.5 rounded-lg px-2 py-1.5 text-left hover:bg-surface-hover"
                  >
                    <span className="grid size-[34px] shrink-0 place-items-center rounded-lg bg-surface text-fg-muted ring-1 ring-border">
                      <CalendarRange size={15} />
                    </span>
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm font-medium text-fg">{s.name}</span>
                      <span
                        className={cn(
                          "block truncate text-xs",
                          stale ? "text-warning" : "text-fg-muted",
                        )}
                      >
                        {s.lastError
                          ? t("Last refresh failed")
                          : until
                            ? until <= now
                              ? t("Listings have run out")
                              : t("{count} channels · to {time}", {
                                  count: formatNumber(s.guideChannelCount ?? 0),
                                  time: dayClock(new Date(until)),
                                })
                            : tn(s.guideChannelCount ?? 0, "{count} channel", "{count} channels")}
                      </span>
                    </span>
                    {stale ? (
                      <AlertTriangle size={13} className="shrink-0 text-warning" />
                    ) : (
                      <ChevronRight size={14} className="shrink-0 text-fg-subtle" />
                    )}
                  </button>
                </li>
              );
            })}
          </ul>
        ) : (
          <p className="px-4 py-3 text-xs text-fg-muted">
            {t("No guide sources yet. Add an XMLTV guide to give channels listings.")}
          </p>
        )}
      </GuideGroup>
    </div>
  );
}

const PREVIEW_HOURS = 12;

/** What a channel would show with one guide channel: now, next and later. */
function ListingsPreview({ channel, pick }: { channel: Channel; pick: GuideChannel | null }) {
  const [items, setItems] = useState<Programme[] | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!pick) {
      setItems([]);
      return;
    }
    setItems(null);
    const ctl = new AbortController();
    const from = new Date();
    const until = new Date(from.getTime() + PREVIEW_HOURS * 60 * 60 * 1000);
    tv.preview({ ...channel, guideSourceId: pick.sourceId, guideId: pick.guideId }, ctl.signal, {
      from,
      until,
    })
      .then((r) => {
        setItems((r ?? []).filter((p) => Date.parse(p.end) > from.getTime()).slice(0, 8));
        setError("");
      })
      .catch((e) => {
        if (!isAbort(e)) setError(errorMessage(e));
      });
    return () => ctl.abort();
  }, [channel, pick]);
  if (error) return <p className="text-xs text-danger">{error}</p>;
  if (items === null)
    return <p className="py-6 text-center text-xs text-fg-muted">{t("Loading listings…")}</p>;
  if (!items.length)
    return (
      <p className="rounded-lg border border-dashed border-border px-3 py-6 text-center text-xs text-fg-muted">
        {pick
          ? t("This guide channel has nothing scheduled in the next {count} hours.", {
              count: PREVIEW_HOURS,
            })
          : t("Pick a guide channel to see what it lists.")}
      </p>
    );
  const at = Date.now();
  return (
    <ol className="divide-y divide-border overflow-hidden rounded-xl border border-border bg-surface">
      {items.map((p) => {
        const live = Date.parse(p.start) <= at && Date.parse(p.end) > at;
        return (
          <li key={p.id} className="flex items-start gap-3 px-3 py-2">
            <span className="w-16 shrink-0 pt-0.5 text-xs tabular-nums text-fg-muted">
              {clock(p.start)}
            </span>
            <span className="min-w-0 flex-1">
              <span className="flex items-center gap-1.5">
                <span className="truncate text-sm font-medium text-fg">{p.item.title}</span>
                {live ? <Badge variant="destructive">{t("Live")}</Badge> : null}
              </span>
              {p.item.summary ? (
                <span className="line-clamp-1 text-xs text-fg-muted">{p.item.summary}</span>
              ) : null}
              {live ? (
                <span className="mt-1 block h-0.5 overflow-hidden rounded-full bg-border">
                  <span
                    className="block h-full rounded-full bg-primary/70"
                    style={{ width: `${progress(p, at) * 100}%` }}
                  />
                </span>
              ) : null}
            </span>
          </li>
        );
      })}
    </ol>
  );
}

const sameGuide = (c: Channel, g: GuideChannel) =>
  g.guideId === c.guideId && g.sourceId === (c.guideSourceId || c.sourceId);

/**
 * One channel's guide, in a drawer: the best matching guide channels with
 * their listings previewed before anything is saved.
 */
export function GuideMatchPanel({
  channel,
  sourceName,
  coverage,
  onUse,
  onOpenChannel,
  setFooter,
}: {
  channel: Channel;
  sourceName: string;
  coverage?: Coverage;
  onUse: (c: Channel) => Promise<void>;
  onOpenChannel: () => void;
  setFooter: (footer: React.ReactNode) => void;
}) {
  const [q, setQ] = useState(channel.name);
  const debounced = useDebouncedValue(q, 200);
  const [items, setItems] = useState<GuideChannel[] | null>(null);
  const [error, setError] = useState("");
  const [pick, setPick] = useState<GuideChannel | null>(null);
  const [busy, setBusy] = useState(false);
  const listed = (coverage?.programmes ?? 0) > 0;

  useEffect(() => {
    const ctl = new AbortController();
    tv.guideChannels(debounced.trim(), ctl.signal, 12)
      .then((r) => {
        const list = r ?? [];
        setItems(list);
        setError("");
        setPick(
          (p) =>
            p ??
            list.find((g) => sameGuide(channel, g)) ??
            list.find((g) => g.programmes > 0) ??
            null,
        );
      })
      .catch((e) => {
        if (!isAbort(e)) setError(errorMessage(e));
      });
    return () => ctl.abort();
  }, [debounced, channel]);

  const changed = pick !== null && !sameGuide(channel, pick);
  const use = async (next: Channel) => {
    setBusy(true);
    try {
      await onUse(next);
    } finally {
      setBusy(false);
    }
  };
  useEffect(() => {
    setFooter(
      <div className="flex w-full flex-wrap items-center gap-2">
        {channel.guideId ? (
          <Button
            variant="ghost"
            disabled={busy}
            onClick={() => void use({ ...channel, guideId: "", guideSourceId: "" })}
          >
            <Link2Off size={14} /> {t("Remove guide")}
          </Button>
        ) : null}
        <Button variant="ghost" onClick={onOpenChannel}>
          {t("Open channel")}
        </Button>
        <Button
          className="ml-auto"
          disabled={busy || !changed}
          onClick={() =>
            pick && void use({ ...channel, guideSourceId: pick.sourceId, guideId: pick.guideId })
          }
        >
          <CheckCircle2 size={14} /> {t("Use this guide")}
        </Button>
      </div>,
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [channel, pick, busy, changed]);

  return (
    <div className="space-y-5">
      <div className="flex items-center gap-3 rounded-xl border border-border bg-surface p-3">
        <ChannelLogo name={channel.name} logo={channel.logo} seed={channel.id} size={40} />
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm font-semibold text-fg">
            <span className="mr-1.5 font-mono text-xs text-fg-subtle">{channel.number}</span>
            {channel.name}
          </p>
          <p className="truncate text-xs text-fg-muted">{sourceName}</p>
        </div>
        {listed ? (
          <Badge variant="success">
            {coverage?.until ? t("to {time}", { time: dayClock(coverage.until) }) : t("Listed")}
          </Badge>
        ) : (
          <Badge variant="warning">{t("No listings")}</Badge>
        )}
      </div>

      <section className="space-y-2">
        <h3 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
          {t("Guide channels")}
        </h3>
        <div className="relative">
          <Search
            size={14}
            className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
            aria-hidden
          />
          <Input
            className="pl-8"
            value={q}
            onChange={(e) => setQ(e.currentTarget.value)}
            placeholder={t("Search guide channels by name or id…")}
            aria-label={t("Search guide channels")}
          />
        </div>
        {error ? <p className="text-xs text-danger">{error}</p> : null}
        {items === null ? (
          <p className="py-3 text-center text-xs text-fg-muted">{t("Searching guides…")}</p>
        ) : items.length === 0 ? (
          <p className="rounded-lg border border-dashed border-border px-3 py-4 text-center text-xs text-fg-muted">
            {t("No guide channels match. Try fewer words, or add an XMLTV guide source.")}
          </p>
        ) : (
          <ul className="space-y-1" aria-label={t("Guide channels")}>
            {items.map((g) => {
              const on =
                pick !== null && pick.sourceId === g.sourceId && pick.guideId === g.guideId;
              const current = sameGuide(channel, g);
              return (
                <li key={`${g.sourceId}/${g.guideId}`}>
                  <button
                    type="button"
                    aria-pressed={on}
                    onClick={() => setPick(g)}
                    className={cn(
                      "flex w-full items-center gap-2.5 rounded-lg border p-2 text-left transition-colors",
                      on
                        ? "border-primary/40 bg-primary-subtle ring-1 ring-primary/30"
                        : "border-transparent hover:bg-surface-hover",
                    )}
                  >
                    <ChannelLogo name={g.name} logo={g.logo} seed={g.guideId} size={30} />
                    <span className="min-w-0 flex-1">
                      <span className="flex items-center gap-1.5">
                        <span className="truncate text-sm font-medium text-fg">{g.name}</span>
                        {current ? <Badge variant="secondary">{t("Current")}</Badge> : null}
                      </span>
                      <span className="block truncate text-[11px] text-fg-muted">
                        {g.sourceName}
                      </span>
                    </span>
                    <span
                      className={cn(
                        "shrink-0 text-right text-[11px]",
                        g.programmes ? "text-fg-muted" : "text-warning",
                      )}
                    >
                      {g.programmes
                        ? g.until
                          ? t("to {time}", { time: dayClock(g.until) })
                          : tn(g.programmes, "{count} listing", "{count} listings")
                        : t("No listings")}
                    </span>
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </section>

      <section className="space-y-2">
        <h3 className="text-xs font-semibold uppercase tracking-wider text-fg-subtle">
          {pick ? t("What {guide} lists", { guide: pick.name }) : t("Listings")}
        </h3>
        <ListingsPreview channel={channel} pick={pick} />
      </section>
    </div>
  );
}

/** Suggested matches for every channel without listings, reviewed before applying. */
export function AutoMatchPanel({
  channels,
  sources,
  onApplied,
  setFooter,
}: {
  channels: Channel[];
  sources: Source[];
  onApplied: () => Promise<void>;
  setFooter: (footer: React.ReactNode) => void;
}) {
  const [proposals, setProposals] = useState<MatchChange[] | null>(null);
  const [accepted, setAccepted] = useState<Set<string>>(new Set());
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const byId = useMemo(() => new Map(channels.map((c) => [c.id, c])), [channels]);
  const sourceName = (id?: string) => sources.find((s) => s.id === id)?.name ?? "";

  useEffect(() => {
    tv.match({ dryRun: true })
      .then((r) => {
        const changes = r.changes ?? [];
        setProposals(changes);
        setAccepted(new Set(changes.map((c) => c.channelId)));
      })
      .catch((e) => setError(errorMessage(e)));
  }, []);

  useEffect(() => {
    setFooter(
      <div className="flex w-full items-center gap-2">
        <span className="text-xs text-fg-muted">
          {t("Matched by guide id, then by channel name.")}
        </span>
        <Button
          className="ml-auto"
          disabled={busy || accepted.size === 0}
          onClick={() => {
            setBusy(true);
            tv.match({ channelIds: [...accepted] })
              .then(async (r) => {
                toast.success(
                  tn(
                    r.matched,
                    "{count} channel matched to a guide",
                    "{count} channels matched to a guide",
                  ),
                );
                await onApplied();
              })
              .catch((e) =>
                toast.error(t("Couldn't apply matches"), { description: errorMessage(e) }),
              )
              .finally(() => setBusy(false));
          }}
        >
          <CheckCircle2 size={14} /> {t("Apply {count}", { count: accepted.size })}
        </Button>
      </div>,
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [accepted, busy]);

  if (error) return <p className="text-sm text-danger">{error}</p>;
  if (proposals === null)
    return <p className="py-10 text-center text-sm text-fg-muted">{t("Finding matches…")}</p>;
  if (!proposals.length)
    return (
      <div className="rounded-xl border border-dashed border-border p-8 text-center">
        <p className="text-sm font-medium text-fg">{t("No new matches found")}</p>
        <p className="mt-1 text-xs text-fg-muted">
          {t("Open a channel to search every guide by hand, or add another guide source.")}
        </p>
      </div>
    );
  return (
    <ul className="space-y-1">
      {proposals.map((p) => {
        const c = byId.get(p.channelId);
        const on = accepted.has(p.channelId);
        return (
          <li key={p.channelId}>
            <label className="flex cursor-pointer items-center gap-3 rounded-lg border border-border bg-surface px-3 py-2 text-sm">
              <input
                type="checkbox"
                checked={on}
                onChange={() =>
                  setAccepted((s) => {
                    const n = new Set(s);
                    if (on) n.delete(p.channelId);
                    else n.add(p.channelId);
                    return n;
                  })
                }
                aria-label={t("Accept match for {channel}", { channel: c?.name ?? p.channelId })}
              />
              {c ? <ChannelLogo name={c.name} logo={c.logo} seed={c.id} size={28} /> : null}
              <span className="min-w-0 flex-1">
                <span className="block truncate text-fg">
                  {c?.name ?? p.channelName ?? p.channelId}
                </span>
                <span className="flex min-w-0 items-center gap-1 text-xs text-fg-muted">
                  <ArrowRight size={11} className="shrink-0" />
                  <span className="truncate">
                    {p.guideName} · {p.guideSourceName || sourceName(p.guideSourceId)}
                  </span>
                </span>
              </span>
              {p.method === "name" ? <Badge variant="secondary">{t("by name")}</Badge> : null}
            </label>
          </li>
        );
      })}
    </ul>
  );
}

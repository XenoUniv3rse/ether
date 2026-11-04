import { Badge, Button, cn } from "@braedonsaunders/appkit-ui";
import {
  AlertTriangle,
  CalendarRange,
  ChevronRight,
  KeyRound,
  ListVideo,
  Radio,
  Sparkles,
  Star,
} from "lucide-react";
import { memo, useMemo, useState } from "react";
import { t, tn } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { formatNumber, relativeTime } from "../../lib/format";
import { ChannelLogo } from "./ChannelLogo";
import {
  clock,
  dayClock,
  duration,
  isChannelSource,
  onNow,
  progress,
  type Channel,
  type Programme,
  type Source,
} from "./model";

export type Selection =
  | { kind: "guide" }
  | { kind: "channel"; id: string }
  | { kind: "new" }
  | { kind: "source"; id: string }
  | { kind: "map"; id: string };

const PAGE = 120;

/** The channel lineup: virtual channels, then each IPTV source's channels. */
export function ChannelList({
  channels,
  sources,
  guide,
  covered,
  selected,
  onSelect,
  onToggleFavorite,
  now,
  emptyHint,
}: {
  channels: Channel[];
  sources: Source[];
  guide: Map<string, Programme[]>;
  covered: (c: Channel) => boolean | undefined;
  selected: string | null;
  onSelect: (id: string) => void;
  onToggleFavorite: (c: Channel) => void;
  now: number;
  emptyHint: React.ReactNode;
}) {
  const { locale } = useLocale();
  // One pass over the lineup; a 16,000-channel provider is one source group.
  const groups = useMemo(() => {
    const out: { key: string; title: string; virtual: boolean; items: Channel[] }[] = [];
    const virtual: Channel[] = [];
    const bySource = new Map<string, Channel[]>();
    for (const c of channels) {
      if (c.kind === "virtual") virtual.push(c);
      else if (c.kind === "iptv") {
        const k = c.sourceId ?? "";
        const list = bySource.get(k);
        if (list) list.push(c);
        else bySource.set(k, [c]);
      }
    }
    if (virtual.length)
      out.push({ key: "virtual", title: t("Virtual channels"), virtual: true, items: virtual });
    const names = new Map(sources.map((s) => [s.id, s.name]));
    for (const [id, items] of bySource)
      out.push({ key: `src-${id}`, title: names.get(id) ?? "IPTV", virtual: false, items });
    return out;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [channels, sources, locale]);
  if (!channels.length) return <div className="px-4 py-10 text-center">{emptyHint}</div>;
  return (
    <div className="pb-6">
      {groups.map((g) => (
        <ChannelGroup
          key={g.key}
          title={g.title}
          icon={g.virtual ? <Sparkles size={12} /> : <Radio size={12} />}
          items={g.items}
          guide={guide}
          covered={covered}
          selected={selected}
          onSelect={onSelect}
          onToggleFavorite={onToggleFavorite}
          now={now}
        />
      ))}
    </div>
  );
}

function ChannelGroup({
  title,
  icon,
  items,
  ...rest
}: {
  title: string;
  icon: React.ReactNode;
  items: Channel[];
  guide: Map<string, Programme[]>;
  covered: (c: Channel) => boolean | undefined;
  selected: string | null;
  onSelect: (id: string) => void;
  onToggleFavorite: (c: Channel) => void;
  now: number;
}) {
  const [open, setOpen] = useState(true);
  const [limit, setLimit] = useState(PAGE);
  return (
    <section aria-label={title}>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        className="sticky top-0 z-10 flex w-full items-center gap-1.5 border-b border-border/60 bg-bg-subtle/95 px-4 py-1.5 text-left text-[11px] font-semibold uppercase tracking-wider text-fg-subtle backdrop-blur hover:text-fg"
      >
        <ChevronRight size={12} className={cn("transition-transform", open && "rotate-90")} />
        {icon}
        <span className="min-w-0 flex-1 truncate">{title}</span>
        <span className="tabular-nums">{formatNumber(items.length)}</span>
      </button>
      {open ? (
        <ul className="px-2 py-1">
          {items.slice(0, limit).map((c) => {
            const live = onNow(rest.guide.get(c.id), rest.now);
            return (
              <ChannelRow
                key={c.id}
                channel={c}
                live={live}
                hasGuide={rest.covered(c)}
                on={rest.selected === c.id}
                // Only rows with a programme on show the clock.
                now={live ? rest.now : 0}
                onSelect={rest.onSelect}
                onToggleFavorite={rest.onToggleFavorite}
              />
            );
          })}
          {items.length > limit ? (
            <li className="px-2 py-1">
              <Button
                variant="ghost"
                size="sm"
                className="w-full text-xs"
                onClick={() => setLimit((l) => l + PAGE * 4)}
              >
                {t("Show {count} more", {
                  count: formatNumber(Math.min(items.length - limit, PAGE * 4)),
                })}
              </Button>
            </li>
          ) : null}
        </ul>
      ) : null}
    </section>
  );
}

/** One lineup row; memoised so a selection or favourite only re-renders the rows it touches. */
const ChannelRow = memo(function ChannelRow({
  channel: c,
  live,
  hasGuide,
  on,
  onSelect,
  onToggleFavorite,
  now,
}: {
  channel: Channel;
  live: Programme | undefined;
  hasGuide: boolean | undefined;
  on: boolean;
  onSelect: (id: string) => void;
  onToggleFavorite: (c: Channel) => void;
  now: number;
}) {
  return (
    <li className="group relative">
      <button
        type="button"
        onClick={() => onSelect(c.id)}
        aria-current={on ? "true" : undefined}
        aria-label={`${c.number} ${c.name}`}
        className={cn(
          "flex w-full items-center gap-2.5 rounded-lg px-2 py-1.5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
          on ? "bg-primary-subtle ring-1 ring-inset ring-primary/30" : "hover:bg-surface-hover",
          !c.enabled && "opacity-55",
        )}
      >
        <span
          className={cn(
            "w-9 shrink-0 text-right font-mono text-xs tabular-nums",
            on ? "text-primary" : "text-fg-subtle",
          )}
        >
          {c.number}
        </span>
        <ChannelLogo name={c.name} logo={c.logo} seed={c.id} size={34} />
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1">
            <span className={cn("truncate text-sm font-medium", on ? "text-primary" : "text-fg")}>
              {c.name}
            </span>
            {c.health === "unavailable" ? (
              <Badge variant="secondary">{t("Unavailable")}</Badge>
            ) : null}
            {c.favorite ? (
              <Star
                size={11}
                className="shrink-0 fill-warning text-warning"
                aria-label={t("Favourite")}
              />
            ) : null}
          </span>
          {live ? (
            <span className="block">
              <span className="block truncate text-xs text-fg-muted">{live.item.title}</span>
              <span className="mt-1 block h-0.5 overflow-hidden rounded-full bg-border">
                <span
                  className="block h-full rounded-full bg-primary/70"
                  style={{ width: `${progress(live, now) * 100}%` }}
                />
              </span>
            </span>
          ) : (
            <span className="block truncate text-xs text-fg-subtle">
              {!c.enabled
                ? t("Off air")
                : hasGuide === false
                  ? t("No guide data")
                  : c.group || (c.kind === "virtual" ? t("Virtual") : t("Live"))}
            </span>
          )}
        </span>
        {hasGuide === false && c.enabled ? (
          <AlertTriangle
            size={13}
            className="shrink-0 text-warning"
            aria-label={t("No guide data")}
          />
        ) : null}
      </button>
      <button
        type="button"
        onClick={() => onToggleFavorite(c)}
        aria-label={
          c.favorite
            ? t("Unfavourite {channel}", { channel: c.name })
            : t("Favourite {channel}", { channel: c.name })
        }
        aria-pressed={c.favorite}
        className="absolute right-2 top-1/2 hidden -translate-y-1/2 rounded-md bg-surface p-1 text-fg-subtle shadow-sm hover:text-warning focus-visible:block group-hover:block"
      >
        <Star size={13} className={cn(c.favorite && "fill-warning text-warning")} />
      </button>
      {live ? (
        <span className="sr-only">
          {t("On now: {title} until {time}", { title: live.item.title, time: clock(live.end) })}
        </span>
      ) : null}
    </li>
  );
});

/** Every IPTV and guide source, on the left panel's Sources tab. */
export function SourceList({
  sources,
  selected,
  onSelect,
  query = "",
}: {
  sources: Source[];
  selected: Selection;
  onSelect: (s: Selection) => void;
  /** The Live TV search: narrows the sources by name or by one of their groups. */
  query?: string;
}) {
  const needle = query.trim().toLowerCase();
  const channelSources = sources
    .filter(isChannelSource)
    .filter(
      (s) =>
        !needle ||
        s.name.toLowerCase().includes(needle) ||
        (s.groups ?? []).some((g) => g.name.toLowerCase().includes(needle)),
    );
  return (
    <div className="space-y-4 p-3 sm:p-4">
      <div className="space-y-1.5">
        <h2 className="px-1 text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
          {t("Channel sources")}
        </h2>
        {channelSources.length === 0 ? (
          <p className="px-1 text-xs text-fg-muted">
            {t(
              "No channel sources yet. Add an M3U playlist or an add-on for IPTV channels. Guides live on the Guide tab.",
            )}
          </p>
        ) : null}
        {channelSources.map((s) => {
          const on = selected.kind === "source" && selected.id === s.id;
          const epgOnly =
            s.hasPlaylist === false ||
            (!s.playlistUrl && !s.channelCount && (s.guideChannelCount ?? 0) > 0);
          return (
            <button
              key={s.id}
              type="button"
              onClick={() => onSelect({ kind: "source", id: s.id })}
              aria-current={on ? "true" : undefined}
              className={cn(
                "flex w-full items-start gap-3 rounded-xl border bg-surface p-3 text-left transition-colors hover:border-border-strong",
                on ? "border-primary/40 ring-1 ring-primary/30" : "border-border",
              )}
            >
              <span
                className={cn(
                  "grid size-9 shrink-0 place-items-center rounded-lg",
                  epgOnly ? "bg-info/15 text-info" : "bg-primary-subtle text-primary",
                )}
              >
                {epgOnly ? <CalendarRange size={17} /> : <ListVideo size={17} />}
              </span>
              <span className="min-w-0 flex-1 space-y-1">
                <span className="flex items-center gap-2">
                  <span className="truncate text-sm font-medium text-fg">{s.name}</span>
                  {s.auth?.type ? (
                    <KeyRound
                      size={12}
                      className="shrink-0 text-fg-subtle"
                      aria-label={s.auth.type === "xtream" ? t("Xtream login") : t("Signs in")}
                    />
                  ) : null}
                  {s.lastError ? (
                    <Badge variant="destructive" className="shrink-0">
                      {t("Refresh failed")}
                    </Badge>
                  ) : null}
                </span>
                <span className="flex flex-wrap gap-x-2 gap-y-0.5 text-xs text-fg-muted">
                  <span>
                    {epgOnly
                      ? t("Guide only")
                      : tn(s.channelCount ?? 0, "{count} channel", "{count} channels")}
                  </span>
                  {s.guideChannelCount ? (
                    <span>
                      · {tn(s.guideChannelCount, "{count} guide channel", "{count} guide channels")}
                    </span>
                  ) : null}
                  {s.guideUntil ? (
                    <span>· {t("through {time}", { time: dayClock(s.guideUntil) })}</span>
                  ) : null}
                </span>
                <span className="block text-[11px] text-fg-subtle">
                  {s.guideFrom && s.guideUntil
                    ? t("Refreshed {when} · {span} of listings", {
                        when: relativeTime(s.updatedAt),
                        span: duration(Date.parse(s.guideUntil) - Date.parse(s.guideFrom)),
                      })
                    : t("Refreshed {when}", { when: relativeTime(s.updatedAt) })}
                </span>
              </span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

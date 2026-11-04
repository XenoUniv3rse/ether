import { DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import { CalendarClock, CalendarRange, Layers, Plug, Radio, Tv, Users2 } from "lucide-react";
import { useLayoutEffect, useMemo, useRef, useState } from "react";
import { Artwork } from "../../components/Artwork";
import { RankedBars } from "../../components/charts";
import { Kpi, KpiRow } from "../../components/Kpi";
import { t, tn } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { formatNumber, formatPercent, unit } from "../../lib/format";
import {
  MINUTE,
  clock,
  onNow,
  progress,
  type Channel,
  type Coverage,
  type LiveUsage,
  type Programme,
} from "./model";

const HOUR = 60 * MINUTE;

type Airing = { channel: Channel; programme: Programme };

/**
 * Live TV at a glance: what is on, what starts soon, how much of the lineup
 * has listings, who is watching and how much of each provider's allowance
 * Ether is using. Built from what the page already loaded plus /usage.
 */
export function LiveOverview({
  channels,
  lineup,
  guide,
  coverage,
  usage,
  now,
  onSelectChannel,
}: {
  /** The lineup after the search and filter. */
  channels: Channel[];
  /** The whole lineup, for totals. */
  lineup: Channel[];
  guide: Map<string, Programme[]>;
  coverage: Map<string, Coverage> | null;
  usage: LiveUsage | null;
  now: number;
  onSelectChannel: (id: string) => void;
}) {
  const { locale } = useLocale();
  const content = useRef<HTMLDivElement>(null);
  const [visibleRows, setVisibleRows] = useState(4);
  useLayoutEffect(() => {
    const el = content.current;
    if (!el) return;
    const measure = () =>
      setVisibleRows(
        window.matchMedia("(min-width: 1024px)").matches
          ? Math.max(1, Math.min(8, Math.floor(el.clientHeight / 60)))
          : 4,
      );
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    window.addEventListener("resize", measure);
    measure();
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", measure);
    };
  }, []);
  const onAir = useMemo(() => lineup.filter((c) => c.enabled), [lineup]);
  const iptvOnAir = useMemo(() => onAir.filter((c) => c.kind === "iptv"), [onAir]);
  const listed = useMemo(
    () =>
      coverage === null
        ? null
        : iptvOnAir.filter((c) => (coverage.get(c.id)?.programmes ?? 0) > 0).length,
    [iptvOnAir, coverage],
  );
  const groupCount = useMemo(() => new Set(onAir.map((c) => c.group ?? "")).size, [onAir]);

  const airing = useMemo(() => {
    const out: Airing[] = [];
    for (const c of channels) {
      if (!c.enabled) continue;
      const p = onNow(guide.get(c.id), now);
      if (p) out.push({ channel: c, programme: p });
    }
    return out;
  }, [channels, guide, now]);

  const watching = useMemo(
    () => new Map((usage?.watching ?? []).map((w) => [w.channelId, w.viewers])),
    [usage],
  );
  const featured = useMemo(() => {
    const rank = (a: Airing) =>
      watching.has(a.channel.id) ? 0 : a.channel.favorite ? 1 : a.programme.item.thumb ? 2 : 3;
    const seen = new Set<string>();
    return [...airing]
      .sort(
        (a, b) =>
          rank(a) - rank(b) ||
          (watching.get(b.channel.id) ?? 0) - (watching.get(a.channel.id) ?? 0) ||
          0,
      )
      .filter((a) => {
        const title = a.programme.item.title.trim().toLowerCase();
        if (seen.has(title)) return false;
        seen.add(title);
        return true;
      })
      .slice(0, 8);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [airing, watching]);

  const soon = useMemo(() => {
    const out: Airing[] = [];
    const seen = new Set<string>();
    for (const c of channels) {
      if (!c.enabled) continue;
      for (const p of guide.get(c.id) ?? []) {
        const start = Date.parse(p.start);
        if (start <= now || start > now + 2 * HOUR) continue;
        const k = p.item.title.trim().toLowerCase();
        if (!k || seen.has(k)) continue;
        out.push({ channel: c, programme: p });
      }
    }
    return out
      .sort((a, b) => Date.parse(a.programme.start) - Date.parse(b.programme.start))
      .filter((a) => {
        const k = a.programme.item.title.trim().toLowerCase();
        if (seen.has(k)) return false;
        seen.add(k);
        return true;
      })
      .slice(0, 8);
  }, [channels, guide, now]);

  const groups = useMemo(() => {
    const counts = new Map<string, number>();
    for (const c of onAir) counts.set(c.group ?? "", (counts.get(c.group ?? "") ?? 0) + 1);
    return [...counts]
      .sort((a, b) => b[1] - a[1])
      .slice(0, 8)
      .map(([g, n]) => ({ key: g || "-", label: g || t("No group"), value: n }));
  }, [onAir, locale]);

  const capped = (usage?.sources ?? []).filter((s) => s.limit > 0);
  const open = capped.reduce((n, s) => n + s.open, 0);
  const limit = capped.reduce((n, s) => n + s.limit, 0);
  const starts = (usage?.sources ?? []).reduce((n, s) => n + s.startsLastHour, 0);
  const pct = listed === null || !iptvOnAir.length ? null : listed / iptvOnAir.length;

  return (
    <div className="flex min-h-0 flex-col gap-4 lg:h-full">
      <KpiRow className="shrink-0">
        <Kpi
          label={t("On air")}
          value={formatNumber(onAir.length)}
          detail={tn(groupCount, "In {count} group", "In {count} groups")}
          icon={<Tv size={17} />}
        />
        <Kpi
          label={t("Listings")}
          value={pct === null ? "—" : formatPercent(pct)}
          detail={
            listed === null
              ? t("Checking listings…")
              : t("{covered} of {total} IPTV channels", {
                  covered: formatNumber(listed),
                  total: formatNumber(iptvOnAir.length),
                })
          }
          icon={<CalendarRange size={17} />}
          tone={pct !== null && pct < 0.8 ? "warning" : "success"}
        />
        <Kpi
          label={t("Watching now")}
          value={usage ? formatNumber(usage.viewers) : "—"}
          detail={usage ? tn(usage.sessions, "{count} stream", "{count} streams") : undefined}
          icon={<Users2 size={17} />}
          tone="info"
        />
        <Kpi
          label={t("Provider connections")}
          value={
            capped.length
              ? t("{open} of {limit}", { open: formatNumber(open), limit: formatNumber(limit) })
              : usage
                ? t("No limit")
                : "—"
          }
          detail={
            usage
              ? tn(starts, "{count} new stream this hour", "{count} new streams this hour")
              : undefined
          }
          icon={<Plug size={17} />}
          tone={capped.length && open >= limit ? "warning" : "primary"}
        />
      </KpiRow>

      <div className="grid gap-4 lg:min-h-0 lg:flex-1 lg:grid-cols-3">
        <DashboardPanel fill title={t("On now")} icon={<Radio size={16} />}>
          <div ref={content} className="h-full">
            {featured.length ? (
              <ul className="divide-y divide-border">
                {featured.slice(0, visibleRows).map((a) => (
                  <li key={a.channel.id}>
                    <OnAirCard
                      airing={a}
                      viewers={watching.get(a.channel.id) ?? 0}
                      now={now}
                      onSelect={onSelectChannel}
                    />
                  </li>
                ))}
              </ul>
            ) : (
              <p className="py-6 text-center text-sm text-fg-muted">
                {t("No programmes in this window.")}
              </p>
            )}
          </div>
        </DashboardPanel>

        <DashboardPanel fill title={t("Starting soon")} icon={<CalendarClock size={16} />}>
          {soon.length ? (
            <ol className="divide-y divide-border">
              {soon.slice(0, visibleRows).map((a) => (
                <li key={`${a.channel.id}-${a.programme.id}`}>
                  <button
                    type="button"
                    onClick={() => onSelectChannel(a.channel.id)}
                    className="flex w-full items-center gap-3 py-2 text-left hover:bg-surface-hover"
                  >
                    <span className="w-16 shrink-0 text-xs font-semibold tabular-nums text-fg">
                      {clock(a.programme.start)}
                    </span>
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm text-fg">
                        {a.programme.item.title}
                      </span>
                      <span className="block truncate text-xs text-fg-muted">
                        {a.channel.number} · {a.channel.name}
                      </span>
                    </span>
                  </button>
                </li>
              ))}
            </ol>
          ) : (
            <p className="py-6 text-center text-sm text-fg-muted">
              {t("No programmes in this window.")}
            </p>
          )}
        </DashboardPanel>

        <DashboardPanel fill title={t("Biggest groups")} icon={<Layers size={16} />}>
          <RankedBars
            ariaLabel={t("Biggest groups")}
            data={groups.slice(0, visibleRows)}
            valueFormat={(v) => formatNumber(v)}
            empty={<p className="py-6 text-center text-sm text-fg-muted">{t("No channels yet")}</p>}
          />
        </DashboardPanel>
      </div>
    </div>
  );
}

/** A compact programme row alongside the other dashboard panels. */
function OnAirCard({
  airing: { channel: c, programme: p },
  viewers,
  now,
  onSelect,
}: {
  airing: Airing;
  viewers: number;
  now: number;
  onSelect: (id: string) => void;
}) {
  const left = Math.max(1, Math.round((Date.parse(p.end) - now) / MINUTE));
  return (
    <button
      type="button"
      onClick={() => onSelect(c.id)}
      aria-label={t("{channel}: {title}, until {time}", {
        channel: c.name,
        title: p.item.title,
        time: clock(p.end),
      })}
      className="flex w-full items-center gap-3 rounded-md py-2 text-left hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
    >
      <Artwork
        src={p.item.thumb || c.logo}
        alt=""
        title=""
        icon={<Tv size={18} className="text-fg-subtle" />}
        className="size-10 shrink-0 rounded-md"
        imgClassName={p.item.thumb ? "object-cover" : "object-contain p-1"}
      />
      <span className="min-w-0 flex-1">
        <span className="block truncate text-sm font-medium text-fg">
          {p.item.title || t("Untitled")}
        </span>
        <span className="block truncate text-xs text-fg-muted">
          {c.number} · {c.name}
        </span>
        <span className="mt-1 block h-0.5 overflow-hidden rounded-full bg-bg-subtle" aria-hidden>
          <span
            className="block h-full bg-fg-subtle/40"
            style={{ width: `${progress(p, now) * 100}%` }}
          />
        </span>
      </span>
      <span className="shrink-0 text-right text-xs tabular-nums text-fg-muted">
        <span className="block">{clock(p.end)}</span>
        <span className="block text-[11px] text-fg-subtle">{unit(left, "minute")}</span>
        {viewers ? (
          <span className="block">{tn(viewers, "{count} watching", "{count} watching")}</span>
        ) : null}
      </span>
    </button>
  );
}

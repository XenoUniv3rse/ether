import { DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import { Badge, cn } from "@braedonsaunders/appkit-ui";
import { CalendarClock, CalendarRange, Layers, Plug, Radio, Tv, Users2 } from "lucide-react";
import { useMemo } from "react";
import { Artwork } from "../../components/Artwork";
import { RankedBars } from "../../components/charts";
import { Kpi, KpiRow } from "../../components/Kpi";
import { t, tn } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { formatNumber, formatPercent, relativeTime, unit } from "../../lib/format";
import { ChannelLogo } from "./ChannelLogo";
import {
  MINUTE,
  clock,
  hueOf,
  isChannelSource,
  onNow,
  progress,
  type Channel,
  type Coverage,
  type LiveUsage,
  type Programme,
  type Source,
} from "./model";

const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

type Airing = { channel: Channel; programme: Programme };

/**
 * Live TV at a glance: what is on, what starts soon, how much of the lineup
 * has listings, who is watching and how much of each provider's allowance
 * Ether is using. Built from what the page already loaded plus /usage.
 */
export function LiveOverview({
  channels,
  lineup,
  sources,
  guide,
  coverage,
  usage,
  now,
  onSelectChannel,
  onSelectSource,
}: {
  /** The lineup after the search and filter. */
  channels: Channel[];
  /** The whole lineup, for totals. */
  lineup: Channel[];
  sources: Source[];
  guide: Map<string, Programme[]>;
  coverage: Map<string, Coverage> | null;
  usage: LiveUsage | null;
  now: number;
  onSelectChannel: (id: string) => void;
  onSelectSource: (id: string) => void;
}) {
  const { locale } = useLocale();
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

  const channelSources = sources.filter(isChannelSource);
  const capped = (usage?.sources ?? []).filter((s) => s.limit > 0);
  const open = capped.reduce((n, s) => n + s.open, 0);
  const limit = capped.reduce((n, s) => n + s.limit, 0);
  const starts = (usage?.sources ?? []).reduce((n, s) => n + s.startsLastHour, 0);
  const pct = listed === null || !iptvOnAir.length ? null : listed / iptvOnAir.length;

  return (
    <div className="space-y-6">
      <KpiRow>
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

      <DashboardPanel title={t("On now")} icon={<Radio size={16} />}>
        {featured.length ? (
          <ul className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
            {featured.map((a) => (
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
      </DashboardPanel>

      <div className="grid gap-6 lg:grid-cols-3">
        <DashboardPanel title={t("Starting soon")} icon={<CalendarClock size={16} />}>
          {soon.length ? (
            <ol className="divide-y divide-border">
              {soon.map((a) => (
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

        <DashboardPanel title={t("Biggest groups")} icon={<Layers size={16} />}>
          <RankedBars
            ariaLabel={t("Biggest groups")}
            data={groups}
            valueFormat={(v) => formatNumber(v)}
            empty={<p className="py-6 text-center text-sm text-fg-muted">{t("No channels yet")}</p>}
          />
        </DashboardPanel>

        <DashboardPanel title={t("Sources")} icon={<Plug size={16} />}>
          {channelSources.length ? (
            <ul className="divide-y divide-border">
              {channelSources.map((s) => (
                <li key={s.id}>
                  <SourceHealthRow
                    source={s}
                    usage={usage?.sources.find((u) => u.sourceId === s.id)}
                    now={now}
                    onSelect={onSelectSource}
                  />
                </li>
              ))}
            </ul>
          ) : (
            <p className="py-6 text-center text-sm text-fg-muted">
              {t("No channel sources yet. Use Add to bring in IPTV channels.")}
            </p>
          )}
        </DashboardPanel>
      </div>
    </div>
  );
}

/** A big picture card for something on air now. */
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
  const hue = hueOf(c.id);
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
      className="group block w-full overflow-hidden rounded-xl border border-border bg-surface text-left shadow-xs transition-all hover:-translate-y-0.5 hover:shadow-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
    >
      <div
        className="relative aspect-video overflow-hidden"
        style={{
          background: `radial-gradient(120% 90% at 30% 20%, hsl(${hue} 55% 30%), hsl(${(hue + 40) % 360} 55% 10%))`,
        }}
      >
        {p.item.thumb ? (
          <Artwork
            src={p.item.thumb}
            alt=""
            title=""
            className="absolute inset-0 size-full object-cover transition-transform duration-500 group-hover:scale-105"
          />
        ) : (
          <div className="absolute inset-0 grid place-items-center transition-transform duration-500 group-hover:scale-105">
            <ChannelLogo name={c.name} logo={c.logo} seed={c.id} size={84} />
          </div>
        )}
        <div className="absolute inset-0 bg-gradient-to-t from-black/60 via-transparent to-transparent" />
        <span className="absolute left-2.5 top-2.5 flex items-center gap-1.5 rounded-md bg-black/55 px-1.5 py-0.5 text-[10px] font-bold uppercase tracking-wider text-white backdrop-blur">
          <span className="size-1.5 rounded-full bg-danger" aria-hidden />
          {t("Live")}
        </span>
        {viewers ? (
          <span className="absolute right-2.5 top-2.5 rounded-md bg-primary px-1.5 py-0.5 text-[10px] font-semibold text-white">
            {tn(viewers, "{count} watching", "{count} watching")}
          </span>
        ) : null}
        <span className="absolute bottom-2 left-2.5 rounded bg-black/55 px-1.5 py-0.5 font-mono text-[11px] tabular-nums text-white">
          {c.number}
        </span>
        <span className="absolute inset-x-0 bottom-0 h-1 bg-black/40" aria-hidden>
          <span
            className="block h-full bg-primary"
            style={{ width: `${progress(p, now) * 100}%` }}
          />
        </span>
      </div>
      <div className="space-y-0.5 p-3">
        <p className="truncate text-xs text-fg-muted">{c.name}</p>
        <p className="line-clamp-2 min-h-10 text-sm font-semibold text-fg">
          {p.item.title || t("Untitled")}
        </p>
        <p className="text-xs tabular-nums text-fg-subtle">
          {t("Until {time} · {left} left", { time: clock(p.end), left: unit(left, "minute") })}
        </p>
      </div>
    </button>
  );
}

/** One source's health at a glance: account, connections, refresh. */
export function SourceHealthRow({
  source: s,
  usage,
  now,
  onSelect,
}: {
  source: Source;
  usage?: LiveUsage["sources"][number];
  now: number;
  onSelect: (id: string) => void;
}) {
  const expires = s.account?.expiresAt ? Date.parse(s.account.expiresAt) : NaN;
  const soon = Number.isFinite(expires) && expires - now < DAY;
  const failed = Boolean(s.lastError);
  return (
    <button
      type="button"
      onClick={() => onSelect(s.id)}
      className="flex w-full items-start gap-3 py-2.5 text-left hover:bg-surface-hover"
    >
      <span
        className={cn(
          "mt-1.5 size-2 shrink-0 rounded-full",
          failed ? "bg-danger" : soon ? "bg-warning" : "bg-success",
        )}
        aria-hidden
      />
      <span className="min-w-0 flex-1 space-y-0.5">
        <span className="flex items-center gap-2">
          <span className="truncate text-sm font-medium text-fg">{s.name}</span>
          {s.account?.trial ? <Badge variant="info">{t("Trial")}</Badge> : null}
        </span>
        <span className="block truncate text-xs text-fg-muted">
          {failed
            ? t("Last refresh failed: {reason}", { reason: s.lastError ?? "" })
            : Number.isFinite(expires)
              ? t("Expires {when}", { when: relativeTime(new Date(expires)) })
              : tn(s.channelCount ?? 0, "{count} channel", "{count} channels")}
        </span>
        {usage && usage.limit > 0 ? (
          <span className="block text-xs tabular-nums text-fg-subtle">
            {tn(
              usage.limit,
              "{open} of {count} connection in use",
              "{open} of {count} connections in use",
              { open: formatNumber(usage.open) },
            )}
          </span>
        ) : null}
      </span>
    </button>
  );
}

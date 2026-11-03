import { DashboardMetricCard, DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import { Badge, Button, Skeleton, Tooltip, cn } from "@braedonsaunders/appkit-ui";
import {
  Activity as ActivityIcon,
  AlertTriangle,
  ArrowUpRight,
  CheckCircle2,
  Film,
  Gauge,
  Info,
  Globe,
  Library,
  Plus,
  RefreshCw,
  Subtitles,
  Tv,
} from "lucide-react";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Artwork } from "../../../components/Artwork";
import { LEVEL_LABEL } from "../../../components/status";
import { t, tn } from "../../../i18n";
import { apiFetch, errorMessage } from "../../../lib/api";
import { invalidateRegistry, textOf, useRegistry } from "../../../lib/extend";
import { formatBytes, formatNumber, relativeTime } from "../../../lib/format";
import { GuideStrip, type GuideChannel } from "../Stage";
import { SidecarPairing } from "../SidecarPairing";
import { buildPlacements, pluginHealthLevel, type PluginDetail } from "../types";
import { callImdb, resolveTitles, resultCount, type PluginCall, type TitleCardLite } from "./calls";
import { liveTVKind, primaryContribution } from "./kinds";

/**
 * The plugin at a glance: what it is doing right now in the surface it
 * feeds, where it shows up, and three health tiles. Problems get a card of
 * their own only when there is one.
 */
export function PluginOverview({
  detail,
  calls,
  onClose,
  onRetry,
}: {
  detail: PluginDetail;
  calls: PluginCall[] | null;
  onClose: () => void;
  onRetry: () => Promise<void>;
}) {
  const level = pluginHealthLevel(detail.healthDetail.status, detail.enabled);
  return (
    <div className="space-y-5">
      {detail.manifest.ether?.runtime?.kind === "remote" ? (
        <SidecarPairing key={detail.id} id={detail.id} />
      ) : null}
      {(level === "warn" || level === "error") && detail.enabled ? (
        <ProblemCard detail={detail} onRetry={onRetry} />
      ) : null}
      <LiveNow detail={detail} calls={calls} onClose={onClose} />
      <Placements detail={detail} onClose={onClose} />
      <HealthTiles detail={detail} />
    </div>
  );
}

function ProblemCard({ detail, onRetry }: { detail: PluginDetail; onRetry: () => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const h = detail.healthDetail;
  return (
    <div
      role="alert"
      className="flex items-start gap-3 rounded-xl border border-warning/40 bg-warning/5 p-4"
    >
      <AlertTriangle size={18} className="mt-0.5 shrink-0 text-warning" aria-hidden />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-fg">
          {h.reason || t("{name} is having trouble", { name: detail.name })}
        </p>
        {h.lastError ? (
          <p className="mt-1 break-words font-mono text-xs text-fg-muted">{h.lastError}</p>
        ) : null}
        {h.lastOk ? (
          <p className="mt-1 text-xs text-fg-subtle">
            {t("Last worked {when}", { when: relativeTime(h.lastOk) })}
          </p>
        ) : null}
      </div>
      <Button
        size="sm"
        variant="outline"
        disabled={busy}
        onClick={() => {
          setBusy(true);
          void onRetry().finally(() => setBusy(false));
        }}
      >
        <RefreshCw size={13} className={cn(busy && "animate-spin")} /> {t("Try again")}
      </Button>
    </div>
  );
}

/* ---------------------------------------------------------------- live card */

function LiveNow({
  detail,
  calls,
  onClose,
}: {
  detail: PluginDetail;
  calls: PluginCall[] | null;
  onClose: () => void;
}) {
  switch (primaryContribution(detail)) {
    case "livetv_channels":
      return <LiveTVNow detail={detail} onClose={onClose} />;
    case "list_source":
      return <CatalogueNow detail={detail} onClose={onClose} />;
    case "stream_source":
      return (
        <LookupsNow
          detail={detail}
          calls={calls}
          resource="stream"
          icon={<Film size={16} />}
          title={t("Recent source lookups")}
          empty={t("Titles show here after someone presses play and Ether asks {name}.", {
            name: detail.name,
          })}
          count={(n) => tn(n, "{count} source", "{count} sources")}
          none={t("No sources")}
        />
      );
    case "subtitles":
      return (
        <LookupsNow
          detail={detail}
          calls={calls}
          resource="subtitles"
          icon={<Subtitles size={16} />}
          title={t("Recent subtitle lookups")}
          empty={t("Titles show here after a viewer looks for subtitles.")}
          count={(n) => tn(n, "{count} subtitle", "{count} subtitles")}
          none={t("No subtitles")}
        />
      );
    case "metadata":
      return (
        <LookupsNow
          detail={detail}
          calls={calls}
          resource="meta"
          icon={<Library size={16} />}
          title={t("Details it filled in")}
          empty={t("Titles show here after Ether asks {name} for details.", { name: detail.name })}
        />
      );
    default:
      return null;
  }
}

interface LiveSource {
  id: string;
  name: string;
  kind: string;
  channelCount?: number;
}

interface LiveChannel {
  id: string;
  number: string;
  name: string;
  enabled: boolean;
  sourceId?: string;
}

interface Programme {
  channelId: string;
  start: string;
  end: string;
  item?: { title?: string };
}

function LiveTVNow({ detail, onClose }: { detail: PluginDetail; onClose: () => void }) {
  const kind = liveTVKind(detail.id);
  const [source, setSource] = useState<LiveSource | null | undefined>(undefined);
  const [rows, setRows] = useState<GuideChannel[] | null>(null);
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");
  const [revision, setRevision] = useState(0);

  useEffect(() => {
    let live = true;
    setRows(null);
    void (async () => {
      try {
        const sources = await apiFetch<LiveSource[]>("/livetv/sources");
        const src = sources.find((s) => s.kind === kind) ?? null;
        if (!live) return;
        setSource(src);
        if (!src) return;
        const now = new Date();
        const to = new Date(now.getTime() + 3 * 3600_000);
        const [channels, guide] = await Promise.all([
          apiFetch<LiveChannel[]>("/livetv/channels"),
          apiFetch<Programme[]>(
            `/livetv/guide?from=${encodeURIComponent(now.toISOString())}&to=${encodeURIComponent(to.toISOString())}`,
          ).catch(() => [] as Programme[]),
        ]);
        const byChannel = new Map<string, Programme[]>();
        for (const p of guide) {
          const list = byChannel.get(p.channelId) ?? [];
          list.push(p);
          byChannel.set(p.channelId, list);
        }
        const mine = channels
          .filter((c) => c.sourceId === src.id && c.enabled)
          .sort((a, b) => Number(a.number) - Number(b.number));
        const out: GuideChannel[] = [];
        for (const c of mine) {
          const progs = (byChannel.get(c.id) ?? []).sort((a, b) => a.start.localeCompare(b.start));
          const idx = progs.findIndex(
            (p) => Date.parse(p.start) <= now.getTime() && now.getTime() < Date.parse(p.end),
          );
          if (idx < 0) continue;
          const cur = progs[idx]!;
          const start = Date.parse(cur.start);
          const end = Date.parse(cur.end);
          out.push({
            number: Number(c.number) || out.length + 1,
            name: c.name,
            now: cur.item?.title ?? "",
            next: progs[idx + 1]?.item?.title ?? "",
            progress: Math.min(1, Math.max(0, (now.getTime() - start) / Math.max(1, end - start))),
          });
          if (out.length >= 6) break;
        }
        if (live) setRows(out);
      } catch (e) {
        if (live) setError(errorMessage(e));
      }
    })();
    return () => {
      live = false;
    };
  }, [kind, revision]);

  const add = async () => {
    setAdding(true);
    setError("");
    try {
      await apiFetch("/livetv/sources", {
        method: "POST",
        body: JSON.stringify({ name: detail.name, kind, playlistUrl: kind }),
      });
      invalidateRegistry();
      setRevision((r) => r + 1);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setAdding(false);
    }
  };

  if (source === null) {
    return (
      <DashboardPanel title={t("Not in Live TV yet")} icon={<Tv size={16} />}>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-fg-muted">{t("Add its channels and guide to Live TV.")}</p>
          <Button size="sm" disabled={adding || !detail.enabled} onClick={() => void add()}>
            <Plus size={14} /> {adding ? t("Adding…") : t("Add to Live TV")}
          </Button>
        </div>
        {error ? <p className="mt-2 text-sm text-danger">{error}</p> : null}
      </DashboardPanel>
    );
  }
  return (
    <DashboardPanel
      title={
        source?.channelCount
          ? tn(source.channelCount, "{count} channel in Live TV", "{count} channels in Live TV")
          : t("Live TV")
      }
      icon={<Tv size={16} />}
      actions={<OpenLink to="/livetv" label={t("Open Live TV")} onClose={onClose} />}
    >
      {rows === null && !error ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-10 rounded-md" />
          ))}
        </div>
      ) : rows?.length ? (
        <GuideStrip channels={rows} />
      ) : (
        <p className="text-sm text-fg-muted">
          {error || t("No programme guide for the next few hours.")}
        </p>
      )}
    </DashboardPanel>
  );
}

interface PreviewItem {
  key: string;
  title: string;
  year?: number;
  poster?: string;
}

function CatalogueNow({ detail, onClose }: { detail: PluginDetail; onClose: () => void }) {
  const { descriptors } = useRegistry<{ key?: string }>("collectionSource");
  const lists = useMemo(
    () => descriptors.filter((d) => d.owner === detail.id),
    [descriptors, detail.id],
  );
  const [pick, setPick] = useState<string>("");
  const current = lists.find((d) => d.id === pick) ?? lists[0];
  const [items, setItems] = useState<PreviewItem[] | null>(null);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!current) return;
    let live = true;
    setItems(null);
    setError("");
    const label = textOf(current.label);
    apiFetch<{ items: PreviewItem[]; total: number }>("/collections/preview?n=12", {
      method: "POST",
      body: JSON.stringify({
        name: label,
        mediaType: /series|show|tv/i.test(label) ? "show" : "movie",
        sources: [{ kind: current.id }],
      }),
    })
      .then((r) => {
        if (!live) return;
        setItems(r.items ?? []);
        setTotal(r.total ?? 0);
      })
      .catch((e) => live && setError(errorMessage(e)));
    return () => {
      live = false;
    };
  }, [current]);

  if (!lists.length) {
    return (
      <DashboardPanel title={t("Catalogues")} icon={<Library size={16} />}>
        <p className="text-sm text-fg-muted">
          {detail.enabled
            ? t("It offers no catalogues Ether can use right now.")
            : t("Turn it on to use its catalogues.")}
        </p>
      </DashboardPanel>
    );
  }
  return (
    <DashboardPanel
      title={t("{plugin} · {catalog}", {
        plugin: detail.name,
        catalog: textOf(current!.label).split("·")[0]!.trim(),
      })}
      icon={<Library size={16} />}
      actions={<OpenLink to="/collections/new" label={t("Make a collection")} onClose={onClose} />}
    >
      {lists.length > 1 ? (
        <div className="mb-3 flex flex-wrap gap-1.5" role="group" aria-label={t("Catalogues")}>
          {lists.slice(0, 8).map((d) => (
            <button
              key={d.id}
              type="button"
              aria-pressed={d.id === current?.id}
              onClick={() => setPick(d.id)}
              className={cn(
                "rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors",
                d.id === current?.id
                  ? "border-primary/40 bg-primary-subtle text-primary"
                  : "border-border text-fg-muted hover:text-fg",
              )}
            >
              {textOf(d.label)}
            </button>
          ))}
        </div>
      ) : null}
      <PosterRow
        items={items?.map((i) => ({
          key: i.key,
          title: i.title,
          poster: i.poster,
          caption: i.year ? String(i.year) : "",
        }))}
        error={error}
        empty={t("This catalogue is empty right now.")}
      />
      {items?.length ? (
        <div className="mt-3 flex justify-end">
          <Badge variant="secondary">{tn(total, "{count} title", "{count} titles")}</Badge>
        </div>
      ) : null}
    </DashboardPanel>
  );
}

function LookupsNow({
  detail,
  calls,
  resource,
  icon,
  title,
  empty,
  count,
  none,
}: {
  detail: PluginDetail;
  calls: PluginCall[] | null;
  resource: "stream" | "subtitles" | "meta";
  icon: ReactNode;
  title: string;
  empty: string;
  count?: (n: number) => string;
  none?: string;
}) {
  const recent = useMemo(() => {
    const seen = new Set<string>();
    const out: { imdb: string; call: PluginCall }[] = [];
    for (const c of calls ?? []) {
      if (!c.route.startsWith(`/${resource}/`) || c.route.startsWith("/stream/tv/")) continue;
      const imdb = callImdb(c.route);
      if (!imdb || seen.has(imdb)) continue;
      seen.add(imdb);
      out.push({ imdb, call: c });
      if (out.length >= 12) break;
    }
    return out;
  }, [calls, resource]);
  const [titles, setTitles] = useState<Record<string, TitleCardLite> | null>(null);
  useEffect(() => {
    let live = true;
    setTitles(null);
    void resolveTitles(recent.map((r) => r.imdb)).then((m) => live && setTitles(m));
    return () => {
      live = false;
    };
  }, [recent]);

  const items =
    calls === null || titles === null
      ? undefined
      : recent
          .filter((r) => titles[r.imdb])
          .map((r) => {
            const n = resultCount(r.call);
            const caption =
              r.call.status !== 200
                ? t("Failed")
                : count && n !== undefined
                  ? n > 0
                    ? count(n)
                    : (none ?? "")
                  : relativeTime(r.call.created);
            return {
              key: r.imdb,
              title: titles[r.imdb]!.title,
              poster: titles[r.imdb]!.poster,
              caption,
              muted: r.call.status !== 200 || !n,
            };
          });
  return (
    <DashboardPanel title={title} icon={icon}>
      <PosterRow items={items} empty={detail.enabled ? empty : t("Turn it on to use it.")} />
    </DashboardPanel>
  );
}

function PosterRow({
  items,
  error,
  empty,
}: {
  items?: { key: string; title: string; poster?: string; caption?: string; muted?: boolean }[];
  error?: string;
  empty: string;
}) {
  if (error) return <p className="text-sm text-danger">{error}</p>;
  if (!items) {
    return (
      <div className="grid grid-cols-4 gap-3 sm:grid-cols-6">
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className="aspect-[2/3] rounded-md" />
        ))}
      </div>
    );
  }
  if (!items.length) return <p className="text-sm text-fg-muted">{empty}</p>;
  return (
    <ul className="grid grid-cols-4 gap-3 sm:grid-cols-6">
      {items.slice(0, 12).map((it) => (
        <li key={it.key} className="min-w-0">
          <Artwork
            src={it.poster}
            alt={it.title}
            title={it.title}
            className="aspect-[2/3] w-full overflow-hidden rounded-md shadow-sm ring-1 ring-border-subtle"
          />
          <p className="mt-1.5 truncate text-xs font-medium text-fg" title={it.title}>
            {it.title}
          </p>
          {it.caption ? (
            <p className={cn("truncate text-[11px]", it.muted ? "text-fg-subtle" : "text-success")}>
              {it.caption}
            </p>
          ) : null}
        </li>
      ))}
    </ul>
  );
}

function OpenLink({ to, label, onClose }: { to: string; label: string; onClose: () => void }) {
  return (
    <Link
      to={to}
      onClick={onClose}
      className="inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline"
    >
      {label} <ArrowUpRight size={13} aria-hidden />
    </Link>
  );
}

/* ---------------------------------------------------------- where it appears */

function Placements({ detail, onClose }: { detail: PluginDetail; onClose: () => void }) {
  const { descriptors } = useRegistry();
  const placements = buildPlacements(descriptors, detail.id);
  const [collections, setCollections] = useState(0);
  useEffect(() => {
    let live = true;
    apiFetch<{ collections: { sources?: { kind?: string }[] }[] }>("/collections")
      .then((r) => {
        const prefix = `plugin:${detail.id}/`;
        const n = (r.collections ?? []).filter((c) =>
          (c.sources ?? []).some((s) => s.kind?.startsWith(prefix)),
        ).length;
        if (live) setCollections(n);
      })
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, [detail.id]);

  // One chip per surface; a plugin with many catalogues still reads as one.
  const chips = new Map<string, { label: string; href: string }>();
  for (const p of placements) {
    if (p.area === t("Console")) continue;
    const label = p.area === t("Plugin page") ? p.label : p.area;
    if (!chips.has(label)) chips.set(label, { label, href: p.href });
  }
  if (collections > 0) {
    chips.set("collections-used", {
      label: tn(collections, "Used by {count} collection", "Used by {count} collections"),
      href: "/collections",
    });
  }
  if (!chips.size) return null;
  return (
    <div className="flex flex-wrap items-center gap-2" aria-label={t("Where it appears")}>
      <span className="text-xs font-medium text-fg-subtle">{t("Appears in")}</span>
      {[...chips.values()].map((c) => (
        <Link
          key={c.label}
          to={c.href}
          onClick={onClose}
          className="inline-flex items-center gap-1 rounded-full border border-border bg-surface px-3 py-1 text-xs font-medium text-fg transition-colors hover:border-primary/40 hover:text-primary"
        >
          {c.label} <ArrowUpRight size={12} aria-hidden />
        </Link>
      ))}
    </div>
  );
}

/* --------------------------------------------------------------- health tiles */

function HealthTiles({ detail }: { detail: PluginDetail }) {
  const level = pluginHealthLevel(detail.healthDetail.status, detail.enabled);
  const im = detail.impact;
  const undeclared = detail.privacy.undeclared ?? [];
  const hosts = Object.keys(detail.privacy.observedHosts ?? {});
  const declared = detail.privacy.declaredHosts ?? [];
  const slow = im.p95Ms > 2000;
  const tone =
    level === "ok" ? "success" : level === "off" ? "info" : level === "warn" ? "warning" : "danger";
  return (
    <div className="grid gap-3 sm:grid-cols-3">
      <DashboardMetricCard
        label={t("Responding")}
        value={LEVEL_LABEL[level]}
        detail={
          im.errors
            ? t("{calls} recent calls · {errors} failed", {
                calls: formatNumber(im.calls, 0),
                errors: formatNumber(im.errors, 0),
              })
            : tn(im.calls, "{count} recent call", "{count} recent calls")
        }
        icon={level === "ok" ? <CheckCircle2 size={16} /> : <ActivityIcon size={16} />}
        tone={tone}
        trend={
          <More
            lines={[
              t("Restarts: {count}", { count: formatNumber(im.restarts, 0) }),
              t("Slow calls: {count}", { count: formatNumber(im.slow, 0) }),
              t("Storage: {size}", { size: formatBytes(im.kvBytes) }),
            ]}
          />
        }
      />
      <DashboardMetricCard
        label={t("Speed")}
        value={
          im.calls
            ? im.p50Ms < 1
              ? t("Under 1 ms")
              : t("{ms} ms", { ms: formatNumber(im.p50Ms, 0) })
            : "—"
        }
        detail={
          im.calls ? t("Slowest {ms} ms", { ms: formatNumber(im.p95Ms, 0) }) : t("Not used yet")
        }
        icon={<Gauge size={16} />}
        tone={slow ? "warning" : "primary"}
      />
      <DashboardMetricCard
        label={t("Network")}
        value={
          undeclared.length
            ? tn(undeclared.length, "{count} site blocked", "{count} sites blocked")
            : detail.host
              ? t("Add-on only")
              : t("As declared")
        }
        detail={
          undeclared.length
            ? undeclared.slice(0, 2).join(", ")
            : detail.host
              ? detail.host
              : tn(Math.max(hosts.length, declared.length), "{count} site", "{count} sites")
        }
        icon={<Globe size={16} />}
        tone={undeclared.length ? "warning" : "success"}
        trend={
          <More
            lines={[
              ...(hosts.length ? hosts : declared).slice(0, 6),
              t("Sent: {size}", { size: formatBytes(im.egressBytes) }),
            ]}
          />
        }
      />
    </div>
  );
}

/** The detail behind a tile, on hover or focus. */
function More({ lines }: { lines: string[] }) {
  return (
    <Tooltip
      content={
        <span className="block space-y-0.5 text-left">
          {lines.map((l) => (
            <span key={l} className="block">
              {l}
            </span>
          ))}
        </span>
      }
    >
      <button
        type="button"
        aria-label={t("More details")}
        className="inline-flex text-fg-subtle hover:text-fg focus-visible:text-fg"
      >
        <Info size={13} aria-hidden />
      </button>
    </Tooltip>
  );
}

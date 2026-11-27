import { DashboardPanel } from "@braedonsaunders/appkit-dashboard/primitives";
import { EmptyState, PagedTable, Select, type PagedColumn } from "@braedonsaunders/appkit-ui";
import { ChevronRight, Clock, Eye, Layers, PlayCircle, TrendingUp } from "lucide-react";
import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { CompactTrend } from "../../components/charts";
import { Drawer } from "../../components/Drawer";
import { ErrorPanel, LoadingRows } from "../../components/feedback";
import { Segmented } from "../../components/fields";
import { Kpi, KpiRow } from "../../components/Kpi";
import { t, tn } from "../../i18n";
import { pagedTableLabels } from "../../i18n/appkit-labels";
import { apiFetch } from "../../lib/api";
import type {
  RecommendationCounts,
  RecommendationDetail,
  RecommendationGroup,
  RecommendationReport,
  RecommendationRow,
  RecommendationTitle,
} from "../../lib/contract";
import { formatNumber, formatPercent } from "../../lib/format";
import { useAsync, useMediaQuery } from "../../lib/hooks";
import type { EtherUser } from "../../lib/types";

const periods = ["7", "30", "90"] as const;
function useRecommendationParams() {
  const [params, setParams] = useSearchParams();
  const period = params.get("days");
  const days = period === "7" || period === "90" ? period : "30";
  const update = (key: string, value: string) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (value) next.set(key, value);
        else next.delete(key);
        if (key === "viewer") next.delete("row");
        return next;
      },
      { preventScrollReset: true },
    );
  return { days, viewer: params.get("viewer") ?? "", kind: params.get("row") ?? "", update };
}

export function RecommendationPeriod() {
  const { days, update } = useRecommendationParams();
  return (
    <Segmented
      ariaLabel={t("Period")}
      size="sm"
      value={days}
      onChange={(value) => update("days", value)}
      options={periods.map((value) => ({
        value,
        label: tn(Number(value), "Last {count} day", "Last {count} days"),
      }))}
    />
  );
}

function rate(count: number, total: number) {
  return total ? formatPercent(count / total, 1) : "—";
}

function hours(ms: number) {
  return t("{hours} h", { hours: formatNumber(ms / 3_600_000, 1) });
}

function audienceName(viewer: EtherUser | undefined) {
  return viewer?.title || viewer?.username || t("Unknown viewer");
}

export function Recommendations({ viewers }: { viewers: EtherUser[] }) {
  const { days, viewer, kind, update } = useRecommendationParams();
  const query = new URLSearchParams({ days, viewer });
  const report = useAsync(
    async (signal) => ({
      days,
      viewer,
      body: await apiFetch<RecommendationReport>(`/insights/recommendations?${query}`, { signal }),
    }),
    [days, viewer],
  );
  const data =
    report.data?.days === days && report.data.viewer === viewer ? report.data.body : null;
  const activeRow = data?.rows.find((row) => row.kind === kind);
  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-fg">{t("Recommendation performance")}</h2>
        </div>
        <Select
          aria-label={t("Viewer")}
          sheetTitle={t("Viewer")}
          searchPlaceholder={t("Find a viewer…")}
          className="w-full sm:w-56"
          value={viewer}
          onChange={(event) => update("viewer", event.target.value)}
        >
          <option value="">{t("All viewers")}</option>
          {viewers.map((item) => (
            <option key={item.id} value={item.id}>
              {audienceName(item)}
            </option>
          ))}
        </Select>
      </div>
      {report.loading && !data ? (
        <LoadingRows rows={5} />
      ) : report.error && !data ? (
        <ErrorPanel error={report.error} onRetry={() => void report.reload()} />
      ) : data ? (
        <>
          <KpiRow className="lg:grid-cols-3 xl:grid-cols-5 [&_.uppercase]:whitespace-normal max-sm:[&>div]:p-3 max-sm:[&_.tabular-nums]:mt-2 max-sm:[&_.leading-snug:empty]:min-h-0">
            <Kpi
              label={t("Impressions")}
              value={formatNumber(data.impressions, 0)}
              icon={<Eye size={18} />}
            />
            <Kpi
              label={t("Plays from rows")}
              value={formatNumber(data.plays, 0)}
              icon={<PlayCircle size={18} />}
            />
            <Kpi
              label={t("Play rate")}
              value={rate(data.plays, data.impressions)}
              icon={<TrendingUp size={18} />}
            />
            <Kpi
              label={t("Hours watched")}
              value={hours(data.watchedMs)}
              icon={<Clock size={18} />}
            />
            <Kpi
              label={t("Share of all plays")}
              value={rate(data.attributedStarts, data.starts)}
              detail={t("Includes search, library and Continue Watching")}
              icon={<Layers size={18} />}
              tone="info"
              className="max-lg:col-span-2"
            />
          </KpiRow>
          <DashboardPanel title={t("Performance by row")} icon={<Layers size={16} />}>
            <PerformanceTable rows={data.rows} onOpen={(row) => update("row", row.kind)} />
            <div className="mt-4 space-y-1 text-xs leading-relaxed text-fg-muted">
              <p>
                {t(
                  "One impression per viewer, row, app and day. The first attributed play counts toward the row’s rate and watch time.",
                )}
              </p>
              <p>
                {t(
                  "Rows need 20 impressions to rank. Ranking uses the low end of the 95% confidence range.",
                )}
              </p>
              <p>
                {t("Play sources are estimated. The share of all plays includes repeat starts.")}
              </p>
              <p>{t("Completion and watch time use progress reported by the app.")}</p>
            </div>
          </DashboardPanel>
        </>
      ) : null}
      <Drawer
        open={Boolean(activeRow)}
        onClose={() => update("row", "")}
        size="lg"
        title={activeRow?.title || t("Row")}
        documentTitle={activeRow?.title}
        description={tn(Number(days), "Last {count} day", "Last {count} days")}
      >
        {activeRow && (
          <RowDetail
            key={`${activeRow.kind}/${days}`}
            row={activeRow}
            days={days}
            viewer={viewer}
            viewers={viewers}
            onViewer={(key) => update("viewer", key)}
          />
        )}
      </Drawer>
    </>
  );
}

function ObservedRate({ row }: { row: RecommendationRow }) {
  if (row.lowSample)
    return <span className="text-xs font-medium text-warning">{t("Not enough data yet")}</span>;
  return (
    <div className="tabular-nums">
      <span className="font-semibold text-fg">{rate(row.plays, row.impressions)}</span>
      <p className="text-xs text-fg-muted">
        {t("{low}–{high} confidence range", {
          low: formatPercent(row.low, 1),
          high: formatPercent(row.high, 1),
        })}
      </p>
    </div>
  );
}

function PerformanceTable({
  rows,
  onOpen,
}: {
  rows: RecommendationRow[];
  onOpen: (row: RecommendationRow) => void;
}) {
  const compact = useMediaQuery("(max-width: 767px)");
  const [sort, setSort] = useState("rate");
  const rowLabel = t("Row");
  const rateLabel = t("Ranked play rate");
  const trendLabel = t("Daily play rate");
  const counts = [
    ["impressions", t("Impressions")],
    ["plays", t("Plays")],
    ["viewers", t("Unique viewers")],
  ] as const;
  const metrics = [
    ...counts.map(([key, header]) => ({
      key,
      header,
      sortValue: (row: RecommendationRow) => row[key],
      cell: (row: RecommendationRow) => formatNumber(row[key], 0),
    })),
    {
      key: "completion",
      header: t("Completion rate"),
      sortValue: (row: RecommendationRow) => (row.measured ? row.completed / row.measured : -1),
      cell: (row: RecommendationRow) => rate(row.completed, row.measured),
    },
    {
      key: "watch",
      header: t("Hours watched"),
      sortValue: (row: RecommendationRow) => row.watchedMs,
      cell: (row: RecommendationRow) => hours(row.watchedMs),
    },
  ];

  const rowButton = (row: RecommendationRow) => (
    <button
      type="button"
      onClick={(event) => {
        event.stopPropagation();
        onOpen(row);
      }}
      className="flex w-full min-w-32 items-center justify-between gap-2 text-start font-medium text-fg hover:text-primary"
    >
      {row.title}
      <ChevronRight size={14} className="shrink-0 text-fg-subtle rtl:rotate-180" aria-hidden />
    </button>
  );
  const columns: PagedColumn<RecommendationRow>[] = [
    { key: "row", header: rowLabel, sortValue: (row) => row.title, cell: rowButton },
    {
      key: "rate",
      header: t("Play rate"),
      align: "right",
      sortValue: (row) => (row.lowSample ? -1 : row.rankRate),
      cell: (row) => <ObservedRate row={row} />,
    },
    ...metrics.map((metric) => ({ ...metric, align: "right" as const })),
    {
      key: "trend",
      header: trendLabel,
      cell: (row) => <CompactTrend values={row.trend} label={trendLabel} />,
    },
  ];
  const order = metrics.find((metric) => metric.key === sort)?.sortValue;
  const sorted = compact && order ? [...rows].sort((a, b) => order(b) - order(a)) : rows;
  return (
    <>
      {compact && rows.length > 0 ? (
        <Select
          aria-label={t("Sort by {column}", { column: rowLabel })}
          className="mb-3 w-full"
          value={sort}
          onChange={(event) => setSort(event.target.value)}
        >
          <option value="rate">{rateLabel}</option>
          {metrics.map((metric) => (
            <option key={metric.key} value={metric.key}>
              {metric.header}
            </option>
          ))}
        </Select>
      ) : null}
      <PagedTable<RecommendationRow>
        key={compact ? sort : "table"}
        rows={sorted}
        rowKey={(row) => row.kind}
        pageSize={20}
        searchable={false}
        onRowClick={onOpen}
        labels={pagedTableLabels()}
        empty={<EmptyState icon={<Layers />} title={t("No row data yet")} />}
        columns={
          compact
            ? [
                {
                  key: "row",
                  header: rowLabel,
                  cell: (row) => (
                    <div className="grid gap-3 py-1">
                      {rowButton(row)}
                      <div className="flex items-center justify-between gap-2">
                        <ObservedRate row={row} />
                        <CompactTrend values={row.trend} label={trendLabel} />
                      </div>
                      <dl className="grid grid-cols-3 gap-2 text-xs">
                        {metrics.map((metric) => (
                          <div key={metric.key}>
                            <dt className="text-fg-muted">{metric.header}</dt>
                            <dd className="font-medium tabular-nums text-fg">{metric.cell(row)}</dd>
                          </div>
                        ))}
                      </dl>
                    </div>
                  ),
                },
              ]
            : columns
        }
      />
    </>
  );
}

function RowDetail({
  row,
  days,
  viewer,
  viewers,
  onViewer,
}: {
  row: RecommendationRow;
  days: string;
  viewer: string;
  viewers: EtherUser[];
  onViewer: (viewer: string) => void;
}) {
  const query = new URLSearchParams({ days, kind: row.kind, viewer });
  const detail = useAsync(
    (signal) =>
      apiFetch<RecommendationDetail>(`/insights/recommendations/row?${query}`, { signal }),
    [row.kind, days, viewer],
  );
  const data = detail.data;
  if (detail.error) return <ErrorPanel error={detail.error} onRetry={() => void detail.reload()} />;
  if (!data) return <LoadingRows rows={5} />;
  return (
    <div className="grid gap-4">
      <div className="grid grid-cols-2 gap-3">
        <Kpi label={t("Impressions")} value={formatNumber(row.impressions, 0)} />
        <Kpi label={t("Plays from rows")} value={formatNumber(row.plays, 0)} />
      </div>
      <TitleList title={t("Most played titles")} items={data.top} metric="plays" />
      <TitleList title={t("Shown but never played")} items={data.unplayed} metric="impressions" />
      <GroupList
        title={t("Play rate by Home position")}
        groups={data.positions}
        label={(key) => t("Position {number}", { number: formatNumber(Number(key), 0) })}
      />
      <GroupList title={t("By app")} groups={data.apps} label={appLabel} />
      <GroupList
        title={t("By viewer")}
        groups={data.viewers}
        label={(key) => audienceName(viewers.find((item) => String(item.id) === key))}
        onClick={onViewer}
      />
    </div>
  );
}

function CountLabel({ row }: { row: RecommendationCounts }) {
  return (
    <span className="text-xs text-fg-muted">
      {tn(row.plays, "{count} play", "{count} plays")} ·{" "}
      {tn(row.impressions, "{count} impression", "{count} impressions")}
    </span>
  );
}

function GroupList({
  title,
  groups,
  label,
  onClick,
}: {
  title: string;
  groups: RecommendationGroup[];
  label: (key: string) => string;
  onClick?: (key: string) => void;
}) {
  return (
    <DashboardPanel title={title}>
      <PagedTable<RecommendationGroup>
        rows={groups}
        rowKey={(group) => group.key}
        onRowClick={onClick && ((group) => onClick(group.key))}
        searchable={false}
        pageSize={8}
        labels={pagedTableLabels()}
        empty={<p className="text-sm text-fg-muted">{t("No row data yet")}</p>}
        columns={[
          {
            key: "group",
            header: title,
            cell: (group) => (
              <div className="grid gap-0.5">
                {onClick ? (
                  <button
                    type="button"
                    onClick={(event) => {
                      event.stopPropagation();
                      onClick(group.key);
                    }}
                    className="w-fit text-start font-medium text-primary hover:underline"
                  >
                    {label(group.key)}
                  </button>
                ) : (
                  <span className="font-medium text-fg">{label(group.key)}</span>
                )}
                <CountLabel row={group} />
              </div>
            ),
          },
          {
            key: "rate",
            header: t("Play rate"),
            align: "right",
            cell: (group) => rate(group.plays, group.impressions),
          },
        ]}
      />
    </DashboardPanel>
  );
}

function TitleList({
  title,
  items,
  metric,
}: {
  title: string;
  items: RecommendationTitle[];
  metric: "plays" | "impressions";
}) {
  return (
    <DashboardPanel title={title}>
      {items.length ? (
        <ul className="divide-y divide-border">
          {items.map((item) => (
            <li key={item.key} className="flex items-baseline justify-between gap-3 py-2 text-sm">
              <span className="font-medium text-fg">{item.title || t("Unknown title")}</span>
              <span className="shrink-0 tabular-nums text-fg-muted">
                {metric === "plays"
                  ? tn(item.plays, "{count} play", "{count} plays")
                  : tn(item.impressions, "{count} impression", "{count} impressions")}
              </span>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-sm text-fg-muted">{t("No titles yet")}</p>
      )}
    </DashboardPanel>
  );
}

function appLabel(key: string) {
  switch (key) {
    case "plex":
      return "Plex";
    case "jellyfin":
      return "Jellyfin";
    case "emby":
      return "Emby";
    case "vela":
      return "Vela";
    case "silo":
      return "Silo";
    case "stremio":
      return "Stremio";
    case "unknown":
      return t("Unknown");
    case "xtream":
      return "Xtream";
    default:
      return t("Other");
  }
}

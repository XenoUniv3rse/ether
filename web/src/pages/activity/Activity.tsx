import { useEffect, useState } from "react";
import { DomainPage, useRouteTab } from "../../components/layout";
import { localized, t } from "../../i18n";
import { api, apiFetch } from "../../lib/api";
import { useAsync, useInterval } from "../../lib/hooks";
import { fetchLiveSessions, LIVE_SESSIONS_MS, useLiveSessionsRefresh } from "../../lib/sessions";
import { clusterActivity } from "./cluster";
import { PlayHistory, ViewingFilters } from "./History";
import { Experience, ExperiencePeriod } from "./Experience";
import { NowPlaying, ProtocolFilter, useNowView, ViewSwitch } from "./NowPlaying";
import { Recommendations, RecommendationPeriod } from "./Recommendations";
import { Delivery } from "./Delivery";
import { Viewing } from "./Viewing";
import { viewingQuery, type Play, type Summary, type ViewingFilter } from "./types";

const TABS = localized(
  () =>
    [
      { key: "now", label: t("Now playing") },
      { key: "history", label: t("History") },
      { key: "viewing", label: t("Viewing") },
      { key: "delivery", label: t("Delivery") },
      { key: "experience", label: t("Experience") },
      { key: "recommendations", label: t("Recommendations") },
    ] as const,
);

/**
 * Activity is the screen owners keep on their phone, so it offers the
 * dashboard as an installable app (manifest + service worker).
 */
function useInstallableDashboard() {
  useEffect(() => {
    const link = document.createElement("link");
    link.rel = "manifest";
    link.href = "/ether/manifest.webmanifest";
    document.head.appendChild(link);
    if ("serviceWorker" in navigator)
      void navigator.serviceWorker
        .register("/ether/service-worker.js", { scope: "/ether/" })
        .catch(() => {});
    return () => link.remove();
  }, []);
}

export default function Activity() {
  const tab = useRouteTab(TABS);
  useInstallableDashboard();
  const [filter, setFilter] = useState<ViewingFilter>({ days: "30", user: "", year: "" });
  const [protocol, setProtocol] = useState("");
  const [nowView, setNowView] = useNowView();
  const query = viewingQuery(filter);
  const viewingTab = tab === "history" || tab === "viewing" || tab === "delivery";

  const [truncated, setTruncated] = useState(false);
  const sessions = useAsync(async (signal) => {
    const [streams, status] = await Promise.all([
      fetchLiveSessions(signal),
      api.clusterStatus(signal).catch(() => null),
    ]);
    if (status?.mode !== "postgres") return streams;
    const console = await api.clusterConsole(signal);
    setTruncated(console.truncated);
    return clusterActivity(streams, status, console);
  }, []);
  const transcoder = useAsync(async () => {
    const body = await apiFetch<{
      value: { notice?: string; playbackError?: string } | null;
    }>("/status/hwaccel");
    return body.value ?? {};
  }, []);
  const viewers = useAsync(() => api.users(), []);
  const summary = useAsync(
    async (signal) => {
      if (!viewingTab) return undefined;
      return apiFetch<Summary>(`/stats/summary${query}`, { signal });
    },
    [query, viewingTab],
  );
  const historyTab = tab === "history";
  const history = useAsync(
    async (signal) => {
      if (!historyTab) return undefined;
      return (await apiFetch<Play[] | null>(`/stats/history${query}`, { signal })) ?? [];
    },
    [query, historyTab],
  );
  const reloadViewing = () => {
    void summary.reload();
    void history.reload();
  };

  useLiveSessionsRefresh(sessions.reload, tab === "now" ? LIVE_SESSIONS_MS : 30_000);
  useInterval(reloadViewing, viewingTab ? 30_000 : null);

  const live = sessions.data?.length;
  return (
    <DomainPage
      title={t("Activity")}
      base="/activity"
      tabs={TABS.map((tab) => (tab.key === "now" && live ? { ...tab, count: live } : tab))}
      active={tab}
      // Every tab keeps its filters here, so its four KPIs open the page at the same place.
      tabActions={
        tab === "now" ? (
          <>
            <ProtocolFilter streams={sessions.data ?? []} value={protocol} onChange={setProtocol} />
            <ViewSwitch value={nowView} onChange={setNowView} />
          </>
        ) : tab === "experience" ? (
          <ExperiencePeriod />
        ) : tab === "recommendations" ? (
          <RecommendationPeriod />
        ) : (
          <ViewingFilters value={filter} onChange={setFilter} viewers={viewers.data ?? []} />
        )
      }
    >
      {tab === "now" ? (
        <>
          {transcoder.data?.playbackError || transcoder.data?.notice ? (
            <p role="alert" className="text-sm text-danger">
              {transcoder.data.playbackError || transcoder.data.notice}
            </p>
          ) : null}
          {sessions.error && sessions.data && (
            <p role="alert" className="text-sm text-danger">
              {t("Activity could not be refreshed. Showing the previous sessions.")}
            </p>
          )}
          {truncated && (
            <p className="text-sm text-fg-muted">{t("Showing the first 200 assignments.")}</p>
          )}
          <NowPlaying state={sessions} protocol={protocol} view={nowView} />
        </>
      ) : tab === "experience" ? (
        <Experience />
      ) : tab === "recommendations" ? (
        <Recommendations viewers={viewers.data ?? []} />
      ) : (
        <>
          {tab === "history" ? (
            <PlayHistory
              history={history.data}
              summary={summary.data}
              error={history.error}
              onRetry={reloadViewing}
            />
          ) : tab === "viewing" ? (
            <Viewing
              summary={summary.data}
              viewers={viewers.data ?? []}
              error={summary.error}
              onRetry={reloadViewing}
            />
          ) : (
            <Delivery summary={summary.data} error={summary.error} onRetry={reloadViewing} />
          )}
        </>
      )}
    </DomainPage>
  );
}

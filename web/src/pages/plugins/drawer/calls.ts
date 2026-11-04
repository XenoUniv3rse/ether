import { useCallback, useEffect, useState } from "react";
import { t } from "../../../i18n";
import { apiFetch } from "../../../lib/api";
import type { PluginDetail } from "../types";

/** One request Ether made to a plugin, as the inspector records it. */
export interface PluginCall {
  id: number;
  route: string;
  method: string;
  status: number;
  ms: number;
  error?: string;
  request?: string;
  response?: string;
  created: string;
}

/** A catalogue title, as /catalog/external returns it. */
export interface TitleCardLite {
  key: string;
  title: string;
  year?: number;
  type?: string;
  poster?: string;
  imdbId?: string;
}

/** Calls slower than this read as slow in Activity. */
export const SLOW_CALL_MS = 2000;

export function usePluginCalls(pluginId: string | null, revision = 0) {
  const [calls, setCalls] = useState<PluginCall[] | null>(null);
  const load = useCallback(async () => {
    if (!pluginId) return;
    try {
      const r = await apiFetch<{ calls: PluginCall[] }>(
        `/plugins/${encodeURIComponent(pluginId)}/calls?limit=200`,
      );
      setCalls(r.calls ?? []);
    } catch {
      setCalls([]);
    }
  }, [pluginId]);
  useEffect(() => {
    setCalls(null);
    void load();
  }, [load, revision]);
  return { calls, reload: load };
}

const ROUTE =
  /^\/(catalog|meta|stream|subtitles|addon_catalog)\/([^/]+)\/([^/]+?)(?:\/[^/]*)?\.json$/;

/** The IMDb title a call was about ("tt123" from "tt123:1:2"), if any. */
export function callImdb(route: string): string | undefined {
  const m = ROUTE.exec(route.split("?")[0] ?? "");
  if (!m || m[1] === "catalog" || m[1] === "addon_catalog") return undefined;
  const id = decodeURIComponent(m[3] ?? "").split(":")[0] ?? "";
  return id.startsWith("tt") ? id : undefined;
}

function episodeOf(route: string): string {
  const m = ROUTE.exec(route.split("?")[0] ?? "");
  const parts = decodeURIComponent(m?.[3] ?? "").split(":");
  return parts.length === 3
    ? t(" · S{season} E{episode}", { season: parts[1]!, episode: parts[2]! })
    : "";
}

/** What Ether asked a plugin for, in plain words. */
export function describeCall(
  route: string,
  detail: PluginDetail,
  titles: Record<string, TitleCardLite>,
): string {
  const path = route.split("?")[0] ?? route;
  const m = ROUTE.exec(path);
  if (m) {
    const [, resource, type, id] = m;
    const decoded = decodeURIComponent(id ?? "");
    const imdb = callImdb(path);
    const name = imdb && titles[imdb] ? titles[imdb]!.title + episodeOf(path) : decoded;
    switch (resource) {
      case "catalog": {
        const cat = detail.manifest.catalogs?.find((c) => c.id === decoded && c.type === type);
        const label = cat?.name || decoded;
        return path.includes("search=")
          ? t("Search in {catalog}", { catalog: label })
          : t("Catalogue page: {catalog}", { catalog: label });
      }
      case "meta":
        return t("Details for {title}", { title: name });
      case "stream":
        return type === "tv"
          ? t("Stream for a channel")
          : t("Sources for {title}", { title: name });
      case "subtitles":
        return t("Subtitles for {title}", { title: name });
      case "addon_catalog":
        return t("Plugin directory");
    }
  }
  if (path === "/ether/channels.json") return t("Channel lineup");
  if (path.startsWith("/ether/guide/")) return t("Guide for a channel");
  if (path.startsWith("/ether/open/")) return t("Opened a source to play");
  if (path === "/ether/test") return t("Connection test");
  if (path === "/ether/health") return t("Health check");
  if (path === "/ether/configure") return t("Settings check");
  if (path.startsWith("/ether/views/")) return t("Page shown in the console");
  if (path.startsWith("/ether/actions/")) return t("Page action");
  if (path.startsWith("/ether/events")) return t("Server event");
  return path;
}

/** How many results a call returned, when its body says so. */
export function resultCount(call: PluginCall): number | undefined {
  if (!call.response || call.status !== 200) return undefined;
  try {
    const body = JSON.parse(call.response) as Record<string, unknown>;
    for (const key of ["streams", "subtitles", "metas", "channels"]) {
      if (Array.isArray(body[key])) return (body[key] as unknown[]).length;
    }
  } catch {
    // A long body is stored cut short; there is no count to show.
  }
  return undefined;
}

/** Names IMDb ids with catalogue titles (posters included), 24 at a time. */
export async function resolveTitles(ids: string[]): Promise<Record<string, TitleCardLite>> {
  const unique = [...new Set(ids)].slice(0, 24);
  if (!unique.length) return {};
  try {
    const r = await apiFetch<{ items: TitleCardLite[] }>(
      `/catalog/external?imdb=${encodeURIComponent(unique.join(","))}`,
    );
    const out: Record<string, TitleCardLite> = {};
    for (const item of r.items ?? []) if (item.imdbId) out[item.imdbId] = item;
    return out;
  } catch {
    return {};
  }
}

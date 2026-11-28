import type {
  RecommendationCounts,
  RecommendationDetail,
  RecommendationReport,
  RecommendationRow,
  RecommendationTitle,
} from "../lib/contract";

const definitions = [
  { kind: "top-10", title: "Top 10 for You", volume: 42, rate: 0.28 },
  { kind: "new-releases", title: "New & Noteworthy", volume: 33, rate: 0.22 },
  { kind: "because-you-watched", title: "Because You Watched…", volume: 38, rate: 0.17 },
  { kind: "hidden-gems", title: "Hidden Gems", volume: 22, rate: 0.14 },
  { kind: "trending", title: "Trending This Week", volume: 31, rate: 0.09 },
  { kind: "world-cinema", title: "World Cinema for You", volume: 0, rate: 1 },
];

export function recommendationPerformance(days: number, viewer: string): RecommendationReport {
  const index = viewer ? Math.max(1, Number(viewer) || 1) : 0;
  const rows: RecommendationRow[] = definitions.map((definition, i) => {
    const volume = index
      ? Math.max(4, Math.round(definition.volume / (3 + index)))
      : definition.volume;
    let impressions = 0;
    let plays = 0;
    const trend = Array.from({ length: days }, (_, day) => {
      const shown = definition.volume
        ? volume + Math.round(Math.cos((day + i) * 0.9) * volume * 0.3)
        : day > days - (index ? 3 : 5)
          ? 1
          : 0;
      const mix = index ? 0.7 + ((i + index) % 5) * 0.16 : 1;
      const variation = 1 + Math.sin(day * 1.26 + i) * 0.32;
      const played = definition.volume
        ? Math.round(shown * definition.rate * mix * variation)
        : shown && day % 2 === 0
          ? 1
          : 0;
      impressions += shown;
      plays += played;
      return shown ? played / shown : null;
    });
    const p = plays / impressions;
    const z = 1.96;
    const d = 1 + (z * z) / impressions;
    const center = (p + (z * z) / (2 * impressions)) / d;
    const margin = (z / d) * Math.sqrt((p * (1 - p) + (z * z) / (4 * impressions)) / impressions);
    const low = plays ? center - margin : 0;
    return {
      kind: definition.kind,
      title: definition.title,
      impressions,
      plays,
      measured: plays,
      completed: Math.round(plays * (0.61 + (i % 3) * 0.07)),
      watchedMs: plays * (49 + i * 5) * 60_000,
      viewers: index ? 1 : definition.volume ? 7 - (i % 3) : 2,
      rankRate: low,
      low,
      high: plays === impressions ? 1 : center + margin,
      lowSample: impressions < 20,
      trend,
    };
  });
  rows.sort((a, b) => Number(a.lowSample) - Number(b.lowSample) || b.rankRate - a.rankRate);
  const total = rows.reduce(
    (sum, row) => ({
      impressions: sum.impressions + row.impressions,
      plays: sum.plays + row.plays,
      measured: sum.measured + row.measured,
      completed: sum.completed + row.completed,
      watchedMs: sum.watchedMs + row.watchedMs,
    }),
    { impressions: 0, plays: 0, measured: 0, completed: 0, watchedMs: 0 },
  );
  return {
    ...total,
    rows,
    starts: Math.round(total.plays * 3.9),
    attributedStarts: Math.round(total.plays * 1.14),
  };
}

export function recommendationPerformanceDetail(
  days: number,
  kind: string,
  viewer: string,
): RecommendationDetail {
  const row = recommendationPerformance(days, viewer).rows.find((item) => item.kind === kind);
  if (!row) return { top: [], unplayed: [], positions: [], viewers: [], apps: [] };
  const distribute = (keys: string[], weights: number[], playWeights: number[]) => {
    let allocated = 0;
    let attributed = 0;
    return keys.map((key, i) => {
      const weight = weights[i]!;
      const impressions =
        i === keys.length - 1 ? row.impressions - allocated : Math.round(row.impressions * weight);
      allocated += impressions;
      const share = row.lowSample ? weight : (playWeights[i] ?? weight);
      const plays = Math.min(
        impressions,
        i === keys.length - 1 ? row.plays - attributed : Math.round(row.plays * share),
      );
      attributed += plays;
      const counts: RecommendationCounts = {
        impressions,
        plays,
        measured: plays,
        completed: Math.round(row.completed * (row.plays ? plays / row.plays : 0)),
        watchedMs: Math.round(row.watchedMs * share),
      };
      return { key, ...counts };
    });
  };
  const titles = (names: string[], played: boolean): RecommendationTitle[] =>
    names.map((title, i) => ({
      key: String(i),
      title,
      impressions: Math.round(row.impressions / (3 + i)),
      plays: played ? Math.round(row.plays / (5 + i * 2)) : 0,
    }));
  return {
    top: titles(["Arrival", "Severance", "The Martian", "Dune: Part Two"], true).filter(
      (item) => item.plays > 0,
    ),
    unplayed: titles(["The Vast of Night", "Moon", "The Green Knight", "Gattaca"], false).filter(
      (item) => item.impressions > 0,
    ),
    positions: distribute(["1", "3", "5"], [0.55, 0.3, 0.15], [0.68, 0.24, 0.08]),
    apps: distribute(
      ["plex", "vela", "jellyfin", "silo"],
      [0.45, 0.31, 0.14, 0.1],
      [0.4, 0.39, 0.12, 0.09],
    ),
    viewers: viewer
      ? [{ key: viewer, ...row }]
      : distribute(["1", "2", "3"], [0.5, 0.32, 0.18], [0.46, 0.28, 0.26]),
  };
}

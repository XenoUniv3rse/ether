import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  formatItemDate,
  librariesApi,
  type Library,
  type LibraryTabsResponse,
} from "../pages/libraries/api";
import { LibraryTabsSection } from "../pages/libraries/LibraryTabsSection";
import { PluginBrowse } from "../pages/libraries/PluginBrowse";

const PLUGIN_LIBRARY: Library = {
  id: 42,
  key: "pk-esports-matches",
  name: "Esports",
  source: "plugin",
  hasVideo: true,
  presentations: { mediaClass: "video", apps: [], available: {}, effective: {} },
  pluginId: "esports",
  kindKey: "matches",
  sort: 10,
  hidden: false,
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
  effectiveTabs: [
    {
      key: "live",
      label: "Live",
      kind: "live",
      card: "landscape",
      search: true,
      searchPlaceholder: "Filter matches",
    },
    { key: "library", label: "Archive", kind: "library", search: true },
  ],
  kind: {
    key: "matches",
    label: "Matches",
    library: { name: "Esports", card: "landscape", dateDisplay: "date" },
    levels: [{ key: "match", label: "Match", playable: true }],
    present: {},
    browse: { sorts: ["released", "title"], search: true },
    sync: { mode: "pull" },
  },
};

afterEach(() => {
  vi.restoreAllMocks();
});

describe("formatItemDate", () => {
  it("formats full UTC YYYY-MM-DD when dateDisplay is date, and year otherwise", () => {
    const released = Math.floor(Date.UTC(2026, 4, 9, 22, 0, 0) / 1000);
    expect(formatItemDate({ year: 2026, released }, "date")).toBe("2026-05-09");
    expect(formatItemDate({ year: 2026, released }, "year")).toBe("2026");
    expect(formatItemDate({ year: 2026, released })).toBe("2026");
    expect(formatItemDate({ year: 2026 }, "date")).toBe("2026");
    expect(formatItemDate({}, "date")).toBe("");
  });
});

describe("PluginBrowse dateDisplay and live tab", () => {
  it("shows full release date on cards and switches to the live tab with filters and preview", async () => {
    const released = Math.floor(Date.UTC(2026, 4, 9, 18, 0, 0) / 1000);
    vi.spyOn(librariesApi, "items").mockResolvedValue({
      total: 1,
      items: [
        {
          ref: "match-1",
          key: 1001,
          id: "match-1",
          level: "match",
          title: "Finals Game 1",
          year: 2026,
          released,
          playable: true,
        },
      ],
    });
    const liveSpy = vi.spyOn(librariesApi, "live").mockResolvedValue({
      items: [
        {
          id: "live-1",
          title: "Main Stage Broadcast",
          subtitle: "Team A vs Team B",
          badge: "1080p60",
          viewers: 1450,
          tags: ["Championship"],
          thumb: "/ether/api/libraries/42/live/thumb/0123456789abcdef0123456789abcdef",
          live: true,
        },
      ],
      filters: [{ key: "finals", label: "Finals" }],
    });
    const playSpy = vi.spyOn(librariesApi, "playLive").mockResolvedValue({
      id: "sess-1",
      title: "Main Stage Broadcast",
      playlist: "/ether/api/libraries/42/live/sessions/sess-1/index.m3u8",
    });
    vi.spyOn(librariesApi, "stopLive").mockResolvedValue({ ok: true });

    render(
      <MemoryRouter>
        <PluginBrowse library={PLUGIN_LIBRARY} />
      </MemoryRouter>,
    );

    expect(await screen.findByText("Finals Game 1")).toBeInTheDocument();
    expect(screen.getByText(/2026-05-09/)).toBeInTheDocument();

    const tablist = screen.getByRole("tablist", { name: "Browse views" });
    fireEvent.click(within(tablist).getByRole("tab", { name: "Live" }));

    expect(await screen.findByText("Main Stage Broadcast")).toBeInTheDocument();
    expect(screen.getByText("Team A vs Team B")).toBeInTheDocument();
    expect(screen.getByText("1080p60")).toBeInTheDocument();
    expect(screen.getByText("Championship")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Finals" })).toBeInTheDocument();
    expect(liveSpy).toHaveBeenCalledWith(
      42,
      expect.objectContaining({ tab: "live", limit: "48" }),
      expect.any(AbortSignal),
    );

    fireEvent.click(screen.getByRole("button", { name: /Main Stage Broadcast/i }));
    await waitFor(() =>
      expect(playSpy).toHaveBeenCalledWith(
        42,
        "live-1",
        "live",
        expect.any(Boolean),
        expect.any(Boolean),
      ),
    );
    expect(
      screen.getByRole("dialog", { name: "Watching Main Stage Broadcast" }),
    ).toBeInTheDocument();
  });
});

describe("LibraryTabsSection live tab management", () => {
  it("allows adding and saving a live tab on plugin libraries with card shape", async () => {
    const initial: LibraryTabsResponse = {
      libraryId: 42,
      customized: false,
      tabs: [{ key: "library", label: "Archive", kind: "library", search: true }],
      effectiveTabs: [{ key: "library", label: "Archive", kind: "library", search: true }],
      defaults: [{ key: "library", label: "Archive", kind: "library", search: true }],
      scopeNote: "Tabs show in Vela.",
      availableFacets: { collections: [], people: [] },
      availableSorts: ["released", "title"],
    };
    vi.spyOn(librariesApi, "tabs").mockResolvedValue(initial);
    const putSpy = vi.spyOn(librariesApi, "putTabs").mockImplementation(async (_id, tabs) => ({
      ...initial,
      customized: true,
      tabs,
      effectiveTabs: tabs,
    }));

    render(
      <MemoryRouter>
        <LibraryTabsSection library={PLUGIN_LIBRARY} />
      </MemoryRouter>,
    );

    await screen.findByText("Archive");
    fireEvent.click(screen.getByRole("button", { name: "Add tab" }));
    const picker = await screen.findByRole("dialog", { name: "Add tab" });
    fireEvent.click(within(picker).getByRole("button", { name: /Live/i }));

    const editDialog = await screen.findByRole("dialog", { name: "New Live tab" });
    fireEvent.click(within(editDialog).getByRole("radio", { name: "Square" }));
    fireEvent.click(within(editDialog).getByRole("button", { name: "Add tab" }));

    await waitFor(() => expect(putSpy).toHaveBeenCalled());
    const saved = putSpy.mock.calls[0]?.[1] ?? [];
    const liveTab = saved.find((tab) => tab.kind === "live");
    expect(liveTab).toEqual(
      expect.objectContaining({
        kind: "live",
        label: "Live",
        card: "square",
        search: true,
      }),
    );
  });
});

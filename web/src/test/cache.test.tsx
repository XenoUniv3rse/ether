import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { presetForFreeBytes } from "../pages/cache/presets";
import { calls, renderApp } from "./harness";
import { renderAppWith } from "./routes";

const WAIT = { timeout: 4000 };
async function chooseProfile(name: RegExp) {
  fireEvent.click(await screen.findByRole("button", { name: /Choose a cache profile/ }, WAIT));
  fireEvent.click(await screen.findByRole("button", { name }, WAIT));
  fireEvent.click(screen.getByRole("button", { name: "Review profile" }));
}

async function pickToolbarOption(label: string, optionName: string | RegExp) {
  fireEvent.click(await screen.findByRole("button", { name: label }, WAIT));
  const menu = await screen.findByRole("menu", { name: label }, WAIT);
  fireEvent.click(within(menu).getByRole("menuitemradio", { name: optionName }));
}

describe("cache preset suggestion", () => {
  it("picks Everyday when the disk can hold 32 GiB, and Feather when it cannot", () => {
    expect(presetForFreeBytes(1.1 * 1024 ** 4)?.key).toBe("everyday");
    expect(presetForFreeBytes(13 * 1024 ** 3)?.key).toBe("feather");
  });
});

describe("Cache management", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });
  it("keeps a preset as a draft until saved and discards it without a request", async () => {
    const { fake } = renderApp("/cache");
    expect(await screen.findByRole("main", { name: "Cache overview" }, WAIT)).toBeInTheDocument();
    expect(screen.queryByLabelText("Search cached media")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Encore/ })).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Media disk budget")).not.toBeInTheDocument();
    await chooseProfile(/Encore/);
    expect(screen.getByLabelText("Media disk budget")).toHaveValue(250);
    expect(calls(fake, "PUT", "/config/cache")).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    expect(calls(fake, "PUT", "/config/cache")).toHaveLength(0);
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Save policy" })).not.toBeInTheDocument(),
    );
    await chooseProfile(/Encore/);
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/cache")).toHaveLength(1));
    expect(fake.state.config.cache.budgetBytes).toBe(250 * 1024 ** 3);
    expect(fake.state.config.cache.mode).toBe("adaptive");
  });
  it("returns from the saved policy editor to presets and reviews a replacement", async () => {
    const { fake } = renderApp("/cache");
    fireEvent.click(await screen.findByRole("button", { name: "Edit policy" }, WAIT));
    fireEvent.click(screen.getByRole("button", { name: "Back to presets" }));
    fireEvent.click(await screen.findByRole("button", { name: /Feather/ }, WAIT));
    fireEvent.click(screen.getByRole("button", { name: "Review profile" }));
    expect(screen.getByLabelText("Media disk budget")).toHaveValue(0.25);
    fireEvent.click(screen.getByRole("button", { name: "Back to presets" }));
    fireEvent.click(await screen.findByRole("button", { name: /Encore/ }, WAIT));
    fireEvent.click(screen.getByRole("button", { name: "Review profile" }));
    expect(screen.getByLabelText("Media disk budget")).toHaveValue(250);
    expect(calls(fake, "PUT", "/config/cache")).toHaveLength(0);
  });
  it("shares NAS policy in a cluster and preserves the node mount path", async () => {
    const { fake } = renderApp("/cache", { cluster: true });
    fireEvent.click(await screen.findByRole("button", { name: "Edit policy" }, WAIT));
    fireEvent.click(screen.getByRole("radio", { name: /Shared NAS/ }));
    fireEvent.change(screen.getByLabelText("NAS mount path on this node"), {
      target: { value: "/mnt/ether-cache" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(fake.state.config.cache.storage).toBe("shared"));
    expect(fake.state.config.cache.dir).toBe("/mnt/ether-cache");
  });
  it("allows NAS storage on a standalone server without cluster setup", async () => {
    const { fake } = renderApp("/cache");
    fireEvent.click(await screen.findByRole("button", { name: "Edit policy" }, WAIT));
    const nas = screen.getByRole("radio", { name: /Shared NAS/ });
    expect(nas).toBeEnabled();
    fireEvent.click(nas);
    expect(screen.getByText(/No cluster is required/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("NAS mount path on this node"), {
      target: { value: "/Volumes/media/ether-cache" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(fake.state.config.cache.storage).toBe("shared"));
    expect(fake.state.config.cache.dir).toBe("/Volumes/media/ether-cache");
  });
  it("disables speculation controls on Preloading in no-media mode and keeps Edit policy storage-only", async () => {
    const { fake } = renderApp("/cache");
    fireEvent.click(await screen.findByRole("button", { name: "Edit policy" }, WAIT));
    const dialog = await screen.findByRole("dialog", { name: "Edit cache policy" }, WAIT);
    expect(
      within(dialog).queryByRole("switch", { name: "Warm detail pages" }),
    ).not.toBeInTheDocument();
    await pickToolbarOption("Retention strategy", "No persistent media");
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(fake.state.config.cache.mode).toBe("off"));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    fireEvent.click(screen.getByRole("tab", { name: "Preloading" }));
    expect(await screen.findByRole("switch", { name: "Warm detail pages" }, WAIT)).toBeDisabled();
    expect(screen.getByText(/Persistent media caching is turned off/)).toBeInTheDocument();
  });
  it("saves the playback read-ahead depth from Preloading and disables it without media caching", async () => {
    const { fake } = renderApp("/cache/preloading");
    const ahead = await screen.findByRole("button", { name: "Read ahead while playing" }, WAIT);
    expect(ahead).toHaveTextContent("5 minutes (recommended)");
    await pickToolbarOption("Read ahead while playing", "10 minutes");
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(fake.state.config.cache.playbackReadAheadMinutes).toBe(10));
    fireEvent.click(await screen.findByRole("button", { name: "Edit policy" }, WAIT));
    await pickToolbarOption("Retention strategy", "No persistent media");
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(fake.state.config.cache.mode).toBe("off"));
    expect(screen.getByRole("button", { name: "Read ahead while playing" })).toBeDisabled();
  });
  it("finds whole library files beside releases and clears only the selected file", async () => {
    const { fake } = renderApp("/cache/explorer");
    expect(await screen.findByText("Arcade.iso", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Arrival")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Clear Adventure.iso" })).toBeDisabled();
    await pickToolbarOption("Filter cached media", "Library files");
    expect(screen.queryByText("Arrival")).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Search cached media"), { target: { value: "Games" } });
    expect(screen.getByText("Arcade.iso")).toBeInTheDocument();
    await pickToolbarOption("Filter cached media", "Reclaimable");
    expect(screen.queryByText("Adventure.iso")).not.toBeInTheDocument();
    vi.spyOn(window, "confirm").mockReturnValue(true);
    fireEvent.click(screen.getByRole("button", { name: "Clear Arcade.iso" }));
    await waitFor(() => expect(screen.queryByText("Arcade.iso")).not.toBeInTheDocument());
    expect(calls(fake, "POST", "/cache/clear-file")[0]?.body).toEqual({
      libraryKey: "pk-demo-games",
      ref: "blob:" + "a".repeat(64),
    });
  });
  it("filters retained titles and updates an owner pin", async () => {
    const { fake } = renderApp("/cache/explorer");
    expect(await screen.findByText("Arrival", {}, WAIT)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Search cached media"), {
      target: { value: "Interstellar" },
    });
    expect(screen.queryByText("Arrival")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Pin Interstellar" }));
    await waitFor(() => expect(calls(fake, "POST", "/cache/pin")).toHaveLength(1));
    expect((await screen.findAllByText("Kept by you")).length).toBeGreaterThan(0);
  });
  it("saves a household blend and rolling TV window from Preloading with explanations", async () => {
    const { fake } = renderApp("/cache");
    await chooseProfile(/Cinema node/);
    fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Save policy" })).not.toBeInTheDocument(),
    );
    fireEvent.click(screen.getByRole("tab", { name: "Preloading" }));
    expect(
      await screen.findByRole("button", { name: "Selection strategy" }, WAIT),
    ).toHaveTextContent("Smart household blend");
    expect(screen.getByRole("button", { name: "Include in your shelf" })).toHaveTextContent(
      "Movies & TV episodes",
    );
    await pickToolbarOption("Include in your shelf", "TV episodes only");
    fireEvent.change(screen.getByLabelText("Household influence"), { target: { value: "80" } });
    fireEvent.change(screen.getByLabelText("Episode window per show"), { target: { value: "6" } });
    await pickToolbarOption("Learn from", "All active shared viewers");
    const savesBefore = calls(fake, "PUT", "/config/cache").length;
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(calls(fake, "PUT", "/config/cache").length).toBeGreaterThan(savesBefore),
    );
    expect(fake.state.config.cache.preloadEpisodesAhead).toBe(6);
    expect(fake.state.config.cache.preloadContent).toBe("tv");
    expect(fake.state.config.cache.preloadHouseholdWeight).toBe(80);
    expect(fake.state.config.cache.preloadHouseholdScope).toBe("all");
    expect((await screen.findAllByText(/Severance · S02E04/)).length).toBeGreaterThan(0);
    expect(screen.getByText(/Next in a household viewing sequence/)).toBeInTheDocument();
  });
  it("edits plugin library cache quotas with standard inputs and SaveBar", async () => {
    let cacheState = {
      used: 512 * 1024 ** 2,
      blobs: 3,
      maxBytes: 4 * 1024 ** 3,
      ttlSeconds: 7 * 86400,
    };
    const { fake } = renderAppWith("/cache/libraries", (method, path, body) => {
      if (method === "GET" && path === "/libraries") {
        return {
          libraries: [
            {
              id: 42,
              key: "audiobooks",
              name: "Audiobooks",
              source: "plugin",
              sort: 1,
              hidden: false,
              createdAt: "",
              updatedAt: "",
            },
          ],
        };
      }
      if (method === "GET" && path === "/libraries/42/cache") {
        return cacheState;
      }
      if (method === "PUT" && path === "/libraries/42/cache") {
        cacheState = { ...cacheState, ...(body as { maxBytes: number; ttlSeconds: number }) };
        return cacheState;
      }
      return undefined;
    });
    expect(await screen.findByRole("heading", { name: "Audiobooks" }, WAIT)).toBeInTheDocument();
    const keep = screen.getByLabelText("Keep at most");
    const forget = screen.getByLabelText("Forget files after");
    expect(keep).toHaveValue(4);
    expect(forget).toHaveValue(7);
    fireEvent.change(keep, { target: { value: "8" } });
    fireEvent.change(forget, { target: { value: "14" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/libraries/42/cache")).toHaveLength(1));
    expect(calls(fake, "PUT", "/libraries/42/cache")[0]!.body).toEqual({
      maxBytes: 8 * 1024 ** 3,
      ttlSeconds: 14 * 86400,
    });
  });
  it("explains releases by reason, file name, filters, and drawer detail", async () => {
    renderApp("/cache/explorer");
    expect(await screen.findByText("Arrival", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Reason" })).toBeInTheDocument();
    expect(screen.queryByRole("columnheader", { name: "Plays" })).not.toBeInTheDocument();
    expect(screen.getAllByText("Unfinished").length).toBeGreaterThan(0);

    fireEvent.change(screen.getByLabelText("Search cached media"), {
      target: { value: "Interstellar.2014" },
    });
    expect(await screen.findByText("Interstellar.2014.1080p.mkv")).toBeInTheDocument();
    expect(screen.queryByText("Arrival")).not.toBeInTheDocument();

    await pickToolbarOption("Filter cached media", "All media");
    fireEvent.change(screen.getByLabelText("Search cached media"), { target: { value: "" } });
    expect(
      await screen.findByText("No reason was recorded for this older entry."),
    ).toBeInTheDocument();
    expect(
      await screen.findByText("The.Americans.2013.S03E01.1080p.BRRip.x264-HiQVE"),
    ).toBeInTheDocument();
    expect(await screen.findByText("Written by a quality check or probe.")).toBeInTheDocument();

    fireEvent.click(await screen.findByText("Interstellar.2014.1080p.mkv"));
    const dialog = await screen.findByRole("dialog", { name: "Interstellar" }, WAIT);
    expect(within(dialog).getByText("What's cached")).toBeInTheDocument();
    expect(within(dialog).getByText("Segment map")).toBeInTheDocument();
    expect(within(dialog).getByText("Where")).toBeInTheDocument();
    expect(within(dialog).getByText("Why it's cached")).toBeInTheDocument();
    expect(within(dialog).getByText("When")).toBeInTheDocument();
    expect(within(dialog).getByText("Fetched because")).toBeInTheDocument();
    expect(within(dialog).getByText("Kept because")).toBeInTheDocument();
    expect(within(dialog).getByText("Play history")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    await pickToolbarOption("Filter cached media", "Unfinished");
    expect(await screen.findByText(/Severance · S02E03/)).toBeInTheDocument();
    expect(screen.queryByText("Arrival")).not.toBeInTheDocument();
  });
  it("opens the release drawer via deep-link and keyboard navigation with segment and shelf details", async () => {
    const { fake } = renderApp("/cache/explorer?release=demo-dune-2160");
    const dialog = await screen.findByRole("dialog", { name: "Dune: Part Two" }, WAIT);
    expect(within(dialog).getByText("4K")).toBeInTheDocument();
    expect(within(dialog).getByText("DV · HDR10")).toBeInTheDocument();
    expect(within(dialog).getByText("Remux")).toBeInTheDocument();
    expect(within(dialog).getByText("72/100")).toBeInTheDocument();
    expect(within(dialog).getByText("Usenet")).toBeInTheDocument();
    expect(await within(dialog).findByText("Segments 0–1,999")).toBeInTheDocument();
    expect(calls(fake, "GET", "/cache/releases/demo-dune-2160").length).toBeGreaterThan(0);

    fireEvent.click(within(dialog).getByRole("button", { name: "Keep in cache" }));
    await waitFor(() => expect(calls(fake, "POST", "/cache/pin")).toHaveLength(1));

    fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    const row = (await screen.findByText(/Severance · S02E03/)).closest("tr")!;
    fireEvent.keyDown(row, { key: "Enter" });
    const nextDialog = await screen.findByRole("dialog", { name: /Severance · S02E03/ }, WAIT);
    expect(await within(nextDialog).findByText("Segments 0–31, 260–767")).toBeInTheDocument();
  });
  it("breaks protected bytes down by cause and names the current warm", async () => {
    renderApp("/cache/overview");
    expect(await screen.findByText(/Playing now: /, {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText(/Unfinished: /)).toBeInTheDocument();
    expect(screen.getByText(/Shelf: /)).toBeInTheDocument();
    expect(screen.getByText(/Woe's Hollow.*Next episode/)).toBeInTheDocument();
  });
  it("shows cached, queued, and failed shelf states", async () => {
    const { fake } = renderApp("/cache/preloading");
    expect(await screen.findByText("Preload selection", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Shelf manifest")).toBeInTheDocument();
    expect(screen.getAllByText("Cached").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Queued").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Failed").length).toBeGreaterThan(0);
    expect(screen.getByText(/Titles chosen for the local shelf/)).toBeInTheDocument();
    expect(calls(fake, "GET", "/cache/").length).toBeGreaterThan(0);
  });
  it("refreshes explorer releases and totals without losing the filter", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let count = 0;
    const { fake } = renderAppWith("/cache/explorer", (method, path) => {
      if (method !== "GET" || path !== "/cache/") return undefined;
      count++;
      return {
        releases:
          count === 1
            ? []
            : [
                {
                  ReleaseID: "new",
                  Title: "New arrival",
                  Bytes: 100,
                  Size: 100,
                  Plays: 0,
                  Pins: [],
                },
              ],
        preload: {},
      };
    });
    expect(await screen.findByText("No retained media yet", {}, WAIT)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Search cached media"), {
      target: { value: "arrival" },
    });
    const before = calls(fake, "GET", "/cache/").length;
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(calls(fake, "GET", "/cache/").length).toBeGreaterThan(before);
    expect(await screen.findByText("New arrival", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("1 item")).toBeInTheDocument();
    expect(screen.getByLabelText("Search cached media")).toHaveValue("arrival");
  });
  it("backs off polling after a cache load fails", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { fake } = renderAppWith("/cache", (method, path) =>
      method === "GET" && path === "/cache/"
        ? new Response(JSON.stringify({ error: { message: "Cache offline" } }), { status: 503 })
        : undefined,
    );
    expect(await screen.findByText(/Cache offline/, {}, WAIT)).toBeInTheDocument();
    await act(async () => {});
    const before = calls(fake, "GET", "/cache/").length;
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(calls(fake, "GET", "/cache/")).toHaveLength(before);
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(calls(fake, "GET", "/cache/").length).toBeGreaterThan(before);
  });
  it("navigates between compact overview and the dedicated explorer", async () => {
    renderApp("/cache");
    expect(await screen.findByRole("tab", { name: "Overview" }, WAIT)).toHaveAttribute(
      "aria-selected",
      "true",
    );
    fireEvent.click(screen.getByRole("tab", { name: /Explorer/ }));
    expect(await screen.findByRole("main", { name: "Cache explorer" })).toBeInTheDocument();
    expect(screen.getByLabelText("Search cached media")).toBeInTheDocument();
    expect(screen.queryByText("Playback readiness")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Overview" }));
    expect(await screen.findByText("Playback readiness")).toBeInTheDocument();
    expect(screen.queryByLabelText("Search cached media")).not.toBeInTheDocument();
  });
  it("styles Clear cache as outline and points Media disk budget to Back to presets → Pass-through", async () => {
    renderApp("/cache");
    const clearBtn = await screen.findByRole("button", { name: "Clear cache" }, WAIT);
    const editBtn = screen.getByRole("button", { name: "Edit policy" });
    expect(clearBtn.className).toContain("border-border");
    expect(clearBtn.className).not.toContain("bg-primary ");
    expect(editBtn.className).toContain("bg-primary");

    fireEvent.click(editBtn);
    const dialog = await screen.findByRole("dialog", { name: "Edit cache policy" }, WAIT);
    expect(
      within(dialog).getByText(
        /Use Back to presets and choose Pass-through to turn disk caching off/,
      ),
    ).toBeInTheDocument();
  });
  it("hides raw SHA1 keys behind a compact copyable drawer field and merges live playback pills", async () => {
    const sha1 = "8b8ad7ac4ea04925d6c6775d072c9e397f177c36";
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    renderAppWith("/cache/explorer", (method, path) => {
      if (method !== "GET" || path !== "/cache/") return undefined;
      return {
        used: 100 * 1024 ** 2,
        budget: 10 * 1024 ** 3,
        directory: "/cache",
        protectedBytes: 100 * 1024 ** 2,
        metrics: { hits: 1, misses: 0, servedBytes: 100, evictedBytes: 0 },
        prefetch: { queued: 0, running: 0, completed: 0, failed: 0, dropped: 0 },
        preload: {
          state: "idle",
          message: "",
          title: "",
          fetched: 0,
          size: 0,
          allocated: 0,
          ready: 0,
          failed: 0,
          paused: false,
          items: [],
        },
        releases: [
          {
            ReleaseID: sha1,
            Title: "Allen.[2026].2160p.4K.WEB-DL",
            Bytes: 52.4 * 1024 ** 2,
            Size: 0,
            Segments: 12,
            Plays: 1,
            LastUsed: new Date().toISOString(),
            LastPlayed: new Date().toISOString(),
            Pins: ["playback:live", "playback:inprogress"],
            reason: {
              causes: [{ code: "playing", firstSeen: new Date().toISOString() }],
              retention: { kind: "pinned", pins: ["playback:live", "playback:inprogress"] },
              file: "",
              source: "",
            },
          },
        ],
      };
    });

    expect(await screen.findByText("Allen.[2026].2160p.4K.WEB-DL", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Unmatched release")).toBeInTheDocument();
    expect(screen.queryByText(sha1)).not.toBeInTheDocument();
    expect(screen.queryByText("Watching now")).not.toBeInTheDocument();

    fireEvent.click(screen.getByText("Allen.[2026].2160p.4K.WEB-DL"));
    const dialog = await screen.findByRole(
      "dialog",
      { name: "Allen.[2026].2160p.4K.WEB-DL" },
      WAIT,
    );
    expect(within(dialog).getByText("Unmatched release")).toBeInTheDocument();
    expect(within(dialog).queryByText(sha1)).not.toBeInTheDocument();
    expect(within(dialog).queryByText("Watching now")).not.toBeInTheDocument();
    // Hero status pill + Fetched because list item = 2 occurrences, not duplicate pills in hero.
    const heroBadges = dialog.querySelectorAll('[class*="rounded-full"]');
    const playingBadges = Array.from(heroBadges).filter((el) => el.textContent === "Playing now");
    expect(playingBadges).toHaveLength(1);

    expect(within(dialog).getByText("8b8ad7ac…177c36")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Copy cache key" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(sha1));
  });
  it("switches Explorer rows to a stacked card layout below 640px", async () => {
    vi.spyOn(window, "matchMedia").mockImplementation((query: string) => ({
      matches: query === "(max-width: 639px)",
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }));

    const { fake } = renderApp("/cache/explorer");
    expect(await screen.findByText("Arrival", {}, WAIT)).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    const cards = screen.getAllByRole("listitem");
    expect(cards.length).toBeGreaterThan(0);
    const disc = screen.getByText("Arcade.iso").closest('[role="listitem"]');
    expect(disc).toHaveTextContent("Games");
    expect(screen.getByRole("button", { name: "Clear Adventure.iso" })).toBeDisabled();

    fireEvent.click(screen.getByRole("button", { name: "Pin Interstellar" }));
    await waitFor(() => expect(calls(fake, "POST", "/cache/pin")).toHaveLength(1));

    fireEvent.click(screen.getByText("Arrival"));
    expect(await screen.findByRole("dialog", { name: "Arrival" }, WAIT)).toBeInTheDocument();
  });
});

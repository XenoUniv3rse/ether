import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { describe, it, expect, vi } from "vitest";
import { setToken } from "../lib/api";
import { descriptors as notifyDescriptors } from "../mock/extend/descriptors/notify";
import Activity from "../pages/activity/Activity";
import Notifications from "../pages/notifications/Notifications";
import { HardwareStatus } from "../pages/playback/Hardware";
import { SourceStatistics } from "../pages/sources/statistics/Statistics";
import { renderApp } from "./harness";

const WAIT = { timeout: 4000 };
const emptySummary = {
  plays: 0,
  watchedMs: 0,
  bytes: 0,
  completed: 0,
  daily: [],
  hourly: [],
  platforms: [],
  titles: [],
  users: [],
};
const schedule = {
  enabled: false,
  weekday: 1,
  hour: 9,
  userId: 0,
  nextDue: 0,
  lastAttempt: 0,
  lastStatus: "",
};

function backend(
  cluster = false,
  summaryOverride: Record<string, unknown> = emptySummary,
  liveTV = false,
) {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, init });
      let v: unknown = { ok: true };
      if (url.endsWith("/cluster/"))
        v = cluster
          ? {
              mode: "postgres",
              self: { id: "server" },
              nodes: [
                { id: "source", name: "Media source" },
                { id: "gpu", name: "GPU worker" },
              ],
            }
          : { mode: "solo" };
      else if (url.endsWith("/cluster/console"))
        v = {
          sampledAt: new Date().toISOString(),
          truncated: false,
          assignments: [
            {
              sessionId: "execution",
              playId: "live",
              sourceNodeId: "source",
              encoderNodeId: "gpu",
              encoder: "h264_nvenc",
            },
            {
              sessionId: "remote",
              sourceNodeId: "source",
              encoderNodeId: "gpu",
              encoder: "cpu",
              playback: { title: "Remote film", viewer: "Jordan", decision: "transcode" },
            },
          ],
          events: [{ sessionId: "execution", status: "recovering" }],
        };
      else if (url.includes("/extend/registry")) v = { version: 1, descriptors: notifyDescriptors };
      else if (url.includes("/stats/summary")) v = summaryOverride;
      else if (url.includes("/stats/history")) v = [];
      else if (url.endsWith("/stats/plays/other"))
        v = {
          id: "other",
          userId: 12,
          username: "Alex",
          ratingKey: 0,
          title: "Other film",
          platform: "Android TV",
          device: "TV",
          decision: "transcode",
          started: Math.floor(Date.now() / 1000) - 900,
          updated: Math.floor(Date.now() / 1000),
          ended: 0,
          watchedMs: 900_000,
          durationMs: 3_600_000,
          bytes: 800 * 1024 ** 2,
          completed: false,
          failovers: 1,
          switches: 1,
          origin: "Usenet",
          release: "Other.Film.2025.2160p.WEB-DL.HDR10.DTS-GRP",
          ip: "198.51.100.8",
          networkReason: "Remote network (source 198.51.100.8)",
          remote: true,
          measured: true,
          startMs: 2400,
          bufferEvents: 2,
          bufferMs: 4200,
          failed: false,
          error: "",
          reasonKind: "audio_codec",
          reasonDetail: "dts",
          sourceKbps: 24000,
          avgBitrateKbps: 7450,
          uhd: true,
          hdr: true,
        };
      else if (url.endsWith("/users"))
        v = {
          users: [
            { id: 7, title: "Sam", username: "sam-account", isOwner: true },
            { id: 12, title: "Alex", username: "alex-account", isOwner: false },
          ],
        };
      else if (url.endsWith("/status/sessions"))
        v = {
          value: [
            ...(liveTV
              ? [
                  {
                    ID: "sports",
                    TitleName: "Football",
                    Username: "Sam",
                    Decision: "copy",
                    liveChannel: {
                      id: "sports",
                      name: "Sports Live",
                      provider: {
                        sourceName: "Sports provider",
                        loginLabel: "jo***",
                        localInUse: 2,
                        loginLimit: 3,
                      },
                      previousProvider: { sourceName: "Weekend Sports" },
                    },
                  },
                ]
              : []),
            {
              ID: "live",
              TitleName: "Example film",
              Username: "Sam",
              Decision: "directplay",
              SpeedBps: 125000,
              ip: "192.168.3.10",
              networkReason: "Home network (source 192.168.3.10)",
              Client: { deviceName: "Phone" },
            },
            {
              ID: "other",
              UserID: 12,
              TitleName: "Other film",
              Username: "Alex",
              Decision: "transcode",
              decisionReason: "transcode; audio codec dts not supported; video -> h264 1080p",
              bitrateKbps: 8000,
              SpeedBps: 1_250_000,
              BytesServed: 800 * 1024 ** 2,
              Failovers: 1,
              sourceVideo: "HEVC 2160p HDR10",
              outputVideo: "H.264 1080p SDR",
              sourceAudio: "DTS 5.1",
              outputAudio: "AAC 2.0",
              container: "hls",
              origin: "Usenet",
              ip: "198.51.100.8",
              networkReason: "Remote network (source 198.51.100.8)",
              viewOffset: 900_000,
              BufferedUntil: 1_200e9,
              Info: { size: 10.8 * 1024 ** 3, duration: 3_600e9, bitrateKbps: 24000 },
              Resolution: { release: { name: "Other.Film.2025.2160p.WEB-DL.HDR10.DTS-GRP" } },
              Client: {
                deviceName: "TV",
                product: "Jellyfin Android TV",
                platform: "Android TV",
                protocol: "jellyfin",
                remote: true,
              },
            },
          ],
        };
      else if (url.endsWith("/indexer-usage"))
        v = {
          today: "2026-11-03",
          weekStart: "2026-10-28",
          monthStart: "2026-11-01",
          indexers: [
            {
              id: "ix1",
              name: "My indexer",
              enabled: true,
              torrent: false,
              dailyLimit: 100,
              dailyGrabs: 0,
              today: {
                hits: 7,
                grabs: 1,
                purposes: [
                  { purpose: "feed", hits: 4, grabs: 0 },
                  { purpose: "playback", hits: 3, grabs: 1 },
                ],
              },
              week: { hits: 30, grabs: 2, purposes: [{ purpose: "feed", hits: 30, grabs: 2 }] },
              month: {
                hits: 50,
                grabs: 2,
                purposes: [{ purpose: "unattributed", hits: 50, grabs: 2 }],
              },
              days: [],
            },
          ],
        };
      else if (url.includes("/stats/sources"))
        v = {
          days: 7,
          since: "2026-10-28",
          until: "2026-11-03",
          plays: [{ day: "2026-11-03", origin: "Usenet", plays: 2, failovers: 1, failed: 0 }],
          providerDays: [
            {
              id: "provider-key",
              day: "2026-11-03",
              bytes: 4e9,
              articles: 900,
              misses: 10,
              errors: 0,
              backfillSaves: 0,
              bodyMisses: 0,
              bodyTakedowns: 0,
              spillovers: 0,
            },
          ],
          indexerDays: [],
          debridDays: [],
          streamDays: [],
        };
      else if (url.endsWith("/config"))
        v = {
          config: {
            usenet: {
              providers: [
                {
                  id: "provider-key",
                  name: "My news provider",
                  host: "news.example",
                  priority: 0,
                  enabled: true,
                },
                {
                  id: "spare-key",
                  name: "Spare block",
                  host: "spare.example",
                  priority: 1,
                  enabled: true,
                },
              ],
            },
          },
        };
      else if (url.endsWith("/stats/infrastructure"))
        v = {
          snapshots: {
            providers: {
              at: 1,
              data: [
                {
                  id: "provider-key",
                  name: "My news provider",
                  host: "news.example",
                  state: "ok",
                  avgLatency: 0,
                },
              ],
            },
            indexers: {
              at: 1,
              data: [
                {
                  indexer: "My indexer",
                  hitsUsed: 2,
                  hitsRemaining: 98,
                  grabsUsed: 1,
                  grabsRemaining: 49,
                },
              ],
            },
          },
          providerMonth: [],
          indexerMonth: [],
          costs: [],
          notes: [],
        };
      else if (url.includes("/discovery/settings"))
        v = {
          enabled: false,
          plex: true,
          trakt: true,
          notify: false,
          prewarm: false,
          checkHours: 6,
          naturalSearch: false,
          recaps: false,
        };
      else if (url.endsWith("/notifications/")) v = { channels: [], deliveries: [] };
      else if (url.endsWith("/status/hwaccel"))
        v = { value: { hwaccel: { name: "none", encoders: [] } } };
      else if (url.endsWith("/newsletter/settings") && !init?.method) v = schedule;
      return new Response(JSON.stringify(v), { status: 200 });
    }),
  );
  return calls;
}

function LocationSearch() {
  const loc = useLocation();
  return <output data-testid="search">{loc.search}</output>;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <LocationSearch />
      <Routes>
        <Route path="/activity/:tab?" element={<Activity />} />
        <Route path="/notifications/:tab?" element={<Notifications />} />
        <Route path="/sources/usage" element={<SourceStatistics view="usage" />} />
        <Route path="/hardware" element={<HardwareStatus />} />
      </Routes>
    </MemoryRouter>,
  );
}

/** Open an AppKit select by its accessible name and pick an option. */
async function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

const body = (c: { init?: RequestInit }) => JSON.parse(String(c.init?.body));

describe("Activity", () => {
  it("shows actual live streams and sends stop action", async () => {
    const calls = backend();
    renderAt("/activity");
    expect(await screen.findByText("Example film", {}, WAIT)).toBeInTheDocument();
    const card = screen.getByText("Example film").closest("article")!;
    expect(within(card).getByText("Sam")).toBeInTheDocument();
    expect(within(card).getByText("Phone")).toBeInTheDocument();
    await choose("Protocol", "jellyfin");
    expect(screen.queryByText("Example film")).toBeNull();
    expect(screen.getByText("Other film")).toBeInTheDocument();
    await choose("Protocol", "plex");
    expect(screen.queryByText("Other film")).toBeNull();
    expect(
      within(screen.getByText("Example film").closest("article")!).getByText("plex"),
    ).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: /Stop stream for/ }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.url.endsWith("/stats/sessions/live/stop") && c.init?.method === "POST"),
      ).toBe(true),
    );
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(screen.getByRole("tab", { name: "History" }));
    expect(
      await screen.findByText(
        "History starts when Ether observes playback. There are no recorded plays in this period.",
        {},
        WAIT,
      ),
    ).toBeInTheDocument();
  });

  it("shows the serving Live TV login, local usage and previous provider in both layouts", async () => {
    localStorage.removeItem("ether.activity.nowView");
    backend(false, emptySummary, true);
    renderAt("/activity");
    const provider = await screen.findByText("Sports provider · jo***", {}, WAIT);
    const card = within(provider.closest("article")!);
    expect(card.getByText("2/3")).toBeVisible();
    expect(card.getByText("Switched from Weekend Sports")).toBeVisible();

    fireEvent.click(screen.getByRole("radio", { name: "List" }));
    const row = within(screen.getByText("Football").closest("li")!);
    expect(row.getByText("Sports provider · jo***")).toBeVisible();
    expect(row.getByText("2/3")).toBeVisible();
    localStorage.removeItem("ether.activity.nowView");
  });

  it("opens the stream drawer from Now Playing cards, list rows and Overview without intercepting Stop", async () => {
    localStorage.removeItem("ether.activity.nowView");
    const calls = backend();
    const view = renderAt("/activity");
    expect(await screen.findByText("Other film", {}, WAIT)).toBeInTheDocument();

    // Clicking anywhere on the stream card opens the live PlayDrawer and syncs ?play=<id>.
    const card = screen.getByText("Other film").closest("article")!;
    fireEvent.click(within(card).getByText("Alex"));
    expect(screen.getByTestId("search")).toHaveTextContent("?play=other");
    const drawer = await screen.findByRole("dialog", { name: "Other film" }, WAIT);
    expect(within(drawer).getByText(/25% watched · 33% buffered/)).toBeInTheDocument();
    expect(within(drawer).getByText("HEVC 2160p HDR10")).toBeInTheDocument();
    expect(within(drawer).getByText("H.264 1080p SDR")).toBeInTheDocument();
    expect(within(drawer).getByText("DTS 5.1")).toBeInTheDocument();
    expect(within(drawer).getByText("AAC 2.0")).toBeInTheDocument();
    expect(
      await within(drawer).findByText(/Converted because of the audio format \(DTS\)/, {}, WAIT),
    ).toBeInTheDocument();
    expect(within(drawer).getByText("8.0 Mbps")).toBeInTheDocument();
    expect(within(drawer).getByText("Source 24.0 Mbps")).toBeInTheDocument();
    expect(within(drawer).getByText("10 Mbps")).toBeInTheDocument();
    expect(
      within(drawer).getByText("Other.Film.2025.2160p.WEB-DL.HDR10.DTS-GRP"),
    ).toBeInTheDocument();
    expect(within(drawer).getByText("Usenet")).toBeInTheDocument();
    expect(within(drawer).getByText("Away from home")).toBeInTheDocument();
    expect(within(drawer).getByText("198.51.100.8")).toBeInTheDocument();
    expect(within(drawer).getByText("Remote network (source 198.51.100.8)")).toBeInTheDocument();
    expect(within(drawer).getByText(/2 times, 4\.2\s*s/)).toBeInTheDocument();
    expect(calls.some((c) => c.url.endsWith("/stats/plays/other"))).toBe(true);

    // Close the drawer, switch to List view, verify Stop does not open the drawer, then click the row.
    fireEvent.click(within(drawer).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByTestId("search")).toHaveTextContent("");

    // Card view shows computed locality reason for LAN sessions.
    const liveCard = screen.getByText("Example film").closest("article")!;
    expect(within(liveCard).getByText("Home network (source 192.168.3.10)")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "List" }));
    const otherRow = screen.getByText("Other film").closest("li")!;
    const otherGrid = otherRow.querySelector(":scope > div.grid")!;
    // Keep strictly 6 grid cells (poster, title+release, codecs, network, progress, action) so long filenames never shift columns.
    expect(otherGrid.children).toHaveLength(6);
    expect(
      within(otherGrid.children[3] as HTMLElement).getByText(
        "Remote network (source 198.51.100.8)",
      ),
    ).toBeInTheDocument();
    expect(
      within(otherGrid.children[1] as HTMLElement).getByText(
        "Other.Film.2025.2160p.WEB-DL.HDR10.DTS-GRP",
      ),
    ).toBeInTheDocument();
    expect(
      within(otherGrid.children[5] as HTMLElement).getByRole("button", {
        name: "Stop stream for Alex",
      }),
    ).toBeInTheDocument();

    const row = screen.getByText("Example film").closest("li")!;
    fireEvent.click(within(row).getByRole("button", { name: "Stop stream for Sam" }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.url.endsWith("/stats/sessions/live/stop") && c.init?.method === "POST"),
      ).toBe(true),
    );
    expect(screen.queryByRole("dialog")).toBeNull();

    // Keyboard-accessible button on the row opens the drawer too.
    fireEvent.click(within(row).getByRole("button", { name: "Example film" }));
    expect(screen.getByTestId("search")).toHaveTextContent("?play=live");
    expect(await screen.findByRole("dialog", { name: "Example film" }, WAIT)).toBeInTheDocument();
    view.unmount();
    localStorage.removeItem("ether.activity.nowView");

    // Overview Now Playing card opens the same live stream drawer in place.
    const overview = renderApp("/");
    const samSession = await screen.findByText(/Sam Rivera · Living Room/, {}, WAIT);
    fireEvent.click(samSession.closest("li")!);
    const overviewDrawer = await screen.findByRole("dialog", {}, WAIT);
    expect(within(overviewDrawer).getByText("Sam Rivera")).toBeInTheDocument();
    expect(within(overviewDrawer).getByText("Real-Debrid")).toBeInTheDocument();
    expect(within(overviewDrawer).getByText("192.168.1.42")).toBeInTheDocument();
    overview.unmount();
  });

  it("joins cluster assignments without duplicating the source session and includes remote executions", async () => {
    backend(true);
    renderAt("/activity");
    await screen.findByText("Remote film", {}, WAIT);
    expect(screen.getAllByText("Example film")).toHaveLength(1);
    const card = screen.getByText("Example film").closest("article")!;
    expect(within(card).getByText("Media source")).toBeVisible();
    expect(within(card).getByText("GPU worker · GPU · h264_nvenc")).toBeVisible();
    expect(within(card).getByText("Any delivery node")).toBeVisible();
    expect(within(card).getByText("Recovering")).toBeVisible();
    expect(within(card).getByRole("link", { name: /Recovery history/ })).toHaveAttribute(
      "href",
      "/cluster?tab=recovery&session=execution",
    );
    expect(screen.getByText("GPU worker · CPU")).toBeVisible();
  });

  it("filters by a named viewer while sending the existing account ID", async () => {
    const calls = backend();
    renderAt("/activity/history");
    await screen.findByText(/History starts when Ether observes playback/, {}, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "Viewer" }));
    expect(await screen.findByRole("option", { name: "Sam (owner)" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("option", { name: "Alex" }));
    await waitFor(() =>
      expect(calls.some((c) => c.url.includes("/stats/summary?days=30&user=12"))).toBe(true),
    );
    await waitFor(() =>
      expect(calls.some((c) => c.url.includes("/stats/history?days=30&user=12"))).toBe(true),
    );
  });

  it("shows viewing totals and breakdowns on the Viewing tab", async () => {
    backend();
    renderAt("/activity/viewing");
    expect(await screen.findByText("Hours watched", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Watch time")).toBeInTheDocument();
    expect(screen.getByText("Top titles")).toBeInTheDocument();
    expect(screen.getByText("Top viewers")).toBeInTheDocument();
  });

  it("normalises and merges Apps and devices platform buckets on the Delivery tab", async () => {
    backend(false, {
      ...emptySummary,
      plays: 64,
      bytes: 28 * 1024 ** 3,
      platforms: [
        { label: "web", plays: 18, watchedMs: 60_000, bytes: 4 * 1024 ** 3 },
        { label: "Web", plays: 15, watchedMs: 60_000, bytes: 4 * 1024 ** 3 },
        { label: "tvos", plays: 10, watchedMs: 60_000, bytes: 8 * 1024 ** 3 },
        { label: "tvOS", plays: 7, watchedMs: 60_000, bytes: 6 * 1024 ** 3 },
        { label: "androidtv", plays: 6, watchedMs: 60_000, bytes: 3 * 1024 ** 3 },
        { label: "Android", plays: 3, watchedMs: 60_000, bytes: 1024 ** 3 },
        { label: "android", plays: 3, watchedMs: 60_000, bytes: 1024 ** 3 },
        { label: "linux", plays: 2, watchedMs: 60_000, bytes: 1024 ** 3 },
      ],
    });
    renderAt("/activity/delivery");
    const panel = (await screen.findByRole("heading", { name: "Apps and devices" }, WAIT)).closest(
      "section",
    )!;
    expect(within(panel).getByText("Web")).toBeInTheDocument();
    expect(within(panel).getByText(/33 plays/)).toBeInTheDocument();
    expect(within(panel).getByText("Apple TV")).toBeInTheDocument();
    expect(within(panel).getByText(/17 plays/)).toBeInTheDocument();
    expect(within(panel).getByText("Android TV")).toBeInTheDocument();
    expect(within(panel).getByText("Android")).toBeInTheDocument();
    expect(within(panel).getByText("Linux")).toBeInTheDocument();
    expect(within(panel).queryByText("web")).toBeNull();
    expect(within(panel).queryByText("tvos")).toBeNull();
    expect(within(panel).queryByText("androidtv")).toBeNull();
  });
});

describe("Notifications", () => {
  it("saves the weekly schedule in place through the save bar", async () => {
    const calls = backend();
    renderAt("/notifications/report");
    const toggle = await screen.findByLabelText("Email the weekly report", {}, WAIT);
    expect(screen.queryByRole("button", { name: "Configure weekly report" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
    fireEvent.click(toggle);
    await choose("Report viewer", "Alex");
    await choose("Day", "Friday");
    const bar = screen.getByRole("region", { name: "Unsaved changes" });
    fireEvent.click(within(bar).getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.url.endsWith("/newsletter/settings") &&
            c.init?.method === "PUT" &&
            body(c).enabled === true &&
            body(c).userId === 12 &&
            body(c).weekday === 5 &&
            body(c).hour === 9,
        ),
      ).toBe(true),
    );
  });

  it("previews the report for the chosen viewer inline", async () => {
    setToken("ether-admin-report");
    const calls = backend();
    renderAt("/notifications/report");
    await screen.findByLabelText("Email the weekly report", {}, WAIT);
    fireEvent.click(screen.getByLabelText("Email the weekly report"));
    await choose("Report viewer", "Alex");
    fireEvent.click(screen.getByRole("button", { name: "Preview weekly report" }));
    await waitFor(() =>
      expect(calls.some((c) => c.url.endsWith("/stats/newsletter?user=12"))).toBe(true),
    );
    const preview = calls.find((c) => c.url.endsWith("/stats/newsletter?user=12"))!;
    const sent = new Headers(preview.init?.headers);
    expect(sent.get("X-Ether-Admin-Token")).toBe("ether-admin-report");
    expect(sent.get("X-Plex-Token")).toBeNull();
    setToken("");
    expect(await screen.findByTitle("Weekly newsletter preview")).toBeInTheDocument();
    expect(screen.getByLabelText("Email the weekly report")).toBeChecked();
    expect(screen.getByRole("button", { name: "Report viewer" })).toHaveTextContent("Alex");
    // A preview never saves the schedule.
    expect(calls.some((c) => c.init?.method === "PUT")).toBe(false);
  });

  it("creates a channel with real event rules in a drawer", async () => {
    const calls = backend();
    renderAt("/notifications");
    fireEvent.click(await screen.findByRole("button", { name: "New channel" }, WAIT));
    const drawer = await screen.findByRole("dialog", { name: "New notification channel" });
    fireEvent.change(await within(drawer).findByLabelText("Endpoint URL"), {
      target: { value: "https://notify.example/hook" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Clear" }));
    expect(within(drawer).getByLabelText("Weekly newsletter")).toBeDisabled();
    fireEvent.click(within(drawer).getByLabelText("Playback failed"));
    fireEvent.click(within(drawer).getByRole("button", { name: "Save channel" }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.url.includes("/notifications/") &&
            c.init?.method === "PUT" &&
            body(c).secrets?.endpoint === "https://notify.example/hook" &&
            body(c).events.includes("playback.error"),
        ),
      ).toBe(true),
    );
  });

  it("shows the delivery history tab", async () => {
    backend();
    renderAt("/notifications/deliveries");
    expect(await screen.findByText("No queued notifications", {}, WAIT)).toBeInTheDocument();
  });
});

describe("Source statistics", () => {
  it("opens a source from the usage table and saves its plan price in the drawer", async () => {
    const calls = backend();
    renderAt("/sources/usage");
    fireEvent.click(await screen.findByText("My news provider", {}, WAIT));
    const price = await screen.findByLabelText("Monthly price for My news provider", {}, WAIT);
    expect(screen.queryByLabelText("Service ID")).not.toBeInTheDocument();
    fireEvent.change(price, { target: { value: "8.50" } });
    fireEvent.click(screen.getByRole("button", { name: "Save price" }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.url.endsWith("/stats/costs") &&
            c.init?.method === "PUT" &&
            body(c).kind === "provider" &&
            body(c).id === "provider-key" &&
            body(c).monthly === 8.5,
        ),
      ).toBe(true),
    );
  });

  it("lists a configured Usenet provider that has no traffic or live sample", async () => {
    backend();
    renderAt("/sources/usage");
    fireEvent.click(await screen.findByText("Spare block", {}, WAIT));
    expect(await screen.findByText("Totals · Last 7 days", {}, WAIT)).toBeInTheDocument();
    expect(
      screen.getByText("Nothing downloaded from this provider in this period."),
    ).toBeInTheDocument();
  });

  it("shows an indexer's real API hits by purpose in its drawer", async () => {
    backend();
    renderAt("/sources/usage?source=indexer:My%20indexer");
    // Real API hits by purpose, matching the indexer's own counter.
    expect(await screen.findByText("7 of 100", {}, WAIT)).toBeInTheDocument();
    const byPurpose = screen.getByRole("list", { name: "API hits by purpose" });
    expect(byPurpose).toHaveTextContent("Release-feed polls");
    expect(byPurpose).toHaveTextContent("Playback searches");
    fireEvent.click(screen.getByRole("radio", { name: "This month" }));
    expect(screen.getByRole("list", { name: "API hits by purpose" })).toHaveTextContent(
      "Before this breakdown",
    );
  });
});

describe("Hardware status", () => {
  it("shows a tone-map failure on Activity and the transcoder card", async () => {
    const notice = "This FFmpeg cannot tone-map HDR video to SDR";
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        const value = url.endsWith("/status/sessions")
          ? []
          : url.endsWith("/users")
            ? undefined
            : undefined;
        if (url.endsWith("/status/hwaccel")) {
          return new Response(
            JSON.stringify({
              value: { hwaccel: { name: "none", encoders: [] }, playbackError: notice },
            }),
            { status: 200 },
          );
        }
        if (url.endsWith("/status/sessions")) {
          return new Response(JSON.stringify({ value: [] }), { status: 200 });
        }
        if (url.endsWith("/users"))
          return new Response(JSON.stringify({ users: [] }), { status: 200 });
        return new Response(JSON.stringify(value ?? { ok: true }), { status: 200 });
      }),
    );
    const activity = renderAt("/activity");
    expect(await screen.findByRole("alert", {}, WAIT)).toHaveTextContent("cannot tone-map");
    activity.unmount();
    renderAt("/hardware");
    expect(await screen.findByRole("alert", {}, WAIT)).toHaveTextContent("cannot tone-map");
  });

  it("brands VA-API by the GPU's vendor, not always AMD", async () => {
    for (const [vendor, title] of [
      ["intel", "Intel VA-API"],
      ["amd", "AMD VA-API"],
      [undefined, "VA-API"],
    ] as const) {
      vi.stubGlobal(
        "fetch",
        vi.fn(async (input: RequestInfo | URL) => {
          const url = String(input);
          if (url.endsWith("/status/hwaccel"))
            return new Response(
              JSON.stringify({
                value: {
                  hwaccel: {
                    name: "vaapi",
                    encoders: ["h264_vaapi"],
                    device: "/dev/dri/renderD128",
                  },
                  ...(vendor ? { vendor } : {}),
                },
              }),
              { status: 200 },
            );
          if (url.endsWith("/users"))
            return new Response(JSON.stringify({ users: [] }), { status: 200 });
          return new Response(JSON.stringify({ value: [] }), { status: 200 });
        }),
      );
      const view = renderAt("/hardware");
      expect(await screen.findByText(title, { exact: true }, WAIT)).toBeInTheDocument();
      view.unmount();
    }
  });

  it("reports software-only transcoding", async () => {
    backend();
    renderAt("/hardware");
    expect(await screen.findByText("Software only", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("No hardware encoders detected")).toBeInTheDocument();
  });
});

describe("Play timeline drawer", () => {
  it("shows highlighted failure cause, collapsible session logs, offsets, and copy for a failed play", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });

    const cause =
      "every release failed (3 tried): open Blade.Runner.2049.2160p: news.primary: 430 no such article";
    const startedSec = 1_763_400_000;
    const calls: string[] = [];

    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        calls.push(url);
        let v: unknown = { ok: true };
        if (url.endsWith("/cluster/")) v = { mode: "solo" };
        else if (url.endsWith("/users"))
          v = { users: [{ id: 7, title: "Sam", username: "sam", isOwner: true }] };
        else if (url.endsWith("/status/sessions")) v = { value: [] };
        else if (url.endsWith("/status/hwaccel"))
          v = { value: { hwaccel: { name: "none", encoders: [] } } };
        else if (url.includes("/stats/summary")) v = { ...emptySummary, plays: 1 };
        else if (url.includes("/stats/history"))
          v = [
            {
              id: "fail-1",
              userId: 7,
              username: "Sam",
              ratingKey: 2049,
              title: "Blade Runner 2049",
              platform: "tvOS",
              device: "Living Room TV",
              decision: "",
              started: startedSec,
              updated: startedSec + 3,
              ended: startedSec + 3,
              bytes: 0,
              watchedMs: 0,
              durationMs: 0,
              completed: false,
              failovers: 0,
              error: cause,
            },
          ];
        else if (url.endsWith("/stats/plays/fail-1"))
          v = {
            id: "fail-1",
            userId: 7,
            username: "Sam",
            ratingKey: 2049,
            title: "Blade Runner 2049",
            platform: "tvOS",
            device: "Living Room TV",
            decision: "",
            started: startedSec,
            updated: startedSec + 3,
            ended: startedSec + 3,
            watchedMs: 0,
            durationMs: 0,
            bytes: 0,
            completed: false,
            failovers: 0,
            switches: 0,
            origin: "Usenet",
            release: "Blade.Runner.2049.2160p",
            ip: "192.168.1.20",
            remote: false,
            measured: true,
            startMs: null,
            bufferEvents: 0,
            bufferMs: 0,
            failed: true,
            error: cause,
            reasonKind: "",
            reasonDetail: "",
            sourceKbps: 0,
            avgBitrateKbps: 0,
            uhd: false,
            hdr: false,
          };
        else if (url.endsWith("/activity/plays/fail-1/timeline"))
          v = {
            playId: "fail-1",
            startedMs: startedSec * 1000,
            failed: true,
            error: cause,
            entries: [
              {
                seq: 1,
                atMs: startedSec * 1000,
                offsetMs: 0,
                type: "event",
                kind: "started",
                level: "info",
                message: "Playback requested",
                detail: "Plex for Apple TV on Living Room TV · tvOS",
              },
              {
                seq: 2,
                atMs: startedSec * 1000 + 200,
                offsetMs: 200,
                type: "log",
                level: "debug",
                message: "probing candidate header",
                detail: "release=Blade.Runner.2049.2160p · apikey=[redacted]",
              },
              {
                seq: 3,
                atMs: startedSec * 1000 + 500,
                offsetMs: 500,
                type: "log",
                level: "info",
                message: "opening nzb segments",
                detail: "provider=news.primary",
              },
              {
                seq: 4,
                atMs: startedSec * 1000 + 800,
                offsetMs: 800,
                type: "event",
                kind: "candidate",
                level: "warn",
                message: "Candidate failed: Blade.Runner.2049.2160p",
                detail: "news.primary: 430 no such article",
              },
              {
                seq: 5,
                atMs: startedSec * 1000 + 1600,
                offsetMs: 1600,
                type: "log",
                level: "warn",
                message: "source read failed",
                detail: "provider=news.primary · err=430 no such article",
              },
              {
                seq: 6,
                atMs: startedSec * 1000 + 2400,
                offsetMs: 2400,
                type: "event",
                kind: "error",
                level: "error",
                message: "Playback failed",
                detail: cause,
              },
            ],
          };
        return new Response(JSON.stringify(v), { status: 200 });
      }),
    );

    renderAt("/activity/history");
    const rowTitle = await screen.findByText("Blade Runner 2049", {}, WAIT);
    expect(screen.getByText(cause)).toBeInTheDocument();

    fireEvent.click(rowTitle);
    const drawer = await screen.findByRole("dialog", { name: "Blade Runner 2049" }, WAIT);
    expect(within(drawer).getByRole("tab", { name: "Overview" })).toHaveAttribute(
      "aria-selected",
      "true",
    );

    fireEvent.click(within(drawer).getByRole("tab", { name: "Timeline" }));
    const alert = await within(drawer).findByRole("alert", {}, WAIT);
    expect(alert).toHaveTextContent("Playback failed");
    expect(alert).toHaveTextContent(cause);

    // Offsets, events, and warn/error logs show by default; routine info + debug logs are hidden.
    const list = within(drawer).getByRole("list", { name: "Playback timeline" });
    expect(within(list).getByText("+0.0 s")).toBeInTheDocument();
    expect(within(list).getByText("+0.8 s")).toBeInTheDocument();
    expect(within(list).getByText("+2.4 s")).toBeInTheDocument();
    expect(within(list).getByText("Playback requested")).toBeInTheDocument();
    expect(within(list).getByText("source read failed")).toBeInTheDocument();
    expect(within(list).queryByText("probing candidate header")).toBeNull();
    expect(within(list).queryByText("opening nzb segments")).toBeNull();
    expect(within(drawer).getByText("2 routine log lines hidden")).toBeInTheDocument();

    // Toggling Show all reveals both info and debug session logs.
    fireEvent.click(within(drawer).getByRole("switch", { name: "Show all" }));
    expect(within(list).getByText("probing candidate header")).toBeInTheDocument();
    expect(within(list).getByText("opening nzb segments")).toBeInTheDocument();

    // Copy timeline copies the full redacted timeline text.
    fireEvent.click(within(drawer).getByRole("button", { name: "Copy timeline" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    const copied = String(writeText.mock.calls[0]?.[0] ?? "");
    expect(copied).toContain("Blade Runner 2049");
    expect(copied).toContain(cause);
    expect(copied).toContain("+0.2 s [DEBUG] (log) probing candidate header");
    expect(copied).toContain("apikey=[redacted]");
    expect(calls.some((u) => u.endsWith("/ether/api/activity/plays/fail-1/timeline"))).toBe(true);
  });

  it("shows an empty state for plays recorded before timelines existed without scanning logs", async () => {
    const calls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        calls.push(url);
        let v: unknown = { ok: true };
        if (url.endsWith("/cluster/")) v = { mode: "solo" };
        else if (url.endsWith("/users")) v = { users: [] };
        else if (url.endsWith("/status/sessions")) v = { value: [] };
        else if (url.endsWith("/status/hwaccel"))
          v = { value: { hwaccel: { name: "none", encoders: [] } } };
        else if (url.includes("/stats/summary")) v = emptySummary;
        else if (url.includes("/stats/history")) v = [];
        else if (url.endsWith("/stats/plays/legacy-1"))
          v = {
            id: "legacy-1",
            userId: 7,
            username: "Sam",
            ratingKey: 101,
            title: "Alien",
            platform: "tvOS",
            device: "Apple TV",
            decision: "directplay",
            started: 1_700_000_000,
            updated: 1_700_003_600,
            ended: 1_700_003_600,
            watchedMs: 3_600_000,
            durationMs: 3_600_000,
            bytes: 4 * 1024 ** 3,
            completed: true,
            failovers: 0,
            switches: 0,
            origin: "Usenet",
            release: "Alien.1979.1080p",
            ip: "192.168.1.20",
            remote: false,
            measured: false,
            startMs: null,
            bufferEvents: null,
            bufferMs: null,
            failed: false,
            error: "",
            reasonKind: "",
            reasonDetail: "",
            sourceKbps: 12000,
            avgBitrateKbps: 12000,
            uhd: false,
            hdr: false,
          };
        else if (url.endsWith("/activity/plays/legacy-1/timeline"))
          v = {
            playId: "legacy-1",
            startedMs: 1_700_000_000_000,
            failed: false,
            error: "",
            entries: [],
          };
        return new Response(JSON.stringify(v), { status: 200 });
      }),
    );

    renderAt("/activity/history?play=legacy-1");
    const drawer = await screen.findByRole("dialog", { name: "Alien" }, WAIT);
    fireEvent.click(within(drawer).getByRole("tab", { name: "Timeline" }));
    expect(
      await within(drawer).findByText("No timeline was recorded for this play.", {}, WAIT),
    ).toBeInTheDocument();
    expect(calls.some((u) => u.includes("/logs"))).toBe(false);
  });

  it("renders Live TV history rows, Viewing live kind and PlayDrawer without looking up ratingKey 0", async () => {
    vi.stubGlobal(
      "IntersectionObserver",
      class {
        readonly root = null;
        readonly rootMargin = "";
        readonly thresholds = [];
        constructor(private readonly callback: IntersectionObserverCallback) {}
        observe(target: Element) {
          this.callback(
            [{ isIntersecting: true, target } as IntersectionObserverEntry],
            this as unknown as IntersectionObserver,
          );
        }
        unobserve() {}
        disconnect() {}
        takeRecords() {
          return [];
        }
      },
    );
    const calls: string[] = [];
    const pngBytes = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10]);
    const liveSummary = {
      plays: 3,
      watchedMs: 5_400_000,
      bytes: 1_500_000_000,
      completed: 1,
      daily: [{ label: "2026-11-20", plays: 3, watchedMs: 5_400_000, bytes: 1_500_000_000 }],
      hourly: [],
      platforms: [],
      titles: [
        {
          label: "ESPN",
          channelId: "ch-espn",
          plays: 2,
          watchedMs: 3_600_000,
          bytes: 1_000_000_000,
        },
        {
          label: "Legacy Live",
          key: "0",
          plays: 1,
          watchedMs: 1_800_000,
          bytes: 500_000_000,
        },
      ],
      users: [{ label: "Sam", key: "7", plays: 3, watchedMs: 5_400_000, bytes: 1_500_000_000 }],
      kinds: [
        { label: "live", plays: 2, watchedMs: 3_600_000, bytes: 1_000_000_000 },
        { label: "movie", plays: 1, watchedMs: 1_800_000, bytes: 500_000_000 },
      ],
      dailyKinds: [{ label: "2026-11-20", movieMs: 1_800_000, episodeMs: 0, liveMs: 3_600_000 }],
    };
    const liveHistory = [
      {
        id: "live-espn-1",
        userId: 7,
        ratingKey: 0,
        username: "Sam",
        title: "SportsCenter",
        channelId: "ch-espn",
        channelName: "ESPN",
        channelNumber: "206",
        liveSourceName: "Sports provider",
        liveLoginLabel: "jo***",
        device: "Living Room Apple TV",
        platform: "tvOS",
        decision: "copy",
        started: 1_763_660_000,
        ended: 1_763_661_800,
        bytes: 800_000_000,
        watchedMs: 1_800_000,
        completed: false,
        failovers: 0,
        error: "",
      },
    ];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        calls.push(url);
        if (url.includes("/livetv/channels/ch-espn/logo")) {
          return new Response(pngBytes, {
            status: 200,
            headers: { "Content-Type": "image/png", "X-Ether-Logo-Background": "light" },
          });
        }
        let v: unknown = { ok: true };
        if (url.endsWith("/cluster/")) v = { mode: "solo" };
        else if (url.endsWith("/users"))
          v = { users: [{ id: 7, title: "Sam", username: "sam", isOwner: true }] };
        else if (url.endsWith("/status/sessions")) v = { value: [] };
        else if (url.endsWith("/status/hwaccel"))
          v = { value: { hwaccel: { name: "none", encoders: [] } } };
        else if (url.includes("/stats/summary")) v = liveSummary;
        else if (url.includes("/stats/history")) v = liveHistory;
        else if (url.endsWith("/stats/plays/live-espn-1"))
          v = {
            ...liveHistory[0],
            updated: 1_763_661_800,
            durationMs: 0,
            switches: 0,
            origin: "Live TV",
            release: "",
            remote: false,
            measured: true,
            startMs: 1200,
            bufferEvents: 0,
            bufferMs: 0,
            failed: false,
            reasonKind: "",
            reasonDetail: "",
          };
        return new Response(JSON.stringify(v), { status: 200 });
      }),
    );

    const view = renderAt("/activity/history");
    expect(await screen.findByText("206 · ESPN", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("SportsCenter · Sam")).toBeInTheDocument();
    expect(screen.getByText("Sports provider · jo***")).toBeVisible();
    expect(screen.getByText("Live TV")).toBeInTheDocument();
    await waitFor(() =>
      expect(calls.some((u) => u.includes("/livetv/channels/ch-espn/logo"))).toBe(true),
    );

    // Clicking the Live TV row opens PlayDrawer with channel logo and without /titles/0 lookup.
    fireEvent.click(screen.getByText("206 · ESPN"));
    const drawer = await screen.findByRole("dialog", { name: "ESPN" }, WAIT);
    expect(within(drawer).getByText("SportsCenter")).toBeInTheDocument();
    expect(within(drawer).getAllByText("Live TV").length).toBeGreaterThan(0);

    view.unmount();

    // Viewing tab renders the "live" kind bucket as "Live TV" and channel logo in Top titles.
    renderAt("/activity/viewing");
    expect((await screen.findAllByText("Live TV", {}, WAIT)).length).toBeGreaterThan(0);
    expect(await screen.findByText("ESPN", {}, WAIT)).toBeInTheDocument();
    expect(calls.some((u) => /\/titles\/0\b/.test(u))).toBe(false);
  });

  it("renders plugin item artwork on Activity Viewing and omits clipped text in w-8 fallback thumbnails", async () => {
    setToken("owner-tok");
    const calls: string[] = [];
    const pluginSummary = {
      plays: 2,
      watchedMs: 3_600_000,
      bytes: 200_000_000,
      completed: 2,
      daily: [{ label: "2026-11-24", plays: 2, watchedMs: 3_600_000, bytes: 200_000_000 }],
      hourly: [],
      platforms: [],
      titles: [
        {
          label: "Pink Floyd - S24E01 - Speak to Me",
          key: "9005000000092613",
          plays: 1,
          watchedMs: 1_800_000,
          bytes: 100_000_000,
        },
        {
          label: "Unknown Plugin Track",
          key: "9005000000092614",
          plays: 1,
          watchedMs: 1_800_000,
          bytes: 100_000_000,
        },
      ],
      users: [{ label: "Sam", key: "7", plays: 2, watchedMs: 3_600_000, bytes: 200_000_000 }],
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        calls.push(url);
        if (url.endsWith("/catalog/title/9005000000092613")) {
          return new Response(
            JSON.stringify({
              key: "9005000000092613",
              type: "episode",
              title: "Speak to Me",
              poster: "https://art.example/dsotm-cover.jpg",
            }),
            { status: 200 },
          );
        }
        if (url.endsWith("/catalog/title/9005000000092614")) {
          return new Response(
            JSON.stringify({
              key: "9005000000092614",
              type: "episode",
              title: "Unknown Plugin Track",
            }),
            { status: 200 },
          );
        }
        let v: unknown = { ok: true };
        if (url.endsWith("/cluster/")) v = { mode: "solo" };
        else if (url.endsWith("/users"))
          v = { users: [{ id: 7, title: "Sam", username: "sam", isOwner: true }] };
        else if (url.endsWith("/status/sessions")) v = { value: [] };
        else if (url.endsWith("/status/hwaccel"))
          v = { value: { hwaccel: { name: "none", encoders: [] } } };
        else if (url.includes("/stats/summary")) v = pluginSummary;
        else if (url.includes("/stats/history")) v = [];
        return new Response(JSON.stringify(v), { status: 200 });
      }),
    );

    const view = renderAt("/activity/viewing");
    expect(
      await screen.findByText("Pink Floyd - S24E01 - Speak to Me", {}, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByText("Unknown Plugin Track")).toBeInTheDocument();

    await waitFor(() => {
      const img = view.container.querySelector('img[src="https://art.example/dsotm-cover.jpg"]');
      expect(img).not.toBeNull();
    });
    expect(calls.some((u) => u.endsWith("/catalog/title/9005000000092613"))).toBe(true);
    // The fallback w-8 thumbnail for Unknown Plugin Track must not duplicate/clip the label text inside the thumb box.
    expect(screen.getAllByText("Unknown Plugin Track")).toHaveLength(1);
    view.unmount();
  });
});

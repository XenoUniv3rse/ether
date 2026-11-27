import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

describe.each(["/", "/activity"])("Playback progress on %s", (path) => {
  it("shows read-ahead buffer ahead of the playhead", async () => {
    renderApp(path, {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const res = await original(input, init);
        const dashboard = String(input).endsWith("/dashboard");
        const sessions = String(input).endsWith("/status/sessions");
        if (!dashboard && !sessions) return res;
        const body = await res.json();
        for (const s of dashboard ? body.sections.sessions : body.value) {
          s.viewOffset = 30000;
          s.Info.duration = 60e9;
          s.Info.size = 100;
          s.BufferedUntil = 45e9;
          s.BufferedBytes = 0;
        }
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    expect(await screen.findByText(/50% watched · 75% buffered/, {}, WAIT)).toBeInTheDocument();
  });
  it.each([undefined, 0, 55000])(
    "uses timeline position %s instead of byte delivery",
    async (position) => {
      renderApp(path, {}, (fake) => {
        const original = fake.fetch;
        fake.fetch = async (input, init) => {
          const res = await original(input, init);
          const dashboard = String(input).endsWith("/dashboard");
          const sessions = String(input).endsWith("/status/sessions");
          if (!dashboard && !sessions) return res;
          const body = await res.json();
          for (const s of dashboard ? body.sections.sessions : body.value) {
            s.viewOffset = position;
            s.Info.duration = 60e9;
            s.Info.size = 100;
            s.BytesServed = 33;
            s.BufferedBytes = 0;
            s.BufferedUntil = 0;
          }
          return new Response(JSON.stringify(body), { status: 200 });
        };
      });
      if (position === undefined) {
        expect(
          await screen.findByText("Waiting for playback position", {}, WAIT),
        ).toBeInTheDocument();
        expect(screen.queryByText(/% watched/)).not.toBeInTheDocument();
      } else {
        expect(
          await screen.findByText(`${Math.round((position / 60000) * 100)}% watched`, {}, WAIT),
        ).toBeInTheDocument();
      }
      expect(screen.queryByText(/of .* delivered/)).not.toBeInTheDocument();
    },
  );
});

describe("Overview", () => {
  it("shows now playing, health, activity and best rows", async () => {
    renderApp("/");
    expect(await screen.findByText(/Sam Rivera · Living Room/, {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText("Direct play")).toBeInTheDocument();
    expect(screen.getByText(/% watched/)).toBeInTheDocument();
    const health = screen.getByText("Health").closest("section")!;
    for (const label of [
      "Usenet",
      "Indexers",
      "Cache",
      "Catalog",
      "Remote access",
      "plex.direct",
    ]) {
      expect(within(health).getByText(label)).toBeInTheDocument();
    }
    expect(
      await screen.findByRole("img", { name: /Plays per day over the last 30 days/ }, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByText("Plays · 30 days")).toBeInTheDocument();
    expect(await screen.findByText("Best rows")).toBeInTheDocument();
    // The header health indicator summarises everything.
    expect(screen.getByRole("button", { name: /Server health/ })).toBeInTheDocument();
  });

  it("shows ranked and low-sample Best rows and renders Activity table with a sticky header", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        if (String(input).endsWith("/insights/rows")) {
          return new Response(
            JSON.stringify({
              rows: [
                {
                  hubId: "top-10",
                  title: "Top 10 for You",
                  impressions7d: 80,
                  plays7d: 22,
                  playRate: 0.191,
                  lowSample: false,
                },
                {
                  hubId: "genre-showcase",
                  title: "Acclaimed by Genre",
                  impressions7d: 5,
                  plays7d: 1,
                  playRate: 0.02,
                  lowSample: true,
                },
                {
                  hubId: "popular-on-server",
                  title: "Popular on This Server",
                  impressions7d: 40,
                  plays7d: 0,
                  playRate: 0,
                  lowSample: false,
                },
              ],
            }),
            { status: 200 },
          );
        }
        return original(input, init);
      };
    });
    const bestRows = (await screen.findByRole("heading", { name: "Best rows" }, WAIT)).closest(
      "section",
    )!;
    const cwItem = (await within(bestRows).findByText("Top 10 for You", {}, WAIT)).closest("li")!;
    expect(within(cwItem).getByText("19.1%")).toBeInTheDocument();

    expect(within(bestRows).getByText("Acclaimed by Genre")).toBeInTheDocument();
    expect(within(bestRows).getByText("Not enough data yet")).toBeInTheDocument();

    const popItem = within(bestRows).getByText("Popular on This Server").closest("li")!;
    expect(within(popItem).getByText("0.0%")).toBeInTheDocument();

    const activitySection = screen
      .getByRole("heading", { name: "Activity · last 30 days" })
      .closest("section")!;
    fireEvent.click(within(activitySection).getByRole("button", { name: "Show as table" }));
    const dayHeader = await within(activitySection).findByRole("columnheader", { name: "Day" });
    expect(dayHeader.closest("thead")).toHaveClass("sticky");
    expect(activitySection.querySelector(".max-h-56")).toBeNull();
  });

  it("computes Overview 30-day hours from exact watchedMs and passes browser timezone", async () => {
    const requestedUrls: string[] = [];
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const url = String(input);
        if (url.includes("/insights/activity")) {
          requestedUrls.push(url);
          // 10 days of 150,000 ms each (2.5 min): each day rounds to 0.0 h,
          // while summed watchedMs = 1,500,000 ms -> 0.4 h (matching Activity › Viewing).
          const days = Array.from({ length: 10 }, (_, i) => ({
            date: `2026-11-${String(i + 15).padStart(2, "0")}`,
            plays: 2,
            hours: 0,
            watchedMs: 150_000,
            uniqueViewers: i === 9 ? 3 : 1,
          }));
          return new Response(JSON.stringify({ days }), { status: 200 });
        }
        return original(input, init);
      };
    });
    const activitySection = (
      await screen.findByRole("heading", { name: "Activity · last 30 days" }, WAIT)
    ).closest("section")!;
    expect(await within(activitySection).findByText("0.4", {}, WAIT)).toBeInTheDocument();
    expect(within(activitySection).getByText("20")).toBeInTheDocument();
    expect(within(activitySection).getByText("3 viewers")).toBeInTheDocument();
    expect(requestedUrls.some((u) => /[?&]tz=-?\d+/.test(u))).toBe(true);
  });

  it("shows live server instruments: CPU, memory, network, GPU and Ether itself", async () => {
    const { fake } = renderApp("/");
    const server = (await screen.findByRole("heading", { name: "Server" }, WAIT)).closest(
      "section",
    )!;
    const cpu = await within(server).findByRole("group", { name: "CPU" }, WAIT);
    expect(within(cpu).getByText(/12 cores/)).toBeInTheDocument();
    expect(
      within(cpu).getByRole("img", { name: /CPU use over the last 5 minutes/ }),
    ).toBeInTheDocument();
    expect(
      within(within(server).getByRole("group", { name: "Memory" })).getByText(/of 24.0 GiB/),
    ).toBeInTheDocument();
    const net = within(server).getByRole("group", { name: "Network" });
    expect(within(net).getByLabelText("Download")).toBeInTheDocument();
    expect(within(net).getByLabelText("Upload")).toBeInTheDocument();
    expect(within(net).getAllByText(/Mbps/).length).toBeGreaterThanOrEqual(2);
    const gpu = within(server).getByRole("group", { name: "GPU · RTX 4080" });
    expect(within(gpu).getByText("Encode")).toBeInTheDocument();
    expect(
      within(within(gpu).getByText("Encode").parentElement!).getByText("18%"),
    ).toBeInTheDocument();
    expect(within(server).getByText(/\d+% CPU · 412\.0 MiB/)).toBeInTheDocument();
    expect(within(server).getByText("videotoolbox")).toBeInTheDocument();
    expect(calls(fake, "GET", "/system/load").length).toBeGreaterThan(0);
  });

  it("says when this hardware reports no GPU", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const res = await original(input, init);
        if (!String(input).endsWith("/system/load")) return res;
        const body = await res.json();
        body.gpus = [];
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    const gpu = await screen.findByRole("group", { name: "GPU" }, WAIT);
    expect(within(gpu).getByText("Not reported by this hardware")).toBeInTheDocument();
  });

  it("lists problems first, each linking to the screen that fixes it", async () => {
    renderApp("/", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const res = await original(input, init);
        if (!String(input).endsWith("/dashboard")) return res;
        const body = await res.json();
        body.sections.providers = { error: "Every provider refused the login" };
        return new Response(JSON.stringify(body), { status: 200 });
      };
    });
    const attention = await screen.findByRole("region", { name: "Needs attention" }, WAIT);
    expect(within(attention).getByText("Every provider refused the login")).toBeInTheDocument();
    expect(within(attention).getByRole("link", { name: "Fix" })).toHaveAttribute(
      "href",
      "/sources/usenet",
    );
  });
});

describe("Updates settings", () => {
  it("shows update status and asks the server to install", async () => {
    const { fake } = renderApp("/maintenance/updates");
    expect(await screen.findByText(/Version 0\.9\.1/, {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText(/03:00–06:00 local time/)).toBeInTheDocument();
    expect(screen.getByText(/Last checked /)).toBeInTheDocument();
    expect(screen.getByText(/Next check/)).toBeInTheDocument();
    expect(calls(fake, "POST", "/updates/check")).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Check now" }));
    await waitFor(() => expect(calls(fake, "POST", "/updates/check")).toHaveLength(1));
    await waitFor(() => expect(screen.getByRole("button", { name: "Update now" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "Update now" }));
    expect(
      await screen.findByRole("button", { name: "Install now anyway" }, WAIT),
    ).toBeInTheDocument();
    expect(calls(fake, "POST", "/updates/apply").length).toBe(1);
  });

  it("opens the available release notes and reopens the installed release notes", async () => {
    renderApp("/maintenance/updates");
    fireEvent.click(await screen.findByRole("button", { name: "Release notes" }, WAIT));
    const available = await screen.findByRole("dialog", { name: "Release notes for v0.9.1" });
    expect(await within(available).findByText(/Library scans/)).toBeInTheDocument();
    expect(within(available).getByRole("link", { name: "Release notes on GitHub" })).toBeVisible();
    fireEvent.keyDown(available, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "What's new in v0.9.0-mock" }));
    expect(await screen.findByText(/Release notes open in Ether/)).toBeInTheDocument();
  });

  it("keeps the channel up front and tucks the window, feed and wait behind more options", async () => {
    renderApp("/maintenance/updates");
    expect(await screen.findByLabelText("Check for updates", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByLabelText("Channel")).toBeInTheDocument();
    expect(screen.getByLabelText("Install updates automatically")).toBeInTheDocument();
    expect(screen.queryByLabelText("Update feed")).toBeNull();
    expect(screen.queryByLabelText("Wait for playback to finish")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "More options: Release channel" }));
    expect(await screen.findByLabelText("Maintenance window starts", {}, WAIT)).toHaveValue(
      "03:00",
    );
    expect(screen.getByLabelText("Maintenance window ends")).toHaveValue("06:00");
    expect(screen.getByLabelText("Update feed")).toHaveValue("");
    expect(screen.getByLabelText("Wait for playback to finish")).toHaveValue(null); // an empty duration amount
  });

  it("disables Check for updates and hides raw Go updater errors when the build lacks a release key", async () => {
    renderApp("/maintenance/updates", {}, (fake) => {
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        if (String(input).endsWith("/updates/")) {
          return new Response(
            JSON.stringify({
              enabled: false,
              current: "0.3.14",
              capability: "disabled",
              disabledReason: "updater: no release public key configured",
              state: "idle",
              autoUpdate: true,
              requested: false,
              force: false,
              window: { start: "03:00", end: "06:00", open: false },
              activeSessions: 0,
              quietPeriodSeconds: 60,
              confirming: false,
            }),
            { status: 200 },
          );
        }
        return original(input, init);
      };
    });
    expect(
      await screen.findByText("This build can't check for signed updates.", {}, WAIT),
    ).toBeInTheDocument();
    expect(screen.queryByText(/updater:/i)).toBeNull();
    const toggle = await screen.findByRole("switch", { name: "Check for updates" }, WAIT);
    expect(toggle).toBeDisabled();
    expect(toggle).not.toBeChecked();
    expect(screen.getByRole("button", { name: "Check now" })).toBeDisabled();
  });
});

describe("Getting started", () => {
  afterEach(() => localStorage.removeItem("ether.getting-started.dismissed"));

  it("lists the remaining fresh-install steps and hides when dismissed", async () => {
    localStorage.removeItem("ether.getting-started.dismissed");
    renderApp("/", {}, (fake) => {
      fake.state.config.debrid = { ...fake.state.config.debrid!, providers: [] };
      fake.state.config.usenet.providers = [];
      fake.state.config.quality.default = "1080p";
      fake.state.config.frontends = {
        jellyfin: { enabled: true },
      } as typeof fake.state.config.frontends;
      const original = fake.fetch;
      fake.fetch = async (input, init) => {
        const response = await original(input, init);
        const url = String(input);
        if (url.endsWith("/status/sessions")) {
          return new Response(JSON.stringify({ name: "sessions", value: [] }), { status: 200 });
        }
        if (url.endsWith("/dashboard")) {
          const body = await response.json();
          body.sections.sessions = [];
          body.setupSteps = [
            { id: "catalog", title: "Metadata sources", complete: false, required: false },
          ];
          return new Response(JSON.stringify(body), { status: 200 });
        }
        if (url.endsWith("/viewers")) {
          const body = await response.json();
          const viewers = (body.viewers ?? body).filter(
            (viewer: { isOwner?: boolean }) => viewer.isOwner,
          );
          return new Response(JSON.stringify({ viewers }), { status: 200 });
        }
        return response;
      };
    });
    const steps = await screen.findByRole("region", { name: "Next steps" }, WAIT);
    expect(within(steps).queryByRole("link", { name: "Add a metadata source" })).toBeNull();
    expect(within(steps).getByRole("link", { name: "Add a source" })).toHaveAttribute(
      "href",
      "/sources",
    );
    expect(screen.getByRole("link", { name: "Connect a device" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Add viewers" })).toHaveAttribute("href", "/viewers");
    expect(
      screen.getByRole("link", { name: "Playback quality: 1080p at home · change" }),
    ).toHaveAttribute("href", "/releases/profiles");
    expect(within(steps).queryByRole("link", { name: "Connect Plex" })).toBeNull(); // Plex is off
    fireEvent.click(within(steps).getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByRole("region", { name: "Next steps" })).not.toBeInTheDocument();
  });

  it("hides once source, device, and viewers are done, even at 1080p", async () => {
    localStorage.removeItem("ether.getting-started.dismissed");
    renderApp("/", {}, (fake) => {
      // A deliberate 1080p default is not a todo and must not keep the bar up.
      fake.state.config.quality.default = "1080p";
    });
    expect(await screen.findByTestId("getting-started-clear", {}, WAIT)).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Next steps" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /Playback quality/ })).not.toBeInTheDocument();
  });

  it("shows the current default on the quality chip without flagging it", async () => {
    localStorage.removeItem("ether.getting-started.dismissed");
    renderApp("/", {}, (fake) => {
      fake.state.config.debrid = { ...fake.state.config.debrid!, providers: [] };
      fake.state.config.usenet.providers = [];
      fake.state.config.quality.default = "4K";
    });
    const steps = await screen.findByRole("region", { name: "Next steps" }, WAIT);
    expect(
      within(steps).getByRole("link", { name: "Playback quality: 4K at home · change" }),
    ).toHaveAttribute("href", "/releases/profiles");
  });
});

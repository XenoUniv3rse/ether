import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { CatalogueResults } from "../pages/libraries/browser/CatalogueResults";
import { calls, renderApp, settleUi } from "./harness";
import { renderAppWith } from "./routes";

describe("Libraries workspace", () => {
  it("lists libraries without global settings or separate plugin buttons", async () => {
    renderApp("/libraries");
    expect(
      await screen.findByRole("heading", { name: "Libraries", level: 1 }, { timeout: 15000 }),
    ).toBeInTheDocument();
    expect(await screen.findByText("Books")).toBeInTheDocument();
    expect(screen.getAllByText("Movies").length).toBeGreaterThan(0);
    const moviesCard = screen.getByRole("region", { name: "Movies" });
    expect(within(moviesCard).getByText(/In apps: Vela, Plex/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Library settings/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /From a plugin/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/Titles from:/)).not.toBeInTheDocument();
  });

  it("explains disabled plugin libraries with a link to Plugins and keeps card actions in the menu", async () => {
    const { fake } = renderApp("/libraries", {}, (server) => {
      const base = server.fetch;
      server.fetch = async (input, init) => {
        const res = await base(input, init);
        const url = new URL(String(input), "http://localhost");
        if (url.pathname !== "/ether/api/libraries" || (init?.method && init.method !== "GET")) {
          return res;
        }
        const body = await res.json();
        body.libraries = body.libraries.map((lib: { id: number; source: string }) =>
          lib.source === "plugin"
            ? { ...lib, sync: { lastError: "plugins: plugin is disabled" } }
            : lib,
        );
        return new Response(JSON.stringify(body), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      };
    });

    const booksCard = await screen.findByRole("region", { name: "Books" }, { timeout: 15000 });
    expect(within(booksCard).getByText("Plugin off")).toBeInTheDocument();
    const alert = within(booksCard).getByRole("alert");
    expect(alert).toHaveTextContent("Its plugin is turned off. Turn it on in Plugins.");
    expect(within(alert).getByRole("link", { name: "Plugins" })).toHaveAttribute(
      "href",
      "/plugins/installed",
    );

    const moviesCard = screen.getByRole("region", { name: "Movies" });
    fireEvent.click(within(moviesCard).getByRole("button", { name: "More actions for Movies" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Move right" }));
    await settleUi(() => {
      expect(calls(fake, "PATCH", "/libraries/").length).toBeGreaterThan(0);
    });
  });

  it("renders aligned list-view headers and placeholders and reveals grid selection on demand", () => {
    const sampleItems = [
      {
        key: "1",
        title: "Sparse Entry",
        type: "movie" as const,
        year: 2021,
      },
      {
        key: "2",
        title: "Full Entry",
        type: "movie" as const,
        year: 2024,
        rating: 8.2,
        runtimeMinutes: 118,
        genres: ["Drama", "Mystery"],
      },
    ];

    const { rerender } = render(
      <CatalogueResults
        items={sampleItems}
        list={true}
        queryKey="q1"
        page={1}
        hasMore={false}
        loading={false}
        onLoadMore={() => {}}
        onInspect={() => {}}
        onBulkWatchlist={async () => {}}
      />,
    );

    expect(screen.getByRole("columnheader", { name: "Title" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Year" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Rating" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Runtime" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Genres" })).toBeInTheDocument();
    // Missing rating, runtime, and genres on Sparse Entry render fixed "—" placeholders.
    expect(screen.getAllByText("—")).toHaveLength(3);
    expect(screen.getByText("118 min")).toBeInTheDocument();

    rerender(
      <CatalogueResults
        items={sampleItems}
        list={false}
        queryKey="q1"
        page={1}
        hasMore={false}
        loading={false}
        onLoadMore={() => {}}
        onInspect={() => {}}
        onBulkWatchlist={async () => {}}
      />,
    );

    const viewport = screen.getByLabelText("Catalog titles");
    expect(viewport).toHaveClass("is-grid");
    expect(viewport).not.toHaveClass("has-selection");
    fireEvent.click(screen.getByRole("checkbox", { name: "Select Sparse Entry" }));
    expect(viewport).toHaveClass("has-selection");
  });

  it("shows Browse, Rules, and Settings tabs on a TMDB library without Collections", async () => {
    renderApp("/libraries/1");
    const nav = await screen.findByRole(
      "tablist",
      { name: "Library sections" },
      { timeout: 15000 },
    );
    expect(within(nav).getByText("Browse")).toBeInTheDocument();
    expect(within(nav).getByText("Rules")).toBeInTheDocument();
    expect(within(nav).getByText("Settings")).toBeInTheDocument();
    expect(within(nav).queryByText("Collections")).not.toBeInTheDocument();
  });

  it("picks TMDB from the New library source drawer to create a library with rules", async () => {
    const { fake } = renderApp("/libraries");
    fireEvent.click(await screen.findByRole("button", { name: "New library" }, { timeout: 15000 }));
    expect(await screen.findByText("Where do titles come from?")).toBeInTheDocument();
    fireEvent.click(await screen.findByRole("checkbox", { name: /TMDB Movies/ }));
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    fireEvent.click(screen.getByRole("button", { name: /Add rules/ }));
    fireEvent.change(await screen.findByLabelText("Library name"), {
      target: { value: "Weekend films" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create library" }));
    await settleUi(() => {
      const post = calls(fake, "POST", "/libraries").at(-1);
      expect(post?.body).toMatchObject({ name: "Weekend films" });
      expect((post?.body as { present?: unknown }).present).toBeUndefined();
    });
    expect(
      await screen.findByRole("heading", { name: "Weekend films", level: 1 }, { timeout: 15000 }),
    ).toBeInTheDocument();
  });

  it("creates a library from a collection through the From a collection step", async () => {
    const { fake } = renderApp("/libraries");
    fireEvent.click(await screen.findByRole("button", { name: "New library" }, { timeout: 15000 }));
    fireEvent.click(await screen.findByRole("button", { name: /From a collection/ }));
    await new Promise((r) => setTimeout(r, 300));

    expect(await screen.findByText("New library from a collection")).toBeInTheDocument();
    // Pick the first collection offered; its name seeds the library name.
    // Every collection card carries its title count; the first reads
    // "10 titles" in the mock. The wait absorbs the drawer's entry animation
    // before the card can take a click.
    const option = await screen.findByRole("button", { name: /Edge of Your Seat/ });
    fireEvent.click(option);
    // The card keeps the current selection.
    await settleUi(() => expect(option.getAttribute("aria-current")).toBe("true"));
    const input = screen.getByLabelText("Library name") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "List picks" } });
    await settleUi(() => expect(input.value).toBe("List picks"));
    const create = screen.getByRole("button", { name: "Create library" }) as HTMLButtonElement;

    await settleUi(() => expect(create.disabled).toBe(false));
    fireEvent.click(create);

    await settleUi(() => {
      const post = calls(fake, "POST", "/libraries").at(-1);
      expect(post?.body).toMatchObject({ name: "List picks", mediaType: "movie" });
      expect(typeof (post?.body as { collectionId?: unknown }).collectionId).toBe("number");
    });
  });

  it("links to Plugins when no content-kind plugin is installed, and ensures plugin setup when picked", async () => {
    renderAppWith("/libraries", (_method, path) => {
      if (path === "/plugins-setup") return { plugins: [] };
      return undefined;
    });
    fireEvent.click(await screen.findByRole("button", { name: "New library" }, { timeout: 15000 }));
    fireEvent.click(await screen.findByRole("button", { name: "Add catalogue source" }));
    const install = await within(await screen.findByRole("dialog")).findByRole("link", {
      name: /Plugin/,
    });
    expect(install).toHaveAttribute("href", "/plugins/marketplace");
  });

  it("browses a plugin library and re-matches from the item drawer", async () => {
    const { fake } = renderApp("/libraries/3");
    fireEvent.click(await screen.findByRole("button", { name: /Dune/ }, { timeout: 15000 }));
    expect(await screen.findByText("Matching")).toBeInTheDocument();
    expect(screen.getByText("Herbert")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Match again" }));
    await settleUi(() => {
      expect(calls(fake, "POST", "/libraries/3/item/").length).toBeGreaterThan(0);
    });
  });

  it("shows plugin-only sections and declared kind in a plugin library's settings", async () => {
    const { fake } = renderApp("/libraries/3/settings", {}, (server) => {
      const base = server.fetch;
      server.fetch = async (input, init) => {
        const res = await base(input, init);
        const url = new URL(String(input), "http://localhost");
        if (url.pathname !== "/ether/api/libraries/3" || (init?.method && init.method !== "GET")) {
          return res;
        }
        const lib = await res.json();
        return new Response(
          JSON.stringify({
            ...lib,
            present: {},
            kind: {
              ...lib.kind,
              present: {
                mediaBrowser: { collectionType: "books", itemTypes: { book: "Book" } },
                silo: { type: "Book" },
              },
            },
            presentations: {
              ...lib.presentations,
              effective: {},
              available: {
                ...lib.presentations.available,
                mediaBrowser: { collectionType: "books", itemTypes: { book: "Book" } },
                silo: { type: "Book" },
              },
              apps: [
                {
                  app: "vela",
                  available: true,
                  enabled: false,
                  defaultOn: true,
                  source: "inferred",
                  hintValue: "movies",
                },
                {
                  app: "plex",
                  available: false,
                  enabled: false,
                  defaultOn: false,
                  source: "unavailable",
                  reason: "Plex does not have a book library type.",
                },
                {
                  app: "mediaBrowser",
                  available: true,
                  enabled: false,
                  defaultOn: true,
                  source: "plugin",
                  hintValue: "books",
                },
                {
                  app: "stremio",
                  available: false,
                  enabled: false,
                  defaultOn: false,
                  source: "unavailable",
                  reason: "Stremio only supports movies, shows, and channels.",
                },
                {
                  app: "xtream",
                  available: false,
                  enabled: false,
                  defaultOn: false,
                  source: "unavailable",
                  reason: "Xtream only supports movies, series, and live TV.",
                },
                {
                  app: "silo",
                  available: true,
                  enabled: false,
                  defaultOn: true,
                  source: "plugin",
                  hintValue: "Book",
                },
              ],
            },
          }),
          {
            status: 200,
            headers: { "Content-Type": "application/json" },
          },
        );
      };
    });
    expect(await screen.findByText("Basics", {}, { timeout: 15000 })).toBeInTheDocument();
    expect(screen.queryByText("Keeping it up to date")).not.toBeInTheDocument();
    expect(screen.queryByText("New releases")).not.toBeInTheDocument();
    expect(screen.queryByText("Source: TMDB")).not.toBeInTheDocument();

    expect(
      screen.getByText("Each app shows Books as its own library. Pick which apps show it."),
    ).toBeInTheDocument();
    expect(screen.getByText("Kind")).toBeInTheDocument();
    expect(screen.getByText("Books from Demo books")).toBeInTheDocument();
    expect(screen.getAllByText("Shown as a book library (from plugin)")).toHaveLength(2);
    expect(screen.getByText("Shown as a movie library (inferred)")).toBeInTheDocument();
    expect(screen.getByText("Plex does not have a book library type.")).toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Show in Plex" })).toBeDisabled();
    expect(screen.getByRole("switch", { name: "Only in its own app" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("switch", { name: "Show in Jellyfin & Emby" }));
    fireEvent.click(screen.getByRole("switch", { name: "Show in Vela" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await settleUi(() => {
      const patch = calls(fake, "PATCH", "/libraries/3").at(-1);
      expect(patch?.body).toMatchObject({
        presentApps: {
          vela: true,
          mediaBrowser: true,
        },
      });
    });
  });

  it("derives Show in apps hints from the library kind without per-app pickers", async () => {
    const { fake } = renderApp("/libraries/7/settings");
    await screen.findByText("Show in apps", {}, { timeout: 15000 });
    expect(
      screen.getByText("Each app shows Anime Shows as its own library. Pick which apps show it."),
    ).toBeInTheDocument();
    expect(screen.getByText("Kind")).toBeInTheDocument();
    expect(screen.getByText("TV shows")).toBeInTheDocument();

    const switches = screen
      .getAllByRole("switch")
      .map((el) => el.getAttribute("aria-label"))
      .filter((label): label is string => Boolean(label?.startsWith("Show in ")));
    expect(switches).toEqual([
      "Show in Vela",
      "Show in Plex",
      "Show in Jellyfin & Emby",
      "Show in Stremio",
      "Show in Xtream",
      "Show in Silo",
    ]);
    expect(screen.getAllByText("Shown as a show library with seasons")).toHaveLength(6);
    expect(screen.queryByRole("button", { name: /How .* shows it/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("switch", { name: "Only in its own app" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("switch", { name: "Show in Vela" }));
    fireEvent.click(screen.getByRole("switch", { name: "Show in Plex" }));
    expect(screen.queryByRole("button", { name: /How .* shows it/ })).not.toBeInTheDocument();
    expect(screen.getByText("In apps: Vela, Plex")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await settleUi(() => {
      const patch = calls(fake, "PATCH", "/libraries/7").at(-1);
      expect(patch?.body).toMatchObject({
        presentApps: { vela: true, plex: true },
      });
    });
  });

  it("shows Releases summary, applicable rules, and adds a library-locked rule in place", async () => {
    const { fake } = renderApp("/libraries/1/settings");
    const section = await screen.findByTestId("library-releases", {}, { timeout: 15000 });
    await settleUi(() => {
      expect(within(section).getByTestId("library-releases-home")).toHaveTextContent("1080p");
      expect(within(section).getByText("Living room Apple TV")).toBeInTheDocument();
    });
    expect(screen.queryByText("Quality profile")).not.toBeInTheDocument();

    fireEvent.click(within(section).getByRole("button", { name: "Add a rule for Movies" }));
    const dialog = await screen.findByRole("dialog", {}, { timeout: 15000 });
    expect(within(dialog).getByText("Only Movies")).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("button", { name: "Profile" }));
    fireEvent.click(await screen.findByRole("option", { name: "4K" }, { timeout: 15000 }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Save rule" }));

    await settleUi(() => {
      const put = calls(fake, "PUT", "/config/quality").at(-1);
      expect(put?.body).toMatchObject({
        rules: expect.arrayContaining([expect.objectContaining({ libraries: [1], profile: "4K" })]),
      });
      expect(within(section).getByTestId("library-releases-home")).toHaveTextContent("4K");
      expect(within(section).getByTestId("library-releases-home")).toHaveTextContent("Rule 2");
    });
  });

  it("hides Releases on a plugin library whose items aren't video (#884)", async () => {
    renderApp("/libraries/3/settings", {}, (fake) => {
      fake.state.config.quality.rules = [
        {
          id: "r_mobile_cap",
          devices: [{ group: "mobile", label: "Phones & tablets" }],
          profile: "",
          maxHeight: 1080,
        },
      ];
    });
    // The Books plugin library is not video: its settings load, without Releases.
    await screen.findByText("Titles come from", {}, { timeout: 15000 });
    expect(screen.queryByTestId("library-releases")).not.toBeInTheDocument();
  });
});

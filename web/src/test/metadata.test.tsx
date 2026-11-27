import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { resetRegistryCache } from "../lib/extend";
import { calls } from "./harness";
import { renderAppWith } from "./routes";

const WAIT = { timeout: 4000 };

async function choose(label: string, option: string | RegExp) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }, WAIT));
}

const ARTWORK_TITLES = [
  {
    key: "1000000000000001",
    title: "Catalogue Movie",
    type: "movie",
    poster: "/ether/api/image?path=/movie.jpg&w=342",
  },
  {
    key: "2000000000000002",
    title: "Catalogue Show",
    type: "show",
    poster: "/ether/api/image?path=/show.jpg&w=342",
  },
  {
    key: "1000000000000003",
    title: "Another Movie",
    type: "movie",
    poster: "/ether/api/image?path=/other.jpg&w=342",
  },
];
const SHOW = "2000000000000010";

describe("Metadata episode orders", () => {
  it("selects a show, persists the owner order, previews canonical identities and resets viewer overrides", async () => {
    // Sam Rivera (viewer 2) follows the default until an override is saved.
    const overrides = new Map<number, { groupId: string; specials: string }>();
    const { fake } = renderAppWith("/metadata/episodes", (method, path, body) => {
      if (path === "/catalog/search")
        return {
          items: [
            {
              key: SHOW,
              title: "Fixture Show",
              type: "show",
              episodeOrder: {
                effective: { source: "tmdb", kind: "aired" },
                libraryDefault: { source: "tmdb", kind: "aired" },
                libraryName: "TV Shows",
              },
            },
          ],
        };
      if (!path.startsWith(`/metadata/shows/${SHOW}`)) return undefined;
      if (path.endsWith("/groups"))
        return [{ id: "dvd", name: "DVD order", type: 3, episode_count: 2 }];
      if (path.endsWith("/preview"))
        return [
          {
            key: "4000000100010002",
            title: "Second canonical episode",
            canonicalSeason: 1,
            canonicalEpisode: 2,
            group: "Disc 1",
            index: 1,
          },
        ];
      const id = Number(path.split("/").at(-1));
      if (method === "PUT") {
        overrides.set(id, body as { groupId: string; specials: string });
        return body;
      }
      if (method === "DELETE") overrides.delete(id);
      return overrides.get(id) ?? { groupId: "", specials: "last", inherited: id !== 0 };
    });

    fireEvent.change(await screen.findByLabelText("Find a show", {}, WAIT), {
      target: { value: "Fixture" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    fireEvent.click(await screen.findByRole("button", { name: "Fixture Show" }, WAIT));

    const drawer = await screen.findByRole("dialog", { name: "Fixture Show" });
    const defaults = within(drawer).getByRole("region", { name: "Default order" });
    fireEvent.change(await within(defaults).findByLabelText("Episode order"), {
      target: { value: "dvd" },
    });
    fireEvent.click(within(defaults).getByRole("button", { name: "Save episode order" }));
    expect(await screen.findByText("Second canonical episode")).toBeInTheDocument();
    await waitFor(() =>
      expect(
        calls(fake, "PUT", `/metadata/shows/${SHOW}/users/0`).some(
          (c) => (c.body as { groupId: string }).groupId === "dvd",
        ),
      ).toBe(true),
    );

    // Viewers are listed with their override state, not picked from a dropdown.
    const overridesList = within(drawer).getByRole("region", { name: "Viewer overrides" });
    const sam = (await within(overridesList).findByText("Sam Rivera")).closest("li")!;
    expect(await within(sam).findByText("Uses default")).toBeInTheDocument();
    fireEvent.click(within(sam).getByRole("button", { name: "Edit order for Sam Rivera" }));
    const viewer = await screen.findByRole("dialog", { name: "Fixture Show for Sam Rivera" });
    expect(await within(viewer).findByText(/uses the default order/)).toBeInTheDocument();
    expect(within(viewer).queryByRole("button", { name: "Use default" })).toBeNull();

    fireEvent.change(within(viewer).getByLabelText("Episode order"), { target: { value: "dvd" } });
    fireEvent.click(within(viewer).getByRole("button", { name: "Save episode order" }));
    await waitFor(() =>
      expect(calls(fake, "PUT", `/metadata/shows/${SHOW}/users/2`)).toHaveLength(1),
    );
    await waitFor(() =>
      expect(within(sam).getByText("Custom for this viewer")).toBeInTheDocument(),
    );

    fireEvent.click(await within(viewer).findByRole("button", { name: "Use default" }));
    await waitFor(() =>
      expect(calls(fake, "DELETE", `/metadata/shows/${SHOW}/users/2`)).toHaveLength(1),
    );
    await waitFor(() => expect(within(sam).getByText("Uses default")).toBeInTheDocument());
  });
});

describe("Metadata artwork studio", () => {
  beforeEach(() => resetRegistryCache());

  it("previews a draft without saving, compares originals, discards and persists the complete design", async () => {
    const { fake } = renderAppWith("/metadata", (_method, path) =>
      path === "/collections/preview" ? { items: [ARTWORK_TITLES[0]!], total: 1 } : undefined,
    );
    const style = await screen.findByRole("button", { name: "Badge finish" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: /Spotlight/ }));
    expect(style).toHaveTextContent("Solid");
    expect(screen.getByRole("button", { name: "Placement" })).toHaveTextContent("Top");
    expect(calls(fake, "PUT", "/discovery/overlays/settings")).toHaveLength(0);
    await waitFor(() => {
      const img = screen.getByRole("img", { name: "Catalogue Movie Your design" });
      const settings = JSON.parse(
        new URL(img.getAttribute("src")!, "http://localhost").searchParams.get("settings")!,
      );
      expect(settings).toMatchObject({ style: "solid", position: "top", accent: "#a7c4b5" });
    });
    fireEvent.click(screen.getByRole("button", { name: "Compare" }));
    const original = await screen.findByRole("img", {
      name: "Catalogue Movie Original",
    });
    expect(
      JSON.parse(
        new URL(original.getAttribute("src")!, "http://localhost").searchParams.get("settings")!,
      ),
    ).toMatchObject({ enabled: false });
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(style).toHaveTextContent("Glass"));
    await choose("Badge size", "Large");
    fireEvent.click(screen.getByRole("button", { name: "Lavender" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(calls(fake, "PUT", "/discovery/overlays/settings").at(-1)?.body).toMatchObject({
        size: "large",
        accent: "#c0a6d8",
        style: "glass",
        position: "bottom",
      }),
    );
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Unsaved changes" })).not.toBeInTheDocument(),
    );
  });

  it("places section tabs in the header bar and picks preview languages by name", async () => {
    renderAppWith("/metadata/artwork", (_method, path) =>
      path === "/collections/preview" ? { items: [ARTWORK_TITLES[0]!], total: 1 } : undefined,
    );
    const langButton = await screen.findByRole("button", { name: "Preview language" }, WAIT);
    const controls = screen.getByRole("complementary", { name: "Metadata controls" });
    expect(within(controls).queryByRole("tablist", { name: "Metadata sections" })).toBeNull();
    const tabs = screen.getByRole("tablist", { name: "Metadata sections" });
    expect(within(tabs).getByRole("tab", { name: "Artwork" })).toHaveAttribute(
      "aria-selected",
      "true",
    );

    expect(langButton).toHaveTextContent("English");
    await choose("Preview language", "French");
    expect(langButton).toHaveTextContent("French");
    await waitFor(() => {
      const img = screen.getByRole("img", { name: "Catalogue Movie Your design" });
      expect(new URL(img.getAttribute("src")!, "http://localhost").searchParams.get("lang")).toBe(
        "fr",
      );
    });
  });

  it("loads real catalogue posters, refreshes to a different title and uses real art across library and compare views", async () => {
    const { fake } = renderAppWith("/metadata/artwork", (_method, path) =>
      path === "/collections/preview"
        ? {
            items: [...ARTWORK_TITLES, { key: "missing", title: "No poster", type: "movie" }],
            total: 4,
          }
        : undefined,
    );
    const canvas = await screen.findByRole("main", { name: "Metadata preview" }, WAIT);
    const initial = await within(canvas).findByRole("img", { name: /Your design/ }, WAIT);
    const initialURL = initial.getAttribute("src")!;
    expect(initialURL).toContain("/overlays/preview/");
    fireEvent.click(screen.getByRole("button", { name: /Spotlight/ }));
    fireEvent.click(screen.getByRole("button", { name: "Try another artwork" }));
    await waitFor(() =>
      expect(
        within(canvas)
          .getByRole("img", { name: /Your design/ })
          .getAttribute("src")!
          .split("?")[0],
      ).not.toBe(initialURL.split("?")[0]),
    );
    expect(screen.getByRole("button", { name: "Badge finish" })).toHaveTextContent("Solid");
    expect(calls(fake, "PUT", "/discovery/overlays/settings")).toHaveLength(0);
    expect(calls(fake, "POST", "/collections/preview")).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Library" }));
    const images = within(canvas).getAllByRole("img");
    expect(images).toHaveLength(3);
    expect(new Set(images.map((img) => img.getAttribute("src")!.split("?")[0])).size).toBe(3);
    for (const img of images) expect(img.getAttribute("src")).toContain("/overlays/preview/");
    fireEvent.click(screen.getByRole("button", { name: "Compare" }));
    expect(within(canvas).getAllByRole("img")).toHaveLength(2);
    expect(
      screen.queryByText(/fictional|sample poster|illustrative scores/i),
    ).not.toBeInTheDocument();
  });

  it("shows a truthful empty state, supports reloading and disables random refresh for one title", async () => {
    let items: typeof ARTWORK_TITLES = [];
    renderAppWith("/metadata/artwork", (_method, path) =>
      path === "/collections/preview" ? { items, total: items.length } : undefined,
    );
    expect(await screen.findByText("No catalogue artwork yet", {}, WAIT)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try another artwork" })).toBeDisabled();
    items = [ARTWORK_TITLES[0]!];
    fireEvent.click(screen.getByRole("button", { name: "Reload catalogue artwork" }));
    expect(
      await screen.findByRole("img", { name: "Catalogue Movie Your design" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try another artwork" })).toBeDisabled();
  });

  it("retries a catalogue failure without losing design edits", async () => {
    let failed = true;
    renderAppWith("/metadata/artwork", (_method, path) => {
      if (path === "/collections/preview")
        return failed
          ? new Response("Catalogue unavailable", { status: 503 })
          : { items: [ARTWORK_TITLES[0]!], total: 1 };
      return undefined;
    });
    const canvas = await screen.findByRole("main", { name: "Metadata preview" }, WAIT);
    const retry = await within(canvas).findByRole("button", { name: "Try again" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: /Spotlight/ }));
    failed = false;
    fireEvent.click(retry);
    expect(
      await screen.findByRole("img", { name: "Catalogue Movie Your design" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Badge finish" })).toHaveTextContent("Solid");
  });

  it("explains unavailable badges and keeps preset detail choices in the draft", async () => {
    const { fake } = renderAppWith("/metadata/artwork", (_method, path, _body, query) => {
      if (path === "/collections/preview") return { items: [ARTWORK_TITLES[0]], total: 1 };
      if (path.startsWith("/discovery/overlays/explain/")) {
        const settings = JSON.parse(query.get("settings")!);
        return {
          labels: ["TMDB 8.3/10"],
          layers: [
            {
              key: "ratings",
              state: settings.ratings ? "ready" : "off",
              labels: ["TMDB 8.3/10"],
              reason: "Catalogue score",
            },
            {
              key: "quality",
              state: settings.quality ? "unavailable" : "off",
              labels: [],
              reason: "No verified playback yet",
            },
            {
              key: "details",
              state: settings.details ? "ready" : "off",
              labels: ["2024 · 120m"],
              reason: "Catalogue details",
            },
          ],
        };
      }
      return undefined;
    });
    fireEvent.click(await screen.findByRole("button", { name: /Cinema/ }, { timeout: 4000 }));
    expect(await screen.findByText("TMDB 8.3/10")).toBeInTheDocument();
    expect(await screen.findByText("No verified playback yet")).toBeInTheDocument();
    expect(await screen.findByText("2024 · 120m")).toBeInTheDocument();
    expect(screen.getByLabelText("Title details")).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: /Essential/ }));
    expect(screen.getByLabelText("Title details")).not.toBeChecked();
    await waitFor(() => expect(screen.queryByText("2024 · 120m")).not.toBeInTheDocument());
    expect(calls(fake, "PUT", "/discovery/overlays/settings")).toHaveLength(0);
  });

  it("shows Rating style only when Rating badges is on and saves One combined score", async () => {
    const { fake } = renderAppWith("/metadata/artwork", (_method, path) =>
      path === "/collections/preview" ? { items: [ARTWORK_TITLES[0]!], total: 1 } : undefined,
    );
    const ratingStyle = await screen.findByRole("button", { name: "Rating style" }, WAIT);
    expect(ratingStyle).toHaveTextContent("Each source");
    expect(await screen.findByText("TMDB 8.6/10", {}, WAIT)).toBeInTheDocument();

    await choose("Rating style", "One combined score");
    expect(ratingStyle).toHaveTextContent("One combined score");
    expect(await screen.findByText("8.6", {}, WAIT)).toBeInTheDocument();
    await waitFor(() => {
      const img = screen.getByRole("img", { name: "Catalogue Movie Your design" });
      const settings = JSON.parse(
        new URL(img.getAttribute("src")!, "http://localhost").searchParams.get("settings")!,
      );
      expect(settings).toMatchObject({ ratings: true, ratingStyle: "combined" });
    });

    const ratingsSwitch = screen.getByRole("switch", { name: "Rating badges" });
    fireEvent.click(ratingsSwitch);
    expect(screen.queryByRole("button", { name: "Rating style" })).toBeNull();

    fireEvent.click(ratingsSwitch);
    expect(screen.getByRole("button", { name: "Rating style" })).toHaveTextContent(
      "One combined score",
    );

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(calls(fake, "PUT", "/discovery/overlays/settings").at(-1)?.body).toMatchObject({
        ratings: true,
        ratingStyle: "combined",
      }),
    );
  });

  it("clears the previous viewer's badge evidence when the next viewer lookup fails", async () => {
    renderAppWith("/metadata/artwork", (_method, path, _body, query) => {
      if (path === "/collections/preview") return { items: [ARTWORK_TITLES[0]], total: 1 };
      if (path.startsWith("/discovery/overlays/explain/")) {
        if (query.get("userId") === "2") return new Response("Unavailable", { status: 503 });
        return {
          labels: ["PLAYED 4K"],
          layers: [
            {
              key: "quality",
              state: "ready",
              labels: ["PLAYED 4K"],
              reason: "Owner's verified playback",
            },
          ],
        };
      }
      return undefined;
    });
    expect(await screen.findByText("PLAYED 4K", {}, WAIT)).toBeInTheDocument();
    await choose("Preview as", "Sam Rivera");
    expect(screen.queryByText("PLAYED 4K")).not.toBeInTheDocument();
    await screen.findByRole("button", { name: "Retry title data" });
    expect(screen.queryByText("PLAYED 4K")).not.toBeInTheDocument();
  });

  it("keeps an unsuccessful save editable and supports retry", async () => {
    let failed = true;
    const { fake } = renderAppWith("/metadata/artwork", (method, path) => {
      if (method === "PUT" && path === "/discovery/overlays/settings" && failed)
        return new Response("Storage unavailable", { status: 500 });
      return undefined;
    });
    await screen.findByRole("button", { name: "Placement" }, WAIT);
    await choose("Placement", "Top");
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await screen.findByRole("alert");
    expect(screen.getByRole("button", { name: "Placement" })).toHaveTextContent("Top");
    failed = false;
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/discovery/overlays/settings")).toHaveLength(2));
    await waitFor(() =>
      expect(screen.queryByRole("region", { name: "Unsaved changes" })).not.toBeInTheDocument(),
    );
  });

  it("hides From plugins when no plugin declares poster badges and toggles disabledBadgeSets when present", async () => {
    let badgeDescriptors: unknown[] = [];
    const { unmount } = renderAppWith("/metadata/artwork", (_method, path, _body, query) => {
      if (path === "/collections/preview") return { items: [ARTWORK_TITLES[0]!], total: 1 };
      if (path === "/extend/registry" && query.get("kinds") === "posterBadge") {
        return { version: 1, descriptors: badgeDescriptors };
      }
      return undefined;
    });
    const layers = await screen.findByRole("region", { name: "Poster layers" }, WAIT);
    expect(within(layers).queryByRole("group", { name: "From plugins" })).toBeNull();
    expect(screen.queryByText("From plugins")).not.toBeInTheDocument();
    unmount();
    resetRegistryCache();

    badgeDescriptors = [
      {
        kind: "posterBadge",
        id: "plugin:org.example.anime/dub_languages",
        owner: "org.example.anime",
        label: { resolved: "Dub languages" },
        hint: { resolved: "Spoken dub languages available for anime." },
        group: "Anime Hub",
        meta: {
          key: "dub_languages",
          layerKey: "plugin:org.example.anime:dub_languages",
          pluginName: "Anime Hub",
        },
      },
      {
        kind: "posterBadge",
        id: "plugin:org.example.editions/editions",
        owner: "org.example.editions",
        label: { resolved: "Editions" },
        group: "Cinema Cuts",
        meta: {
          key: "editions",
          layerKey: "plugin:org.example.editions:editions",
          pluginName: "Cinema Cuts",
        },
      },
    ];
    const { fake } = renderAppWith("/metadata/artwork", (_method, path, _body, query) => {
      if (path === "/collections/preview") return { items: [ARTWORK_TITLES[0]!], total: 1 };
      if (path === "/extend/registry" && query.get("kinds") === "posterBadge") {
        return { version: 2, descriptors: badgeDescriptors };
      }
      return undefined;
    });

    const pluginGroup = await screen.findByRole("group", { name: "From plugins" }, WAIT);
    const dubSwitch = within(pluginGroup).getByRole("switch", {
      name: "Dub languages · Anime Hub",
    });
    const editionsSwitch = within(pluginGroup).getByRole("switch", {
      name: "Editions · Cinema Cuts",
    });
    expect(dubSwitch).toBeChecked();
    expect(editionsSwitch).toBeChecked();
    expect(
      within(pluginGroup).getByText("Spoken dub languages available for anime."),
    ).toBeInTheDocument();

    fireEvent.click(dubSwitch);
    expect(dubSwitch).not.toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(calls(fake, "PUT", "/discovery/overlays/settings").at(-1)?.body).toMatchObject({
        disabledBadgeSets: ["org.example.anime:dub_languages"],
      }),
    );

    fireEvent.click(dubSwitch);
    expect(dubSwitch).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(calls(fake, "PUT", "/discovery/overlays/settings").at(-1)?.body).toMatchObject({
        disabledBadgeSets: [],
      }),
    );
  });

  it("keeps the Plex metadata switch off the Artwork page", async () => {
    renderAppWith("/metadata/artwork", () => undefined);
    await screen.findByLabelText("Ether posters", {}, WAIT);
    expect(screen.queryByLabelText("Use Plex's metadata for Plex apps")).toBeNull();
  });

  it("saves the Plex metadata switch from Apps › Plex › Account", async () => {
    const { fake } = renderAppWith("/apps/plex/account", () => undefined);
    const toggle = await screen.findByLabelText("Use Plex's metadata for Plex apps", {}, WAIT);
    // On by default: a config that never set it shows the switch on.
    expect(toggle).toBeChecked();
    fireEvent.click(toggle);
    await waitFor(() => expect(calls(fake, "PUT", "/config/plex")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/plex")[0]!.body).toEqual({ usePlexMetadata: false });
  });

  it("opens Recommendations at /recommendations", async () => {
    renderAppWith("/recommendations", () => undefined);
    expect(
      await screen.findByRole("heading", { name: "Recommendations" }, WAIT),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Recommendations" })).toHaveAttribute(
      "href",
      "/recommendations",
    );
  });
});

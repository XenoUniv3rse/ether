import { fireEvent, screen, waitFor, within, cleanup } from "@testing-library/react";
import { afterEach, it, expect } from "vitest";
import { renderApp } from "./harness";
afterEach(cleanup);
it.each([
  "Trakt",
  "Simkl",
  "Language model",
  "OpenSubtitles",
  "SubDL",
  "SubSource",
  "TheIntroDB",
  "OMDb and MDBList",
  "PostersPlus",
  "XRDB",
  "BetterPosters",
  "RPDB",
  "Custom posters",
])("%s opens setup before activation", async (name) => {
  renderApp("/services/overview", {}, (f) => {
    f.state.config.posters = undefined;
    f.state.config.trakt.enabled = false;
    f.state.config.simkl = { enabled: false, clientId: "", clientSecret: "" };
    f.state.config.llm.enabled = false;
    f.state.config.openSubtitles.enabled = false;
    f.state.config.subdl.enabled = false;
    f.state.config.subsource.enabled = false;
    f.state.config.markers = { theIntroDb: false, apiKey: "" };
  });
  const toggle =
    name === "TMDB"
      ? await screen.findByRole("button", { name: "Set up TMDB" })
      : await screen.findByRole("switch", { name: name + " enabled" });
  await waitFor(() => expect(toggle).toBeEnabled());
  fireEvent.click(toggle);
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  expect(within(dialog).getByRole("button", { name: "Save and enable" })).toBeInTheDocument();
  expect(
    dialog.querySelectorAll('input,select,button[aria-haspopup="listbox"]').length,
  ).toBeGreaterThan(0);
  const fields = Array.from(dialog.querySelectorAll("label")).map((el) => el.textContent);
  const routes: Record<string, string> = {
    Trakt: "trakt",
    Simkl: "simkl",
    "Language model": "llm",
    OpenSubtitles: "opensubtitles",
    SubDL: "subdl",
    SubSource: "subsource",
    TheIntroDB: "introdb",
    TMDB: "tmdb",
    "OMDb and MDBList": "ratings",
    PostersPlus: "postersplus",
    XRDB: "xrdb",
    BetterPosters: "betterposters",
    RPDB: "rpdb",
    "Custom posters": "custom",
  };
  cleanup();
  renderApp("/services/" + routes[name], {}, (f) => {
    f.state.config.trakt.enabled = true;
    f.state.config.simkl = { enabled: true, clientId: "", clientSecret: "" };
    f.state.config.llm.enabled = true;
    f.state.config.openSubtitles.enabled = true;
    f.state.config.subdl = { enabled: true, apiKey: "" };
    f.state.config.subsource = { enabled: true, apiKey: "" };
    f.state.config.markers = { theIntroDb: true, apiKey: "" };
    f.state.config.posters = {
      version: 1,
      enabled: false,
      source: "tmdb",
      baseUrl: "",
      apiKey: "",
      urlTemplate: "",
      accounts: ["postersplus", "xrdb", "betterposters", "rpdb", "custom"].includes(routes[name]!)
        ? {
            [routes[name]!]: {
              enabled: true,
              source: routes[name]!,
              baseUrl: "",
              apiKey: "",
              urlTemplate: "",
            },
          }
        : {},
    };
  });
  await waitFor(() =>
    expect(
      screen.getByRole("tab", {
        name: name === "OMDb and MDBList" ? "Ratings" : name,
      }),
    ).toHaveAttribute("aria-selected", "true"),
  );
  await waitFor(() => expect(screen.getAllByRole("heading", { name })).toHaveLength(1));
  if (name === "TMDB")
    expect(screen.queryByRole("switch", { name: "TMDB account enabled" })).toBeNull();
  else
    expect(
      await screen.findByRole("switch", { name: name + " account enabled" }),
    ).toBeInTheDocument();
  for (const field of fields)
    if (field) expect((await screen.findAllByText(field)).length).toBeGreaterThan(0);
  if (name === "Language model")
    expect(screen.getByRole("button", { name: "Provider" })).toBeInTheDocument();
});

it("turns TheIntroDB lookups on from its card switch", async () => {
  const { fake } = renderApp("/services/overview", {}, (f) => {
    f.state.config.markers = { theIntroDb: false, apiKey: "" };
  });
  fireEvent.click(await screen.findByRole("switch", { name: "TheIntroDB enabled" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("Optional TheIntroDB key"), {
    target: { value: "intro-key" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Save and enable" }));
  await waitFor(() => expect(fake.state.config.markers?.theIntroDb).toBe(true));
  expect(fake.state.config.markers?.apiKey).toBe("intro-key");
});

it("keeps configured TMDB available and opens its full settings tab", async () => {
  const { fake } = renderApp("/services/overview", {}, (f) => {
    f.state.config.catalog.tmdbKey = "[configured]";
  });
  const manage = await screen.findByRole("link", { name: "Manage TMDB" });
  // Catalogue cards use the shared card: a configured TMDB shows its switch on.
  expect(await screen.findByRole("switch", { name: "TMDB enabled" })).toBeChecked();
  fireEvent.click(manage);
  const keyField = await screen.findByLabelText("TMDB API key");
  expect(screen.getByRole("radio", { name: /On digital release/ })).toBeVisible();
  expect(keyField).toHaveValue("[configured]");
  expect(screen.getByRole("button", { name: "Test" })).toBeInTheDocument();
  expect(fake.state.config.catalog.tmdbKey).toBe("[configured]");
});

it("enables and disables MDBList and Ratings from /services/overview", async () => {
  const { fake } = renderApp("/services/overview", {}, (f) => {
    f.state.config.mdblist = { enabled: false };
  });

  // Enable MDBList via overview card switch -> setup dialog -> Save and enable -> PUT /ether/api/config/mdblist, then toggle off and back on.
  const mdbSwitch = await screen.findByRole("switch", { name: "MDBList enabled" });
  await waitFor(() => expect(mdbSwitch).toBeEnabled());
  fireEvent.click(mdbSwitch);
  const mdbDialog = await screen.findByRole("dialog");
  fireEvent.click(within(mdbDialog).getByRole("button", { name: "Save and enable" }));
  await waitFor(() => expect(fake.state.config.mdblist?.enabled).toBe(true));
  fireEvent.click(screen.getByRole("switch", { name: "MDBList enabled" }));
  await waitFor(() => expect(fake.state.config.mdblist?.enabled).toBe(false));

  // Enable OMDb and MDBList (Ratings) via setup dialog -> Save and enable, then toggle off cleanly.
  const ratingsSwitch = await screen.findByRole("switch", { name: "OMDb and MDBList enabled" });
  await waitFor(() => expect(ratingsSwitch).toBeEnabled());
  fireEvent.click(ratingsSwitch);
  const ratingsDialog = await screen.findByRole("dialog");
  fireEvent.change(within(ratingsDialog).getByLabelText(/API key/i), {
    target: { value: "ratings-key" },
  });
  fireEvent.click(within(ratingsDialog).getByRole("button", { name: "Save and enable" }));
  await waitFor(() =>
    expect(screen.getByRole("switch", { name: "OMDb and MDBList enabled" })).toBeChecked(),
  );
  fireEvent.click(screen.getByRole("switch", { name: "OMDb and MDBList enabled" }));
  await waitFor(() =>
    expect(screen.getByRole("switch", { name: "OMDb and MDBList enabled" })).not.toBeChecked(),
  );
});

it("opens MDBList setup drawer when routed to /services/mdblist while disabled", async () => {
  renderApp("/services/mdblist", {}, (f) => {
    f.state.config.mdblist = { enabled: false };
  });
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByRole("button", { name: "Save and enable" })).toBeInTheDocument();
});

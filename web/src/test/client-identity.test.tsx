import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Indexer } from "../lib/types";
import { calls, renderApp } from "./harness";

const WAIT = { timeout: 4000 };

/** SearchSelect: the trigger is a button named by its aria label. */
async function choose(label: string, option: string) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  fireEvent.click(await screen.findByRole("option", { name: option }, WAIT));
}

describe("Sources › Indexers client identity", () => {
  it("saves the global policy for each purpose", async () => {
    const { fake } = renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    await choose("TV searches", "Sonarr");
    await choose("Movie downloads", "NZBGet");
    const bar = screen.getByRole("region", { name: "Unsaved changes" });
    fireEvent.click(within(bar).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/clientIdentity")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/clientIdentity")[0]!.body).toMatchObject({
      tvSearch: "sonarr",
      movieSearch: "",
      tvGrab: "",
      movieGrab: "nzbget",
    });
  });

  it("overrides the identity on one indexer", async () => {
    const { fake } = renderApp("/sources/indexers");
    fireEvent.click(await screen.findByRole("button", { name: /Add indexer/ }, WAIT));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Newznab or Torznab URL…" }));
    const drawer = await screen.findByRole("dialog", { name: "New indexer" }, WAIT);
    fireEvent.change(within(drawer).getByLabelText("Name"), { target: { value: "Idx" } });
    fireEvent.change(within(drawer).getByLabelText("URL"), {
      target: { value: "https://idx.example" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Identity" }));
    await screen.findByRole("option", { name: "Sonarr" }, WAIT);
    // The override governs searches too, so downloaders are not offered.
    expect(screen.getAllByRole("option").map((o) => o.textContent.trim())).toEqual([
      "Inherit",
      "Ether",
      "Sonarr",
      "Radarr",
      "Prowlarr",
      "NZBHydra2",
    ]);
    fireEvent.click(screen.getByRole("option", { name: "Sonarr" }));
    fireEvent.click(within(drawer).getByRole("button", { name: "Save indexer" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/indexers")).toHaveLength(1), WAIT);
    const body = calls(fake, "PUT", "/config/indexers")[0]!.body as Indexer[];
    expect(body.at(-1)).toMatchObject({ name: "Idx", clientIdentity: "sonarr" });
  });

  it("keeps downloaders out of the search lists", async () => {
    renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "TV searches" }));
    await screen.findByRole("option", { name: "Sonarr" }, WAIT);
    expect(screen.getAllByRole("option").map((o) => o.textContent.trim())).toEqual([
      "Ether",
      "Sonarr",
      "Radarr",
      "Prowlarr",
      "NZBHydra2",
      "Custom",
    ]);
    fireEvent.click(screen.getByRole("button", { name: "TV searches" }));
    fireEvent.click(screen.getByRole("button", { name: "Movie downloads" }));
    await screen.findByRole("option", { name: "NZBGet" }, WAIT);
    const grab = screen.getAllByRole("option").map((o) => o.textContent.trim());
    expect(grab).toContain("SABnzbd");
    expect(grab).toContain("NZBGet");
  });

  it("refuses two rows with the same header name", async () => {
    const { fake } = renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "More options: Client identity" }));
    fireEvent.click(screen.getByRole("button", { name: "Add header" }));
    fireEvent.click(screen.getByRole("button", { name: "Add header" }));
    fireEvent.change(screen.getAllByLabelText("Header name")[0]!, {
      target: { value: "X-Test" },
    });
    fireEvent.change(screen.getAllByLabelText("Header name")[1]!, {
      target: { value: "X-Test" },
    });
    // The duplicate keystroke is refused rather than silently collapsing.
    expect((screen.getAllByLabelText("Header name")[1] as HTMLInputElement).value).toBe("");
    fireEvent.change(screen.getAllByLabelText("Value")[0]!, { target: { value: "one" } });
    const bar = screen.getByRole("region", { name: "Unsaved changes" });
    fireEvent.click(within(bar).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/clientIdentity")).toHaveLength(1), WAIT);
    // The unfilled row is a placeholder, not a header.
    expect(calls(fake, "PUT", "/config/clientIdentity")[0]!.body).toMatchObject({
      headers: { "X-Test": "one" },
    });
  });

  it("renders custom headers as secret rows", async () => {
    const { fake } = renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "More options: Client identity" }));
    fireEvent.click(screen.getByRole("button", { name: "Add header" }));
    fireEvent.change(screen.getByLabelText("Header name"), { target: { value: "X-Api-Client" } });
    fireEvent.change(screen.getByLabelText("Value"), { target: { value: "s3cret" } });
    const bar = screen.getByRole("region", { name: "Unsaved changes" });
    fireEvent.click(within(bar).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(calls(fake, "PUT", "/config/clientIdentity")).toHaveLength(1), WAIT);
    expect(calls(fake, "PUT", "/config/clientIdentity")[0]!.body).toMatchObject({
      headers: { "X-Api-Client": "s3cret" },
    });
  });

  it("shows the user agent each selector sends", async () => {
    renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    // Four selectors default to Ether, whose literal agent comes from the server.
    expect(screen.getAllByText("Ether/1.0")).toHaveLength(4);
    await choose("TV searches", "Sonarr");
    expect(screen.getByText("Sonarr/4.0.20.3014 (ubuntu 26.04)")).toBeInTheDocument();
    // Custom falls back to Ether until the owner supplies an agent.
    await choose("TV searches", "Custom");
    expect(screen.getAllByText("Ether/1.0")).toHaveLength(4);
    fireEvent.click(screen.getByRole("button", { name: "More options: Client identity" }));
    fireEvent.change(screen.getByLabelText("Custom user agent"), {
      target: { value: "MyClient/1.0" },
    });
    expect(screen.getByText("MyClient/1.0")).toBeInTheDocument();
  });

  it("counts inheriting indexers and names the backend exceptions", async () => {
    renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    // The mock has three Newznab/Torznab indexers and one Stremio addon.
    expect(screen.getByText("All 3 indexers inherit this policy")).toBeInTheDocument();
    expect(
      screen.getByText("Built-in sites and Stremio addons use their own identity."),
    ).toBeInTheDocument();
  });

  it("counts overrides and follows an indexer edit", async () => {
    renderApp("/sources/indexers", {}, (fake) => {
      fake.state.config.indexers!.find((ix) => ix.id === "i1")!.clientIdentity = "sonarr";
    });
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    expect(
      screen.getByText("2 of 3 indexers inherit this policy; 1 has its own identity"),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Edit Indexer One" }));
    const drawer = await screen.findByRole("dialog", { name: "Indexer One" }, WAIT);
    fireEvent.click(within(drawer).getByRole("button", { name: "Identity" }));
    fireEvent.click(await screen.findByRole("option", { name: "Inherit" }, WAIT));
    fireEvent.click(within(drawer).getByRole("button", { name: "Save indexer" }));
    await waitFor(
      () => expect(screen.getByText("All 3 indexers inherit this policy")).toBeInTheDocument(),
      WAIT,
    );
  });

  it("omits the count when no indexer can inherit", async () => {
    renderApp("/sources/indexers", {}, (fake) => {
      fake.state.config.indexers = fake.state.config.indexers!.filter(
        (ix) => ix.kind === "stremio",
      );
    });
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    expect(screen.queryByText(/inherit this policy/)).toBeNull();
  });

  it("labels the disclosure Less options while it is open", async () => {
    renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    const more = screen.getByRole("button", { name: "More options: Client identity" });
    fireEvent.click(more);
    expect(more).toHaveTextContent("Less options");
    fireEvent.click(more);
    expect(more).toHaveTextContent("More options");
  });

  it("shows both header fields and the fallback agent", async () => {
    renderApp("/sources/indexers");
    await screen.findByRole("heading", { name: "Client identity" }, WAIT);
    fireEvent.click(screen.getByRole("button", { name: "More options: Client identity" }));
    expect(screen.getByLabelText("Custom user agent")).toHaveAttribute("placeholder", "Ether/1.0");
    fireEvent.click(screen.getByRole("button", { name: "Add header" }));
    expect(screen.getByLabelText("Header name")).toBeInTheDocument();
    expect(screen.getByLabelText("Value")).toBeInTheDocument();
  });
});

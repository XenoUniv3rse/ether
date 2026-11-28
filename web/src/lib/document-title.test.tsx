import { fireEvent, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { fixturePluginPage } from "./console-descriptors.test";
import type { PageMeta } from "./console-descriptors";
import { formatDocumentTitle, resolveRouteTitle } from "./document-title";
import { resetRegistryCache, type Descriptor } from "./extend";
import { descriptors } from "../mock/extend/descriptors/console";
import { renderApp, settleUi } from "../test/harness";

describe("formatDocumentTitle", () => {
  it("joins segments with middots and appends the server brand", () => {
    expect(formatDocumentTitle(["Libraries", "Movies", "Rules"], "Living Room Ether")).toBe(
      "Libraries · Movies · Rules — Living Room Ether",
    );
    expect(formatDocumentTitle(["Overview"])).toBe("Overview — Ether");
    expect(formatDocumentTitle([], "Home Theater")).toBe("Home Theater");
    expect(formatDocumentTitle(["", null, undefined])).toBe("Ether");
  });

  it("collapses adjacent duplicate segments case-insensitively", () => {
    expect(formatDocumentTitle(["Integrations", "integrations", "Trakt"], "Ether")).toBe(
      "Integrations · Trakt — Ether",
    );
  });
});

describe("resolveRouteTitle", () => {
  const allDescriptors = [...descriptors, fixturePluginPage] as Descriptor<PageMeta>[];

  it("resolves built-in routes, sub-tabs, nested sections, and route drawers", () => {
    const titleFor = (path: string, search = "") =>
      formatDocumentTitle(resolveRouteTitle(path, search, allDescriptors).parts, "Home");

    expect(titleFor("/")).toBe("Overview — Home");
    expect(titleFor("/activity")).toBe("Activity · Now playing — Home");
    expect(titleFor("/activity/history")).toBe("Activity · History — Home");
    expect(titleFor("/viewers")).toBe("Viewers — Home");
    expect(titleFor("/viewers/watchlists")).toBe("Viewers · Watchlists — Home");
    expect(titleFor("/viewers/security")).toBe("Viewers · Security — Home");
    expect(titleFor("/viewers/3/sign-in", "?signin=sync")).toBe(
      "Viewers · Sign-in & devices · Connected accounts — Home",
    );
    expect(titleFor("/viewers/3/lists", "?list=alerts")).toBe(
      "Viewers · Lists & alerts · Alerts — Home",
    );
    expect(titleFor("/libraries/1/rules")).toBe("Libraries · Rules — Home");
    expect(titleFor("/collections")).toBe("Collections — Home");
    expect(titleFor("/collections", "?tab=templates")).toBe("Collections · Templates — Home");
    expect(titleFor("/collections/new")).toBe("Collections · New collection — Home");
    expect(titleFor("/collections/42")).toBe("Collections · Edit collection — Home");
    expect(titleFor("/recommendations")).toBe("Recommendations — Home");
    expect(titleFor("/livetv")).toBe("Live TV · Channels — Home");
    expect(titleFor("/livetv/looks")).toBe("Live TV · Styles — Home");
    expect(titleFor("/calendar/week")).toBe("Calendar · Week — Home");
    expect(titleFor("/metadata/episodes")).toBe("Metadata · Episode order — Home");
    expect(titleFor("/sources/statistics")).toBe("Sources · Statistics — Home");
    expect(titleFor("/releases/rules")).toBe("Releases · Rules — Home");
    expect(titleFor("/cache")).toBe("Cache — Home");
    expect(titleFor("/cache/explorer")).toBe("Cache · Explorer — Home");
    expect(titleFor("/playback/devices/seen")).toBe(
      "Playback · App compatibility · Seen devices — Home",
    );
    expect(titleFor("/apps")).toBe("Apps — Home");
    expect(titleFor("/apps/plex/remote")).toBe("Apps · Plex · Remote access — Home");
    expect(titleFor("/services/trakt")).toBe("Integrations · Trakt — Home");
    expect(titleFor("/notifications/report")).toBe("Notifications · Weekly report — Home");
    expect(titleFor("/flows")).toBe("Flows — Home");
    expect(titleFor("/flows/list")).toBe("Flows — Home");
    expect(titleFor("/flows/runs")).toBe("Flows · Runs — Home");
    expect(titleFor("/flows/templates")).toBe("Flows · Templates — Home");
    expect(titleFor("/cluster", "?tab=nodes")).toBe("Cluster · Nodes — Home");
    expect(titleFor("/maintenance/backups/import")).toBe(
      "Maintenance · Backups & import · Import & export — Home",
    );
  });

  it("resolves plugin pages, plugin screens, setup, portal, MCP, and 404", () => {
    const titleFor = (path: string, search = "", registryLoading = false) =>
      formatDocumentTitle(
        resolveRouteTitle(path, search, allDescriptors, { registryLoading }).parts,
        "Home",
      );

    expect(titleFor("/plugins")).toBe("Plugins · Installed — Home");
    expect(titleFor("/plugins/marketplace")).toBe("Plugins · Marketplace — Home");
    expect(titleFor("/plugins/studio")).toBe("Plugins · Studio — Home");
    expect(titleFor("/plugins/reference")).toBe("Plugins · Developer reference — Home");
    expect(titleFor("/p/dev.example/wrapped")).toBe("Ether Wrapped — Home");
    expect(titleFor("/p/other.plugin/custom")).toBe("custom — Home");
    expect(titleFor("/setup")).toBe("Setup — Home");
    expect(titleFor("/me")).toBe("Approve a TV — Home");
    expect(titleFor("/mcp/authorize")).toBe("Connect an AI app — Home");
    expect(titleFor("/does-not-exist")).toBe("Not found — Home");
    expect(titleFor("/does-not-exist", "", true)).toBe("Home");
  });
});

describe("document.title integration", () => {
  it("updates across routes, sub-tabs, and never leaves a stale title", async () => {
    resetRegistryCache();
    renderApp("/libraries/1/rules");
    await screen.findByRole("heading", { name: "Movies" }, { timeout: 15000 });
    await waitFor(() => {
      expect(document.title).toBe("Libraries · Movies · Rules — Living Room Ether");
    });

    fireEvent.click(screen.getAllByRole("link", { name: "Overview" })[0]!);
    await waitFor(() => {
      expect(document.title).toBe("Overview — Living Room Ether");
    });

    fireEvent.click(screen.getAllByRole("link", { name: "Recommendations" })[0]!);
    await waitFor(() => {
      expect(document.title).toBe("Recommendations — Living Room Ether");
    });
  });

  it("sets titles for login, setup, portal, and URL-backed drawers", async () => {
    resetRegistryCache();
    const login = renderApp("/", {
      bootstrap: { authRequired: true, authenticated: false },
    });
    await settleUi(() => {
      expect(document.title).toBe("Sign in — Living Room Ether");
    });
    login.unmount();

    const setup = renderApp("/setup");
    await screen.findByRole("heading", { name: "Set up Ether" }, { timeout: 15000 });
    await waitFor(() => {
      expect(document.title).toBe("Setup · Language — Living Room Ether");
    });
    setup.unmount();

    const portal = renderApp("/me");
    await settleUi(() => {
      expect(document.title).toBe("Approve a TV — Ether");
    });
    portal.unmount();

    const drawer = renderApp("/maintenance/logs?record=1");
    await screen.findByRole("dialog", {}, { timeout: 15000 });
    await waitFor(() => {
      expect(document.title).toBe("Maintenance · Logs · Record a problem — Living Room Ether");
    });
    drawer.unmount();

    const newFlowDrawer = renderApp("/flows/list/new");
    await screen.findByRole("dialog", {}, { timeout: 15000 });
    await waitFor(() => {
      expect(document.title).toBe("Flows · New flow — Living Room Ether");
    });
    newFlowDrawer.unmount();

    const editFlowDrawer = renderApp("/flows/list/flow-stop-remote-4k-transcodes");
    await screen.findByRole("dialog", {}, { timeout: 15000 });
    await waitFor(() => {
      expect(document.title).toBe("Flows · Stop remote 4K video transcodes — Living Room Ether");
    });
    editFlowDrawer.unmount();
  });
});

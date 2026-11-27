import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Library } from "../pages/libraries/api";
import { PluginActivity } from "../pages/plugins/drawer/PluginActivity";
import { PluginCatalogueSettings } from "../pages/plugins/drawer/PluginCatalogueSettings";
import type { PluginDetail } from "../pages/plugins/types";

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function makeBookLibrary(overrides: Partial<Library> = {}): Library {
  return {
    id: 42,
    key: "plugin:ether.books:audiobooks",
    name: "Audiobooks",
    source: "plugin",
    hasVideo: false,
    presentations: { mediaClass: "audiobook", apps: [], available: {}, effective: {} },
    pluginId: "ether.books",
    kindKey: "audiobooks",
    pluginName: "Open Books",
    sort: 1,
    hidden: false,
    createdAt: "2026-11-01T00:00:00Z",
    updatedAt: "2026-11-21T00:00:00Z",
    itemCount: 526,
    storedItems: 526,
    maxPluginItems: 2_000_000,
    kind: {
      key: "audiobooks",
      label: "Audiobooks",
      media: "audiobook",
      library: { name: "Audiobooks" },
      levels: [{ key: "audiobook", label: "Audiobooks", role: "audiobook", playable: true }],
      browse: { search: true },
      sync: { mode: "pull", refresh: "6h" },
    },
    sync: {
      pluginId: "ether.books",
      kindKey: "audiobooks",
      generation: 3,
      items: 526,
      lastError:
        "pluginitems: sync aborted by 50% delete guard: 334 of 526 items missing in walk for ether.books/audiobooks",
      deleteGuard: {
        source: "walk",
        missing: 334,
        total: 526,
        protected: 14,
      },
    },
    ...overrides,
  };
}

function makeBookPluginDetail(): PluginDetail {
  return {
    id: "ether.books",
    name: "Open Books",
    version: "1.0.7",
    kind: "ether",
    enabled: true,
    health: "ok",
    healthDetail: { status: "ok", reason: "", recentErrors: 0, recentCalls: 4 },
    impact: { calls: 4, errors: 0, p50Ms: 120, p95Ms: 250 },
    privacy: { undeclared: [], observedHosts: {}, declaredHosts: [] },
    permissions: { requested: [], granted: [] },
    manifest: {
      id: "ether.books",
      name: "Open Books",
      version: "1.0.7",
      ether: {
        runtime: { kind: "wasm" },
        contributes: {
          mediaKinds: [
            {
              key: "audiobooks",
              label: "Audiobooks",
              library: { name: "Audiobooks" },
            },
          ],
        },
      },
    } as unknown as PluginDetail["manifest"],
  } as unknown as PluginDetail;
}

describe("Plugin library sync feedback and delete guard choice", () => {
  const realFetch = window.fetch.bind(window);

  beforeEach(() => {
    vi.restoreAllMocks();
  });

  afterEach(() => {
    window.fetch = realFetch;
    cleanup();
  });

  it("rewords the storage cap, explains the delete guard in plain words, and offers Keep vs Remove without acting automatically", async () => {
    let lib = makeBookLibrary();
    const syncBodies: Record<string, unknown>[] = [];
    const onSyncActivity = vi.fn();

    window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(
        input instanceof Request ? input.url : String(input),
        window.location.origin,
      );
      if (url.pathname === "/ether/api/plugins-setup/ether.books") {
        return jsonResponse({
          pluginId: "ether.books",
          pluginName: "Open Books",
          steps: [],
          blockers: [],
          settings: {},
          schema: null,
          kinds: [],
          libraries: [lib],
        });
      }
      if (url.pathname === "/ether/api/libraries/catalogue-sources") {
        return jsonResponse({ sources: [] });
      }
      if (url.pathname === "/ether/api/libraries/42/sync" && init?.method === "POST") {
        const body = JSON.parse(String(init.body ?? "{}")) as Record<string, unknown>;
        syncBodies.push(body);
        if (body.keepExisting) {
          lib = makeBookLibrary({
            sync: {
              pluginId: "ether.books",
              kindKey: "audiobooks",
              generation: 3,
              items: 526,
              lastOutcome: {
                mode: "keep",
                total: 526,
                summary: "Kept all 526 existing items",
                finishedAt: "2026-11-21T16:50:00Z",
              },
            },
          });
          return jsonResponse({ kept: true, running: false, sync: lib.sync });
        }
        if (body.force) {
          lib = makeBookLibrary({
            itemCount: 192,
            storedItems: 192,
            sync: {
              pluginId: "ether.books",
              kindKey: "audiobooks",
              generation: 4,
              items: 192,
              lastOutcome: {
                mode: "force",
                deleted: 334,
                protected: 14,
                total: 192,
                summary:
                  "Removed 334 missing items · 192 items in library (14 kept by viewer activity)",
                finishedAt: "2026-11-21T16:52:00Z",
              },
            },
          });
          return jsonResponse({ deleted: 334, protected: 14, running: false, sync: lib.sync });
        }
      }
      return jsonResponse({});
    };

    render(
      <MemoryRouter>
        <PluginCatalogueSettings
          pluginId="ether.books"
          onSetup={() => {}}
          onSyncActivity={onSyncActivity}
        />
      </MemoryRouter>,
    );

    // Storage cap is worded as a limit, never "526 of 2,000,000 items".
    expect(await screen.findByText(/526 items \(storage limit 2,000,000\)/)).toBeInTheDocument();
    expect(screen.queryByText(/526 of 2,000,000 items/)).not.toBeInTheDocument();

    // Plain-language delete guard explanation + protected item note + explicit choices.
    expect(
      screen.getByText(/Sync paused — 334 of 526 items were not returned in the walk/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/14 opened, played, favourited, or searched items are protected/),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Keep existing items" })).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Remove the 334 missing items" }),
    ).toBeInTheDocument();

    // No automatic sync call occurred.
    expect(syncBodies).toHaveLength(0);

    // Owner clicks "Remove the 334 missing items" -> sends force:true, fullWalk:true.
    fireEvent.click(screen.getByRole("button", { name: "Remove the 334 missing items" }));

    await waitFor(() => {
      expect(syncBodies).toEqual([{ force: true, fullWalk: true }]);
    });
    expect(
      await screen.findByText(/Removed 334 missing items · 192 items in library/),
    ).toBeInTheDocument();
    expect(onSyncActivity).toHaveBeenCalled();
  });

  it("shows live sync progress and surfaces sync jobs and kind browse calls in Activity", async () => {
    const runningLib = makeBookLibrary({
      sync: {
        pluginId: "ether.books",
        kindKey: "audiobooks",
        generation: 3,
        items: 526,
        running: true,
        phase: "syncing",
        pages: 2,
        syncedItems: 180,
      },
    });

    window.fetch = async (input: RequestInfo | URL) => {
      const url = new URL(
        input instanceof Request ? input.url : String(input),
        window.location.origin,
      );
      if (url.pathname === "/ether/api/plugins-setup/ether.books") {
        return jsonResponse({
          pluginId: "ether.books",
          pluginName: "Open Books",
          steps: [],
          blockers: [],
          settings: {},
          schema: null,
          kinds: [],
          libraries: [runningLib],
        });
      }
      if (url.pathname === "/ether/api/libraries/catalogue-sources") {
        return jsonResponse({ sources: [] });
      }
      if (url.pathname === "/ether/api/plugins/ether.books/jobs") {
        return jsonResponse([
          {
            path: "/ether/v1/kinds/audiobooks/sync",
            label: "Full re-sync · Audiobooks",
            kind: "audiobooks",
            state: "running",
            progress: "Page 2 · 180 items checked",
            started: "2026-11-21T16:55:00Z",
          },
        ]);
      }
      return jsonResponse({});
    };

    const detail = makeBookPluginDetail();
    render(
      <MemoryRouter>
        <PluginCatalogueSettings pluginId="ether.books" onSetup={() => {}} />
        <PluginActivity
          detail={detail}
          calls={[
            {
              id: 1,
              route: "/ether/v1/kinds/audiobooks/browse",
              method: "POST",
              status: 200,
              ms: 145,
              response: JSON.stringify({ items: [{ id: "1" }, { id: "2" }, { id: "3" }] }),
              created: "2026-11-21T16:55:01Z",
            },
          ]}
          onReloadCalls={async () => {}}
          journal={[]}
          onReloadJournal={async () => {}}
          liveLogLines={[]}
          onError={() => {}}
          onChanged={() => {}}
        />
      </MemoryRouter>,
    );

    // Live progress in Settings.
    expect(await screen.findByText(/Syncing… · Page 2 · 180 items checked/)).toBeInTheDocument();

    // Background job + plain-language kind browse call in Activity.
    expect(await screen.findByText("Background jobs")).toBeInTheDocument();
    expect(screen.getByText("Full re-sync · Audiobooks")).toBeInTheDocument();
    expect(screen.getByText("Page 2 · 180 items checked")).toBeInTheDocument();
    expect(screen.getByText("Catalogue walk: Audiobooks")).toBeInTheDocument();
    expect(screen.getByText("3 results")).toBeInTheDocument();
  });
});

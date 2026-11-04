import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { createExperienceMock } from "../mock/experience";
import Activity from "../pages/activity/Activity";

const WAIT = { timeout: 4000 };
const NOW = Date.UTC(2026, 10, 3, 18, 0, 0);

function backend() {
  const experience = createExperienceMock(NOW);
  const urls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), "http://ether.test");
      urls.push(url.pathname + url.search);
      const seg = url.pathname
        .replace(/^\/ether\/api/, "")
        .split("/")
        .filter(Boolean);
      const hit = experience.handle("GET", seg, url.searchParams);
      let body: unknown = hit?.body ?? { value: null };
      if (!hit && url.pathname.endsWith("/users")) body = { users: [] };
      return new Response(JSON.stringify(body), { status: hit?.status ?? 200 });
    }),
  );
  return urls;
}

function Where() {
  const l = useLocation();
  return <output data-testid="where">{l.pathname + l.search}</output>;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/activity/:tab?" element={<Activity />} />
      </Routes>
      <Where />
    </MemoryRouter>,
  );
}

describe("Activity › Experience", () => {
  beforeEach(() => vi.useRealTimers());

  it("shows household tiles and opens a viewer's drawer from the worst-first table", async () => {
    const urls = backend();
    renderAt("/activity/experience");
    expect(await screen.findByText("Median start time", {}, WAIT)).toBeInTheDocument();
    expect(screen.getAllByText("Failed starts").length).toBeGreaterThan(0);
    expect(urls.some((u) => u.startsWith("/ether/api/experience?days=7"))).toBe(true);
    // Worst first: the healthy household owner sorts last.
    const rows = screen.getAllByRole("row").slice(1);
    expect(within(rows[rows.length - 1]!).getByText("Alex Rivera")).toBeInTheDocument();
    fireEvent.click(screen.getByText("Jordan Lee"));
    expect(screen.getByTestId("where").textContent).toContain("viewer=4");
    const dialog = await screen.findByRole("dialog", {}, WAIT);
    expect(await within(dialog).findByText("Most likely cause", {}, WAIT)).toBeInTheDocument();
    expect(within(dialog).getByText(/never reached a picture/)).toBeInTheDocument();
    expect(within(dialog).getByText("Last 20 plays")).toBeInTheDocument();
    const historyLinks = within(dialog).getAllByRole("link", { name: /In History/ });
    expect(historyLinks[0]!.getAttribute("href")).toMatch(/^\/activity\/history\?play=play-4-/);
  });

  it("deep-links a viewer and names the device and reason in plain words", async () => {
    backend();
    renderAt("/activity/experience?viewer=3");
    const dialog = await screen.findByRole("dialog", {}, WAIT);
    expect(
      await within(dialog).findByText(
        /of plays on Playroom Fire TV are converted because of the audio format \(DTS\)\./,
        {},
        WAIT,
      ),
    ).toBeInTheDocument();
  });

  it("shows — for plays recorded before measurement", async () => {
    backend();
    renderAt("/activity/experience?days=30&viewer=1");
    const dialog = await screen.findByRole("dialog", {}, WAIT);
    await within(dialog).findByText("Last 20 plays", {}, WAIT);
    // Alex plays ~2 a day, so the last 20 reach back before measurement began.
    expect(within(dialog).getAllByText("—").length).toBeGreaterThan(0);
  });

  it("opens a play from a History link", async () => {
    backend();
    const experience = createExperienceMock(NOW);
    const viewer = experience.handle("GET", ["experience", "viewers", "2"], new URLSearchParams())!
      .body as { plays: { id: string; title: string }[] };
    const play = viewer.plays[0]!;
    renderAt(`/activity/history?play=${play.id}`);
    const dialog = await screen.findByRole("dialog", {}, WAIT);
    expect(await within(dialog).findByText("Source", {}, WAIT)).toBeInTheDocument();
    expect(within(dialog).getByRole("link", { name: "Sam Rivera" }).getAttribute("href")).toBe(
      "/activity/experience?viewer=2",
    );
  });
});

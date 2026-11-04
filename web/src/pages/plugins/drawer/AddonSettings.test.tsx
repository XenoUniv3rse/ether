import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AddonSettings } from "./AddonSettings";
import type { PluginDetail } from "../types";
import { apiFetch } from "../../../lib/api";
vi.mock("../../../lib/api", () => ({ apiFetch: vi.fn(), errorMessage: String }));
vi.mock("../../../lib/extend", () => ({ invalidateRegistry: vi.fn() }));
vi.mock("./liveForm", () => ({ useLiveForm: () => ({ status: "idle" }), LiveFields: () => null }));
afterEach(cleanup);
const detail = { id: "cinemeta" } as PluginDetail;
it("explains an addon without configuration instead of leaving Setup blank", async () => {
  vi.mocked(apiFetch).mockResolvedValue({ configurable: false, fields: [] });
  render(<AddonSettings detail={detail} onSaved={() => {}} />);
  expect(await screen.findByText("Nothing to set up. It works as installed.")).toBeInTheDocument();
});
it("does not show the empty message alongside native settings", async () => {
  vi.mocked(apiFetch).mockResolvedValue({ configurable: false, fields: [] });
  const { container } = render(
    <AddonSettings detail={detail} onSaved={() => {}} showEmpty={false} />,
  );
  await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
  expect(container).toBeEmptyDOMElement();
});
it("shows settings-load failures", async () => {
  vi.mocked(apiFetch).mockRejectedValue(new Error("Settings offline"));
  render(<AddonSettings detail={detail} onSaved={() => {}} />);
  expect(await screen.findByRole("alert")).toHaveTextContent("Settings offline");
});

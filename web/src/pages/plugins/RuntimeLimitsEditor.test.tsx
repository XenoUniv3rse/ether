import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { RuntimeLimitsEditor } from "./RuntimeLimitsEditor";

const apiFetch = vi.hoisted(() => vi.fn());
vi.mock("../../lib/api", () => ({ apiFetch, ApiError: Error, errorMessage: String }));

beforeEach(() => {
  apiFetch.mockReset();
});

it("edits the global catalogue item ceiling above 2,000,000 with no upper cap", async () => {
  apiFetch.mockImplementation(async (path: string, init?: RequestInit) => {
    if (path === "/plugins/runtime-limits" && !init?.method) {
      return {};
    }
    if (path === "/plugins") {
      return { plugins: [] };
    }
    if (path === "/plugins/runtime-limits" && init?.method === "PUT") {
      return JSON.parse(String(init.body));
    }
    throw new Error(`unexpected request: ${path}`);
  });

  render(<RuntimeLimitsEditor />);

  const input = await screen.findByLabelText("Catalogue items");
  await waitFor(() => expect(input).not.toBeDisabled());
  expect(input).toHaveAttribute("placeholder", "2,000,000 (default)");
  expect(input).not.toHaveAttribute("max");

  fireEvent.change(screen.getByLabelText("Shared library cache (GiB)"), {
    target: { value: "48" },
  });
  fireEvent.change(input, { target: { value: "5000000" } });
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

  await waitFor(() =>
    expect(apiFetch).toHaveBeenCalledWith(
      "/plugins/runtime-limits",
      expect.objectContaining({
        method: "PUT",
      }),
    ),
  );
  const putCall = apiFetch.mock.calls.find(
    ([path, init]) => path === "/plugins/runtime-limits" && init?.method === "PUT",
  );
  expect(JSON.parse(String(putCall?.[1]?.body))).toEqual({
    plugin: "",
    limits: {
      memoryBytes: 0,
      deadline: "",
      unitBudget: 0,
      poolSize: 0,
      fetchBytes: 0,
      archiveMemberBytes: 0,
      archiveExpandedBytes: 0,
      archiveMembers: 0,
      maxItems: 5000000,
      listItems: 0,
      durableFreePercent: 0,
      sharedCacheBytes: 48 * 2 ** 30,
    },
  });
});

it("shows stored catalogue items against the plugin cap even when hosted limits are absent", async () => {
  apiFetch.mockResolvedValue({});

  render(
    <RuntimeLimitsEditor
      plugin="community.example.remote"
      maxItems={2_000_000}
      storedItems={1250}
      hasMediaKinds
    />,
  );

  expect(await screen.findByText("1,250 stored (limit 2,000,000)")).toBeInTheDocument();
  expect(screen.queryByLabelText(/Shared library cache/)).toBeNull();
  const overrideInput = screen.getByLabelText("Override: Catalogue items");
  await waitFor(() => expect(overrideInput).not.toBeDisabled());
  expect(overrideInput).toHaveAttribute("placeholder", "2,000,000 (default)");
});

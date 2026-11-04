import { waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { calls, renderApp } from "./harness";
import { pluginCollectionHref } from "../pages/collections/links";

describe("plugin catalogue collection handoff", () => {
  it("previews the selected plugin source rather than the default catalogue", async () => {
    const source = "plugin:community.jay-classic-movies-copy/movie-classics";
    const { fake } = renderApp(pluginCollectionHref(source, "Jay Classics & favourites"));
    await waitFor(
      () => {
        const previews = calls(fake, "POST", "/collections/preview");
        expect(previews.length).toBeGreaterThan(0);
        expect(previews.at(-1)?.body).toMatchObject({
          name: "Jay Classics & favourites",
          sources: [{ kind: source }],
        });
      },
      { timeout: 10000 },
    );
  });
});

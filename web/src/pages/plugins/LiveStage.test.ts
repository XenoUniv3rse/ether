import { expect, it } from "vitest";
import { momentsForTool } from "./LiveStage";

it("shows actionable failed-check details instead of marking every step successful", async () => {
  const moments = await momentsForTool(
    "studio_project_save",
    {},
    {
      compile: { ok: true },
      preview: { ok: false, error: "route did not return a ViewSpec document" },
      installCheck: "unknown contribution capability",
    },
  );
  expect(moments[0]).toMatchObject({
    kind: "checks",
    failed: [false, true, true],
    issues: ["route did not return a ViewSpec document", "unknown contribution capability"],
  });
});

it("does not claim preview or installation checks ran after compilation failed", async () => {
  const moments = await momentsForTool(
    "studio_project_save",
    {},
    {
      compile: {
        ok: false,
        diagnostics: [{ file: "src/plugin.ts", line: 4, text: "Unexpected token" }],
      },
    },
  );
  expect(moments[0]).toMatchObject({
    kind: "checks",
    items: ["Has compile errors"],
    failed: [true],
    issues: ["src/plugin.ts:4 Unexpected token"],
  });
});

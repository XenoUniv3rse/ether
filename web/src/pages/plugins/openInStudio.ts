import { apiFetch } from "../../lib/api";

/**
 * Opens any installed plugin in Studio: its newest draft, or a new draft
 * made from what is installed (a Stremio add-on becomes an editable
 * native plugin). Resolves to the Studio route to navigate to.
 */
export async function openInStudio(pluginId: string): Promise<string> {
  const r = await apiFetch<{ project: { id: string } }>("/plugins/studio/import", {
    method: "POST",
    body: JSON.stringify({ pluginId }),
  });
  return `/plugins/studio?project=${encodeURIComponent(r.project.id)}&view=code`;
}

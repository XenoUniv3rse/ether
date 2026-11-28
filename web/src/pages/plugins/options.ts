import { useCallback } from "react";
import { useLocale } from "../../i18n/react";
import { apiFetch } from "../../lib/api";

export function usePluginOptions(plugin: string) {
  const { locale } = useLocale();
  return useCallback(
    async (field: string, query?: string, signal?: AbortSignal) => {
      if (!plugin) return [];
      const params = new URLSearchParams();
      if (query !== undefined) params.set("q", query);
      params.set("locale", locale);
      const result = await apiFetch<{
        options?: { value: string; label: string; hint?: string }[];
      }>(`/plugins/${encodeURIComponent(plugin)}/options/${encodeURIComponent(field)}?${params}`, {
        signal,
      });
      return (result.options ?? []).map((option) => ({
        value: String(option.value),
        label: String(option.label),
        description: option.hint,
      }));
    },
    [plugin, locale],
  );
}

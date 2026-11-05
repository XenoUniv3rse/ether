import { t } from "../i18n";

export function qualityFallback(reason?: string): string {
  if (!reason) return "";
  const match = /^Played in (.+): no acceptable (\d+p) or better release was found\.$/.exec(reason);
  return match
    ? t("Played in {quality}: no acceptable {minimum} or better release was found.", {
        quality: match[1],
        minimum: match[2],
      })
    : reason;
}

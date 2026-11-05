import { Input } from "@braedonsaunders/appkit-ui";
import { Field, Segmented } from "../../components/fields";
import { t, tn } from "../../i18n";
import type { SourceAccount, SourceLimitRules } from "./model";

/** Whole numbers only; an empty or invalid entry clears the setting. */
function wholeNumber(raw: string): number | undefined {
  const n = Number(raw);
  return raw.trim() === "" || !Number.isFinite(n) || n <= 0 ? undefined : Math.floor(n);
}

/**
 * How gently Ether uses the provider: how many connections it may hold, how
 * far apart new streams start and how many may start in an hour. Strict
 * providers flag accounts that open many streams at once or in quick
 * succession; these keep every viewer inside what the account is sold for.
 */
export function SourceLimits({
  rules,
  account,
  onChange,
}: {
  rules: SourceLimitRules;
  account?: SourceAccount;
  onChange: (next: SourceLimitRules) => void;
}) {
  const panel = account?.maxConnections ?? 0;
  const gap = rules.tuneGapSeconds ?? 0;
  const gapChoice = gap < 0 ? "off" : gap === 0 ? "auto" : String(gap);
  return (
    <div className="space-y-5">
      <div className="space-y-1">
        <h3 className="text-sm font-semibold text-fg">{t("Play nicely with your provider")}</h3>
        <p className="text-sm text-fg-muted">
          {t(
            "Ether opens one stream per channel and shares it between everyone watching that channel. These limits keep it well inside what your account allows.",
          )}
        </p>
      </div>

      <Field
        label={t("Connections at once")}
        htmlFor="src-max-connections"
        hint={
          panel > 0
            ? tn(
                panel,
                "Your provider allows {count} connection. Leave empty to use all of it, or set fewer to keep one free for another app.",
                "Your provider allows {count} connections. Leave empty to use all of them, or set fewer to keep some free for other apps.",
              )
            : t("Leave empty for no limit. Set it to the number of connections your plan includes.")
        }
      >
        <Input
          id="src-max-connections"
          type="number"
          inputMode="numeric"
          min={1}
          max={panel > 0 ? panel : 100}
          className="w-32"
          placeholder={panel > 0 ? String(panel) : t("No limit")}
          value={rules.maxConnections ? String(rules.maxConnections) : ""}
          onChange={(e) => {
            const n = wholeNumber(e.currentTarget.value);
            onChange({ ...rules, maxConnections: n && panel > 0 ? Math.min(n, panel) : n });
          }}
        />
      </Field>

      <Field
        label={t("Wait between new streams")}
        hint={
          gapChoice === "auto"
            ? t(
                "Automatic waits 2 seconds between new streams when your provider limits connections, so flicking through channels never looks like a scan.",
              )
            : gapChoice === "off"
              ? t("New streams start straight away. Only for providers without limits.")
              : t("A channel change may take a moment longer to start.")
        }
      >
        <Segmented
          ariaLabel={t("Wait between new streams")}
          size="sm"
          value={gapChoice}
          onChange={(v) =>
            onChange({
              ...rules,
              tuneGapSeconds: v === "off" ? -1 : v === "auto" ? undefined : Number(v),
            })
          }
          options={[
            { value: "auto", label: t("Automatic") },
            { value: "5", label: t("5 seconds") },
            { value: "10", label: t("10 seconds") },
            { value: "off", label: t("Off") },
          ]}
        />
      </Field>

      <Field
        label={t("New streams per hour")}
        htmlFor="src-tunes-per-hour"
        hint={t(
          "Leave empty for no cap. Past the cap, viewers are asked to try again later instead of Ether starting another stream.",
        )}
      >
        <Input
          id="src-tunes-per-hour"
          type="number"
          inputMode="numeric"
          min={1}
          max={10000}
          className="w-32"
          placeholder={t("No cap")}
          value={rules.tunesPerHour ? String(rules.tunesPerHour) : ""}
          onChange={(e) => onChange({ ...rules, tunesPerHour: wholeNumber(e.currentTarget.value) })}
        />
      </Field>
    </div>
  );
}

import { Badge } from "@braedonsaunders/appkit-ui";
import { RadioTower } from "lucide-react";
import { t } from "../../i18n";
import { formatNumber } from "../../lib/format";
import type { LiveProviderIdentity, LiveProviderInfo } from "../../lib/health";

function providerLabel(provider: LiveProviderIdentity) {
  const login = provider.loginNumber
    ? t("Login {number}", { number: formatNumber(provider.loginNumber) })
    : undefined;
  return [provider.sourceName, login, provider.loginLabel].filter(Boolean).join(" · ");
}

export function LiveProvider({
  provider,
  previous,
  onScrim = false,
}: {
  provider?: LiveProviderInfo;
  previous?: LiveProviderIdentity;
  onScrim?: boolean;
}) {
  if (!provider?.sourceName) return null;
  const label = providerLabel(provider);
  const usage =
    provider.localInUse !== undefined && provider.loginLimit
      ? {
          used: formatNumber(provider.localInUse),
          limit: formatNumber(provider.loginLimit),
        }
      : undefined;
  const usageLabel = usage && t("{used} of {limit} connections on this server", usage);
  return (
    <div className={onScrim ? "text-on-scrim/75" : "text-fg-muted"}>
      <div className="flex min-w-0 items-center gap-1.5 text-xs">
        <RadioTower size={13} aria-hidden="true" className="shrink-0" />
        <span className="min-w-0 truncate" title={label}>
          <span className="sr-only">{t("Provider")} </span>
          {label}
        </span>
        {usage && (
          <span className="shrink-0" title={usageLabel}>
            <Badge variant="secondary">
              <span aria-hidden="true">
                {usage.used}/{usage.limit}
              </span>
              <span className="sr-only">{usageLabel}</span>
            </Badge>
          </span>
        )}
      </div>
      {previous?.sourceName && (
        <p className="mt-0.5 pl-[19px] text-xs">
          {t("Switched from {provider}", { provider: providerLabel(previous) })}
        </p>
      )}
    </div>
  );
}

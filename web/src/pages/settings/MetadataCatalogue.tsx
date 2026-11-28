import { Badge } from "@braedonsaunders/appkit-ui";
import { Tv } from "lucide-react";
import { useState } from "react";
import { Drawer } from "../../components/Drawer";
import { secretConfigured } from "../../components/IntegrationAccount";
import { IntegrationAppCard } from "../../components/IntegrationAppCard";
import { IntegrationSetupFooter } from "../../components/IntegrationSetupFooter";
import { TmdbMark } from "../../components/integration-brands";
import { MetadataKeyFields } from "../../components/MetadataKeyFields";
import type { TestState } from "../../components/fields";
import { t } from "../../i18n";
import { builtinMetadataSources, type BuiltinMetadataSource } from "../../lib/metadataSources";

export function MetadataCatalogue({
  source,
  identity,
  storedKey,
  bundled,
  enabled,
  onSetEnabled,
  locked,
  onSave,
}: {
  source: BuiltinMetadataSource;
  identity: { id: string; name: string };
  storedKey: string;
  bundled: boolean;
  enabled: boolean;
  onSetEnabled: (enabled: boolean) => Promise<void>;
  locked: boolean;
  onSave: (key: string) => Promise<void>;
}) {
  const [key, setKey] = useState<string | null>(null);
  const [test, setTest] = useState<TestState>({ status: "idle" });
  const [saving, setSaving] = useState(false);
  const [saveFailed, setSaveFailed] = useState(false);
  const provider = builtinMetadataSources[source];
  const ready = bundled || secretConfigured(storedKey);
  const allowEmptyTest = bundled && enabled;
  const busy = saving || test.status === "running";
  const actionLabel =
    enabled && ready
      ? t("Manage {app}", { app: provider.name })
      : t("Set up {app}", { app: provider.name });

  function openDrawer() {
    setKey(storedKey);
    setTest({ status: "idle" });
    setSaveFailed(false);
  }

  async function save() {
    if (key === null) return;
    setSaving(true);
    setSaveFailed(false);
    try {
      await onSave(key.trim());
      setKey(null);
    } catch {
      setSaveFailed(true);
    } finally {
      setSaving(false);
    }
  }

  async function toggle(next: boolean) {
    if (source === "tvdb" && next) {
      openDrawer();
      return;
    }
    setSaving(true);
    setSaveFailed(false);
    try {
      await onSetEnabled(next);
    } catch {
      setSaveFailed(true);
    } finally {
      setSaving(false);
    }
  }

  async function runTest() {
    if (key === null || (!allowEmptyTest && !key.trim())) return;
    setTest({ status: "running" });
    const failed: TestState = { status: "fail", message: t("Could not verify the API key.") };
    try {
      const result = await provider.test(key.trim());
      setTest(result.ok ? { status: "ok", durationMs: result.durationMs } : failed);
    } catch {
      setTest(failed);
    }
  }

  return (
    <>
      <IntegrationAppCard
        identity={identity}
        logo={source === "tmdb" ? <TmdbMark /> : <Tv size={24} aria-hidden />}
        description={provider.description()}
        enabled={enabled}
        onToggle={(next) => void toggle(next)}
        disabled={locked}
        busy={busy}
        status={
          <Badge variant={enabled ? (ready ? "success" : "warning") : "secondary"}>
            {enabled ? (ready ? t("On") : t("Needs an API key")) : t("Off")}
          </Badge>
        }
        configureLabel={actionLabel}
        configureHref={source === "tmdb" && enabled && ready ? "/services/tmdb" : undefined}
        onConfigure={openDrawer}
        details={
          saveFailed && key === null ? (
            <p role="alert" className="text-danger">
              {t("Couldn't save the change")}
            </p>
          ) : null
        }
      />
      {key !== null ? (
        <Drawer
          open
          title={actionLabel}
          description={
            !enabled ? t("Save and enable turns this on. Cancel leaves it off.") : undefined
          }
          onClose={() => {
            if (!busy) setKey(null);
          }}
          footer={
            <IntegrationSetupFooter
              disabled={busy}
              saveDisabled={
                locked || (enabled ? key.trim() === storedKey : !bundled && !key.trim())
              }
              saveLabel={enabled ? t("Save") : undefined}
              onCancel={() => setKey(null)}
              onSave={() => void save()}
            />
          }
        >
          {bundled ? (
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <Badge variant="secondary">{t("Built in")}</Badge>
              <p className="text-sm text-fg-muted">{t("Use your own key")}</p>
            </div>
          ) : null}
          <MetadataKeyFields
            source={source}
            value={key}
            test={test}
            disabled={locked || saving}
            allowEmptyTest={allowEmptyTest}
            onChange={(value) => {
              setKey(value);
              setTest({ status: "idle" });
            }}
            onTest={() => void runTest()}
          />
          {source === "tvdb" && enabled && !key.trim() ? (
            <p className="mt-3 text-sm text-fg-muted">
              {t("Saving an empty key disconnects this source.")}
            </p>
          ) : null}
          {saveFailed ? (
            <p role="alert" className="mt-3 text-danger">
              {t("Couldn't save the change")}
            </p>
          ) : null}
        </Drawer>
      ) : null}
    </>
  );
}

import { Button, Input, SearchSelect } from "@braedonsaunders/appkit-ui";
import { useEffect, useState } from "react";
import { Drawer } from "../components/Drawer";
import { Field } from "../components/fields";
import { localized, t } from "../i18n";
import { searchSelectLabels } from "../i18n/appkit-labels";
import { api, errorMessage, type SupportCapture, type SupportDevice } from "../lib/api";

type ProblemCategory = "playback" | "picture" | "subtitles" | "library" | "other";

const CATEGORIES = localized((): { value: ProblemCategory; label: string }[] => [
  { value: "playback", label: t("Playback") },
  { value: "picture", label: t("Picture") },
  { value: "subtitles", label: t("Subtitles") },
  { value: "library", label: t("Library") },
  { value: "other", label: t("Other") },
]);

export function RecordProblem({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [devices, setDevices] = useState<SupportDevice[]>([]);
  const [deviceId, setDeviceId] = useState("");
  const [category, setCategory] = useState<ProblemCategory>("playback");
  const [note, setNote] = useState("");
  const [capture, setCapture] = useState<SupportCapture | null>(null);
  const [titles, setTitles] = useState(true);
  const [usernames, setUsernames] = useState(false);
  const [addresses, setAddresses] = useState(false);
  const [busy, setBusy] = useState(false);
  const [reproduced, setReproduced] = useState(false);
  const [error, setError] = useState("");
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!open) return;
    let stop = false;
    void api
      .supportDevices()
      .then((r) => {
        if (!stop) setDevices(r.devices ?? []);
      })
      .catch((e) => {
        if (!stop) setError(errorMessage(e));
      });
    return () => {
      stop = true;
    };
  }, [open]);

  const recordingId = capture?.state === "recording" ? capture.id : undefined;
  useEffect(() => {
    if (!recordingId || busy) return;
    let stop = false;
    let polling = false;
    const timer = window.setInterval(() => {
      setNow(Date.now());
      if (polling) return;
      polling = true;
      void api
        .supportGet(recordingId)
        .then((next) => {
          if (!stop) setCapture(next);
        })
        .catch((e) => {
          if (!stop) setError(errorMessage(e));
        })
        .finally(() => {
          polling = false;
        });
    }, 2000);
    return () => {
      stop = true;
      window.clearInterval(timer);
    };
  }, [recordingId, busy]);

  const close = async () => {
    if (capture && capture.state === "recording") {
      if (!window.confirm(t("Discard this recording?"))) return;
      try {
        await api.supportDiscard(capture.id);
      } catch (e) {
        setError(errorMessage(e));
        return;
      }
    }
    setCapture(null);
    setNote("");
    setReproduced(false);
    setError("");
    onClose();
  };

  const start = async () => {
    setBusy(true);
    setError("");
    try {
      const next = await api.supportStart({
        deviceId: deviceId || undefined,
        category,
        note,
      });
      setNow(Date.now());
      setReproduced(false);
      setCapture(next);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const choose = async (id: string) => {
    if (!capture) return;
    setBusy(true);
    setError("");
    try {
      setCapture(await api.supportSelect(capture.id, id));
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const finish = async () => {
    if (!capture) return;
    setBusy(true);
    setError("");
    try {
      setCapture(
        await api.supportFinish(capture.id, {
          includeTitles: titles,
          includeUsernames: usernames,
          includeAddresses: addresses,
        }),
      );
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const download = async () => {
    if (!capture) return;
    setBusy(true);
    setError("");
    try {
      const blob = await api.supportArchive(capture.id);
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = "ether-support.zip";
      a.click();
      window.setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const elapsed = capture
    ? Math.max(0, Math.floor((now - Date.parse(capture.startedAt)) / 1000))
    : 0;
  const recording = capture?.state === "recording";
  const finished = capture?.state === "finished";
  const sealed = (capture?.preview?.files?.length ?? 0) > 0;
  const canSeal = recording || (finished && !sealed);
  const discovery = capture?.discovery ?? [];
  const hypotheses = (capture?.hypotheses ?? []).filter((h) => h.code !== "device_unconfirmed");

  return (
    <Drawer
      open={open}
      onClose={() => void close()}
      title={t("Record a problem")}
      documentTitle={t("Record a problem")}
      description={t("Record what Ether sees and download a support package.")}
      size="lg"
      footer={
        <div className="flex w-full flex-wrap items-center justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={() => void close()} disabled={busy}>
            {recording ? t("Discard") : t("Close")}
          </Button>
          {!capture && (
            <Button size="sm" onClick={() => void start()} disabled={busy}>
              {t("Start recording")}
            </Button>
          )}
          {canSeal && (
            <Button
              size="sm"
              onClick={() => void finish()}
              disabled={busy || (recording && (elapsed < 30 || !reproduced))}
            >
              {t("Finish")}
            </Button>
          )}
          {finished && sealed && (
            <Button size="sm" onClick={() => void download()} disabled={busy}>
              {t("Download support ZIP")}
            </Button>
          )}
        </div>
      }
    >
      <div className="space-y-4 pb-2">
        {error && <p className="text-sm text-danger">{error}</p>}
        {!capture && (
          <>
            <Field label={t("Device")} htmlFor="record-problem-device">
              <SearchSelect
                {...searchSelectLabels()}
                id="record-problem-device"
                ariaLabel={t("Device")}
                value={deviceId}
                onChange={setDeviceId}
                searchable={devices.length > 7}
                options={[
                  { value: "", label: t("I don't know which device") },
                  ...devices.map((d) => ({
                    value: d.id,
                    label:
                      [d.name, d.model, d.product, d.version].filter(Boolean).join(" · ") || d.id,
                  })),
                ]}
              />
            </Field>
            <Field label={t("Category")} htmlFor="record-problem-category">
              <SearchSelect
                {...searchSelectLabels()}
                id="record-problem-category"
                ariaLabel={t("Category")}
                value={category}
                onChange={(next) => setCategory(next as ProblemCategory)}
                options={CATEGORIES}
              />
            </Field>
            <Field
              label={t("Note")}
              htmlFor="record-problem-note"
              hint={t(
                "The note is saved as data, not instructions. Links and addresses are redacted.",
              )}
            >
              <Input
                id="record-problem-note"
                value={note}
                onChange={(e) => setNote(e.currentTarget.value)}
              />
            </Field>
          </>
        )}
        {capture && (
          <>
            <p className="text-sm text-fg-muted">
              {recording
                ? t("Recording. {seconds}s elapsed.", { seconds: String(elapsed) })
                : t("Recording finished.")}
            </p>
            {recording && (
              <div className="space-y-2 text-sm">
                <p>
                  {t("On your device, reproduce the problem now. Keep recording until it happens.")}
                </p>
                <p>{t("Recording ends automatically after 10 minutes.")}</p>
                {elapsed < 30 && <p>{t("Wait at least 30 seconds before finishing.")}</p>}
                {!reproduced && <p>{t("Tick the box below to enable Finish.")}</p>}
                <label className="flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={reproduced}
                    onChange={(e) => setReproduced(e.currentTarget.checked)}
                  />
                  {t("I have reproduced the problem")}
                </label>
              </div>
            )}
            {!capture.deviceConfirmed && canSeal && discovery.length === 0 && (
              <p className="text-sm">
                {t(
                  "No app has contacted Ether during this recording. You can still finish; the bundle will include the server's side.",
                )}
              </p>
            )}
            {!capture.deviceConfirmed && recording && discovery.length > 0 && (
              <div className="space-y-2">
                <p className="text-sm">
                  {t("If you see the device that had the problem, you can select it here.")}
                </p>
                {discovery.map((d) => (
                  <Button
                    key={d.id}
                    variant="outline"
                    size="sm"
                    disabled={busy}
                    onClick={() => void choose(d.id)}
                  >
                    {[d.name, d.model, d.product, d.version].filter(Boolean).join(" · ") || d.id}
                  </Button>
                ))}
              </div>
            )}
            {!capture.deviceConfirmed && finished && (sealed || discovery.length > 0) && (
              <p className="text-sm">{t("Device not identified.")}</p>
            )}
            <div>
              <h3 className="mb-1 text-sm font-medium">{t("Evidence")}</h3>
              <ul className="space-y-1 text-sm">
                {(capture.observations ?? []).map((o, i) => (
                  <li key={`${o.at}-${o.kind}-${i}`}>
                    {o.kind}
                    {o.count ? ` ×${o.count}` : ""} {o.route ?? ""}
                  </li>
                ))}
                {(capture.observations ?? []).length === 0 && (
                  <li className="text-fg-muted">{t("No evidence yet.")}</li>
                )}
              </ul>
            </div>
            {hypotheses.length > 0 && (
              <div>
                <h3 className="mb-1 text-sm font-medium">{t("Hypotheses")}</h3>
                <ul className="space-y-1 text-sm text-fg-muted">
                  {hypotheses.map((h) => (
                    <li key={h.code}>{h.text}</li>
                  ))}
                </ul>
              </div>
            )}
            {canSeal && (
              <div className="space-y-2 text-sm">
                <p>
                  {t(
                    "These choices control the ZIP. The summary lists the exact values before download.",
                  )}
                </p>
                <label className="flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={titles}
                    onChange={(e) => setTitles(e.currentTarget.checked)}
                  />
                  {t("Include titles and source names")}
                </label>
                <label className="flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={usernames}
                    onChange={(e) => setUsernames(e.currentTarget.checked)}
                  />
                  {t("Include usernames")}
                </label>
                <label className="flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={addresses}
                    onChange={(e) => setAddresses(e.currentTarget.checked)}
                  />
                  {t("Include addresses")}
                </label>
              </div>
            )}
            {finished && capture.preview && (
              <div className="space-y-2 text-sm">
                <p>{capture.preview.identity.explanation}</p>
                {(capture.preview.identity.usernames ?? []).length > 0 && (
                  <p>
                    {t("Usernames")}: {capture.preview.identity.usernames?.join(", ")}
                  </p>
                )}
                {(capture.preview.identity.addresses ?? []).length > 0 && (
                  <p>
                    {t("Addresses")}: {capture.preview.identity.addresses?.join(", ")}
                  </p>
                )}
                <p className="text-fg-muted">{capture.preview.redaction}</p>
                <ul className="text-fg-muted">
                  {capture.coverage
                    .filter((g) => g.collector !== "device")
                    .map((g) => (
                      <li key={`${g.collector}-${g.status}`}>
                        {g.collector}: {g.detail}
                      </li>
                    ))}
                </ul>
              </div>
            )}
          </>
        )}
      </div>
    </Drawer>
  );
}

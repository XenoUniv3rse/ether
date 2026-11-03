import { Badge, Button, Dialog, Input, Switch, cn, toast } from "@braedonsaunders/appkit-ui";
import {
  CheckCircle2,
  ExternalLink,
  Loader2,
  Settings2,
  Sparkles,
  TriangleAlert,
} from "lucide-react";
import { Fragment, useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { ListInput, SecretInput } from "../../../components/fields";
import { t } from "../../../i18n";
import { apiFetch, errorMessage } from "../../../lib/api";
import { invalidateRegistry } from "../../../lib/extend";
import type { PluginDetail } from "../types";

interface ConfigField {
  key: string;
  label: string;
  kind: "text" | "number" | "switch" | "list" | "secret" | "select" | "fixed";
  value?: unknown;
  set?: boolean;
  options?: { value: string; label: string }[];
  required?: boolean;
  summary?: string;
  /** The nested setting this field belongs to, e.g. "Languages". */
  group?: string;
}

interface AddonConfig {
  configurable: boolean;
  required: boolean;
  configured: boolean;
  layer: "declared" | "decoded" | "page" | "none";
  fields: ConfigField[];
  host: string;
}

export interface ConfigResult {
  name: string;
  added?: string[];
  removed?: string[];
  roles: string[];
  sample?: string;
  problems?: string[];
}

const SAVED = "Saved";

/**
 * An add-on's own settings, inside Ether. Readable settings become native
 * fields; anything else opens the add-on's settings page in Ether and
 * picks up the link it makes. Every change is tried before it is saved.
 */
export function AddonSettings({ detail, onSaved }: { detail: PluginDetail; onSaved: () => void }) {
  const [cfg, setCfg] = useState<AddonConfig | null>(null);
  const [values, setValues] = useState<Record<string, unknown>>({});
  const [result, setResult] = useState<ConfigResult | null>(null);
  const [busy, setBusy] = useState<"" | "test" | "save">("");
  const [error, setError] = useState("");
  const [page, setPage] = useState(false);
  const base = `/plugins/${encodeURIComponent(detail.id)}/addon-config`;

  const load = useCallback(async () => {
    try {
      const c = await apiFetch<AddonConfig>(base);
      setCfg(c);
      const v: Record<string, unknown> = {};
      for (const f of c.fields) v[f.key] = f.kind === "secret" ? (f.set ? SAVED : "") : f.value;
      setValues(v);
      setResult(null);
    } catch {
      setCfg(null);
    }
  }, [base]);
  useEffect(() => {
    void load();
  }, [load]);

  if (!cfg || !cfg.configurable) return null;
  const native = cfg.layer === "declared" || cfg.layer === "decoded";

  const payload = () => {
    const out: Record<string, unknown> = {};
    for (const f of cfg.fields) {
      const v = values[f.key];
      out[f.key] = f.kind === "secret" && v === SAVED ? "" : v;
    }
    return { values: out };
  };

  const run = async (kind: "test" | "save", body: object) => {
    setBusy(kind);
    setError("");
    try {
      if (kind === "test") {
        setResult(
          await apiFetch<ConfigResult>(`${base}/test`, {
            method: "POST",
            body: JSON.stringify(body),
          }),
        );
      } else {
        await apiFetch(base, { method: "PUT", body: JSON.stringify(body) });
        invalidateRegistry();
        toast.success(t("Saved {name}'s settings", { name: detail.name }));
        await load();
        onSaved();
      }
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy("");
    }
  };

  return (
    <section
      aria-label={t("Add-on settings")}
      className="overflow-hidden rounded-xl border border-border bg-surface"
    >
      <header className="flex items-start gap-3 border-b border-border px-5 py-4">
        <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
          <Settings2 size={18} aria-hidden />
        </span>
        <div className="min-w-0 flex-1">
          <h3 className="text-sm font-semibold text-fg">{t("Add-on settings")}</h3>
          <p className="mt-0.5 text-sm text-fg-muted">
            {native
              ? t("Read from {host}. Changes are tried before they're saved.", { host: cfg.host })
              : t("{host} has its own settings page. Set it up here; Ether picks up the result.", {
                  host: cfg.host,
                })}
          </p>
        </div>
        {cfg.required && !cfg.configured ? (
          <Badge variant="warning">{t("Needs setting up")}</Badge>
        ) : cfg.configured ? (
          <Badge variant="success">
            <CheckCircle2 size={12} aria-hidden /> {t("Set up")}
          </Badge>
        ) : null}
      </header>

      {native ? (
        <div className="divide-y divide-border">
          {cfg.fields.map((f, i) => (
            <Fragment key={f.key}>
              {f.group && f.group !== cfg.fields[i - 1]?.group ? (
                <h4 className="bg-bg-subtle px-5 pb-1.5 pt-3 text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
                  {f.group}
                </h4>
              ) : null}
              <FieldRow
                field={f}
                value={values[f.key]}
                onChange={(v) => {
                  setValues((all) => ({ ...all, [f.key]: v }));
                  setResult(null);
                }}
              />
            </Fragment>
          ))}
        </div>
      ) : (
        <div className="px-5 py-4">
          <Button onClick={() => setPage(true)}>
            <ExternalLink size={14} /> {t("Open {host} settings", { host: cfg.host })}
          </Button>
        </div>
      )}

      {result ? <ResultCard result={result} /> : null}
      {error ? (
        <p role="alert" className="border-t border-border px-5 py-3 text-sm text-danger">
          {error}
        </p>
      ) : null}

      {native ? (
        <footer className="flex flex-wrap items-center gap-2 border-t border-border bg-bg-subtle px-5 py-3">
          <Button variant="ghost" size="sm" onClick={() => setPage(true)}>
            {t("Use {host}'s own page", { host: cfg.host })}
          </Button>
          <div className="ml-auto flex items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={busy !== ""}
              onClick={() => void run("test", payload())}
            >
              {busy === "test" ? <Loader2 size={13} className="animate-spin" /> : null}
              {t("Try it")}
            </Button>
            <Button size="sm" disabled={busy !== ""} onClick={() => void run("save", payload())}>
              {busy === "save" ? t("Saving…") : t("Save")}
            </Button>
          </div>
        </footer>
      ) : null}

      <AddonSettingsPage
        open={page}
        name={detail.name}
        host={cfg.host}
        openPage={() =>
          apiFetch<{ page: string }>(`${base}/page`, { method: "POST", body: "{}" }).then(
            (r) => r.page,
          )
        }
        tryLink={(link) =>
          apiFetch<ConfigResult>(`${base}/test`, { method: "POST", body: JSON.stringify({ link }) })
        }
        onUse={async (link) => {
          await apiFetch(base, { method: "PUT", body: JSON.stringify({ link }) });
          invalidateRegistry();
          toast.success(t("Saved {name}'s settings", { name: detail.name }));
          setPage(false);
          await load();
          onSaved();
        }}
        onClose={() => setPage(false)}
      />
    </section>
  );
}

function FieldRow({
  field,
  value,
  onChange,
}: {
  field: ConfigField;
  value: unknown;
  onChange: (v: unknown) => void;
}) {
  const id = `addon-cfg-${field.key}`;
  let control: ReactNode;
  switch (field.kind) {
    case "switch":
      control = (
        <Switch
          id={id}
          checked={Boolean(value)}
          onChange={(e) => onChange(e.currentTarget.checked)}
          aria-label={field.label}
        />
      );
      break;
    case "number":
      control = (
        <Input
          id={id}
          type="number"
          className="w-32"
          value={value === undefined || value === null ? "" : String(value)}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
      );
      break;
    case "list":
      control = (
        <ListInput
          id={id}
          ariaLabel={field.label}
          value={Array.isArray(value) ? (value as string[]) : []}
          onChange={(v) => onChange(v)}
        />
      );
      break;
    case "secret":
      control = (
        <SecretInput
          id={id}
          ariaLabel={field.label}
          value={String(value ?? "")}
          onChange={(v) => onChange(v)}
        />
      );
      break;
    case "select":
      control = (
        <select
          id={id}
          value={String(value ?? "")}
          onChange={(e) => onChange(e.currentTarget.value)}
          className="h-9 rounded-lg border border-border bg-surface px-2 text-sm text-fg"
        >
          {(field.options ?? []).map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      );
      break;
    case "fixed":
      control = <span className="text-sm text-fg-muted">{field.summary}</span>;
      break;
    default:
      control = (
        <Input
          id={id}
          value={String(value ?? "")}
          onChange={(e) => onChange(e.currentTarget.value)}
        />
      );
  }
  return (
    <div className="grid items-center gap-2 px-5 py-3 sm:grid-cols-[minmax(0,14rem)_minmax(0,1fr)]">
      <label htmlFor={id} className="text-sm font-medium text-fg">
        {field.label}
        {field.required ? <span className="text-danger"> *</span> : null}
      </label>
      <div
        className={cn("min-w-0", field.kind === "switch" && "flex justify-end sm:justify-start")}
      >
        {control}
      </div>
    </div>
  );
}

export function ResultCard({ result }: { result: ConfigResult }) {
  const ok = !result.problems?.length;
  return (
    <div className="space-y-2 border-t border-border bg-bg-subtle px-5 py-4" role="status">
      <p className="flex items-center gap-2 text-sm font-medium text-fg">
        {ok ? (
          <CheckCircle2 size={16} className="text-success" aria-hidden />
        ) : (
          <TriangleAlert size={16} className="text-warning" aria-hidden />
        )}
        {result.sample || (ok ? t("The add-on answers with these settings.") : result.problems![0])}
      </p>
      {result.added?.length || result.removed?.length ? (
        <div className="flex flex-wrap gap-1.5">
          {(result.added ?? []).map((c) => (
            <Badge key={`+${c}`} variant="success">
              + {c}
            </Badge>
          ))}
          {(result.removed ?? []).map((c) => (
            <Badge key={`-${c}`} variant="secondary">
              − {c}
            </Badge>
          ))}
        </div>
      ) : null}
    </div>
  );
}

/**
 * The add-on's own settings page, inside Ether. It runs sandboxed through
 * Ether's relay; when the page produces its install link (Install button,
 * copy, or shown on the page) Ether tries it and offers to save it.
 */
export function AddonSettingsPage({
  open,
  name,
  host,
  openPage,
  tryLink,
  onUse,
  onClose,
  useLabel,
}: {
  open: boolean;
  name: string;
  host: string;
  openPage: () => Promise<string>;
  tryLink?: (link: string) => Promise<ConfigResult>;
  onUse: (link: string) => Promise<void>;
  onClose: () => void;
  useLabel?: string;
}) {
  const [src, setSrc] = useState("");
  const [link, setLink] = useState("");
  const [candidate, setCandidate] = useState("");
  const [pasted, setPasted] = useState("");
  const [result, setResult] = useState<ConfigResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const frame = useRef<HTMLIFrameElement>(null);

  useEffect(() => {
    if (!open) return;
    setSrc("");
    setLink("");
    setCandidate("");
    setPasted("");
    setResult(null);
    setError("");
    void openPage()
      .then(setSrc)
      .catch((e) => setError(errorMessage(e)));
    // openPage is a fresh closure per render; the page opens once per open.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const take = useCallback(
    async (url: string) => {
      setLink(url);
      setResult(null);
      setError("");
      if (!tryLink) return;
      setBusy(true);
      try {
        setResult(await tryLink(url));
      } catch (e) {
        setError(errorMessage(e));
      } finally {
        setBusy(false);
      }
    },
    [tryLink],
  );

  useEffect(() => {
    if (!open) return;
    const onMessage = (e: MessageEvent) => {
      if (e.source !== frame.current?.contentWindow) return;
      const d = e.data as { source?: string; type?: string; url?: string };
      if (d?.source !== "ether-addon-relay" || typeof d.url !== "string") return;
      if (d.type === "link") void take(d.url);
      else if (d.type === "candidate") setCandidate(d.url);
    };
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, [open, take]);

  return (
    <Dialog
      open={open}
      onClose={onClose}
      size="xl"
      fullHeight
      title={t("{name} settings", { name })}
      description={t(
        "This is {host}'s own page. When it gives you an install link, Ether picks it up.",
        {
          host,
        },
      )}
      footer={
        <div className="flex w-full flex-wrap items-center gap-2">
          {link ? (
            <span className="flex min-w-0 flex-1 items-center gap-2 text-sm">
              {busy ? (
                <Loader2 size={14} className="animate-spin text-fg-muted" aria-hidden />
              ) : result?.problems?.length || error ? (
                <TriangleAlert size={14} className="text-warning" aria-hidden />
              ) : (
                <Sparkles size={14} className="text-primary" aria-hidden />
              )}
              <span className="truncate text-fg">
                {busy
                  ? t("Trying the new settings…")
                  : error ||
                    result?.sample ||
                    result?.problems?.[0] ||
                    t("Settings ready from {host}", { host })}
              </span>
            </span>
          ) : candidate ? (
            <span className="flex min-w-0 flex-1 items-center gap-2 text-sm text-fg-muted">
              {t("The page shows an install link.")}
              <Button size="sm" variant="outline" onClick={() => void take(candidate)}>
                {t("Use it")}
              </Button>
            </span>
          ) : (
            <form
              className="flex min-w-0 flex-1 items-center gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                if (pasted.trim()) void take(pasted.trim());
              }}
            >
              <Input
                aria-label={t("Or paste the install link")}
                placeholder={t("Or paste the install link it gives you")}
                value={pasted}
                onChange={(e) => setPasted(e.currentTarget.value)}
              />
              <Button type="submit" size="sm" variant="outline" disabled={!pasted.trim()}>
                {t("Use")}
              </Button>
            </form>
          )}
          <Button variant="ghost" onClick={onClose}>
            {t("Cancel")}
          </Button>
          <Button
            disabled={!link || busy || Boolean(error)}
            onClick={() => {
              setBusy(true);
              void onUse(link)
                .catch((e) => setError(errorMessage(e)))
                .finally(() => setBusy(false));
            }}
          >
            {useLabel ?? t("Save settings")}
          </Button>
        </div>
      }
    >
      {src ? (
        <iframe
          ref={frame}
          title={t("{host} settings page", { host })}
          src={src}
          sandbox="allow-scripts allow-forms allow-popups allow-modals"
          referrerPolicy="no-referrer"
          className="h-full min-h-[60vh] w-full flex-1 border-0 bg-white"
        />
      ) : (
        <div className="grid flex-1 place-items-center p-8 text-sm text-fg-muted">
          {error || (
            <span className="flex items-center gap-2">
              <Loader2 size={16} className="animate-spin" aria-hidden />{" "}
              {t("Opening {host}…", { host })}
            </span>
          )}
        </div>
      )}
    </Dialog>
  );
}

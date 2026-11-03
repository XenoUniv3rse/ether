import { Button, cn } from "@braedonsaunders/appkit-ui";
import {
  Bell,
  Bot,
  FileCode2,
  FileJson,
  Film,
  FolderOpen,
  LayoutTemplate,
  ListOrdered,
  PanelsTopLeft,
  Play,
  Tv,
  type LucideIcon,
} from "lucide-react";
import { Suspense, lazy, useEffect, useState } from "react";
import { t } from "../../i18n";
import { apiFetch, errorMessage } from "../../lib/api";
import { Reference } from "./CodeStudio";
import type { StudioProject } from "./develop/types";

const CodeMirrorPane = lazy(() => import("./code/CodeMirrorPane"));

interface Template {
  id: string;
  defaultName: string;
  files: Record<string, string>;
}

/** Starters in the order an owner most often wants them. */
const STARTERS: { id: string; icon: LucideIcon; title: () => string; about: () => string }[] = [
  {
    id: "channels",
    icon: Tv,
    title: () => t("Live TV channels"),
    about: () => t("Channels with a programme guide, shown in Live TV."),
  },
  {
    id: "video_source",
    icon: Film,
    title: () => t("Video sources"),
    about: () => t("Playable copies of catalogue titles, found when someone presses play."),
  },
  {
    id: "list",
    icon: LayoutTemplate,
    title: () => t("Home rows"),
    about: () => t("A list of titles for Collections and Home rows."),
  },
  {
    id: "local_files",
    icon: FolderOpen,
    title: () => t("Local files"),
    about: () => t("Files from folders you pick, attached to catalogue titles."),
  },
  {
    id: "notification",
    icon: Bell,
    title: () => t("Notifications"),
    about: () => t("Server alerts sent to a webhook or messaging service."),
  },
  {
    id: "ranking",
    icon: ListOrdered,
    title: () => t("Ranking step"),
    about: () => t("Scores releases while a quality profile picks one."),
  },
  {
    id: "page",
    icon: PanelsTopLeft,
    title: () => t("Console page"),
    about: () => t("A page or panel in this console."),
  },
  {
    id: "tool",
    icon: Bot,
    title: () => t("Assistant tool"),
    about: () => t("Something Ask Ether and connected AI apps can do."),
  },
];

const ENTRY = "src/plugin.ts";

/**
 * Code view with no draft open: the editor straight away. Pick a starter,
 * read and change its code, and the first save turns it into a draft.
 */
export function CodeHome({ onOpenProject }: { onOpenProject: (id: string) => void }) {
  const [templates, setTemplates] = useState<Template[]>([]);
  const [starter, setStarter] = useState(STARTERS[0]!.id);
  const [file, setFile] = useState(ENTRY);
  const [edits, setEdits] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    apiFetch<{ templates: Template[] }>("/plugins/studio/templates")
      .then((r) => setTemplates(r.templates))
      .catch((e) => setError(errorMessage(e)));
  }, []);

  const tpl = templates.find((x) => x.id === starter);
  const files = Object.keys(tpl?.files ?? {}).sort((a, b) =>
    a === ENTRY ? -1 : b === ENTRY ? 1 : a.localeCompare(b),
  );
  const current = edits[file] ?? tpl?.files[file] ?? "";
  const meta = STARTERS.find((s) => s.id === starter)!;

  const pick = (id: string) => {
    if (id === starter) return;
    if (Object.keys(edits).length && !window.confirm(t("Discard your changes to this starter?")))
      return;
    setStarter(id);
    setEdits({});
    setFile(ENTRY);
  };

  // The first save makes the draft: the starter, plus whatever was typed.
  const save = async () => {
    if (!tpl || busy) return;
    setBusy(true);
    setError("");
    try {
      const r = await apiFetch<{ project: StudioProject }>("/plugins/studio/projects", {
        method: "POST",
        body: JSON.stringify({ template: tpl.id, name: meta.title() }),
      });
      if (Object.keys(edits).length)
        await apiFetch(`/plugins/studio/projects/${encodeURIComponent(r.project.id)}/files`, {
          method: "PUT",
          body: JSON.stringify({ files: edits }),
        });
      onOpenProject(r.project.id);
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
    }
  };

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "s") {
        e.preventDefault();
        void save();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="grid min-h-0 flex-1 grid-cols-1 lg:grid-cols-[15rem_minmax(0,1fr)_minmax(320px,26rem)]">
        <aside
          aria-label={t("Start from")}
          className="min-h-0 overflow-y-auto border-r border-border bg-bg-subtle"
        >
          <p className="px-3 pb-1 pt-3 text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
            {t("Start from")}
          </p>
          <ul className="space-y-0.5 px-1.5 pb-2">
            {STARTERS.map((s) => (
              <li key={s.id}>
                <button
                  type="button"
                  onClick={() => pick(s.id)}
                  aria-current={starter === s.id ? "true" : undefined}
                  className={cn(
                    "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm transition",
                    starter === s.id
                      ? "bg-primary/10 text-fg"
                      : "text-fg-muted hover:bg-surface-hover hover:text-fg",
                  )}
                >
                  <s.icon size={15} className="shrink-0" aria-hidden />
                  <span className="truncate">{s.title()}</span>
                </button>
              </li>
            ))}
          </ul>
          {files.length ? (
            <>
              <p className="border-t border-border px-3 pb-1 pt-3 text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
                {t("Files")}
              </p>
              <ul className="space-y-0.5 px-1.5 pb-3">
                {files.map((f) => (
                  <li key={f}>
                    <button
                      type="button"
                      onClick={() => setFile(f)}
                      aria-current={file === f ? "page" : undefined}
                      className={cn(
                        "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left font-mono text-xs transition",
                        file === f
                          ? "bg-primary/10 text-fg"
                          : "text-fg-muted hover:bg-surface-hover hover:text-fg",
                      )}
                    >
                      {f.endsWith(".ts") ? <FileCode2 size={14} /> : <FileJson size={14} />}
                      <span className="min-w-0 flex-1 truncate">{f}</span>
                      {f in edits ? (
                        <span
                          className="size-1.5 rounded-full bg-primary"
                          aria-label={t("Not saved")}
                        />
                      ) : null}
                    </button>
                  </li>
                ))}
              </ul>
            </>
          ) : null}
        </aside>

        <section
          aria-label={t("Code")}
          className="flex min-h-[32rem] flex-col bg-surface lg:min-h-0"
        >
          <div className="flex shrink-0 items-center gap-2 border-b border-border bg-bg-subtle px-3 py-1.5">
            <span className="font-mono text-xs text-fg">{file}</span>
            <span className="ml-auto text-xs text-fg-muted">
              {t("Starter code. Saving makes it your draft.")}
            </span>
            <Button size="sm" disabled={busy || !tpl} onClick={() => void save()}>
              <Play size={13} /> {busy ? t("Creating…") : t("Save and run")}
            </Button>
          </div>
          <div className="min-h-0 flex-1">
            {tpl ? (
              <Suspense
                fallback={<p className="p-4 text-xs text-fg-muted">{t("Loading editor…")}</p>}
              >
                <CodeMirrorPane
                  key={`${starter}:${file}`}
                  file={file === ENTRY ? "plugin.ts" : file}
                  value={current}
                  onChange={(v) => setEdits((all) => ({ ...all, [file]: v }))}
                />
              </Suspense>
            ) : (
              <p className="p-4 text-xs text-fg-muted">{t("Loading editor…")}</p>
            )}
          </div>
          <div className="flex shrink-0 items-center gap-3 border-t border-border bg-bg-subtle px-3 py-1 text-[11px] text-fg-muted">
            <span>{file.endsWith(".ts") ? "TypeScript" : "JSON"}</span>
            <span>⌘S {t("saves, compiles and refreshes the preview")}</span>
          </div>
          {error ? (
            <p role="alert" className="border-t border-border px-3 py-2 text-xs text-danger">
              {error}
            </p>
          ) : null}
        </section>

        <section
          aria-label={t("Tools")}
          className="min-h-0 overflow-y-auto border-t border-border p-4 lg:border-l lg:border-t-0"
        >
          <div className="mb-5 flex items-start gap-3">
            <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <meta.icon size={18} aria-hidden />
            </span>
            <div>
              <h2 className="text-sm font-semibold text-fg">{meta.title()}</h2>
              <p className="text-sm text-fg-muted">{meta.about()}</p>
            </div>
          </div>
          <Reference template={starter} />
        </section>
      </div>
    </div>
  );
}

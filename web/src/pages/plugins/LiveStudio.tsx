import { AgentPanel, type AgentMessage } from "@braedonsaunders/appkit-ai/react";
import { Badge, Button, Dialog, SearchSelect, cn, toast } from "@braedonsaunders/appkit-ui";
import { Suspense, lazy, useCallback, useEffect, useRef, useState } from "react";
import { FileCode2, FileJson, Film, LayoutTemplate, Sparkles, Tv } from "lucide-react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { Segmented } from "../../components/fields";
import { Workspace } from "../../components/workspace";
import {
  decideAgentApproval,
  fetchAgentMessages,
  fetchAgentStatus,
} from "../../components/agent/agentApi";
import { approvalRequestLabels } from "../../components/agent/approvalLabels";
import { apiFetch, apiStream, errorMessage } from "../../lib/api";
import { PluginView } from "../../viewspec/PluginView";
import { formatDateTime } from "../../lib/format";
import { invalidateRegistry } from "../../lib/extend";
import { PermissionSheet } from "./PermissionSheet";
import { Composer } from "./Composer";
import { LiveStageView, NativePreview, momentsForTool, readAgentStream } from "./LiveStage";
import type { Moment } from "./Stage";
import type { InstallPreview } from "./types";
import { t } from "../../i18n";
import { searchSelectLabels } from "../../i18n/appkit-labels";
import type { StudioProject, StudioProjectSummary, StudioCompileResult } from "./develop/types";

/**
 * Studio on the real agent: the approved home, a live stage that draws each
 * agent step with real results, then the editor for refinements and install.
 */
export default function LiveStudio() {
  const [params, setParams] = useSearchParams();
  const mode: StudioMode = params.get("view") === "code" ? "code" : "ai";
  const opened = params.get("project") ?? undefined;
  const [phase, setPhase] = useState<"home" | "stage" | "editor">(() =>
    opened || sessionStorage.getItem("ether-studio-thread") ? "editor" : "home",
  );
  const [creating, setCreating] = useState(false);
  const setMode = (next: StudioMode, project?: string) => {
    const q = new URLSearchParams(params);
    if (next === "code") q.set("view", "code");
    else q.delete("view");
    if (project) q.set("project", project);
    setParams(q, { replace: true });
  };
  /** Code-first: start a draft straight from a starter template, no agent. */
  const startFromTemplate = async (template: string, name: string) => {
    setCreating(true);
    try {
      const r = await apiFetch<{ project: StudioProject }>("/plugins/studio/projects", {
        method: "POST",
        body: JSON.stringify({ template, name }),
      });
      sessionStorage.removeItem("ether-studio-thread");
      setMode("code", r.project.id);
      setPhase("editor");
    } catch (e) {
      setStageError(errorMessage(e));
    } finally {
      setCreating(false);
    }
  };
  const [prompt, setPrompt] = useState("");
  const [moments, setMoments] = useState<Moment[]>([]);
  const [narration, setNarration] = useState("");
  const [stageError, setStageError] = useState("");
  const abort = useRef<AbortController | null>(null);

  const build = async (text: string) => {
    const ask = text.trim();
    if (!ask) return;
    setPrompt("");
    setStageError("");
    setNarration("");
    setMoments([{ kind: "intent", text: ask, tool: "studio" }]);
    setPhase("stage");
    sessionStorage.removeItem("ether-studio-thread");
    const ctl = new AbortController();
    abort.current = ctl;
    const calls = new Map<string, { name: string; input: Record<string, unknown> }>();
    let said = "";
    try {
      const res = await apiStream("/agent/turn", {
        method: "POST",
        signal: ctl.signal,
        body: JSON.stringify({ prompt: ask, context: { route: "/plugins/studio" } }),
      });
      const id = res.headers.get("X-Ether-Thread-Id");
      if (id) sessionStorage.setItem("ether-studio-thread", id);
      const pending: Promise<void>[] = [];
      await readAgentStream(res, (c) => {
        if (c.type === "text-delta" && c.delta) {
          said += c.delta;
          setNarration(said.split(/(?<=[.!?])\s/)[0] ?? said);
        } else if (c.type === "tool-input-available" && c.toolCallId && c.toolName) {
          calls.set(c.toolCallId, { name: c.toolName, input: c.input ?? {} });
        } else if (c.type === "tool-output-available" && c.toolCallId) {
          const call = calls.get(c.toolCallId);
          if (call)
            pending.push(
              momentsForTool(call.name, call.input, c.output).then((m) =>
                setMoments((all) => [...all, ...m]),
              ),
            );
        } else if (c.type === "error" && c.errorText) {
          setStageError(c.errorText);
        }
      });
      await Promise.all(pending);
      const summary = said.trim().split(/(?<=[.!?])\s/)[0];
      setMoments((all) => [...all, { kind: "done", text: summary || t("Your draft is ready") }]);
      window.setTimeout(() => setPhase("editor"), 1800);
    } catch (e) {
      if (!ctl.signal.aborted) setStageError(errorMessage(e));
      setPhase("editor");
    }
  };

  if (phase === "editor")
    return (
      <LiveWorkspace
        key={opened ?? "agent"}
        mode={mode}
        onMode={(m) => setMode(m)}
        initialProject={opened}
        onNew={() => {
          sessionStorage.removeItem("ether-studio-thread");
          setMoments([]);
          const q = new URLSearchParams(params);
          q.delete("project");
          setParams(q, { replace: true });
          setPhase("home");
        }}
      />
    );
  return (
    <Workspace title={t("Studio")} actions={<Link to="/plugins">{t("Plugins")}</Link>}>
      <div className="flex min-h-0 flex-1 flex-col">
        <div className="app-scroll min-h-0 flex-1 overflow-y-auto">
          {phase === "stage" ? (
            <>
              <LiveStageView moments={moments} narration={narration} />
              {stageError ? (
                <p role="alert" className="mx-auto max-w-4xl px-6 text-sm text-danger">
                  {stageError}
                </p>
              ) : null}
            </>
          ) : (
            <LiveHome
              mode={mode}
              busy={creating}
              onMode={(m) => setMode(m)}
              onPick={(starter) =>
                mode === "code"
                  ? void startFromTemplate(starter.template, starter.title())
                  : void build(starter.prompt())
              }
            />
          )}
          {phase === "home" && stageError ? (
            <p role="alert" className="mx-auto max-w-3xl px-4 text-sm text-danger">
              {stageError}
            </p>
          ) : null}
        </div>
        {mode === "code" && phase === "home" ? null : (
          <Composer
            value={prompt}
            onChange={setPrompt}
            onSend={(text) => void build(text)}
            working={phase === "stage"}
            onStop={() => {
              abort.current?.abort();
              setPhase("editor");
            }}
            placeholder={
              phase === "stage"
                ? t("Building. You can add to it as it goes")
                : t("Describe the plugin you want")
            }
          />
        )}
      </div>
    </Workspace>
  );
}

type StudioMode = "ai" | "code";

/** Chat and Code are two views of the same Studio and the same draft. */
function ModeSwitch({ mode, onMode }: { mode: StudioMode; onMode: (m: StudioMode) => void }) {
  return (
    <Segmented
      ariaLabel={t("Studio view")}
      value={mode}
      onChange={(v) => onMode(v as StudioMode)}
      options={[
        { value: "ai", label: t("Describe") },
        { value: "code", label: t("Code") },
      ]}
    />
  );
}

const STARTERS = [
  {
    template: "channels",
    icon: Tv,
    title: () => t("Live TV channels"),
    description: () => t("Free channels with a programme guide, straight into Live TV."),
    prompt: () => t("Add free live channels to Live TV, with a programme guide"),
  },
  {
    template: "video_source",
    icon: Film,
    title: () => t("Video sources"),
    description: () => t("Playable copies of titles in your catalogue, found when you press play."),
    prompt: () => t("Find free, legal copies of classic films for movies in my catalogue"),
  },
  {
    template: "list",
    icon: LayoutTemplate,
    title: () => t("Home rows"),
    description: () => t("A row or collection on Home built from an outside list."),
    prompt: () => t("Make a Home row from an external list of films"),
  },
];

/** The approved Studio home: one question, ideas, and the native starters. */
function LiveHome({
  mode,
  busy,
  onMode,
  onPick,
}: {
  mode: StudioMode;
  busy: boolean;
  onMode: (m: StudioMode) => void;
  onPick: (starter: (typeof STARTERS)[number]) => void;
}) {
  return (
    <div className="mx-auto max-w-3xl space-y-9 px-4 pb-10 pt-[min(10vh,5rem)]">
      <div className="space-y-4 text-center">
        <span className="mx-auto flex size-12 items-center justify-center rounded-2xl bg-primary/10 text-primary">
          <Sparkles size={24} aria-hidden />
        </span>
        <h2 className="text-2xl font-semibold tracking-tight text-fg">
          {t("What should Ether do?")}
        </h2>
        <p className="text-sm text-fg-muted">
          {mode === "code"
            ? t(
                "Pick a starting point. You get the code, live data and a preview; nothing changes until you install.",
              )
            : t("It builds here, tested on your server. Nothing changes until you install.")}
        </p>
        <div className="flex justify-center">
          <ModeSwitch mode={mode} onMode={onMode} />
        </div>
      </div>
      <section aria-label={t("Start from")} className="grid gap-3 sm:grid-cols-3">
        {STARTERS.map((c) => (
          <button
            key={c.template}
            type="button"
            disabled={busy}
            onClick={() => onPick(c)}
            className="group flex flex-col items-start gap-3 rounded-xl border border-border bg-surface p-4 text-left transition hover:border-primary/40 hover:shadow-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            <span className="flex size-10 items-center justify-center rounded-lg bg-primary/10 text-primary transition group-hover:scale-105">
              <c.icon size={20} aria-hidden />
            </span>
            <span>
              <span className="block text-sm font-semibold text-fg">{c.title()}</span>
              <span className="mt-0.5 block text-sm text-fg-muted">{c.description()}</span>
            </span>
          </button>
        ))}
      </section>
    </div>
  );
}

/** The editor: conversation for refinements, the draft's native preview, code and install. */
function LiveWorkspace({
  onNew,
  mode,
  onMode,
  initialProject,
}: {
  onNew: () => void;
  mode: StudioMode;
  onMode: (m: StudioMode) => void;
  initialProject?: string;
}) {
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const thread = useRef<string | undefined>(
    sessionStorage.getItem("ether-studio-thread") || undefined,
  );
  const [projects, setProjects] = useState<StudioProjectSummary[]>([]);
  const [project, setProject] = useState<StudioProject | null>(null);
  const [file, setFile] = useState("src/plugin.ts");
  const [source, setSource] = useState("");
  const [edits, setEdits] = useState<Record<string, string>>({});
  const [dirty, setDirty] = useState(false);
  const [enabled, setEnabled] = useState(false);
  const [model, setModel] = useState("");
  const [error, setError] = useState("");
  const [epoch, setEpoch] = useState(0);
  const [saving, setSaving] = useState(false);
  const chosen = useRef<string | undefined>(initialProject);
  const stamps = useRef<Record<string, string>>({});
  const dirtyRef = useRef(false);
  dirtyRef.current = dirty;
  const loadProject = useCallback(async (id: string) => {
    const r = await apiFetch<{ project: StudioProject }>(
      `/plugins/studio/projects/${encodeURIComponent(id)}`,
    );
    setProject(r.project);
    chosen.current = id;
    if (!dirtyRef.current) {
      setFile("src/plugin.ts");
      setSource(r.project.files["src/plugin.ts"] ?? "");
      setEdits({});
    }
    setEpoch((e) => e + 1);
  }, []);
  const refresh = useCallback(async () => {
    if (thread.current) {
      const r = await fetchAgentMessages(thread.current);
      setMessages(r.messages);
    }
    const r = await apiFetch<{ projects: StudioProjectSummary[] }>("/plugins/studio/projects");
    setProjects(r.projects);
    // Follow the draft the agent just created or changed; otherwise keep the
    // owner's pick. First load opens the most recently edited draft.
    const latest = [...r.projects].sort(
      (a, b) => Date.parse(b.updatedAt) - Date.parse(a.updatedAt),
    );
    const changed = latest.find((p) => stamps.current[p.id] !== p.updatedAt);
    const first = Object.keys(stamps.current).length === 0;
    stamps.current = Object.fromEntries(r.projects.map((p) => [p.id, p.updatedAt]));
    const follow = !dirtyRef.current && (first ? (initialProject ?? latest[0]?.id) : changed?.id);
    const id = follow || chosen.current || latest[0]?.id;
    if (id) await loadProject(id);
  }, [loadProject]);
  useEffect(() => {
    void fetchAgentStatus()
      .then((s) => {
        setEnabled(s.enabled);
        setModel(s.model || "");
      })
      .catch((e) => setError(errorMessage(e)));
    void refresh().catch((e) => setError(errorMessage(e)));
  }, [refresh]);
  const send = async (prompt: string, signal: AbortSignal) => {
    const response = await apiStream("/agent/turn", {
      method: "POST",
      signal,
      body: JSON.stringify({
        threadId: thread.current,
        prompt,
        context: {
          route: "/plugins/studio",
          record: project ? { id: project.id, type: "studio_project" } : undefined,
        },
      }),
    });
    const id = response.headers.get("X-Ether-Thread-Id");
    if (id) {
      thread.current = id;
      sessionStorage.setItem("ether-studio-thread", id);
    }
    if (!response.ok || !response.body) return response;
    const reader = response.body.getReader();
    return new Response(
      new ReadableStream<Uint8Array>({
        async pull(controller) {
          try {
            const r = await reader.read();
            if (r.done) {
              controller.close();
              void refresh().catch((e) => setError(errorMessage(e)));
            } else controller.enqueue(r.value);
          } catch (e) {
            controller.error(e);
          }
        },
        cancel(reason) {
          return reader.cancel(reason);
        },
      }),
      { status: response.status, headers: response.headers },
    );
  };
  const navigate = useNavigate();
  const [installing, setInstalling] = useState<InstallPreview | null>(null);
  const [grantViewers, setGrantViewers] = useState<number[]>([]);
  const [grantFolders, setGrantFolders] = useState<string[]>([]);
  const [installBusy, setInstallBusy] = useState(false);
  const [installError, setInstallError] = useState("");
  const openInstall = async () => {
    if (!project) return;
    setInstallError("");
    setGrantViewers([]);
    setGrantFolders([]);
    try {
      const r = await apiFetch<{ preview: InstallPreview }>(
        `/plugins/studio/projects/${encodeURIComponent(project.id)}/install/preview`,
        { method: "POST", body: "{}" },
      );
      setInstalling(r.preview);
    } catch (e) {
      setError(errorMessage(e));
    }
  };
  const install = async () => {
    if (!project || !installing) return;
    setInstallBusy(true);
    setInstallError("");
    try {
      const r = await apiFetch<{ plugin: { id: string; name: string } }>(
        `/plugins/studio/projects/${encodeURIComponent(project.id)}/install`,
        {
          method: "POST",
          body: JSON.stringify({
            grants: { viewers: grantViewers, folders: grantFolders },
            acceptAddedPermissions: true,
          }),
        },
      );
      setInstalling(null);
      invalidateRegistry();
      toast.success(t("{name} is installed", { name: r.plugin.name }));
      navigate("/plugins/installed");
    } catch (e) {
      setInstallError(errorMessage(e));
    } finally {
      setInstallBusy(false);
    }
  };
  const save = async () => {
    if (!project) return;
    setSaving(true);
    setError("");
    try {
      const r = await apiFetch<{ project: StudioProject; compile: StudioCompileResult }>(
        `/plugins/studio/projects/${encodeURIComponent(project.id)}/files`,
        { method: "PUT", body: JSON.stringify({ files: { ...edits, [file]: source } }) },
      );
      setProject(r.project);
      setEdits({});
      setDirty(false);
      setEpoch((e) => e + 1);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  };
  return (
    <Workspace
      title={t("Studio")}
      actions={
        <>
          <ModeSwitch mode={mode} onMode={onMode} />
          <Button variant="outline" size="sm" onClick={onNew}>
            <Sparkles size={14} /> {t("New plugin")}
          </Button>
          <Link to="/plugins">{t("Plugins")}</Link>
        </>
      }
    >
      <div
        className={
          mode === "code"
            ? "grid min-h-0 flex-1 grid-cols-1 lg:grid-cols-[minmax(0,1.3fr)_minmax(320px,0.7fr)]"
            : "grid min-h-0 flex-1 grid-cols-1 lg:grid-cols-[minmax(320px,0.85fr)_minmax(0,1.15fr)]"
        }
      >
        {mode === "code" ? (
          <CodePane
            project={project}
            file={file}
            source={source}
            edits={edits}
            dirty={dirty}
            saving={saving}
            onFile={(f) => {
              if (!project) return;
              const next = { ...edits };
              if (dirty) next[file] = source;
              setEdits(next);
              setFile(f);
              setSource(next[f] ?? project.files[f] ?? "");
            }}
            onChange={(v) => {
              setSource(v);
              setDirty(true);
            }}
            onSave={() => void save()}
          />
        ) : (
          <section
            className="order-2 flex min-h-[500px] flex-col border-t border-border lg:order-1 lg:border-r lg:border-t-0"
            aria-label={t("Studio conversation")}
          >
            <div className="flex items-center gap-2 border-b border-border p-3">
              <Badge>{t("Live model")}</Badge>
              <span className="text-xs text-fg-muted">{model || t("Model unavailable")}</span>
            </div>
            <AgentPanel
              enabled={enabled}
              initialMessages={messages}
              send={send}
              suggestions={[
                t("Add free live channels to Live TV, with a programme guide"),
                t("Find free, legal copies of classic films for movies in my catalogue"),
                t("Make a Home row from an external list of films"),
              ]}
              labels={{
                title: t("Build with Ether"),
                welcomeTitle: t("Describe your plugin"),
                welcomeDescription: t(
                  "Real model responses and saved drafts. Review source changes before applying them.",
                ),
                disabledTitle: t("Connect a model"),
              }}
              approvalRequestLabels={approvalRequestLabels()}
              onDecideApprovalRequest={async (id, decision, note) => {
                await decideAgentApproval(id, decision, note, {
                  route: "/plugins/studio",
                  record: project ? { id: project.id, type: "studio_project" } : undefined,
                });
                await refresh();
              }}
            />
          </section>
        )}
        <section
          className="order-1 min-h-[420px] overflow-auto p-5 lg:order-2"
          aria-label={t("Draft workspace")}
        >
          {error ? (
            <p role="alert" className="mb-4 text-danger">
              {error}
            </p>
          ) : null}
          <div className="mb-4 flex flex-wrap items-center gap-2">
            <SearchSelect
              value={project?.id || ""}
              onChange={(id) => {
                if (!dirty) void loadProject(id).catch((e) => setError(errorMessage(e)));
              }}
              options={projects.map((p) => ({ value: p.id, label: p.name }))}
              ariaLabel={t("Saved project")}
              disabled={dirty}
              {...searchSelectLabels()}
            />
            {project ? (
              <Button
                className="ml-auto"
                disabled={dirty || (project.diagnostics || []).length > 0}
                onClick={() => void openInstall()}
              >
                {t("Install")}
              </Button>
            ) : null}
          </div>
          {project ? (
            <>
              <h2 className="text-lg font-semibold">{project.name}</h2>
              <p className="mb-4 text-xs text-fg-muted">
                {dirty ? t("Unsaved draft") : t("Saved on server")} ·{" "}
                {formatDateTime(project.updatedAt)}
              </p>
              {(project.diagnostics || []).map((d, i) => (
                <p key={i} className="mb-2 text-sm text-danger">
                  {d.file}:{d.line} {d.text}
                </p>
              ))}
              {project.defaultPage ? (
                <div key={`${project.id}:${epoch}`} className="space-y-4">
                  <NativePreview projectId={project.id} template={project.template} />
                  <div className="rounded-xl border border-border p-4">
                    <PluginView plugin={`studio:${project.id}`} page={project.defaultPage} />
                  </div>
                </div>
              ) : (
                <p>{t("This draft has no page preview. Switch to Code to see how it works.")}</p>
              )}
              <p className="mt-4 text-xs text-fg-muted">
                {t(
                  "Preview runs the saved draft in the Studio sandbox. It does not install the plugin.",
                )}
              </p>
            </>
          ) : (
            <div className="rounded-xl border border-dashed border-border p-8">
              <h2 className="font-medium">{t("Your plugin will appear here")}</h2>
              <p className="mt-2 text-sm text-fg-muted">
                {t(
                  "Start with a description. Approved draft changes are saved and compiled by Ether.",
                )}
              </p>
            </div>
          )}
        </section>
      </div>
      {installing ? (
        <Dialog
          open
          onClose={() => setInstalling(null)}
          title={t("Install {name}", { name: installing.manifest.name })}
          footer={
            <>
              <Button variant="ghost" onClick={() => setInstalling(null)}>
                {t("Cancel")}
              </Button>
              <Button disabled={installBusy} onClick={() => void install()}>
                {installBusy
                  ? t("Installing…")
                  : installing.alreadyInstalled
                    ? t("Update")
                    : t("Install")}
              </Button>
            </>
          }
        >
          <div className="space-y-4">
            <p className="text-sm text-fg-muted">
              {installing.alreadyInstalled
                ? t("This replaces version {version} with the saved draft.", {
                    version: installing.installedVersion ?? "",
                  })
                : t("The saved draft becomes a real plugin on this server.")}
            </p>
            <PermissionSheet
              permissions={installing.permissions}
              verified={installing.verified}
              needsViewers={installing.needsViewers}
              needsFolders={installing.needsFolders}
              viewers={grantViewers}
              onChangeViewers={setGrantViewers}
              folders={grantFolders}
              onChangeFolders={setGrantFolders}
              addedPermissions={installing.addedPermissions}
              ownDraft
            />
            {installError ? (
              <p role="alert" className="text-sm text-danger">
                {installError}
              </p>
            ) : null}
          </div>
        </Dialog>
      ) : null}
    </Workspace>
  );
}

const CodeMirrorPane = lazy(() => import("./code/CodeMirrorPane"));

/** The code view: every draft file, a real editor with plugin types, and save that compiles and checks. */
function CodePane({
  project,
  file,
  source,
  edits,
  dirty,
  saving,
  onFile,
  onChange,
  onSave,
}: {
  project: StudioProject | null;
  file: string;
  source: string;
  edits: Record<string, string>;
  dirty: boolean;
  saving: boolean;
  onFile: (f: string) => void;
  onChange: (v: string) => void;
  onSave: () => void;
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "s") {
        e.preventDefault();
        if (dirty || Object.keys(edits).length) onSave();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [dirty, edits, onSave]);
  if (!project) {
    return (
      <section className="flex items-center justify-center border-r border-border p-8 text-sm text-fg-muted">
        {t("Loading the draft…")}
      </section>
    );
  }
  const files = Object.keys(project.files).sort((a, b) =>
    a === "src/plugin.ts" ? -1 : b === "src/plugin.ts" ? 1 : a.localeCompare(b),
  );
  const changed = (f: string) => (f === file ? dirty : f in edits);
  const diags = project.diagnostics || [];
  return (
    <section
      aria-label={t("Code")}
      className="flex min-h-[32rem] flex-col border-b border-border bg-surface lg:min-h-0 lg:border-b-0 lg:border-r"
    >
      <div className="flex shrink-0 flex-wrap items-center gap-1 border-b border-border bg-bg-subtle px-2">
        {files.map((f) => (
          <button
            key={f}
            type="button"
            onClick={() => onFile(f)}
            aria-current={file === f ? "page" : undefined}
            className={cn(
              "-mb-px inline-flex h-10 shrink-0 items-center gap-1.5 border-b-2 px-3 font-mono text-xs transition",
              file === f
                ? "border-primary bg-surface text-fg"
                : "border-transparent text-fg-muted hover:text-fg",
            )}
          >
            {f.endsWith(".ts") ? <FileCode2 size={14} /> : <FileJson size={14} />}
            {f}
            {changed(f) ? (
              <span className="size-1.5 rounded-full bg-primary" aria-label={t("Not saved")} />
            ) : null}
          </button>
        ))}
        <Button
          size="sm"
          className="ml-auto"
          disabled={saving || !(dirty || Object.keys(edits).length)}
          onClick={onSave}
        >
          {saving ? t("Saving…") : t("Save and run")}
        </Button>
      </div>
      <div className="min-h-0 flex-1">
        <Suspense fallback={<p className="p-4 text-xs text-fg-muted">{t("Loading editor…")}</p>}>
          <CodeMirrorPane
            key={file}
            file={file === "src/plugin.ts" ? "plugin.ts" : file}
            value={source}
            onChange={onChange}
          />
        </Suspense>
      </div>
      <div className="shrink-0 border-t border-border bg-bg-subtle px-3 py-1.5 text-xs">
        {diags.length ? (
          <ul className="space-y-0.5 text-danger">
            {diags.map((d, i) => (
              <li key={i} className="font-mono">
                {d.file}:{d.line} {d.text}
              </li>
            ))}
          </ul>
        ) : (
          <span className="text-fg-muted">
            {dirty || Object.keys(edits).length
              ? t("Unsaved changes · Save and run to compile, check and refresh the preview")
              : t("Saved · compiles cleanly · preview is live")}
          </span>
        )}
      </div>
    </section>
  );
}

import { AgentPanel, type AgentMessage } from "@braedonsaunders/appkit-ai/react";
import { Alert, AlertDescription, AlertTitle, Button, toast } from "@braedonsaunders/appkit-ui";
import { Sparkles } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Drawer } from "../../components/Drawer";
import { Segmented } from "../../components/fields";
import {
  agentPanelLabels,
  cancelAgentTurn,
  deleteAgentThread,
  fetchAgentMessages,
  fetchAgentStatus,
} from "../../components/agent/agentApi";
import { localized, t, tx } from "../../i18n";
import { useLocale } from "../../i18n/react";
import { api, ApiError, apiStream, errorMessage } from "../../lib/api";
import type { CollectionSpec } from "../../lib/contract";
import { registryDescriptors } from "../../lib/extend";
import { normalizeSpec, type SourceDescriptor } from "../../lib/rules";
import {
  LiveStageView,
  collectionSteps,
  momentsForTool,
  readAgentStream,
} from "../plugins/LiveStage";
import type { Moment } from "../plugins/Stage";
import { TitleInspector } from "../catalog/TitleInspector";

/** The agent route whose turns build one collection and outlive a reload. */
const ROUTE = "/collections/describe";
/** The conversation a reload rejoins. */
const THREAD_KEY = "ether-describe-thread";
const POLL_MS = 1500;

type Media = "movie" | "show" | "mixed";

const EXAMPLES = localized(() => [
  t("Cosy British mysteries under two hours"),
  t("Mind-bending sci-fi from the 90s"),
  t("Acclaimed animated films the whole family will like"),
  t("Slow-burn horror with great reviews"),
]);

interface ToolStep {
  name: string;
  input: Record<string, unknown>;
  output?: Record<string, unknown>;
}

type Entry = { intent: string } | { tool: ToolStep };

/** The owner's asks and the finished tool calls of a stored transcript, in order. */
function transcriptEntries(messages: AgentMessage[]): Entry[] {
  const out: Entry[] = [];
  for (const message of messages) {
    for (const raw of message.parts) {
      const part = (raw ?? {}) as Record<string, unknown>;
      const type = typeof part.type === "string" ? part.type : "";
      if (message.role === "user" && type === "text" && typeof part.text === "string") {
        out.push({ intent: part.text });
      } else if (message.role === "assistant" && part.output && typeof part.output === "object") {
        const name =
          type === "dynamic-tool"
            ? String(part.toolName ?? "")
            : type.startsWith("tool-")
              ? type.slice(5)
              : "";
        if (name)
          out.push({
            tool: {
              name,
              input: (part.input ?? {}) as Record<string, unknown>,
              output: part.output as Record<string, unknown>,
            },
          });
      }
    }
  }
  return out;
}

function draftOf(step: ToolStep): CollectionSpec | null {
  const spec = step.output?.spec as Partial<CollectionSpec> | undefined;
  return step.name === "collection_draft" && spec ? normalizeSpec(spec) : null;
}

/** Rebuilds the stage from a stored transcript; only the latest draft fetches posters. */
async function replay(
  messages: AgentMessage[],
): Promise<{ moments: Moment[]; spec: CollectionSpec | null }> {
  const entries = transcriptEntries(messages);
  let last = -1;
  entries.forEach((e, i) => {
    if ("tool" in e && draftOf(e.tool)) last = i;
  });
  const kinds = (await registryDescriptors("collectionSource").catch(
    () => [],
  )) as SourceDescriptor[];
  const moments: Moment[] = [];
  let spec: CollectionSpec | null = null;
  for (const [i, e] of entries.entries()) {
    if ("intent" in e) {
      moments.push({ kind: "intent", text: e.intent, tool: "describe" });
      continue;
    }
    const draft = draftOf(e.tool);
    if (draft) spec = draft;
    if (draft && i !== last) moments.push(collectionSteps(draft, kinds));
    else moments.push(...(await momentsForTool(e.tool.name, e.tool.input, e.tool.output)));
  }
  return { moments, spec };
}

/** Finished tool calls in a transcript; a change means the stage has news. */
function toolCount(messages: AgentMessage[]): number {
  return transcriptEntries(messages).filter((e) => "tool" in e).length;
}

/**
 * "Describe it": the owner says what they want and watches Ether build it.
 * The model's words and steps stream into the conversation, each draft
 * ticks off its filters on the stage and its posters flow into place. The
 * build runs on the server, so a reload or a closed drawer rejoins it.
 * Nothing is saved until the owner saves, opens the draft in the editor,
 * or discards it.
 */
export function DescribeDialog({
  open,
  onClose,
  onApply,
  applyLabel = t("Open in editor"),
  onSaved,
  secondary,
}: {
  open: boolean;
  onClose: () => void;
  onApply: (spec: CollectionSpec) => void;
  applyLabel?: string;
  /** Offers one-click save; called with the stored collection. */
  onSaved?: (saved: CollectionSpec) => void;
  /** A second way to use the draft, beside applyLabel. */
  secondary?: { label: string; onApply: (spec: CollectionSpec) => void };
}) {
  const { locale } = useLocale();
  const [enabled, setEnabled] = useState<boolean | null>(null);
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const [moments, setMoments] = useState<Moment[]>([]);
  const [spec, setSpec] = useState<CollectionSpec | null>(null);
  const [media, setMedia] = useState<Media>("movie");
  const [streaming, setStreaming] = useState(false);
  const [rejoined, setRejoined] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const thread = useRef<string | undefined>(undefined);
  const turn = useRef<string | null>(null);
  const closing = useRef(false);
  const seen = useRef(0);

  const reset = () => {
    thread.current = undefined;
    turn.current = null;
    seen.current = 0;
    sessionStorage.removeItem(THREAD_KEY);
    setMessages([]);
    setMoments([]);
    setSpec(null);
    setRejoined(false);
    setStreaming(false);
    setError("");
  };

  /** Reads the stored conversation; redraws the stage when a step finished. */
  const sync = useCallback(async (id: string) => {
    const page = await fetchAgentMessages(id);
    if (thread.current !== id) return;
    setMessages(page.messages);
    const count = toolCount(page.messages);
    if (count !== seen.current || !page.running) {
      seen.current = count;
      const r = await replay(page.messages);
      if (thread.current !== id) return;
      setMoments(r.moments);
      setSpec(r.spec);
    }
    setRejoined(page.running);
  }, []);

  // Opening the drawer rejoins the last build, finished or still running.
  useEffect(() => {
    if (!open) return;
    closing.current = false;
    let live = true;
    void fetchAgentStatus()
      .then((s) => live && setEnabled(s.enabled))
      .catch(() => live && setEnabled(false));
    const saved = sessionStorage.getItem(THREAD_KEY);
    if (saved && !streaming) {
      thread.current = saved;
      void sync(saved).catch((e) => {
        if (!live) return;
        if (e instanceof ApiError && e.status === 404) reset();
        else setError(errorMessage(e));
      });
    }
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- runs on open only
  }, [open]);

  // A build started before a reload keeps running on the server; follow it.
  useEffect(() => {
    if (!open || !rejoined || streaming || !thread.current) return;
    const id = thread.current;
    const timer = window.setInterval(() => {
      void sync(id).catch((e) => setError(errorMessage(e)));
    }, POLL_MS);
    return () => window.clearInterval(timer);
  }, [open, rejoined, streaming, sync]);

  const follow = async (body: ReadableStream<Uint8Array>, signal: AbortSignal) => {
    const calls = new Map<string, { name: string; input: Record<string, unknown> }>();
    const pending: Promise<void>[] = [];
    try {
      await readAgentStream(new Response(body), (c) => {
        if (c.type === "start" && c.messageId) turn.current = c.messageId;
        else if (c.type === "tool-input-available" && c.toolCallId && c.toolName)
          calls.set(c.toolCallId, { name: c.toolName, input: c.input ?? {} });
        else if (c.type === "tool-output-available" && c.toolCallId) {
          const call = calls.get(c.toolCallId);
          if (!call) return;
          const draft = draftOf({ ...call, output: c.output });
          if (draft) setSpec(draft);
          pending.push(
            momentsForTool(call.name, call.input, c.output).then((m) =>
              setMoments((all) => [...all, ...m]),
            ),
          );
        } else if (c.type === "error" && c.errorText) setError(c.errorText);
      });
    } catch (e) {
      if (!signal.aborted) setError(errorMessage(e));
    }
    await Promise.all(pending);
  };

  const send = async (prompt: string, signal: AbortSignal): Promise<Response> => {
    setError("");
    setMoments((all) => [...all, { kind: "intent", text: prompt, tool: "describe" }]);
    const res = await apiStream("/agent/turn", {
      method: "POST",
      signal,
      body: JSON.stringify({
        threadId: thread.current,
        prompt,
        context: { route: ROUTE, mediaType: media },
      }),
    });
    const id = res.headers.get("X-Ether-Thread-Id");
    if (id) {
      thread.current = id;
      sessionStorage.setItem(THREAD_KEY, id);
    }
    if (!res.ok || !res.body) return res;
    setStreaming(true);
    // The build is not tied to this request: Stop cancels it on the server,
    // while closing the drawer leaves it running to rejoin.
    const stop = () => {
      if (!closing.current && turn.current) void cancelAgentTurn(turn.current).catch(() => {});
    };
    signal.addEventListener("abort", stop, { once: true });
    const [mine, panel] = res.body.tee();
    void follow(mine, signal).finally(() => {
      signal.removeEventListener("abort", stop);
      turn.current = null;
      setStreaming(false);
      if (id && !signal.aborted) void sync(id).catch(() => {});
    });
    return new Response(panel, { status: res.status, headers: res.headers });
  };

  const busy = streaming || rejoined;
  const [openTitle, setOpenTitle] = useState<string | null>(null);
  // The top shows the request and the draft's posters; each step is a tool
  // call in the chat below.
  const stage = useMemo(() => {
    const shown = moments.filter((m) => m.kind === "intent" || m.kind === "catalog");
    if (busy || !spec) return shown;
    const name = spec.name || t("Untitled collection");
    return [...shown, { kind: "done", text: t("“{name}” is ready", { name }) } as Moment];
    // eslint-disable-next-line react-hooks/exhaustive-deps -- copy follows the language
  }, [moments, busy, spec, locale]);
  const lastAssistant = [...messages].reverse().find((m) => m.role === "assistant");

  /** Ends the conversation: the collection (or nothing) is the outcome. */
  const finish = (cancel: boolean) => {
    const id = thread.current;
    const running = cancel ? (turn.current ?? (rejoined ? lastAssistant?.id : null)) : null;
    closing.current = true;
    reset();
    onClose();
    void (async () => {
      if (running) await cancelAgentTurn(running).catch(() => {});
      if (id) await deleteAgentThread(id).catch(() => {});
    })();
  };

  const save = async () => {
    if (!spec || !onSaved) return;
    setSaving(true);
    setError("");
    try {
      const saved = await api.createCollection(spec);
      toast.success(t("Saved “{name}”", { name: saved.name }));
      onSaved(saved);
      finish(false);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  };

  const labels = useMemo(
    () => ({
      ...agentPanelLabels(),
      title: t("Build with Ether"),
      welcomeTitle: t("What should this collection be?"),
      welcomeDescription: t(
        "Say it in plain words. Ether searches your catalogue, tries a few drafts and shows the posters as it goes. Nothing is saved until you choose.",
      ),
      disabledDescription: t("Connect a language model to describe collections."),
      placeholder: t("e.g. Feel-good 80s adventure films for a Saturday night"),
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- copy follows the language
    [locale],
  );
  const toolLabels = useMemo(
    () => ({
      collection_draft: t("Draft the collection"),
      catalog_search: t("Search the catalogue"),
      title_details: t("Title details"),
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- copy follows the language
    [locale],
  );

  return (
    <Drawer
      open={open}
      onClose={() => {
        closing.current = true;
        onClose();
      }}
      size="xl"
      bodyClassName="flex min-h-0 flex-col p-0"
      title={
        <span className="flex items-center gap-2">
          <Sparkles size={18} className="text-primary" /> {t("Describe a collection")}
        </span>
      }
      description={t(
        "Say what you want in plain English. Ether drafts the sources and rules; you review them before anything is saved.",
      )}
      footer={
        <div className="flex w-full flex-wrap items-center justify-end gap-2">
          {busy ? (
            <span className="mr-auto text-xs text-fg-muted" aria-live="polite">
              {t("Ether is building. You can close this and come back.")}
            </span>
          ) : null}
          <Button variant="ghost" onClick={() => finish(true)}>
            {spec || busy ? t("Discard") : t("Cancel")}
          </Button>
          {secondary ? (
            <Button
              variant="outline"
              disabled={!spec || busy}
              onClick={() => {
                if (!spec) return;
                secondary.onApply(spec);
                finish(false);
              }}
            >
              {secondary.label}
            </Button>
          ) : null}
          <Button
            variant={onSaved ? "outline" : undefined}
            disabled={!spec || busy}
            onClick={() => {
              if (!spec) return;
              onApply(spec);
              finish(false);
            }}
          >
            {applyLabel}
          </Button>
          {onSaved ? (
            <Button disabled={!spec || busy || saving} onClick={() => void save()}>
              {saving ? t("Saving…") : t("Save collection")}
            </Button>
          ) : null}
        </div>
      }
    >
      {enabled === false ? (
        <Alert variant="info" className="m-4">
          <AlertTitle>{t("Describe it needs a language model")}</AlertTitle>
          <AlertDescription>
            {tx(
              "Connect an OpenAI-compatible endpoint (a local Ollama works) in {integrations}, then try again. You can still build collections by hand or from a template.",
              {
                integrations: (
                  <Link to="/services" className="font-medium underline" onClick={onClose}>
                    {t("Integrations")}
                  </Link>
                ),
              },
            )}
          </AlertDescription>
        </Alert>
      ) : null}
      {stage.length ? (
        // Sized to its content so every poster shows; the chat below scrolls.
        <div className="shrink-0 border-b border-border" aria-label={t("Live build")} role="region">
          <LiveStageView
            moments={stage}
            narration=""
            showCompleted={false}
            onOpenTitle={setOpenTitle}
          />
        </div>
      ) : null}
      {error ? (
        <p role="alert" className="border-b border-border px-4 py-2 text-sm text-danger">
          {error}
        </p>
      ) : null}
      <div className="describe-chat flex min-h-[220px] flex-1 flex-col">
        <AgentPanel
          enabled={enabled !== false}
          initialMessages={messages}
          suggestions={EXAMPLES}
          labels={labels}
          toolLabels={toolLabels}
          working={rejoined && !streaming}
          send={send}
          composerActions={
            // The composer row aligns to its bottom; centre the switch on the input line.
            <div className="flex h-10 shrink-0 items-center">
              <Segmented
                ariaLabel={t("Media type")}
                size="sm"
                value={media}
                onChange={setMedia}
                options={[
                  { value: "movie", label: t("Movies") },
                  { value: "show", label: t("TV") },
                  { value: "mixed", label: t("Both") },
                ]}
              />
            </div>
          }
        />
      </div>
      {openTitle ? (
        <TitleInspector key={openTitle} titleKey={openTitle} onClose={() => setOpenTitle(null)} />
      ) : null}
    </Drawer>
  );
}

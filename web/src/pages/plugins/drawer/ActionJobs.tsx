import {
  PluginFailureDetails,
  pluginFailureSentence,
  type PluginFailure,
} from "../../../components/PluginFailure";
import { useEffect, useState } from "react";
import { Badge, Button } from "@braedonsaunders/appkit-ui";
import { AlertTriangle, RefreshCw } from "lucide-react";
import { apiFetch } from "../../../lib/api";
import { relativeTime } from "../../../lib/format";
import { t } from "../../../i18n";

export interface ActionJob {
  path: string;
  label?: string;
  kind?: string;
  state: string;
  code?: string;
  progress?: string;
  outcome?: string;
  error?: string;
  failure?: PluginFailure;
  started?: string;
  finished?: string;
}

export function ActionJobs({ id }: { id: string }) {
  const [jobs, setJobs] = useState<ActionJob[]>([]);
  const [error, setError] = useState("");
  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    setJobs([]);
    async function load() {
      try {
        const next = await apiFetch<ActionJob[]>(`/plugins/${encodeURIComponent(id)}/jobs`);
        if (live) {
          setJobs(next);
          setError("");
        }
      } catch (e) {
        if (live) setError(String(e));
      }
      if (live) timer = setTimeout(load, 2000);
    }
    void load();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [id]);
  if (!jobs.length && !error) return null;
  return (
    <div className="space-y-3 rounded-xl border border-border bg-surface p-4">
      <h3 className="text-sm font-medium">{t("Background jobs")}</h3>
      {error ? (
        <p role="alert" className="text-xs text-danger">
          {error}
        </p>
      ) : null}
      <div className="space-y-3">
        {jobs.map((job) => {
          const isRunning = job.state === "running";
          const isGuard = job.code === "delete_guard";
          const isCanceled = job.state === "canceled" || job.state === "cancelled";
          const isDone = job.state === "done";
          const timestamp = job.finished || job.started;
          return (
            <div
              key={job.path}
              className="flex flex-wrap items-start justify-between gap-3 rounded-lg border border-border/60 bg-surface-muted/30 px-3 py-2.5 text-sm"
            >
              <div className="min-w-0 flex-1 space-y-1">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium text-fg">{job.label || job.path}</span>
                  {isRunning ? (
                    <Badge variant="secondary" className="gap-1">
                      <RefreshCw size={11} className="animate-spin" aria-hidden />
                      {t("Running")}
                    </Badge>
                  ) : isGuard ? (
                    <Badge variant="outline" className="gap-1 border-warning/50 text-warning">
                      <AlertTriangle size={11} aria-hidden />
                      {t("Paused · Needs choice")}
                    </Badge>
                  ) : isDone ? (
                    <Badge variant="secondary">{t("Completed")}</Badge>
                  ) : isCanceled ? (
                    <Badge variant="outline">{t("Cancelled")}</Badge>
                  ) : (
                    <Badge variant="destructive">{t("Failed")}</Badge>
                  )}
                  {timestamp ? (
                    <span className="text-xs text-fg-muted">{relativeTime(timestamp)}</span>
                  ) : null}
                </div>
                {!job.failure && job.label && job.path && job.label !== job.path ? (
                  <p className="break-all font-mono text-[11px] text-fg-subtle">{job.path}</p>
                ) : null}
                {job.progress ? (
                  <p className="text-xs text-fg-muted" role="status">
                    {job.progress}
                  </p>
                ) : null}
                {job.outcome ? <p className="text-xs text-fg-muted">{job.outcome}</p> : null}
                {job.failure ? (
                  <>
                    <p className="text-xs text-danger">{pluginFailureSentence(job.failure)}</p>
                    <PluginFailureDetails failure={job.failure} />
                  </>
                ) : job.error ? (
                  <p className={isGuard ? "text-xs text-warning" : "text-xs text-danger"}>
                    {job.error}
                  </p>
                ) : null}
              </div>
              {isRunning ? (
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    void apiFetch(`/plugins/${encodeURIComponent(id)}/jobs/cancel`, {
                      method: "POST",
                      body: JSON.stringify({ path: job.path }),
                    }).catch((e) => setError(String(e)));
                  }}
                >
                  {t("Cancel")}
                </Button>
              ) : null}
            </div>
          );
        })}
      </div>
    </div>
  );
}

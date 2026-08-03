import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Ban, RefreshCw } from "lucide-react";

import { api, type Job } from "../api";

const STATUS: Record<string, { label: string; color: string }> = {
  done: { label: "Complete", color: "var(--benign-fg)" },
  running: { label: "Running", color: "var(--vus-fg)" },
  queued: { label: "Queued", color: "var(--text-3)" },
  error: { label: "Failed", color: "var(--path-fg)" },
  cancelled: { label: "Cancelled", color: "var(--text-3)" },
};

function when(sec?: number): string {
  if (!sec) return "—";
  return new Date(sec * 1000).toLocaleString();
}

/** How long a job took, or has been going. */
function duration(job: Job): string {
  if (!job.started_at) return "—";
  const end = job.finished_at || Math.floor(Date.now() / 1000);
  const secs = Math.max(0, end - job.started_at);
  if (secs < 60) return `${secs}s`;
  const m = Math.floor(secs / 60);
  if (m < 60) return `${m}m ${secs % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="label" style={{ marginBottom: 3 }}>
        {label}
      </div>
      <div style={{ fontSize: 13 }}>{children}</div>
    </div>
  );
}

/**
 * One job, in full.
 *
 * The list answers "what ran"; this answers "what happened", which needs the
 * run's own output and does not belong inline on a row — a stack trace in a
 * table makes both harder to read.
 */
export default function JobDetail() {
  const { jobId = "" } = useParams();
  const nav = useNavigate();
  const [job, setJob] = useState<Job | null>(null);
  const [log, setLog] = useState<{ output: string; recorded: boolean } | null>(null);
  const [err, setErr] = useState("");
  const [cancelling, setCancelling] = useState(false);

  async function load() {
    try {
      const j = await api.job(jobId);
      setJob(j);
      setErr("");
      try {
        setLog(await api.jobLog(jobId));
      } catch {
        // A job can exist with no log — one from before logs were kept, or one
        // still queued. Not an error worth replacing the whole page with.
        setLog(null);
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }

  useEffect(() => {
    load();
  }, [jobId]);

  async function cancel() {
    if (!confirm("Cancel this job? Work already done is not undone.")) return;
    setCancelling(true);
    try {
      const r = await api.cancelJob(jobId);
      setJob(r.job);
      // Reload rather than trusting the returned row: a running job is stopped
      // asynchronously by its worker, so the status here is the state at the
      // moment of asking, not the settled one.
      window.setTimeout(load, 600);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setCancelling(false);
    }
  }

  // Follow a job that is still going, and stop once it settles.
  useEffect(() => {
    if (!job || job.status === "done" || job.status === "error" || job.status === "cancelled")
      return;
    const t = window.setInterval(load, 3000);
    return () => window.clearInterval(t);
  }, [job?.status, jobId]);

  if (err) {
    return (
      <div className="page page-wide" style={{ paddingTop: 30 }}>
        <button className="btn link" style={{ fontSize: 13 }} onClick={() => nav(-1)}>
          ← Back
        </button>
        <p className="err" style={{ marginTop: 16, fontSize: 13 }}>
          {err}
        </p>
      </div>
    );
  }
  if (!job) {
    return (
      <div className="page page-wide" style={{ paddingTop: 30 }}>
        <p className="lede">Loading…</p>
      </div>
    );
  }

  const st = STATUS[job.status] ?? STATUS.queued;
  const live = job.status === "queued" || job.status === "running";

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <button className="btn link" style={{ fontSize: 13 }} onClick={() => nav(-1)}>
        ← Back
      </button>

      <div className="between" style={{ margin: "14px 0 6px" }}>
        <h1 className="title mono" style={{ fontSize: 21 }}>
          #{job.job_id.slice(0, 12)}
        </h1>
        <div className="row gap-8">
          {live && (
            <button className="btn secondary" onClick={cancel} disabled={cancelling}>
              <Ban size={14} /> {cancelling ? "Cancelling…" : "Cancel job"}
            </button>
          )}
          <button className="btn secondary" onClick={load} title="Refresh">
            <RefreshCw size={14} /> Refresh
          </button>
        </div>
      </div>
      <p className="lede" style={{ fontSize: 13.5, margin: "0 0 20px" }}>
        {job.label || job.kind}
      </p>

      <div className="card" style={{ padding: 16, marginBottom: 18 }}>
        <div className="job-fields">
          <Field label="Status">
            <span className="row gap-8">
              <i
                className={`status-dot ${live ? "blink" : ""}`}
                style={{ background: st.color }}
              />
              <span style={{ color: st.color, fontWeight: 500 }}>{st.label}</span>
            </span>
          </Field>
          <Field label="Kind">{job.kind}</Field>
          <Field label="Snapshot">{job.snapshot || "—"}</Field>
          <Field label="Variants">{job.n_variants || "—"}</Field>
          <Field label="Created">{when(job.created_at)}</Field>
          <Field label="Started">{when(job.started_at)}</Field>
          <Field label="Finished">{when(job.finished_at)}</Field>
          {/* Counted to now while a job is still going, so a stuck run is
              visible as one rather than looking merely unfinished. */}
          <Field label="Duration">{duration(job)}</Field>
        </div>
      </div>

      {job.error && job.status !== "cancelled" && (
        <>
          <h2 className="label" style={{ marginBottom: 8 }}>
            Error
          </h2>
          <div
            className="mono"
            style={{
              padding: "11px 14px",
              marginBottom: 18,
              borderRadius: 8,
              background: "var(--path-bg)",
              color: "var(--path-fg)",
              fontSize: 12,
              lineHeight: 1.6,
              whiteSpace: "pre-wrap",
              wordBreak: "break-word",
            }}
          >
            {job.error}
          </div>
        </>
      )}

      <h2 className="label" style={{ marginBottom: 8 }}>
        Output
      </h2>
      {log?.output ? (
        <pre
          className="mono"
          style={{
            margin: 0,
            padding: 14,
            borderRadius: 8,
            background: "var(--neutral-fill)",
            fontSize: 11.5,
            lineHeight: 1.6,
            maxHeight: 520,
            overflow: "auto",
            whiteSpace: "pre-wrap",
            wordBreak: "break-word",
          }}
        >
          {log.output}
        </pre>
      ) : (
        <p style={{ fontSize: 12.5, color: "var(--text-3)" }}>
          {live
            ? "Output is recorded when the job finishes."
            : log?.recorded === false
              ? "No output was recorded for this run."
              : "This run produced no output."}
        </p>
      )}
    </div>
  );
}

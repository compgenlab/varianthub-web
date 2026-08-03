import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ChevronRight, Plus, RefreshCw } from "lucide-react";

import { api, type Job } from "../api";

const STATUS: Record<Job["status"], { label: string; color: string }> = {
  done: { label: "Complete", color: "var(--benign-dot)" },
  running: { label: "Running", color: "var(--vus-dot)" },
  queued: { label: "Queued", color: "var(--text-4)" },
  error: { label: "Failed", color: "var(--path-dot)" },
};

function when(sec: number) {
  if (!sec) return "";
  return new Date(sec * 1000).toLocaleString();
}

/**
 * A job's failure message.
 *
 * varhub writes a diagnostic that usually names both the problem and its
 * subject — "REVEL:1.3: required software not found on PATH: python3" — so it
 * is shown verbatim rather than mapped to a friendlier phrasing that would
 * throw away the half telling you what to fix.
 */
function JobError({ message }: { message: string }) {
  return (
    <div
      className="mono"
      style={{
        padding: "9px 18px 12px",
        borderBottom: "1px solid var(--hairline)",
        background: "var(--path-bg)",
        color: "var(--path-fg)",
        fontSize: 11.5,
        lineHeight: 1.55,
        whiteSpace: "pre-wrap",
        wordBreak: "break-word",
      }}
    >
      {message}
    </div>
  );
}

export default function JobsList({
  kind = "annotation",
  title = "Jobs",
  lede = "Submitted annotation runs. Open a completed job to review its variants.",
}: {
  kind?: "annotation" | "download";
  title?: string;
  lede?: string;
} = {}) {
  const nav = useNavigate();
  const [jobs, setJobs] = useState<Job[] | null>(null);
  const [err, setErr] = useState("");
  const isDownloads = kind === "download";

  async function load() {
    try {
      const r = await api.jobs({ limit: 100, kind });
      setJobs(r.jobs ?? []);
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }

  useEffect(() => {
    load();
    // Refresh while anything is in flight, so a running job's status updates
    // without the user reloading.
    const t = window.setInterval(load, 4000);
    return () => window.clearInterval(t);
  }, []);

  const cols = ".7fr 1.5fr 1.1fr .7fr .9fr 34px";

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <div className="between" style={{ marginBottom: 18 }}>
        <div>
          <h1 className="title">{title}</h1>
          <p className="lede" style={{ fontSize: 13.5 }}>
            {lede}
          </p>
        </div>
        <div className="row gap-8">
          <button className="icon-btn" onClick={load} title="Refresh">
            <RefreshCw size={15} />
          </button>
          {!isDownloads && (
            <button className="btn sm" onClick={() => nav("/annotate/sources")}>
              <Plus size={15} /> New annotation
            </button>
          )}
        </div>
      </div>

      {err && <p className="err">{err}</p>}

      <div className="card">
        <div className="rowgrid thead" style={{ gridTemplateColumns: cols }}>
          <span>Job</span>
          <span>Input</span>
          <span>Snapshot</span>
          <span style={{ textAlign: "right" }}>{isDownloads ? "Files" : "Variants"}</span>
          <span>Status</span>
          <span />
        </div>

        {jobs === null && !err && <div className="empty">Loading…</div>}
        {jobs?.length === 0 && (
          <div className="empty">
            {isDownloads
              ? "No provisioning jobs yet. Start one from Sources & snapshots → Files."
              : "No jobs yet. Start one from New annotation."}
          </div>
        )}

        {jobs?.map((j) => {
          const st = STATUS[j.status] ?? STATUS.queued;
          // A download has no results table to open.
          const open = j.status === "done" && !isDownloads;
          return (
            <div key={j.job_id}>
            <button
              className="trow rowgrid"
              style={{
                gridTemplateColumns: cols,
                cursor: open ? "pointer" : "default",
                width: "100%",
                borderBottom: j.error ? "none" : undefined,
              }}
              onClick={() => open && nav(`/jobs/${j.job_id}`)}
            >
              <span className="mono" style={{ fontSize: 12, color: "var(--accent-text)" }}>
                #{j.job_id.slice(0, 8)}
              </span>
              <span>
                <span style={{ fontSize: 13.5, fontWeight: 500 }}>
                  {j.label || j.kind}
                </span>
                <br />
                <span style={{ fontSize: 11, color: "var(--text-3)" }}>
                  {when(j.created_at)}
                </span>
              </span>
              <span style={{ fontSize: 12.5 }}>{j.snapshot || "—"}</span>
              <span className="num">{j.n_variants || "—"}</span>
              <span className="row gap-8">
                <i
                  className={`status-dot ${j.status === "running" ? "blink" : ""}`}
                  style={{ background: st.color }}
                />
                <span style={{ fontSize: 12.5, color: st.color }}>{st.label}</span>
              </span>
              <ChevronRight
                size={15}
                color={open ? "var(--text-3)" : "rgba(22,24,29,.12)"}
              />
            </button>
            {/* Why it failed, on the row that failed.
                The message was always captured and returned; it was simply
                never rendered, so a failure said "Failed" and nothing else and
                the only way to learn more was the database. */}
            {j.error && <JobError message={j.error} />}
            </div>
          );
        })}
      </div>
    </div>
  );
}

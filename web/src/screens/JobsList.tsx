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

export default function JobsList() {
  const nav = useNavigate();
  const [jobs, setJobs] = useState<Job[] | null>(null);
  const [err, setErr] = useState("");

  async function load() {
    try {
      const r = await api.jobs({ limit: 100 });
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
          <h1 className="title">Jobs</h1>
          <p className="lede" style={{ fontSize: 13.5 }}>
            Submitted annotation runs. Open a completed job to review its variants.
          </p>
        </div>
        <div className="row gap-8">
          <button className="icon-btn" onClick={load} title="Refresh">
            <RefreshCw size={15} />
          </button>
          <button className="btn sm" onClick={() => nav("/annotate/sources")}>
            <Plus size={15} /> New annotation
          </button>
        </div>
      </div>

      {err && <p className="err">{err}</p>}

      <div className="card">
        <div className="rowgrid thead" style={{ gridTemplateColumns: cols }}>
          <span>Job</span>
          <span>Input</span>
          <span>Snapshot</span>
          <span style={{ textAlign: "right" }}>Variants</span>
          <span>Status</span>
          <span />
        </div>

        {jobs === null && !err && <div className="empty">Loading…</div>}
        {jobs?.length === 0 && (
          <div className="empty">
            No jobs yet. Start one from <strong>New annotation</strong>.
          </div>
        )}

        {jobs?.map((j) => {
          const st = STATUS[j.status] ?? STATUS.queued;
          const open = j.status === "done";
          return (
            <button
              key={j.job_id}
              className="trow rowgrid"
              style={{
                gridTemplateColumns: cols,
                cursor: open ? "pointer" : "default",
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
              <span style={{ fontSize: 12.5 }}>{j.snapshot}</span>
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
          );
        })}
      </div>
    </div>
  );
}

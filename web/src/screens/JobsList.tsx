import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ChevronRight, Plus, RefreshCw } from "lucide-react";

import { api, type Job } from "../api";

const STATUS: Record<Job["status"], { label: string; color: string }> = {
  done: { label: "Complete", color: "var(--benign-dot)" },
  running: { label: "Running", color: "var(--vus-dot)" },
  // A submission part-way through its chunks. The count beside it says how far,
  // which is the whole reason these are separate states.
  partial_running: { label: "Running", color: "var(--vus-dot)" },
  partial_queued: { label: "Waiting", color: "var(--text-4)" },
  queued: { label: "Queued", color: "var(--text-4)" },
  error: { label: "Failed", color: "var(--path-dot)" },
  cancelled: { label: "Cancelled", color: "var(--text-3)" },
};

/** Whether a job is still going. Four of the seven statuses are. */
function inFlight(s: Job["status"]): boolean {
  return s !== "done" && s !== "error" && s !== "cancelled";
}

function when(sec: number) {
  if (!sec) return "";
  return new Date(sec * 1000).toLocaleString();
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

  // An annotation job opens its results; anything else opens the run itself.
  // A download has no results table, and its interesting content is the output.
  const detailPath = (id: string) =>
    isDownloads ? `/admin/jobs/${id}` : `/jobs/${id}`;

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
          return (
            <button
              key={j.job_id}
              className="trow rowgrid"
              style={{ gridTemplateColumns: cols, cursor: "pointer", width: "100%" }}
              // Every job opens, whatever its state. A failed one is the case
              // most worth opening, and the detail page is where its output
              // lives — the row says only that it failed.
              onClick={() => nav(detailPath(j.job_id))}
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
                  className={`status-dot ${inFlight(j.status) ? "blink" : ""}`}
                  style={{ background: st.color }}
                />
                <span style={{ fontSize: 12.5, color: st.color }}>
                  {st.label}
                  {/* Only where it says something a label cannot: a job of one
                      chunk is fully described by its status. */}
                  {j.chunks_total > 1 && inFlight(j.status)
                    ? ` ${j.chunks_done}/${j.chunks_total}`
                    : ""}
                </span>
              </span>
              <ChevronRight size={15} color="var(--text-3)" />
            </button>
          );
        })}
      </div>
    </div>
  );
}

import { useEffect, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Loader2 } from "lucide-react";

import { api, type Job } from "../api";

/**
 * Polls a job until it reaches a terminal state, then goes to its results.
 *
 * The design shows a four-stage checklist with a percentage. There is none, and
 * there will not be: an annotation run is a single opaque pass through the CLI,
 * so a percentage could only ever be a timer pretending to be progress. Status —
 * queued, running, done, error, cancelled — is what is actually known, so that
 * is what this shows, with an indeterminate indicator rather than a fake bar. Wire the real stages in when the backend reports them.
 */
export default function Running() {
  const { jobId = "" } = useParams();
  const nav = useNavigate();
  const [job, setJob] = useState<Job | null>(null);
  const [err, setErr] = useState("");
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => {
    let cancelled = false;

    async function poll() {
      try {
        const j = await api.job(jobId);
        if (cancelled) return;
        setJob(j);
        if (j.status === "done") {
          nav(`/jobs/${jobId}`, { replace: true });
          return;
        }
        if (j.status === "error") return; // stop polling; show the error
        timer.current = window.setTimeout(poll, 700);
      } catch (e) {
        if (!cancelled) setErr(e instanceof Error ? e.message : String(e));
      }
    }
    poll();

    // Clearing on unmount matters: without it, navigating away leaves a timer
    // polling a finished job for as long as the tab is open.
    return () => {
      cancelled = true;
      if (timer.current) window.clearTimeout(timer.current);
    };
  }, [jobId, nav]);

  const failed = job?.status === "error";

  return (
    <div className="page page-tight">
      <div
        style={{
          background: "var(--surface)",
          border: "1px solid var(--border-card)",
          borderRadius: 12,
          boxShadow: "var(--sh-raised)",
          padding: 30,
        }}
      >
        <div className="row gap-10">
          {!failed && <Loader2 size={18} color="var(--accent)" className="spin" />}
          <h2 style={{ fontSize: 20, fontWeight: 600 }}>
            {failed ? "Annotation failed" : "Annotating variants"}
          </h2>
        </div>
        <p style={{ fontSize: 13, color: "var(--text-2)", margin: "6px 0 20px" }}>
          Job <span className="mono">#{jobId.slice(0, 8)}</span>
          {job?.snapshot && <> · {job.snapshot}</>}
          {!!job?.n_variants && <> · {job.n_variants} variants</>}
        </p>

        {!failed && (
          <>
            <div className="bar">
              {/* Indeterminate: the API reports status, not percent. */}
              <div style={{ width: "40%", opacity: 0.5 }} className="blink" />
            </div>
            <div
              className="between mono"
              style={{ fontSize: 11, color: "var(--text-2)", marginTop: 8 }}
            >
              <span>
                {job && job.chunks_total > 1
                  ? `Annotating… ${job.chunks_done}/${job.chunks_total}`
                  : job?.status === "running"
                    ? "Annotating…"
                    : "Queued…"}
              </span>
              <span>—</span>
            </div>
          </>
        )}

        {failed && (
          <>
            <p className="err" style={{ fontSize: 13.5 }}>
              {job?.error || "The job failed."}
            </p>
            <div className="row gap-10" style={{ marginTop: 20 }}>
              <button className="btn secondary sm" onClick={() => nav("/annotate/sources")}>
                Start over
              </button>
              <button className="btn sm" onClick={() => nav("/jobs")}>
                All jobs
              </button>
            </div>
          </>
        )}

        {err && (
          <p className="err" style={{ fontSize: 13, marginTop: 14 }}>
            {err}
          </p>
        )}
      </div>
    </div>
  );
}

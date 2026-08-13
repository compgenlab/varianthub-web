import { useEffect, useState } from "react";
import {
  Activity,
  CircleAlert,
  CircleCheck,
  Clock,
  Cloud,
  Database,
  HardDrive,
  Hourglass,
  Loader,
  Ban,
  Skull,
} from "lucide-react";

import { api, type Metrics as MetricsData } from "../api";
import { humanSize } from "./Files";
import Usage from "./Usage";

function humanCount(n: number): string {
  return n.toLocaleString();
}

// How long the longest-waiting job has been waiting. A queue depth alone does
// not distinguish one that is moving from one that is stuck.
function waitedFor(since: number): string {
  const secs = Math.max(0, Math.floor(Date.now() / 1000) - since);
  if (secs < 60) return `${secs}s`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ${Math.floor((secs % 3600) / 60)}m`;
  return `${Math.floor(secs / 86400)}d`;
}

function Stat({
  icon,
  label,
  value,
  sub,
  tone,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  sub?: string;
  tone?: "ok" | "warn" | "bad";
}) {
  const color =
    tone === "bad"
      ? "var(--path-fg)"
      : tone === "warn"
        ? "var(--vus-fg)"
        : tone === "ok"
          ? "var(--benign-fg)"
          : "var(--text)";
  return (
    <div className="card" style={{ padding: "15px 17px" }}>
      <div
        className="row gap-8"
        style={{ fontSize: 11, textTransform: "uppercase", letterSpacing: ".06em", color: "var(--text-3)" }}
      >
        {icon}
        {label}
      </div>
      <div style={{ fontSize: 27, fontWeight: 600, marginTop: 8, lineHeight: 1.1, color }}>
        {value}
      </div>
      {sub && (
        <div style={{ fontSize: 12, color: "var(--text-3)", marginTop: 4 }}>{sub}</div>
      )}
    </div>
  );
}

export default function Metrics() {
  const [m, setM] = useState<MetricsData | null>(null);
  const [err, setErr] = useState("");

  async function load() {
    try {
      setM(await api.metrics());
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
    // The queue moves; storage does not, but it is cheap to read alongside.
    const t = window.setInterval(load, 10000);
    return () => window.clearInterval(t);
  }, []);

  if (err) {
    return (
      <div className="page page-wide" style={{ paddingTop: 30 }}>
        <h1 className="title">Usage &amp; Metrics</h1>
        <p className="err" style={{ marginTop: 16, fontSize: 13 }}>
          {err}
        </p>
      </div>
    );
  }
  if (!m) {
    return (
      <div className="page page-wide" style={{ paddingTop: 30 }}>
        <h1 className="title">Usage &amp; Metrics</h1>
        <p className="lede" style={{ marginTop: 16 }}>
          Loading…
        </p>
      </div>
    );
  }

  const j = m.jobs;
  const stored = m.storage ?? [];
  const remote = m.remote ?? [];
  const onDisk = stored.filter((s) => s.kind === "path");
  const inBuckets = stored.filter((s) => s.kind === "s3");
  // Cancelled jobs are excluded from the denominator: a rate that moves
  // because someone stopped a job on purpose is not a rate worth watching.
  const decided = Math.max(0, j.total - (j.cancelled ?? 0));
  const failRate = decided > 0 ? (j.failed / decided) * 100 : 0;
  // The worker table is hidden while nothing has been abandoned. A row per
  // healthy pod is noise on a page read to find out whether anything is wrong,
  // and this section only says anything when something is.
  const workers = m.workers ?? [];
  const abandoned = j.abandoned_attempts_24h ?? 0;

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <h1 className="title">Usage &amp; Metrics</h1>

      {/* Who has been asking, then what the machine did about it. People first:
          "is anyone using this" is the question somebody opens this page with,
          and queue depth only means something once you know. */}
      <Usage />

      <h2 className="subtitle" style={{ marginTop: 34 }}>
        Service
      </h2>
      <p className="lede" style={{ fontSize: 13.5, margin: "6px 0 20px" }}>
        Annotation throughput, what the queue is doing right now, and where source
        data lives.
      </p>

      <h2 className="label" style={{ marginBottom: 10 }}>
        Annotation
      </h2>
      <div className="metric-grid">
        <Stat
          icon={<Activity size={13} />}
          label="Jobs"
          value={humanCount(j.total)}
          sub={`${humanCount(j.last_24h)} finished in the last 24h · ${humanCount(j.last_7d)} in 7d`}
        />
        <Stat
          icon={<CircleCheck size={13} />}
          label="Successful"
          value={humanCount(j.succeeded)}
          sub={decided > 0 ? `${(100 - failRate).toFixed(1)}% of completed jobs` : "no jobs yet"}
          tone="ok"
        />
        <Stat
          icon={<CircleAlert size={13} />}
          label="Failed"
          value={humanCount(j.failed)}
          sub={decided > 0 ? `${failRate.toFixed(1)}% of completed jobs` : undefined}
          tone={j.failed > 0 ? "bad" : undefined}
        />
        <Stat
          icon={<Ban size={13} />}
          label="Cancelled"
          value={humanCount(j.cancelled ?? 0)}
          sub="stopped on purpose"
        />
        <Stat
          icon={<Database size={13} />}
          label="Variants annotated"
          value={humanCount(j.variants)}
          // Failed jobs are excluded upstream: their variant count is what was
          // submitted, not what was annotated.
          sub="across successful jobs"
        />
      </div>

      <h2 className="label" style={{ margin: "26px 0 10px" }}>
        Queue
      </h2>
      <div className="metric-grid">
        <Stat
          icon={<Hourglass size={13} />}
          label="Queued"
          value={humanCount(j.queued)}
          sub={
            j.oldest_queued_at
              ? `longest wait ${waitedFor(j.oldest_queued_at)}`
              : "nothing waiting"
          }
          tone={j.queued > 0 ? "warn" : undefined}
        />
        <Stat
          icon={<Loader size={13} />}
          label="Running"
          value={humanCount(j.running)}
          sub={j.running > 0 ? "workers busy" : "workers idle"}
        />
        <Stat
          icon={<Clock size={13} />}
          label="In flight"
          value={humanCount(j.queued + j.running)}
          sub="queued and running"
        />
        {/* Only when it is not zero. A permanent "Abandoned 0" trains the eye to
            skip it, which is the one thing this counter must not do. */}
        {abandoned > 0 && (
          <Stat
            icon={<Skull size={13} />}
            label="Abandoned"
            value={humanCount(abandoned)}
            sub={
              j.abandoned_exhausted > 0
                ? `attempts in 24h · ${humanCount(j.abandoned_exhausted)} job${
                    j.abandoned_exhausted === 1 ? "" : "s"
                  } gave up`
                : "attempts in 24h · all retried successfully"
            }
            tone={j.abandoned_exhausted > 0 ? "bad" : "warn"}
          />
        )}
        <Stat
          icon={<Database size={13} />}
          label="Sources"
          value={humanCount(m.sources.total)}
          sub={
            `${m.sources.provisioned} downloaded · ${m.sources.streamed} streamed · ` +
            `${m.sources.builtin} builtin` +
            (m.sources.pending > 0 ? ` · ${m.sources.pending} awaiting data` : "")
          }
        />
      </div>

      {/* Workers, and only when something has gone wrong with one. An
          abandonment is a worker killed mid-job rather than a job failing, so
          the usual cause is the container's memory limit — and the counters
          above cannot say whether one process lost everything or the losses are
          spread, which is the difference between fixing a pod and fixing a job. */}
      {abandoned > 0 && (
        <>
          <h2 className="label" style={{ margin: "26px 0 10px" }}>
            Workers
          </h2>
          <p className="lede" style={{ fontSize: 13.5, margin: "0 0 12px" }}>
            {humanCount(abandoned)} attempt{abandoned === 1 ? "" : "s"} in the last
            24h ended with the worker gone rather than reporting. A single worker
            accounting for most of them points at that process — commonly its
            memory limit; spread evenly, it points at the jobs instead.
          </p>
          <div className="card">
            <div className="thead rowgrid metric-worker-row">
              <span>Worker</span>
              <span style={{ textAlign: "right" }}>Attempts</span>
              <span style={{ textAlign: "right" }}>Abandoned</span>
              <span style={{ textAlign: "right" }}>Typically after</span>
            </div>
            {workers.map((wk) => (
              <div key={wk.worker} className="rowgrid metric-worker-row">
                <span className="mono">{wk.worker}</span>
                <span style={{ textAlign: "right" }}>{humanCount(wk.attempts)}</span>
                <span
                  style={{ textAlign: "right" }}
                  className={wk.abandoned > 0 ? "bad" : undefined}
                >
                  {humanCount(wk.abandoned)}
                </span>
                <span style={{ textAlign: "right" }}>
                  {wk.abandoned > 0 && wk.median_abandoned_after
                    ? `${wk.median_abandoned_after}s`
                    : "—"}
                </span>
              </div>
            ))}
          </div>
        </>
      )}

      <h2 className="label" style={{ margin: "26px 0 10px" }}>
        Storage
      </h2>
      <div className="card">
        <div className="thead rowgrid metric-storage-row">
          <span>Location</span>
          <span>Kind</span>
          <span>Sources</span>
          <span>Files</span>
          <span style={{ textAlign: "right" }}>Size</span>
        </div>
        {stored.length === 0 && (
          <div style={{ padding: "16px 18px", fontSize: 13, color: "var(--text-3)" }}>
            No storage locations configured.
          </div>
        )}
        {[...onDisk, ...inBuckets].map((s) => (
          <div key={s.storage_id} className="trow rowgrid metric-storage-row">
            <span>
              <span style={{ fontWeight: 500 }}>{s.name}</span>
              <span className="mono" style={{ display: "block", fontSize: 11, color: "var(--text-3)" }}>
                {s.uri}
              </span>
            </span>
            <span className="row gap-8" style={{ fontSize: 12.5, color: "var(--text-2)" }}>
              {s.kind === "s3" ? <Cloud size={13} /> : <HardDrive size={13} />}
              {/* Buckets are listed one per row, never merged, so two
                  locations sharing a bucket stay distinguishable. */}
              {s.kind === "s3" ? (s.bucket ?? "S3") : "filesystem"}
            </span>
            <span className="num">{humanCount(s.sources)}</span>
            <span className="num">{humanCount(s.files)}</span>
            <span className="num" style={{ textAlign: "right" }}>
              {humanSize(s.bytes)}
            </span>
          </div>
        ))}
        <div
          className="between"
          style={{ padding: "12px 18px", fontSize: 13, background: "var(--neutral-fill)" }}
        >
          <span style={{ color: "var(--text-2)" }}>Total stored by this deployment</span>
          <span className="mono" style={{ fontWeight: 600 }}>
            {humanSize(m.storage_bytes)}
          </span>
        </div>
      </div>

      <h2 className="label" style={{ margin: "26px 0 10px" }}>
        Remotely accessible
      </h2>
      <p className="lede" style={{ fontSize: 12.5, margin: "0 0 10px" }}>
        Streamed sources are read from their origin with range requests and stored
        nowhere here. These sizes are what sits behind the network hop — they are
        not part of the total above.
      </p>
      <div className="card">
        <div className="thead rowgrid metric-remote-row">
          <span>Source</span>
          <span>Origin</span>
          <span>Files</span>
          <span style={{ textAlign: "right" }}>Size</span>
        </div>
        {remote.length === 0 && (
          <div style={{ padding: "16px 18px", fontSize: 13, color: "var(--text-3)" }}>
            No streamed sources.
          </div>
        )}
        {remote.map((r) => (
          <div key={r.source_id} className="trow rowgrid metric-remote-row">
            <span style={{ fontWeight: 500 }}>{r.name}</span>
            <span className="mono" style={{ fontSize: 11.5, color: "var(--text-3)" }}>
              {r.host}
            </span>
            <span className="num">
              {humanCount(r.files)}
              {/* An origin that reports no length leaves the total a floor;
                  say which rows are incomplete rather than quietly under-report. */}
              {r.unmeasured ? (
                <span style={{ color: "var(--vus-fg)" }}> ({r.unmeasured} unmeasured)</span>
              ) : null}
            </span>
            <span className="num" style={{ textAlign: "right" }}>
              {humanSize(r.bytes)}
            </span>
          </div>
        ))}
        {remote.length > 0 && (
          <div
            className="between"
            style={{ padding: "12px 18px", fontSize: 13, background: "var(--neutral-fill)" }}
          >
            <span style={{ color: "var(--text-2)" }}>
              Total read remotely
              {!m.remote_measured && " (at least — some origins report no size)"}
            </span>
            <span className="mono" style={{ fontWeight: 600 }}>
              {!m.remote_measured && "≥ "}
              {humanSize(m.remote_bytes)}
            </span>
          </div>
        )}
      </div>
    </div>
  );
}

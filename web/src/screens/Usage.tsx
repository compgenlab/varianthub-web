import { useEffect, useState } from "react";
import { Activity, UserRound, UsersRound } from "lucide-react";

import { api, type Split, type Usage as UsageData, type UserUsage } from "../api";

/**
 * Who has been using this installation, and how much.
 *
 * Separate from Metrics, which is about the machine — queue depth, bytes on
 * disk. This is about people, read at a different time and for a different
 * reason, and it scans the job table to answer.
 */
export default function Usage() {
  const [u, setU] = useState<UsageData | null>(null);
  const [err, setErr] = useState("");

  useEffect(() => {
    api
      .usage()
      .then((d) => {
        setU(d);
        setErr("");
      })
      .catch((e) => setErr(e instanceof Error ? e.message : String(e)));
    // No polling: this is a report somebody reads, not a dial they watch, and
    // every load is a scan of the job table.
  }, []);

  if (err) {
    return (
      <div className="page page-wide" style={{ paddingTop: 30 }}>
        <h1 className="title">Usage</h1>
        <p className="err" style={{ marginTop: 16, fontSize: 13 }}>
          {err}
        </p>
      </div>
    );
  }
  if (!u) {
    return (
      <div className="page page-wide" style={{ paddingTop: 30 }}>
        <h1 className="title">Usage</h1>
        <p className="lede" style={{ marginTop: 16 }}>
          Loading…
        </p>
      </div>
    );
  }

  const active = u.users.filter((r) => r.jobs.total > 0);

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <h1 className="title">Usage</h1>
      <p className="lede" style={{ marginTop: 6 }}>
        Annotation work only — provisioning downloads are the deployment's own
        doing, not somebody's usage.
      </p>

      <div className="row gap-10" style={{ marginTop: 22, flexWrap: "wrap" }}>
        <Stat icon={<UsersRound size={15} />} label="Accounts" value={u.accounts} />
        <Stat
          icon={<UserRound size={15} />}
          label="Disabled"
          value={u.disabled}
          sub="still own their old jobs"
        />
        <Stat
          icon={<Activity size={15} />}
          label="Active (90d)"
          value={active.length}
          sub="accounts that submitted"
        />
      </div>

      <h2 className="subtitle" style={{ marginTop: 30 }}>
        Over time
      </h2>
      <div className="card" style={{ marginTop: 10 }}>
        <div className="thead rowgrid usage-row">
          <span>Window</span>
          <span style={{ textAlign: "right" }}>Jobs</span>
          <span style={{ textAlign: "right" }}>web</span>
          <span style={{ textAlign: "right" }}>api</span>
          <span style={{ textAlign: "right" }}>Variants</span>
          <span style={{ textAlign: "right" }}>Accounts</span>
          <span style={{ textAlign: "right" }}>Anonymous</span>
        </div>
        {u.windows.map((w) => (
          <div key={w.days} className="trow rowgrid usage-row">
            <span style={{ fontWeight: 500 }}>{w.days} days</span>
            <Num v={w.jobs.total} strong />
            <Num v={w.jobs.web} />
            <Num v={w.jobs.api} />
            <Num v={w.variants.total} />
            <Num v={w.accounts} />
            {/* Browsers, not people: the same person on two machines is two,
                which is why this is not added to the account count. */}
            <Num v={w.anonymous} />
          </div>
        ))}
      </div>
      <Unrecorded splits={u.windows.map((w) => w.jobs)} />

      <h2 className="subtitle" style={{ marginTop: 30 }}>
        By account
        <span style={{ fontWeight: 400, fontSize: 13, color: "var(--text-2)" }}>
          {" "}
          — last 90 days
        </span>
      </h2>
      <div className="card" style={{ marginTop: 10 }}>
        <div className="thead rowgrid people-usage-row">
          <span>Account</span>
          <span>Tier</span>
          <span style={{ textAlign: "right" }}>Jobs</span>
          <span style={{ textAlign: "right" }}>web</span>
          <span style={{ textAlign: "right" }}>api</span>
          <span style={{ textAlign: "right" }}>Variants</span>
          <span style={{ textAlign: "right" }}>Last</span>
        </div>
        {u.users.map((r) => (
          <UserRow key={r.user_id} r={r} />
        ))}
        {u.users.length === 0 && (
          <div className="trow" style={{ color: "var(--text-2)", fontSize: 13 }}>
            No accounts yet.
          </div>
        )}
      </div>
    </div>
  );
}

function UserRow({ r }: { r: UserUsage }) {
  // An account that has submitted nothing is dimmed rather than hidden: who is
  // not using this is an answer too, and dropping the row reads as an account
  // that does not exist.
  const idle = r.jobs.total === 0;
  return (
    <div
      className="trow rowgrid people-usage-row"
      style={idle ? { color: "var(--text-3)" } : undefined}
    >
      <span style={{ fontWeight: idle ? 400 : 500 }}>{r.email}</span>
      <span style={{ fontSize: 12.5 }}>{r.tier || "standard"}</span>
      <Num v={r.jobs.total} strong={!idle} />
      <Num v={r.jobs.web} />
      <Num v={r.jobs.api} />
      <Num v={r.variants.total} />
      <span className="mono" style={{ textAlign: "right", fontSize: 12.5 }}>
        {r.last_submitted ? ago(r.last_submitted) : "—"}
      </span>
    </div>
  );
}

function Num({ v, strong }: { v: number; strong?: boolean }) {
  return (
    <span
      className="mono"
      style={{
        textAlign: "right",
        fontSize: 13,
        fontWeight: strong ? 600 : 400,
        color: v === 0 ? "var(--text-3)" : undefined,
      }}
    >
      {v.toLocaleString()}
    </span>
  );
}

// Said once, below the table, rather than as a column that is zero forever
// afterwards: jobs from before the origin was recorded are a fact about the
// history, not a third kind of traffic.
function Unrecorded({ splits }: { splits: Split[] }) {
  const most = Math.max(0, ...splits.map((s) => s.unknown));
  if (most === 0) return null;
  return (
    <p style={{ fontSize: 12, color: "var(--text-3)", marginTop: 8 }}>
      {most.toLocaleString()} job(s) in these windows predate the web/api split
      being recorded, so they are counted in the totals but in neither column.
    </p>
  );
}

function ago(unix: number): string {
  const secs = Math.max(0, Math.floor(Date.now() / 1000) - unix);
  if (secs < 3600) return `${Math.floor(secs / 60)}m`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h`;
  return `${Math.floor(secs / 86400)}d`;
}

function Stat({
  icon,
  label,
  value,
  sub,
}: {
  icon: React.ReactNode;
  label: string;
  value: number;
  sub?: string;
}) {
  return (
    <div
      className="card"
      style={{ padding: "14px 18px", minWidth: 168, display: "grid", gap: 3 }}
    >
      <span
        className="row gap-8"
        style={{ fontSize: 11.5, color: "var(--text-2)", letterSpacing: ".02em" }}
      >
        {icon}
        {label}
      </span>
      <span className="mono" style={{ fontSize: 22, fontWeight: 600 }}>
        {value.toLocaleString()}
      </span>
      {sub && <span style={{ fontSize: 11.5, color: "var(--text-3)" }}>{sub}</span>}
    </div>
  );
}

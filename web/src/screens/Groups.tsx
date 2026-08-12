import { useEffect, useState } from "react";
import { ShieldCheck, Trash2, UserPlus, Users } from "lucide-react";

import { api, type AccessRequest, type Me, type Team, type User } from "../api";

/**
 * Users and groups.
 *
 * Groups exist because grants attach to them rather than to individuals: a
 * restricted source is shared with a group, and someone joining or leaving it
 * should not mean revisiting every source.
 *
 * The API and the schema still say "team". The rename is deliberate at this
 * layer only — renaming the tables would be a migration through the grant
 * model to change a word nobody outside this screen reads.
 */
// Mirrors catalog.Tiers. The server rejects anything it does not recognize
// rather than coercing it, so a list that drifts shows up as a refused change
// rather than as a tier that silently means "standard".
const TIERS = ["standard", "elevated", "unlimited"];

export default function Groups({ me }: { me: Me }) {
  const [users, setUsers] = useState<User[]>([]);
  const [teams, setTeams] = useState<Team[]>([]);
  const [requests, setRequests] = useState<AccessRequest[]>([]);
  const [err, setErr] = useState("");
  const [adding, setAdding] = useState(false);

  async function load() {
    try {
      const [u, t, a] = await Promise.all([
        api.users(),
        api.teams(),
        api.accessRequests(),
      ]);
      setUsers(u.users ?? []);
      setTeams(t.teams ?? []);
      setRequests(a.requests ?? []);
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function act<T>(fn: () => Promise<T>) {
    try {
      await fn();
      await load();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <h1 className="title">Users &amp; groups</h1>
      <p className="lede" style={{ fontSize: 13.5, margin: "6px 0 18px" }}>
        Users sign in with a password and carry their own API tokens. Groups are
        what restricted sources are granted to.
      </p>

      {err && (
        <p className="err" style={{ fontSize: 13, marginBottom: 12 }}>
          {err}
        </p>
      )}

      <Waitlist requests={requests} act={act} />

      <div className="between" style={{ marginBottom: 10 }}>
        <span className="label" style={{ margin: 0 }}>
          Users
        </span>
        <button className="btn" onClick={() => setAdding(!adding)}>
          <UserPlus size={14} /> New user
        </button>
      </div>

      {adding && <NewUser onDone={() => { setAdding(false); load(); }} onError={setErr} />}

      <div className="card">
        <div className="thead rowgrid people-row">
          <span>Email</span>
          <span>Name</span>
          <span>Role</span>
          <span>Tier</span>
          <span>Status</span>
          <span />
        </div>
        {users.map((u) => (
          <div key={u.id} className="trow rowgrid people-row">
            <span style={{ fontWeight: 500 }}>{u.email}</span>
            <span style={{ fontSize: 13, color: "var(--text-2)" }}>{u.name || "—"}</span>
            <span>
              <select
                className="select"
                style={{ fontSize: 12.5, padding: "4px 8px" }}
                value={u.role}
                onChange={(e) => act(() => api.updateUser(u.id, { role: e.target.value }))}
              >
                <option value="member">member</option>
                <option value="admin">admin</option>
              </select>
            </span>
            {/* Capacity, not permission: what this account may ask of the
                service, as opposed to what it may administer. Raising someone's
                limits should not mean making them an administrator. */}
            <span>
              <select
                className="select"
                style={{ fontSize: 12.5, padding: "4px 8px" }}
                value={u.tier || "standard"}
                onChange={(e) => act(() => api.updateUser(u.id, { tier: e.target.value }))}
              >
                {TIERS.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            </span>
            <span style={{ fontSize: 12.5, color: u.disabled ? "var(--path-fg)" : "var(--benign-fg)" }}>
              {u.disabled ? "disabled" : "active"}
            </span>
            <span style={{ textAlign: "right" }}>
              {/* Disabling rather than deleting: a job's owner should stay
                  resolvable after the person leaves. */}
              <button
                className="btn link"
                style={{ fontSize: 12.5 }}
                onClick={() => act(() => api.updateUser(u.id, { disabled: !u.disabled }))}
              >
                {u.disabled ? "Enable" : "Disable"}
              </button>
            </span>
          </div>
        ))}
      </div>

      <div className="between" style={{ margin: "26px 0 10px" }}>
        <span className="label" style={{ margin: 0 }}>
          Groups
        </span>
        <NewTeam onDone={load} onError={setErr} />
      </div>

      <div className="card">
        {teams.length === 0 && (
          <div style={{ padding: "16px 18px", fontSize: 13, color: "var(--text-3)" }}>
            No groups yet. Create one to grant access to restricted sources.
          </div>
        )}
        {teams.map((t) => (
          <div key={t.id} style={{ padding: "14px 18px", borderBottom: "1px solid var(--hairline)" }}>
            <div className="between">
              <span className="row gap-8" style={{ fontWeight: 500 }}>
                <Users size={14} /> {t.name}
              </span>
              <button
                className="btn link"
                onClick={() => {
                  if (
                    confirm(
                      `Delete group "${t.name}"? Sources granted only to this group ` +
                        `become invisible again.`,
                    )
                  )
                    act(() => api.deleteTeam(t.id));
                }}
              >
                <Trash2 size={14} />
              </button>
            </div>
            <div className="row gap-8" style={{ flexWrap: "wrap", marginTop: 8 }}>
              {(t.members ?? []).map((m) => (
                <span key={m.user.id} className="tag" style={{ display: "inline-flex", gap: 6 }}>
                  {m.role === "owner" && <ShieldCheck size={11} />}
                  {m.user.email}
                  <button
                    className="btn link"
                    style={{ padding: 0, fontSize: 11 }}
                    onClick={() => act(() => api.removeMember(t.id, m.user.id))}
                    title="Remove from group"
                  >
                    ×
                  </button>
                </span>
              ))}
              {(t.members ?? []).length === 0 && (
                <span style={{ fontSize: 12.5, color: "var(--text-3)" }}>No members.</span>
              )}
            </div>
            <select
              className="select"
              style={{ fontSize: 12.5, padding: "4px 8px", marginTop: 10, maxWidth: 280 }}
              value=""
              onChange={(e) => {
                if (e.target.value) act(() => api.addMember(t.id, e.target.value));
              }}
            >
              <option value="">Add a user…</option>
              {users
                .filter((u) => !(t.members ?? []).some((m) => m.user.id === u.id))
                .map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.email}
                  </option>
                ))}
            </select>
          </div>
        ))}
      </div>

      <p style={{ fontSize: 11.5, color: "var(--text-3)", marginTop: 14 }}>
        Signed in as {me.user?.email ?? me.label}. The last administrator cannot be
        demoted or disabled — otherwise nobody could administer this installation.
      </p>
    </div>
  );
}

function NewUser({ onDone, onError }: { onDone: () => void; onError: (s: string) => void }) {
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [role, setRole] = useState("member");
  const [password, setPassword] = useState("");

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    try {
      await api.createUser({ email: email.trim(), name: name.trim(), role, password });
      setEmail("");
      setName("");
      setPassword("");
      onDone();
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <form onSubmit={submit} className="card" style={{ padding: 14, marginBottom: 12 }}>
      <div className="row gap-8" style={{ flexWrap: "wrap" }}>
        <input
          className="input"
          style={{ flex: "2 1 200px" }}
          placeholder="email"
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
        />
        <input
          className="input"
          style={{ flex: "2 1 160px" }}
          placeholder="name (optional)"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <select
          className="select"
          style={{ flex: "0 0 120px" }}
          value={role}
          onChange={(e) => setRole(e.target.value)}
        >
          <option value="member">member</option>
          <option value="admin">admin</option>
        </select>
        <input
          className="input"
          style={{ flex: "1 1 160px" }}
          placeholder="initial password"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        <button className="btn primary" disabled={!email.trim() || password.length < 8}>
          Create
        </button>
      </div>
      <p style={{ fontSize: 11.5, color: "var(--text-3)", margin: "8px 0 0" }}>
        At least 8 characters. Tell the person to change it after signing in.
      </p>
    </form>
  );
}

function NewTeam({ onDone, onError }: { onDone: () => void; onError: (s: string) => void }) {
  const [name, setName] = useState("");
  return (
    <form
      className="row gap-8"
      onSubmit={async (e) => {
        e.preventDefault();
        try {
          await api.createTeam(name.trim());
          setName("");
          onDone();
        } catch (e) {
          onError(e instanceof Error ? e.message : String(e));
        }
      }}
    >
      <input
        className="input"
        style={{ width: 200, padding: "6px 10px", fontSize: 13 }}
        placeholder="New group name"
        value={name}
        onChange={(e) => setName(e.target.value)}
      />
      <button className="btn" disabled={!name.trim()}>
        Create
      </button>
    </form>
  );
}

/**
 * People who authenticated and have no account here.
 *
 * Everything shown was verified by the identity provider — there is no form for
 * them to fill in and nothing here they typed. Approving is agreeing that a
 * verified address should have an account, not taking somebody's word for who
 * they are.
 */
function Waitlist({
  requests,
  act,
}: {
  requests: AccessRequest[];
  act: <T>(fn: () => Promise<T>) => Promise<void>;
}) {
  const [showDecided, setShowDecided] = useState(false);
  const pending = requests.filter((r) => r.status === "pending");
  const decided = requests.filter((r) => r.status !== "pending");

  // Nothing waiting and nothing ever decided: say nothing at all rather than
  // showing an empty table for a queue this installation may never use.
  if (requests.length === 0) return null;

  const shown = showDecided ? requests : pending;

  return (
    <div style={{ marginBottom: 26 }}>
      <div className="between" style={{ marginBottom: 10 }}>
        <span className="label" style={{ margin: 0 }}>
          Access requests
          {pending.length > 0 && (
            <span
              style={{
                marginLeft: 8,
                fontSize: 11,
                padding: "1px 7px",
                borderRadius: 10,
                background: "var(--accent)",
                color: "#fff",
              }}
            >
              {pending.length}
            </span>
          )}
        </span>
        {decided.length > 0 && (
          <button
            className="btn link"
            style={{ fontSize: 12.5 }}
            onClick={() => setShowDecided(!showDecided)}
          >
            {showDecided ? "Pending only" : `Show decided (${decided.length})`}
          </button>
        )}
      </div>

      {shown.length === 0 ? (
        <div className="card empty">Nothing waiting.</div>
      ) : (
        <div className="card">
          <div className="thead rowgrid request-row">
            <span>Verified address</span>
            <span>Name</span>
            <span>Via</span>
            <span>Asked</span>
            <span />
          </div>
          {shown.map((r) => (
            <div key={r.id} className="trow rowgrid request-row">
              <span style={{ fontWeight: 500 }}>{r.email}</span>
              <span style={{ fontSize: 13, color: "var(--text-2)" }}>{r.name || "—"}</span>
              <span style={{ fontSize: 12.5, color: "var(--text-2)" }}>{r.provider}</span>
              <span style={{ fontSize: 12.5, color: "var(--text-2)" }}>
                {new Date(r.created_at * 1000).toLocaleDateString()}
                {/* Still trying, versus gave up in March. */}
                {r.last_seen_at > r.created_at && (
                  <span style={{ color: "var(--text-3)" }}>
                    {" "}
                    · back {new Date(r.last_seen_at * 1000).toLocaleDateString()}
                  </span>
                )}
              </span>
              <span style={{ textAlign: "right" }}>
                {r.status === "pending" ? (
                  <>
                    <button
                      className="btn link"
                      style={{ fontSize: 12.5 }}
                      onClick={() => act(() => api.approveAccess(r.id))}
                    >
                      Approve
                    </button>
                    <button
                      className="btn link"
                      style={{ fontSize: 12.5, marginLeft: 12, color: "var(--path-fg)" }}
                      onClick={() => act(() => api.declineAccess(r.id))}
                    >
                      Decline
                    </button>
                  </>
                ) : (
                  <span style={{ fontSize: 12.5, color: "var(--text-3)" }}>{r.status}</span>
                )}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

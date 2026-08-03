import { useEffect, useState } from "react";
import { ShieldCheck, Trash2, UserPlus, Users } from "lucide-react";

import { api, type Me, type Team, type User } from "../api";

/**
 * Users and groups.
 *
 * Groups exist because grants attach to them rather than to individuals: a
 * private source is shared with a group, and someone joining or leaving it
 * should not mean revisiting every source.
 *
 * The API and the schema still say "team". The rename is deliberate at this
 * layer only — renaming the tables would be a migration through the grant
 * model to change a word nobody outside this screen reads.
 */
export default function Groups({ me }: { me: Me }) {
  const [users, setUsers] = useState<User[]>([]);
  const [teams, setTeams] = useState<Team[]>([]);
  const [err, setErr] = useState("");
  const [adding, setAdding] = useState(false);

  async function load() {
    try {
      const [u, t] = await Promise.all([api.users(), api.teams()]);
      setUsers(u.users ?? []);
      setTeams(t.teams ?? []);
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
        what private sources are granted to.
      </p>

      {err && (
        <p className="err" style={{ fontSize: 13, marginBottom: 12 }}>
          {err}
        </p>
      )}

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
            No groups yet. Create one to grant access to private sources.
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

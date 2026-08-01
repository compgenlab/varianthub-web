import { useState } from "react";

import { api, setToken, type Me } from "../api";

/**
 * Sign-in.
 *
 * Two ways in, because an installation has two states. Normally it is an email
 * and password against an account. Before the first administrator exists there
 * are no accounts to sign into, so the server prints a bootstrap token at
 * startup and this screen takes it — and then immediately asks for the account
 * to create, because the token's only purpose is to make that account.
 */
export default function SignIn({ me, onDone }: { me: Me; onDone: () => void }) {
  const bootstrapping = !!me.needs_bootstrap;
  return bootstrapping ? <Bootstrap onDone={onDone} /> : <Password onDone={onDone} />;
}

function Card({ children }: { children: React.ReactNode }) {
  return (
    <div style={{ height: "100%", display: "grid", placeItems: "center", background: "var(--bg)" }}>
      <div
        style={{
          width: 400,
          background: "var(--surface)",
          border: "1px solid var(--border-card)",
          borderRadius: 12,
          boxShadow: "0 8px 30px rgba(22,24,29,.06)",
          padding: "34px 32px",
        }}
      >
        <div className="wordmark" style={{ padding: 0, marginBottom: 22, fontSize: 17 }}>
          <span className="mark" />
          VariantHub
        </div>
        {children}
      </div>
    </div>
  );
}

function Password({ onDone }: { onDone: () => void }) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setErr("");
    setBusy(true);
    try {
      // The session lives in an HttpOnly cookie the server sets, so there is
      // nothing to store here — which is the point: a token this page could
      // read is a token a script on this page could exfiltrate.
      await api.login(email.trim(), password);
      setToken(""); // drop any stale bearer token; the cookie is authoritative now
      onDone();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "Could not reach the API.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card>
      <form onSubmit={submit}>
        <h1 style={{ fontSize: 22, fontWeight: 600, marginBottom: 5 }}>Sign in</h1>
        <p style={{ fontSize: 13, color: "var(--text-2)", margin: "0 0 22px" }}>
          Use your VariantHub account.
        </p>

        <label className="label" htmlFor="email">
          Email
        </label>
        <input
          id="email"
          className="input"
          type="email"
          autoFocus
          autoComplete="username"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
        />

        <label className="label" htmlFor="pw" style={{ marginTop: 14 }}>
          Password
        </label>
        <input
          id="pw"
          className="input"
          type="password"
          autoComplete="current-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />

        {err && (
          <p className="err" style={{ fontSize: 12.5, marginTop: 10 }}>
            {err}
          </p>
        )}
        <button
          className="btn"
          style={{ width: "100%", justifyContent: "center", marginTop: 20 }}
          disabled={busy || !email.trim() || !password}
        >
          {busy ? "Signing in…" : "Sign in"}
        </button>

        <p style={{ fontSize: 11.5, color: "var(--text-3)", marginTop: 16, lineHeight: 1.5 }}>
          Accounts are created by an administrator. A personal API token can be
          used from scripts, not here.
        </p>
      </form>
    </Card>
  );
}

function Bootstrap({ onDone }: { onDone: () => void }) {
  const [token, setTokenValue] = useState("");
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (password !== confirm) {
      setErr("The passwords do not match.");
      return;
    }
    setErr("");
    setBusy(true);
    try {
      // The bootstrap token authorizes exactly this one call, so it is held
      // only for the duration of it.
      setToken(token.trim());
      await api.createUser({ email: email.trim(), name: name.trim(), role: "admin", password });
      setToken("");
      await api.login(email.trim(), password);
      onDone();
    } catch (e) {
      setToken("");
      setErr(e instanceof Error ? e.message : "Could not reach the API.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card>
      <form onSubmit={submit}>
        <h1 style={{ fontSize: 22, fontWeight: 600, marginBottom: 5 }}>
          Create the first administrator
        </h1>
        <p style={{ fontSize: 13, color: "var(--text-2)", margin: "0 0 20px", lineHeight: 1.55 }}>
          This installation has no accounts yet. The server printed a bootstrap
          token to its log at startup — paste it below. It stops working as soon
          as this account exists.
        </p>

        <label className="label" htmlFor="boot">
          Bootstrap token
        </label>
        <input
          id="boot"
          className="input mono"
          type="password"
          autoFocus
          placeholder="cgl_vhb_…"
          value={token}
          onChange={(e) => setTokenValue(e.target.value)}
        />

        <label className="label" htmlFor="bemail" style={{ marginTop: 14 }}>
          Email
        </label>
        <input
          id="bemail"
          className="input"
          type="email"
          autoComplete="username"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
        />

        <label className="label" htmlFor="bname" style={{ marginTop: 14 }}>
          Name <span style={{ textTransform: "none", letterSpacing: 0 }}>(optional)</span>
        </label>
        <input
          id="bname"
          className="input"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />

        <label className="label" htmlFor="bpw" style={{ marginTop: 14 }}>
          Password
        </label>
        <input
          id="bpw"
          className="input"
          type="password"
          autoComplete="new-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />

        <label className="label" htmlFor="bpw2" style={{ marginTop: 14 }}>
          Confirm password
        </label>
        <input
          id="bpw2"
          className="input"
          type="password"
          autoComplete="new-password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
        />

        {err && (
          <p className="err" style={{ fontSize: 12.5, marginTop: 10 }}>
            {err}
          </p>
        )}
        <button
          className="btn"
          style={{ width: "100%", justifyContent: "center", marginTop: 20 }}
          disabled={busy || !token.trim() || !email.trim() || password.length < 8}
        >
          {busy ? "Creating…" : "Create administrator"}
        </button>
        <p style={{ fontSize: 11.5, color: "var(--text-3)", marginTop: 12 }}>
          Passwords must be at least 8 characters.
        </p>
      </form>
    </Card>
  );
}

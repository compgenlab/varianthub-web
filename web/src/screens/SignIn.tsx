import { useState } from "react";
import { Building2 } from "lucide-react";

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
export default function SignIn({
  me,
  onDone,
  onCancel,
}: {
  me: Me;
  onDone: () => void;
  onCancel?: () => void;
}) {
  // Bootstrapping wins: an installation with no administrator has no account to
  // sign in to, however you would otherwise authenticate.
  //
  // No way out of it either: there is exactly one thing to do with a fresh
  // installation, and offering to leave would put a marketing page in front of
  // the only action available.
  if (me.needs_bootstrap) return <Bootstrap onDone={onDone} />;
  return <Password onDone={onDone} sso={!!me.sso_enabled} onCancel={onCancel} />;
}

/** The error codes the CILogon callback redirects back with. */
const SSO_ERRORS: Record<string, string> = {
  sso_no_account:
    "That login worked, but there is no VariantHub account for it. Ask an administrator to add you.",
  sso_disabled: "That account has been disabled.",
  sso_denied: "The sign-in was cancelled at your institution.",
  sso_state: "The sign-in took too long or was interrupted. Please try again.",
  sso_not_configured: "Institutional sign-in is not configured on this server.",
  sso_no_code: "Your institution did not complete the sign-in. Please try again.",
  sso_exchange: "Could not complete sign-in with your institution.",
  sso_session: "Signed in, but the session could not be started.",
  sso_internal: "Something went wrong completing sign-in.",
};

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

function Password({
  onDone,
  sso,
  onCancel,
}: {
  onDone: () => void;
  sso: boolean;
  onCancel?: () => void;
}) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  // The callback redirects back to "/?error=..." rather than rendering its own
  // page, so the message it wants shown arrives in the query string.
  const [err, setErr] = useState(() => {
    const code = new URLSearchParams(location.search).get("error");
    return code ? (SSO_ERRORS[code] ?? "Sign-in failed.") : "";
  });
  const [busy, setBusy] = useState(false);
  // Both default closed: the institutional button is the answer for nearly
  // everyone, and anything shown beside it competes with it.
  const [showLocal, setShowLocal] = useState(false);
  const [showInfo, setShowInfo] = useState(false);

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

  // At card level rather than inside the form, because the errors that land on
  // this page most often are the ones CILogon redirects back with — and the
  // form is collapsed by default, which would make "there is no account for
  // that login" invisible to the person it is addressed to.
  const banner = err ? (
    <p className="err" style={{ fontSize: 12.5, margin: "0 0 16px" }}>
      {err}
    </p>
  ) : null;

  // Signing in is a choice made on the landing page, so it has to be one that
  // can be unmade. Without this the only ways back are the browser's back
  // button and editing the URL — and neither is obvious to someone who clicked
  // "Sign in" to see what it did.
  const escape = onCancel ? (
    <div style={{ textAlign: "center", marginTop: 18 }}>
      <button type="button" className="btn link" style={{ fontSize: 12.5 }} onClick={onCancel}>
        ← Back to VariantHub
      </button>
    </div>
  ) : null;

  const localForm = (
    <form onSubmit={submit}>
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
  );

  // With no institutional sign-in configured there is only one way in, and
  // putting it behind a disclosure would hide the whole page.
  if (!sso) {
    return (
      <Card>
        <h1 style={{ fontSize: 22, fontWeight: 600, marginBottom: 5 }}>Sign in</h1>
        <p style={{ fontSize: 13, color: "var(--text-2)", margin: "0 0 22px" }}>
          Use your VariantHub account.
        </p>
        {banner}
        {localForm}
        {escape}
      </Card>
    );
  }

  return (
    <Card>
      <h1 style={{ fontSize: 22, fontWeight: 600, marginBottom: 5 }}>Sign in</h1>
      <p style={{ fontSize: 13, color: "var(--text-2)", margin: "0 0 22px" }}>
        Use your VariantHub account.
      </p>
      {banner}

      {/* A plain link, not fetch: the whole point is to leave the SPA for the
          provider and come back with a cookie set. */}
      <a
        className="btn"
        style={{ width: "100%", justifyContent: "center", textDecoration: "none" }}
        href="/auth/cilogon"
      >
        <Building2 size={15} /> Sign in with your institution
      </a>
      {/* Named because "your institution" is the wrong mental model for half the
          people it works for: CILogon federates Google, Microsoft, GitHub and
          ORCID too, and someone without a university login otherwise reads the
          button as not for them and goes looking for an account they do not
          have. */}
      <p
        style={{
          margin: "10px 2px 0",
          fontSize: 12.5,
          lineHeight: 1.5,
          color: "var(--text-2)",
        }}
      >
        Use your university login — or Google, Microsoft, GitHub, or ORCID. All
        are supported through CILogon.
      </p>
      <div style={{ textAlign: "center", marginTop: 8 }}>
        <button
          type="button"
          className="btn link"
          style={{ fontSize: 12.5, textDecoration: "underline" }}
          onClick={() => setShowInfo(true)}
        >
          What is CILogon?
        </button>
      </div>

      {/* Tucked away rather than shown beside it. The institutional path is the
          one nearly everyone should take, and an email field sitting next to it
          invites the habit of typing an institutional address into a form that
          has never heard of it — which fails as "invalid email or password". */}
      {!showLocal ? (
        <div
          style={{
            marginTop: 16,
            paddingTop: 14,
            borderTop: "1px solid var(--border)",
            textAlign: "center",
          }}
        >
          <button
            type="button"
            className="btn link"
            style={{ fontSize: 13 }}
            onClick={() => setShowLocal(true)}
          >
            Non-institutional account
          </button>
        </div>
      ) : (
        <div style={{ marginTop: 16, paddingTop: 18, borderTop: "1px solid var(--border)" }}>
          <p style={{ margin: "0 0 14px", fontSize: 12.5, color: "var(--text-2)" }}>
            Non-institutional account
          </p>
          {localForm}
        </div>
      )}

      {escape}

      {showInfo && <CILogonInfo onClose={() => setShowInfo(false)} />}
    </Card>
  );
}

/** What CILogon is, for someone deciding whether that button is for them. */
function CILogonInfo({ onClose }: { onClose: () => void }) {
  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="cilogon-info-title"
      onClick={onClose}
      style={{
        position: "fixed",
        inset: 0,
        background: "rgba(22,24,29,.45)",
        display: "grid",
        placeItems: "center",
        padding: 22,
        zIndex: 50,
      }}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        style={{
          width: "min(440px, 100%)",
          background: "var(--surface)",
          border: "1px solid var(--border-card)",
          borderRadius: 12,
          boxShadow: "0 8px 30px rgba(22,24,29,.12)",
          padding: 26,
        }}
      >
        <div
          className="between"
          style={{ alignItems: "flex-start", gap: 12, marginBottom: 12 }}
        >
          <h2 id="cilogon-info-title" style={{ margin: 0, fontSize: 19, fontWeight: 600 }}>
            What is CILogon?
          </h2>
          <button
            type="button"
            className="btn link"
            aria-label="Close"
            style={{ fontSize: 21, lineHeight: 1, color: "var(--text-2)" }}
            onClick={onClose}
          >
            ×
          </button>
        </div>
        <div
          style={{
            display: "flex",
            flexDirection: "column",
            gap: 12,
            fontSize: 13.5,
            lineHeight: 1.6,
            color: "var(--text-1)",
          }}
        >
          <p style={{ margin: 0 }}>
            CILogon is a secure, trusted way to sign in to VariantHub using an
            account you already have — at your university, or with Google,
            Microsoft, GitHub or ORCID.
          </p>
          <p style={{ margin: 0 }}>
            Whoever you sign in through handles the authentication, so VariantHub
            never sees or stores a password for you. Your credentials stay with
            them.
          </p>
          <p style={{ margin: 0 }}>
            It also confirms the address you sign in with is really yours, which
            is what lets an administrator grant you access to a private source
            knowing it reached the right person.
          </p>
          <p style={{ margin: 0 }}>
            The first time you sign in, choose the “remember this selection”
            option and future logins will be seamless — you won’t have to pick
            your provider again.
          </p>
        </div>
        <button
          type="button"
          className="btn"
          style={{ marginTop: 20, width: "100%", justifyContent: "center" }}
          onClick={onClose}
        >
          Got it
        </button>
      </div>
    </div>
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

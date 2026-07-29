import { useState } from "react";
import { api, setToken } from "../api";

/**
 * Stands in for the design's login screen.
 *
 * There is no account system yet — the API takes one shared HMAC bearer token.
 * Rather than build a sign-in form against endpoints that do not exist, this asks
 * for that token and verifies it against a real endpoint, so a wrong token fails
 * here instead of as a confusing 401 three screens later.
 */
export default function TokenGate({ onDone }: { onDone: () => void }) {
  const [value, setValue] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setErr("");
    setBusy(true);
    setToken(value.trim());
    try {
      await api.snapshots(); // any authenticated endpoint proves the token
      onDone();
    } catch (e) {
      setToken("");
      setErr(
        e instanceof Error && e.message.includes("bearer")
          ? "That token was not accepted."
          : e instanceof Error
            ? e.message
            : "Could not reach the API.",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <div
      style={{
        height: "100%",
        display: "grid",
        placeItems: "center",
        background: "var(--bg)",
      }}
    >
      <form
        onSubmit={submit}
        style={{
          width: 392,
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
        <h1 style={{ fontSize: 22, fontWeight: 600, marginBottom: 5 }}>Sign in</h1>
        <p style={{ fontSize: 13, color: "var(--text-2)", margin: "0 0 22px" }}>
          Enter the API token for this deployment. Accounts and per-user tokens are
          not available yet.
        </p>

        <label className="label" htmlFor="tok">
          API token
        </label>
        <input
          id="tok"
          className="input mono"
          type="password"
          autoFocus
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder="eyJzdWIiOi…"
        />
        {err && (
          <p className="err" style={{ fontSize: 12.5, marginTop: 10 }}>
            {err}
          </p>
        )}
        <button
          className="btn"
          style={{ width: "100%", justifyContent: "center", marginTop: 20 }}
          disabled={busy || !value.trim()}
        >
          {busy ? "Checking…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}

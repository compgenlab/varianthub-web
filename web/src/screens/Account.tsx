import { useEffect, useState } from "react";
import { Copy, KeyRound, Trash2 } from "lucide-react";

import { api, type ApiToken, type Me } from "../api";

function when(ts?: number): string {
  if (!ts) return "—";
  return new Date(ts * 1000).toLocaleString();
}

export default function Account({ me }: { me: Me }) {
  const [tokens, setTokens] = useState<ApiToken[]>([]);
  const [name, setName] = useState("");
  const [fresh, setFresh] = useState<{ secret: string; name?: string } | null>(null);
  const [err, setErr] = useState("");
  const [copied, setCopied] = useState(false);

  async function load() {
    try {
      setTokens((await api.tokens()).tokens ?? []);
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function create(e: React.FormEvent) {
    e.preventDefault();
    try {
      const r = await api.createToken(name.trim());
      setFresh({ secret: r.secret, name: r.token.name });
      setName("");
      setCopied(false);
      await load();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }

  async function revoke(t: ApiToken) {
    if (!confirm(`Revoke ${t.name || t.prefix}? Anything using it stops working immediately.`))
      return;
    try {
      await api.revokeToken(t.id);
      await load();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }

  const active = tokens.filter((t) => !t.revoked_at);
  const revoked = tokens.filter((t) => t.revoked_at);

  return (
    <div className="page" style={{ paddingTop: 30, maxWidth: 820 }}>
      <h1 className="title">Account</h1>
      <p className="lede" style={{ fontSize: 13.5, margin: "6px 0 22px" }}>
        Signed in as <strong>{me.user?.email ?? me.label}</strong>
        {me.admin && " · administrator"}
      </p>

      {err && (
        <p className="err" style={{ fontSize: 13, marginBottom: 12 }}>
          {err}
        </p>
      )}

      <h2 className="label" style={{ marginBottom: 8 }}>
        API tokens
      </h2>
      <p className="lede" style={{ fontSize: 12.5, margin: "0 0 14px" }}>
        For scripts and the <span className="mono">varhub</span> CLI. A token acts
        as you — including your administrator rights, if you have them — and each
        one can be revoked on its own, so a token on a machine you lose does not
        take the others with it.
      </p>

      {fresh && (
        <div
          className="card"
          style={{ padding: 16, marginBottom: 16, borderColor: "var(--accent)" }}
        >
          <div className="label" style={{ marginBottom: 6 }}>
            New token{fresh.name ? ` · ${fresh.name}` : ""}
          </div>
          <div className="row gap-8">
            <code
              className="mono"
              style={{
                flex: 1,
                fontSize: 12,
                wordBreak: "break-all",
                background: "var(--neutral-fill)",
                padding: "9px 11px",
                borderRadius: 6,
              }}
            >
              {fresh.secret}
            </code>
            <button
              className="btn"
              type="button"
              onClick={() => {
                navigator.clipboard?.writeText(fresh.secret);
                setCopied(true);
              }}
            >
              <Copy size={14} /> {copied ? "Copied" : "Copy"}
            </button>
          </div>
          {/* Stored only as a hash, so this really is the only time it exists. */}
          <p style={{ fontSize: 12, color: "var(--path-fg)", margin: "10px 0 0" }}>
            Copy it now — it is stored hashed and cannot be shown again.
          </p>
        </div>
      )}

      <form onSubmit={create} className="row gap-8" style={{ marginBottom: 18 }}>
        <input
          className="input"
          style={{ flex: 1 }}
          placeholder="What is this token for? (e.g. laptop, CI)"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <button className="btn primary">
          <KeyRound size={14} /> Create token
        </button>
      </form>

      <div className="card">
        <div className="thead rowgrid token-row">
          <span>Name</span>
          <span>Prefix</span>
          <span>Created</span>
          <span>Last used</span>
          <span />
        </div>
        {active.length === 0 && (
          <div style={{ padding: "16px 18px", fontSize: 13, color: "var(--text-3)" }}>
            No active tokens.
          </div>
        )}
        {active.map((t) => (
          <div key={t.id} className="trow rowgrid token-row">
            <span style={{ fontWeight: 500 }}>{t.name || "—"}</span>
            <span className="mono" style={{ fontSize: 11.5, color: "var(--text-3)" }}>
              {t.prefix}…
            </span>
            <span style={{ fontSize: 12.5 }}>{when(t.created_at)}</span>
            {/* A token that has never been used is one nobody will miss. */}
            <span style={{ fontSize: 12.5, color: t.last_used_at ? undefined : "var(--text-3)" }}>
              {t.last_used_at ? when(t.last_used_at) : "never used"}
            </span>
            <span style={{ textAlign: "right" }}>
              <button className="btn link" onClick={() => revoke(t)} title="Revoke">
                <Trash2 size={14} />
              </button>
            </span>
          </div>
        ))}
      </div>

      {revoked.length > 0 && (
        <>
          <h2 className="label" style={{ margin: "22px 0 8px" }}>
            Revoked
          </h2>
          <div className="card">
            {revoked.map((t) => (
              <div
                key={t.id}
                className="trow rowgrid token-row"
                style={{ color: "var(--text-3)" }}
              >
                <span>{t.name || "—"}</span>
                <span className="mono" style={{ fontSize: 11.5 }}>
                  {t.prefix}…
                </span>
                <span style={{ fontSize: 12.5 }}>{when(t.created_at)}</span>
                <span style={{ fontSize: 12.5 }}>revoked {when(t.revoked_at)}</span>
                <span />
              </div>
            ))}
          </div>
        </>
      )}
    </div>
  );
}

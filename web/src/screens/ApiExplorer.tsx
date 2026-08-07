import { useEffect, useMemo, useState } from "react";
import { KeyRound, Play, ChevronRight, Copy, Check } from "lucide-react";

import { api, BASE } from "../api";

/**
 * Run real requests against the published REST API.
 *
 * The requests are real, not simulated: they go out with a bearer token, which
 * is what makes the server treat them as an external caller. So what this shows
 * is exactly what a script would get — including the endpoints that are not
 * part of the contract, which simply are not listed, because the page is a view
 * of the OpenAPI document and the document is the published surface.
 */

type Param = {
  name: string;
  in: "path" | "query";
  description?: string;
  required?: boolean;
  schema?: { type?: string; enum?: string[] };
};

type Operation = {
  operationId: string;
  summary: string;
  tags?: string[];
  parameters?: Param[];
  requestBody?: {
    content: Record<string, { schema: JsonSchema }>;
  };
  responses?: Record<string, { description?: string; content?: Record<string, { schema?: JsonSchema }> }>;
};

type JsonSchema = {
  type?: string;
  format?: string;
  description?: string;
  properties?: Record<string, JsonSchema>;
  required?: string[];
  items?: JsonSchema;
  oneOf?: JsonSchema[];
  enum?: string[];
};

type Endpoint = { method: string; path: string; op: Operation };

type Spec = {
  info: { title: string; version: string; description?: string };
  paths: Record<string, Record<string, Operation>>;
};

export default function ApiExplorer() {
  const [anonymous, setAnonymous] = useState<boolean | null>(null);
  const [spec, setSpec] = useState<Spec | null>(null);
  const [err, setErr] = useState("");
  const [selected, setSelected] = useState<string>("");

  // Held in state only. A token is a credential, and the explorer's is meant to
  // last as long as the page is open — putting it in localStorage would leave a
  // working credential behind on a shared machine.
  const [token, setToken] = useState("");
  const [tokenNote, setTokenNote] = useState("");
  const [minting, setMinting] = useState(false);
  const [remember, setRemember] = useState(false);

  // The nav hides this from an anonymous visitor, but a link can be typed or
  // bookmarked, and the page is useless without an account: every request needs
  // a token, and a token belongs to somebody.
  useEffect(() => {
    api
      .me()
      .then((m) => setAnonymous(!!m.anonymous))
      .catch(() => setAnonymous(true));
  }, []);

  useEffect(() => {
    api
      .openapi()
      .then((d) => {
        setSpec(d as Spec);
        const first = Object.keys(d.paths ?? {}).sort()[0];
        if (first) {
          const m = Object.keys(d.paths[first])[0];
          setSelected(`${m.toUpperCase()} ${first}`);
        }
      })
      .catch((e) => setErr(e.message));
    // A token kept for this tab only, if one was.
    const saved = sessionStorage.getItem("vh_explorer_token");
    if (saved) {
      setToken(saved);
      setRemember(true);
      setTokenNote("restored for this tab");
    }
  }, []);

  const endpoints = useMemo<Endpoint[]>(() => {
    if (!spec) return [];
    const out: Endpoint[] = [];
    for (const path of Object.keys(spec.paths).sort()) {
      for (const [method, op] of Object.entries(spec.paths[path])) {
        out.push({ method: method.toUpperCase(), path, op });
      }
    }
    return out;
  }, [spec]);

  const current = endpoints.find((e) => `${e.method} ${e.path}` === selected) ?? null;

  async function mint() {
    setMinting(true);
    setErr("");
    try {
      // One day, always. A token minted to try something out should stop
      // mattering on its own; anything longer is a decision to make on the
      // account page, deliberately.
      const r = await api.createToken("API explorer", 1);
      setToken(r.secret);
      setTokenNote("minted just now, valid for 1 day");
      if (remember) sessionStorage.setItem("vh_explorer_token", r.secret);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setMinting(false);
    }
  }

  function keepToken(v: boolean) {
    setRemember(v);
    if (v && token) sessionStorage.setItem("vh_explorer_token", token);
    else sessionStorage.removeItem("vh_explorer_token");
  }

  if (anonymous) {
    return (
      <div className="page" style={{ paddingTop: 26 }}>
        <h1 className="title">API</h1>
        <p className="lede">
          The REST API is authenticated with a personal token, and a token belongs
          to an account — so this page needs you signed in. Annotating here does
          not: browsing anonymously works, it just cannot issue a credential to
          call the API with.
        </p>
        <div className="card" style={{ padding: 16, marginTop: 14 }}>
          <p style={{ margin: 0, fontSize: 13.5 }}>
            Sign in from the menu at the top right, then come back. Your token is
            issued here and carries your own permissions.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="page page-wide" style={{ paddingTop: 26 }}>
      <h1 className="title">API</h1>
      <p className="lede">
        {spec?.info.description?.split("\n\n")[0] ??
          "Run requests against the published REST API."}{" "}
        Requests from this page are real and go out with the token below, so what
        you see is what a script gets.
      </p>

      {err && <p className="err">{err}</p>}

      <TokenBar
        token={token}
        note={tokenNote}
        minting={minting}
        remember={remember}
        onMint={mint}
        onPaste={(v) => {
          setToken(v);
          setTokenNote(v ? "supplied" : "");
          if (remember) {
            if (v) sessionStorage.setItem("vh_explorer_token", v);
            else sessionStorage.removeItem("vh_explorer_token");
          }
        }}
        onRemember={keepToken}
      />

      <div className="row gap-14" style={{ alignItems: "flex-start", marginTop: 18 }}>
        <div className="card" style={{ width: 320, flexShrink: 0 }}>
          <div className="thead">Endpoints</div>
          {endpoints.length === 0 && <div className="empty">Loading…</div>}
          {endpoints.map((e) => {
            const key = `${e.method} ${e.path}`;
            const on = key === selected;
            return (
              <button
                key={key}
                className="trow"
                aria-pressed={on}
                onClick={() => setSelected(key)}
                style={{
                  cursor: "pointer",
                  padding: "9px 12px",
                  width: "100%",
                  textAlign: "left",
                  background: on ? "var(--neutral-fill)" : undefined,
                }}
              >
                <span className="row gap-8">
                  <MethodTag method={e.method} />
                  <span className="mono" style={{ fontSize: 12 }}>
                    {e.path.replace("/api/v1", "")}
                  </span>
                </span>
              </button>
            );
          })}
        </div>

        <div style={{ flex: 1, minWidth: 0 }}>
          {current ? (
            <OperationPanel key={selected} endpoint={current} token={token} />
          ) : (
            <div className="card">
              <div className="empty">Choose an endpoint.</div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function MethodTag({ method }: { method: string }) {
  const colour =
    method === "GET" ? "#2f6f4f" : method === "POST" ? "#8a5a1f" : "var(--text-2)";
  return (
    <span
      className="mono"
      style={{ fontSize: 10, fontWeight: 700, color: colour, width: 34, flexShrink: 0 }}
    >
      {method}
    </span>
  );
}

function TokenBar({
  token,
  note,
  minting,
  remember,
  onMint,
  onPaste,
  onRemember,
}: {
  token: string;
  note: string;
  minting: boolean;
  remember: boolean;
  onMint: () => void;
  onPaste: (v: string) => void;
  onRemember: (v: boolean) => void;
}) {
  return (
    <div className="card" style={{ padding: 14 }}>
      <div className="row gap-8" style={{ flexWrap: "wrap", alignItems: "center" }}>
        <button className="btn sm" onClick={onMint} disabled={minting}>
          <KeyRound size={14} /> {minting ? "Minting…" : "Mint a 1-day token"}
        </button>
        <input
          className="input mono"
          style={{ flex: 1, minWidth: 260, fontSize: 12 }}
          placeholder="…or paste a token you already have (cgl_vh_…)"
          value={token}
          onChange={(e) => onPaste(e.target.value.trim())}
        />
        <label className="row gap-8" style={{ fontSize: 12.5, whiteSpace: "nowrap" }}>
          <input
            type="checkbox"
            checked={remember}
            onChange={(e) => onRemember(e.target.checked)}
          />
          Keep for this tab
        </label>
      </div>
      <p className="lede" style={{ fontSize: 12, margin: "8px 0 0" }}>
        {note && <span style={{ color: "var(--accent-text)" }}>{note}. </span>}
        The token is held in memory, and in this tab&apos;s storage only if you ask —
        never anywhere that outlives the browser session. It carries your own
        permissions.
      </p>
    </div>
  );
}

function OperationPanel({ endpoint, token }: { endpoint: Endpoint; token: string }) {
  const { method, path, op } = endpoint;
  const [values, setValues] = useState<Record<string, string>>({});
  const [body, setBody] = useState(() => exampleBody(op));
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<{ status: number; text: string; ms: number } | null>(
    null,
  );
  const [copied, setCopied] = useState(false);

  const params = op.parameters ?? [];
  const url = useMemo(() => {
    let p = path;
    for (const prm of params.filter((x) => x.in === "path")) {
      p = p.replace(`{${prm.name}}`, encodeURIComponent(values[prm.name] ?? `{${prm.name}}`));
    }
    const q = new URLSearchParams();
    for (const prm of params.filter((x) => x.in === "query")) {
      const v = values[prm.name];
      if (v) q.set(prm.name, v);
    }
    const qs = q.toString();
    return p + (qs ? `?${qs}` : "");
  }, [path, params, values]);

  const missing = params
    .filter((p) => p.in === "path" && p.required && !values[p.name])
    .map((p) => p.name);

  async function run() {
    setRunning(true);
    setResult(null);
    const started = performance.now();
    try {
      const headers: Record<string, string> = {};
      if (token) headers.Authorization = `Bearer ${token}`;
      const init: RequestInit = { method, headers };
      if (method !== "GET" && body.trim()) {
        headers["Content-Type"] = "application/json";
        init.body = body;
      }
      const res = await fetch(BASE + url, init);
      const text = await res.text();
      let pretty = text;
      try {
        pretty = JSON.stringify(JSON.parse(text), null, 2);
      } catch {
        /* not JSON — an export, most likely. Show it as it came. */
      }
      setResult({ status: res.status, text: pretty, ms: Math.round(performance.now() - started) });
    } catch (e) {
      setResult({
        status: 0,
        text: e instanceof Error ? e.message : String(e),
        ms: Math.round(performance.now() - started),
      });
    } finally {
      setRunning(false);
    }
  }

  const curl = [
    `curl ${method === "GET" ? "" : `-X ${method} `}'${location.origin}${BASE}${url}'`,
    `  -H 'Authorization: Bearer ${token || "cgl_vh_…"}'`,
    ...(method !== "GET" && body.trim()
      ? [`  -H 'Content-Type: application/json'`, `  -d '${body.replace(/\n\s*/g, "")}'`]
      : []),
  ].join(" \\\n");

  return (
    <>
      <div className="card" style={{ padding: 16, marginBottom: 14 }}>
        <div className="row gap-8" style={{ alignItems: "baseline" }}>
          <MethodTag method={method} />
          <span className="mono" style={{ fontSize: 14, fontWeight: 600 }}>
            {path}
          </span>
        </div>
        <p className="lede" style={{ fontSize: 13.5, margin: "8px 0 0" }}>
          {op.summary}
        </p>

        {params.length > 0 && (
          <div style={{ marginTop: 14 }}>
            <label className="label">Parameters</label>
            {params.map((p) => (
              <div key={p.name} style={{ marginBottom: 10 }}>
                <div className="row gap-8" style={{ alignItems: "center" }}>
                  <span className="mono" style={{ fontSize: 12, width: 92, flexShrink: 0 }}>
                    {p.name}
                    {p.required && <span style={{ color: "var(--danger, #8f2f2f)" }}>*</span>}
                  </span>
                  {p.schema?.enum ? (
                    <select
                      className="select mono"
                      style={{ flex: 1, fontSize: 12 }}
                      value={values[p.name] ?? ""}
                      onChange={(e) => setValues({ ...values, [p.name]: e.target.value })}
                    >
                      <option value="">(default)</option>
                      {p.schema.enum.map((v) => (
                        <option key={v}>{v}</option>
                      ))}
                    </select>
                  ) : (
                    <input
                      className="input mono"
                      style={{ flex: 1, fontSize: 12 }}
                      placeholder={p.in === "path" ? "required" : p.schema?.type ?? "string"}
                      value={values[p.name] ?? ""}
                      onChange={(e) => setValues({ ...values, [p.name]: e.target.value })}
                    />
                  )}
                </div>
                {p.description && (
                  <p
                    className="lede"
                    style={{ fontSize: 11.5, margin: "3px 0 0 100px", lineHeight: 1.45 }}
                  >
                    {p.description}
                  </p>
                )}
              </div>
            ))}
          </div>
        )}

        {method !== "GET" && (
          <div style={{ marginTop: 12 }}>
            <label className="label">Request body</label>
            <textarea
              className="input mono"
              style={{ width: "100%", minHeight: 130, fontSize: 12, lineHeight: 1.5 }}
              value={body}
              onChange={(e) => setBody(e.target.value)}
              spellCheck={false}
            />
          </div>
        )}

        <div className="between" style={{ marginTop: 14 }}>
          <span className="mono" style={{ fontSize: 11.5, color: "var(--text-3)" }}>
            {BASE}
            {url}
          </span>
          <button
            className="btn primary sm"
            onClick={run}
            disabled={running || missing.length > 0 || !token}
          >
            <Play size={14} />
            {running ? "Running…" : "Send"}
          </button>
        </div>
        {!token && (
          <p className="lede" style={{ fontSize: 12, margin: "8px 0 0" }}>
            Mint or paste a token above to send a request.
          </p>
        )}
        {missing.length > 0 && (
          <p className="lede" style={{ fontSize: 12, margin: "8px 0 0" }}>
            Fill in {missing.join(", ")} first.
          </p>
        )}
      </div>

      {result && (
        <div className="card" style={{ marginBottom: 14 }}>
          <div className="thead between">
            <span>
              <span
                className="mono"
                style={{
                  color: result.status >= 200 && result.status < 300 ? "#2f6f4f" : "#8f2f2f",
                  fontWeight: 700,
                }}
              >
                {result.status || "network error"}
              </span>{" "}
              <span style={{ color: "var(--text-3)", fontWeight: 400 }}>{result.ms} ms</span>
            </span>
            <button
              className="btn link"
              style={{ fontSize: 12 }}
              onClick={() => {
                navigator.clipboard?.writeText(result.text);
                setCopied(true);
                setTimeout(() => setCopied(false), 1200);
              }}
            >
              {copied ? <Check size={12} /> : <Copy size={12} />} {copied ? "Copied" : "Copy"}
            </button>
          </div>
          <pre
            className="mono"
            style={{
              margin: 0,
              padding: "12px 13px",
              fontSize: 12,
              lineHeight: 1.5,
              maxHeight: 460,
              overflow: "auto",
              whiteSpace: "pre-wrap",
              wordBreak: "break-word",
            }}
          >
            {result.text}
          </pre>
        </div>
      )}

      <details className="card" style={{ padding: "10px 13px", marginBottom: 14 }}>
        <summary style={{ cursor: "pointer", fontSize: 13, fontWeight: 500 }}>
          <ChevronRight size={12} style={{ verticalAlign: -1 }} /> As curl
        </summary>
        <pre
          className="mono"
          style={{ fontSize: 11.5, margin: "10px 0 0", whiteSpace: "pre-wrap" }}
        >
          {curl}
        </pre>
      </details>

      <ResponseShape op={op} />
    </>
  );
}

/** The documented response, so the fields can be read without running anything. */
function ResponseShape({ op }: { op: Operation }) {
  const ok = op.responses?.["200"] ?? op.responses?.["202"];
  const sch = ok?.content?.["application/json"]?.schema;
  if (!sch) {
    return null;
  }
  return (
    <details className="card" style={{ padding: "10px 13px" }}>
      <summary style={{ cursor: "pointer", fontSize: 13, fontWeight: 500 }}>
        <ChevronRight size={12} style={{ verticalAlign: -1 }} /> Response fields
      </summary>
      <div style={{ marginTop: 10 }}>
        <SchemaTree schema={sch} depth={0} />
      </div>
    </details>
  );
}

function SchemaTree({ schema, depth, name }: { schema: JsonSchema; depth: number; name?: string }) {
  // Deep enough to read, shallow enough not to unroll the whole catalog.
  if (depth > 3) {
    return null;
  }
  const props = schema.properties ?? schema.items?.properties;
  const required = new Set(schema.required ?? schema.items?.required ?? []);
  if (!props) {
    return null;
  }
  return (
    <div style={{ marginLeft: depth ? 14 : 0 }}>
      {name && (
        <div className="mono" style={{ fontSize: 11.5, color: "var(--text-3)", marginBottom: 4 }}>
          {name}
        </div>
      )}
      {Object.entries(props).map(([k, v]) => (
        <div key={k} style={{ marginBottom: 7 }}>
          <span className="mono" style={{ fontSize: 12, fontWeight: 500 }}>
            {k}
          </span>{" "}
          <span className="mono" style={{ fontSize: 11, color: "var(--accent-text)" }}>
            {v.type === "array" ? `${v.items?.type ?? "object"}[]` : v.type ?? "any"}
          </span>
          {!required.has(k) && (
            <span style={{ fontSize: 11, color: "var(--text-3)" }}> optional</span>
          )}
          {v.description && (
            <p className="lede" style={{ fontSize: 11.5, margin: "2px 0 0", lineHeight: 1.45 }}>
              {v.description}
            </p>
          )}
          {(v.properties || v.items?.properties) && (
            <SchemaTree schema={v} depth={depth + 1} />
          )}
        </div>
      ))}
    </div>
  );
}

/** A body prefilled from the documented schema, so Send works without reading docs first. */
function exampleBody(op: Operation): string {
  const sch = op.requestBody?.content?.["application/json"]?.schema;
  if (!sch?.properties) {
    return "";
  }
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(sch.properties)) {
    if (!(sch.required ?? []).includes(k)) continue;
    out[k] = v.type === "array" ? [] : v.type === "integer" || v.type === "number" ? 0 : "";
  }
  // The one call worth making runnable out of the box.
  if (op.operationId === "annotate") {
    return JSON.stringify(
      {
        variants: ["chr12:25245350:C:T"],
        build: "GRCh38",
        sources: ["gencode-48", "builtins"],
        annotations: "all",
      },
      null,
      2,
    );
  }
  return JSON.stringify(out, null, 2);
}

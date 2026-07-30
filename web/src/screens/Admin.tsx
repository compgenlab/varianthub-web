import { useEffect, useState } from "react";
import { Check, Plus, X } from "lucide-react";

import { api, type Snapshot, type Source } from "../api";

const DEFAULT_TOML = `[[sources]]
  type    = "vcf"          # builtin | vcf | bed | gtf | tab | genelist | tool
  name    = "clinvar"
  version = "2026-06"
  title   = "ClinVar"
  url     = "https://ftp.ncbi.nlm.nih.gov/pub/clinvar/vcf_GRCh38/clinvar.vcf.gz"

  [[sources.annotations]]
    name  = "clinvar_sig"
    field = "CLNSIG"
    type  = "categorical"
`;

const CODE_STYLE: React.CSSProperties = {
  width: "100%",
  minHeight: 320,
  padding: 15,
  fontFamily: "var(--mono)",
  fontSize: 12.5,
  lineHeight: 1.7,
  background: "#1c2733",
  color: "#d6e3ea",
  border: "none",
  borderRadius: 10,
  resize: "vertical",
  tabSize: 2,
};

type Tab = "sources" | "snapshots";

export default function Admin() {
  const [tab, setTab] = useState<Tab>("sources");
  const [adding, setAdding] = useState(false);
  const [building, setBuilding] = useState(false);
  const [sources, setSources] = useState<Source[]>([]);
  const [snapshots, setSnapshots] = useState<Snapshot[]>([]);
  const [err, setErr] = useState("");

  async function load() {
    try {
      const [s, n] = await Promise.all([api.sources(), api.snapshots(true)]);
      setSources(s.sources ?? []);
      setSnapshots(n.snapshots ?? []);
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  if (adding)
    return (
      <AddSource
        onCancel={() => setAdding(false)}
        onDone={() => {
          setAdding(false);
          load();
        }}
      />
    );
  if (building)
    return (
      <BuildSnapshot
        sources={sources}
        onCancel={() => setBuilding(false)}
        onDone={() => {
          setBuilding(false);
          load();
        }}
      />
    );

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <div className="between">
        <h1 className="title">Administration</h1>
        <span style={{ fontSize: 12.5, color: "var(--text-2)" }}>
          Any valid token can administer — roles are not implemented yet
        </span>
      </div>

      <div
        style={{
          display: "flex",
          gap: 4,
          margin: "16px 0 24px",
          borderBottom: "1px solid rgba(22,24,29,.1)",
        }}
      >
        {(["sources", "snapshots"] as Tab[]).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            style={{
              padding: "9px 15px",
              background: "none",
              border: "none",
              borderBottom: `2px solid ${tab === t ? "var(--accent)" : "transparent"}`,
              marginBottom: -1,
              fontSize: 14,
              fontWeight: tab === t ? 600 : 500,
              color: tab === t ? "var(--text)" : "var(--text-2)",
            }}
          >
            {t === "sources" ? "Sources" : "Snapshots"}
          </button>
        ))}
      </div>

      {err && <p className="err">{err}</p>}

      {tab === "sources" ? (
        <>
          <div className="between" style={{ marginBottom: 14 }}>
            <p className="lede" style={{ fontSize: 13.5, margin: 0 }}>
              Any tabix-indexed file — BED, GTF, VCF or tab-delimited — plus gene
              lists and tool sources. Registered from a varhub source manifest.
            </p>
            <button className="btn sm" onClick={() => setAdding(true)}>
              <Plus size={15} /> Add source
            </button>
          </div>
          <SourceTable sources={sources} />
        </>
      ) : (
        <>
          <div className="between" style={{ marginBottom: 14 }}>
            <p className="lede" style={{ fontSize: 13.5, margin: 0 }}>
              A snapshot pins specific source versions so a run is reproducible.
              Drafts are hidden from the annotation flow until published.
            </p>
            <button
              className="btn sm"
              disabled={sources.length === 0}
              onClick={() => setBuilding(true)}
            >
              <Plus size={15} /> New snapshot
            </button>
          </div>
          <SnapshotList snapshots={snapshots} onChange={load} />
        </>
      )}
    </div>
  );
}

function SourceTable({ sources }: { sources: Source[] }) {
  const cols = "1.6fr .8fr .7fr .8fr 1fr";
  return (
    <div className="card">
      <div className="rowgrid thead" style={{ gridTemplateColumns: cols }}>
        <span>Source</span>
        <span>Version</span>
        <span>Kind</span>
        <span>Access</span>
        <span>Index</span>
      </div>
      {sources.length === 0 && <div className="empty">No sources registered yet.</div>}
      {sources.map((s) => (
        <div
          key={s.id}
          className="rowgrid"
          style={{
            gridTemplateColumns: cols,
            padding: "12px 18px",
            borderBottom: "1px solid var(--hairline)",
          }}
        >
          <span>
            <span style={{ fontSize: 14, fontWeight: 500 }}>{s.title || s.name}</span>
            <br />
            <span className="mono" style={{ fontSize: 11, color: "var(--text-3)" }}>
              {s.origin || s.id}
            </span>
          </span>
          <span className="mono" style={{ fontSize: 12, color: "var(--accent-text)" }}>
            {s.version}
          </span>
          <span className="tag">{s.kind}</span>
          <span
            style={{
              fontSize: 12.5,
              color: s.visibility === "private" ? "var(--private)" : "var(--text-2)",
            }}
          >
            {s.visibility === "private" ? "Private" : "Public"}
          </span>
          <span className="row gap-8">
            <i
              className={`status-dot ${s.index_status === "building" ? "blink" : ""}`}
              style={{
                background:
                  s.index_status === "indexed"
                    ? "var(--benign-dot)"
                    : s.index_status === "building"
                      ? "var(--vus-dot)"
                      : "var(--path-dot)",
              }}
            />
            <span style={{ fontSize: 12 }}>{s.index_status}</span>
          </span>
        </div>
      ))}
    </div>
  );
}

function SnapshotList({
  snapshots,
  onChange,
}: {
  snapshots: Snapshot[];
  onChange: () => void;
}) {
  const [busy, setBusy] = useState("");
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      {snapshots.length === 0 && <div className="card empty">No snapshots yet.</div>}
      {snapshots.map((s) => (
        <div
          key={s.id}
          className="card between"
          style={{ padding: "15px 18px", gap: 14 }}
        >
          <div>
            <div className="row gap-10">
              <span style={{ fontSize: 15, fontWeight: 600 }}>{s.title || s.id}</span>
              <span
                className="mono"
                style={{
                  fontSize: 9.5,
                  padding: "2px 7px",
                  borderRadius: 5,
                  background: s.state === "published" ? "var(--benign-bg)" : "var(--vus-bg)",
                  color: s.state === "published" ? "var(--benign-fg)" : "var(--vus-fg)",
                }}
              >
                {s.state}
              </span>
            </div>
            <div className="mono" style={{ fontSize: 11.5, color: "var(--text-3)" }}>
              {s.id} · {s.build} · {s.source_count ?? 0} sources
            </div>
          </div>
          {s.state !== "published" && (
            <button
              className="btn secondary sm"
              disabled={busy === s.id}
              onClick={async () => {
                setBusy(s.id);
                try {
                  await api.publishSnapshot(s.id);
                  onChange();
                } finally {
                  setBusy("");
                }
              }}
            >
              Publish
            </button>
          )}
        </div>
      ))}
    </div>
  );
}

function AddSource({ onCancel, onDone }: { onCancel: () => void; onDone: () => void }) {
  const [toml, setToml] = useState(DEFAULT_TOML);
  const [check, setCheck] = useState<{ valid: boolean; error?: string; id?: string; kind?: string } | null>(null);
  const [priv, setPriv] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  // Validate server-side as you type: the TOML parser lives there, and a typo
  // should surface while editing rather than on submit.
  useEffect(() => {
    const t = window.setTimeout(() => {
      api
        .validateSource(toml)
        .then(setCheck)
        .catch(() => setCheck(null));
    }, 350);
    return () => window.clearTimeout(t);
  }, [toml]);

  async function register() {
    setBusy(true);
    setErr("");
    try {
      await api.createSource({ toml, visibility: priv ? "private" : "public" });
      onDone();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <button className="btn link" style={{ fontSize: 13 }} onClick={onCancel}>
        ← Back to sources
      </button>
      <h1 style={{ fontSize: 24, fontWeight: 600, margin: "14px 0 6px" }}>
        Register a source
      </h1>
      <p className="lede" style={{ fontSize: 13.5 }}>
        Paste a varhub source manifest — the same TOML{" "}
        <code className="mono">varhub source add</code> writes. It is stored
        verbatim and handed to the engine unchanged.
      </p>

      <div className="between" style={{ margin: "20px 0 6px" }}>
        <span className="label" style={{ margin: 0 }}>
          source.toml
        </span>
        {check && (
          <span
            className="mono row gap-8"
            style={{ fontSize: 11, color: check.valid ? "var(--benign-fg)" : "var(--path-fg)" }}
          >
            {check.valid ? (
              <>
                <Check size={12} /> valid · {check.id} ({check.kind})
              </>
            ) : (
              <>
                <X size={12} /> {check.error}
              </>
            )}
          </span>
        )}
      </div>

      <textarea
        style={CODE_STYLE}
        spellCheck={false}
        value={toml}
        onChange={(e) => setToml(e.target.value)}
      />

      <label className="row gap-8" style={{ marginTop: 14, fontSize: 13 }}>
        <input type="checkbox" checked={priv} onChange={(e) => setPriv(e.target.checked)} />
        Private (licensed data — access grants are not implemented yet)
      </label>

      {err && <p className="err" style={{ fontSize: 13, marginTop: 12 }}>{err}</p>}

      <div className="row gap-10" style={{ marginTop: 14, justifyContent: "flex-end" }}>
        <button className="btn secondary" onClick={onCancel}>
          Cancel
        </button>
        <button className="btn" disabled={busy || !check?.valid} onClick={register}>
          {busy ? "Registering…" : "Validate & register"}
        </button>
      </div>
    </div>
  );
}

function BuildSnapshot({
  sources,
  onCancel,
  onDone,
}: {
  sources: Source[];
  onCancel: () => void;
  onDone: () => void;
}) {
  const [id, setId] = useState("");
  const [title, setTitle] = useState("");
  const [build, setBuild] = useState("GRCh38");
  const [defaults, setDefaults] = useState("");
  const [picked, setPicked] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const chosen = sources.filter((s) => picked[s.id]).map((s) => s.id);

  async function save(publish: boolean) {
    setBusy(true);
    setErr("");
    try {
      await api.createSnapshot({
        id: id.trim(),
        title: title.trim() || undefined,
        build,
        sources: chosen,
        defaults: defaults
          .split(",")
          .map((d) => d.trim())
          .filter(Boolean),
        publish,
      });
      onDone();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <button className="btn link" style={{ fontSize: 13 }} onClick={onCancel}>
        ← Back to snapshots
      </button>
      <h1 style={{ fontSize: 24, fontWeight: 600, margin: "14px 0 18px" }}>
        Build a snapshot
      </h1>

      <div className="row gap-14" style={{ flexWrap: "wrap", marginBottom: 20 }}>
        <div style={{ flex: 1, minWidth: 220 }}>
          <label className="label">Snapshot id</label>
          <input
            className="input mono"
            value={id}
            onChange={(e) => setId(e.target.value)}
            placeholder="clinical-v4"
          />
        </div>
        <div style={{ flex: 1, minWidth: 220 }}>
          <label className="label">Title</label>
          <input
            className="input"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            placeholder="GRCh38 Clinical v4"
          />
        </div>
        <div style={{ minWidth: 190 }}>
          <label className="label">Build</label>
          <select className="select mono" value={build} onChange={(e) => setBuild(e.target.value)}>
            <option>GRCh38</option>
            <option>GRCh37</option>
            <option>T2T-CHM13v2.0</option>
            <option>GRCm39</option>
          </select>
        </div>
      </div>

      <label className="label">Sources to pin</label>
      <div className="card">
        {sources.map((s) => (
          <button
            key={s.id}
            className="trow rowgrid"
            aria-pressed={!!picked[s.id]}
            style={{ gridTemplateColumns: "24px 1.6fr 1fr", cursor: "pointer" }}
            onClick={() => setPicked({ ...picked, [s.id]: !picked[s.id] })}
          >
            <span className={`check sm ${picked[s.id] ? "on" : ""}`}>
              {picked[s.id] && <Check size={12} strokeWidth={3} />}
            </span>
            <span>
              <span style={{ fontWeight: 500 }}>{s.title || s.name}</span>{" "}
              <span className="tag">{s.kind}</span>
            </span>
            <span className="mono" style={{ fontSize: 12, color: "var(--accent-text)" }}>
              {s.version}
            </span>
          </button>
        ))}
      </div>

      <div style={{ marginTop: 16, maxWidth: 420 }}>
        <label className="label">Default annotations (comma-separated)</label>
        <input
          className="input mono"
          value={defaults}
          onChange={(e) => setDefaults(e.target.value)}
          placeholder="clinvar_sig, gnomad_af"
        />
      </div>

      {err && <p className="err" style={{ fontSize: 13, marginTop: 14 }}>{err}</p>}

      <div className="between" style={{ marginTop: 18 }}>
        <span style={{ fontSize: 13, color: "var(--text-2)" }}>
          <strong style={{ fontSize: 15, color: "var(--text)" }}>{chosen.length}</strong>{" "}
          sources pinned
        </span>
        <div className="row gap-10">
          <button
            className="btn secondary"
            disabled={busy || !id.trim() || chosen.length === 0}
            onClick={() => save(false)}
          >
            Save draft
          </button>
          <button
            className="btn"
            disabled={busy || !id.trim() || chosen.length === 0}
            onClick={() => save(true)}
          >
            {busy ? "Saving…" : "Publish snapshot"}
          </button>
        </div>
      </div>
    </div>
  );
}

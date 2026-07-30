import { useEffect, useState } from "react";
import { Check, Plus, Trash2, X } from "lucide-react";

import {
  api,
  type Registry,
  type RegistryEntry,
  type Snapshot,
  type Source,
  type SourceFile,
  type StorageLocation,
} from "../api";
import { humanSize } from "./Files";

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
  const [files, setFiles] = useState<SourceFile[]>([]);
  const [storage, setStorage] = useState<StorageLocation[]>([]);
  const [err, setErr] = useState("");

  async function load() {
    try {
      const [s, n, f, st] = await Promise.all([
        api.sources(),
        api.snapshots(true),
        api.files(),
        api.storage(),
      ]);
      setSources(s.sources ?? []);
      setSnapshots(n.snapshots ?? []);
      setFiles(f.files ?? []);
      setStorage(st.storage ?? []);
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
          <SourceTable
            sources={sources}
            files={files}
            storage={storage}
            onChange={load}
          />
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

function SourceTable({
  sources,
  files,
  storage,
  onChange,
}: {
  sources: Source[];
  files: SourceFile[];
  storage: StorageLocation[];
  onChange: () => void;
}) {
  const cols = "1.4fr .6fr .6fr .6fr 1.5fr 34px";

  // A source is "provisioned" when files are recorded for it. Summarize per
  // source so the row can show a footprint instead of a bare yes/no.
  const provisioned = new Map<string, { bytes: number; count: number }>();
  for (const f of files) {
    const cur = provisioned.get(f.source_id) ?? { bytes: 0, count: 0 };
    cur.bytes += f.size_bytes;
    cur.count += 1;
    provisioned.set(f.source_id, cur);
  }
  const targets = storage.filter((l) => l.usable);

  return (
    <div className="card">
      <div className="rowgrid thead" style={{ gridTemplateColumns: cols }}>
        <span>Source</span>
        <span>Version</span>
        <span>Kind</span>
        <span>Access</span>
        <span>Data</span>
        <span />
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
          <ProvisionCell
            source={s}
            have={provisioned.get(s.id)}
            targets={targets}
            onChange={onChange}
          />
          <DeleteSource source={s} onChange={onChange} />
        </div>
      ))}
    </div>
  );
}

/**
 * Removes a source. Refused server-side while a snapshot pins it — removing it
 * would silently change what those snapshots mean — so the error names the
 * snapshots to detach rather than the button being pre-disabled on a guess.
 */
function DeleteSource({ source, onChange }: { source: Source; onChange: () => void }) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  return (
    <span>
      <button
        className="icon-btn"
        title={`Remove ${source.name}`}
        disabled={busy}
        onClick={async () => {
          if (
            !confirm(
              `Remove source "${source.title || source.name}"?\n\n` +
                `Its downloaded files are reclaimed in the background. ` +
                `This is refused if any snapshot pins it.`,
            )
          )
            return;
          setBusy(true);
          setErr("");
          try {
            await api.deleteSource(source.id);
            onChange();
          } catch (e) {
            setErr(e instanceof Error ? e.message : String(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        <Trash2 size={14} />
      </button>
      {err && (
        <span className="err" style={{ fontSize: 11, display: "block", marginTop: 4 }}>
          {err}
        </span>
      )}
    </span>
  );
}

/**
 * Shows a source's data footprint, or offers to fetch it.
 *
 * A registered source is not usable until its data is downloaded — annotating
 * without it fails with "sources not downloaded". Putting the action on the row
 * means the gap and the fix are in the same place, rather than sending someone
 * to a different screen to work out which sources are missing.
 */
function ProvisionCell({
  source,
  have,
  targets,
  onChange,
}: {
  source: Source;
  have?: { bytes: number; count: number };
  targets: StorageLocation[];
  onChange: () => void;
}) {
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState("");
  const [err, setErr] = useState("");

  // A builtin computes from the variant; there is nothing to fetch, and it is
  // usable the moment it is registered.
  if (source.needs_data === false) {
    return (
      <span style={{ fontSize: 12.5, color: "var(--text-3)" }}>
        no data required
      </span>
    );
  }

  if (have) {
    return (
      <span className="row gap-8">
        <i className="status-dot" style={{ background: "var(--benign-dot)" }} />
        <span style={{ fontSize: 12.5 }}>
          {humanSize(have.bytes)}{" "}
          <span style={{ color: "var(--text-3)" }}>
            ({have.count} file{have.count === 1 ? "" : "s"})
          </span>
        </span>
      </span>
    );
  }

  if (msg) {
    return (
      <span style={{ fontSize: 12, color: "var(--accent-text)" }}>
        {msg}
      </span>
    );
  }

  if (targets.length === 0) {
    return (
      <span style={{ fontSize: 12, color: "var(--vus-fg)" }}>
        no usable storage configured
      </span>
    );
  }

  async function provision() {
    setBusy(true);
    setErr("");
    try {
      const r = await api.download({
        sources: [source.id],
        storage_id: target || targets[0].id,
      });
      setMsg(`queued #${r.job_id.slice(0, 8)}`);
      onChange();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <span>
      <span className="row gap-8">
        <select
          className="select"
          style={{ height: 30, fontSize: 12, padding: "0 8px", flex: 1, minWidth: 0 }}
          value={target || targets[0].id}
          onChange={(e) => setTarget(e.target.value)}
          disabled={busy}
          aria-label={`Storage location for ${source.name}`}
        >
          {targets.map((l) => (
            <option key={l.id} value={l.id}>
              {l.name}
            </option>
          ))}
        </select>
        <button
          className="btn secondary"
          style={{ height: 30, padding: "0 11px", fontSize: 12 }}
          disabled={busy}
          onClick={provision}
        >
          {busy ? "…" : "Download"}
        </button>
      </span>
      {err && (
        <span className="err" style={{ fontSize: 11.5, display: "block", marginTop: 4 }}>
          {err}
        </span>
      )}
    </span>
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
  const [editing, setEditing] = useState("");
  const [err, setErr] = useState("");

  async function act(id: string, fn: () => Promise<unknown>) {
    setBusy(id);
    setErr("");
    try {
      await fn();
      onChange();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy("");
    }
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      {err && <p className="err">{err}</p>}
      {snapshots.length === 0 && <div className="card empty">No snapshots yet.</div>}
      {snapshots.map((s) => (
        <div key={s.id} className="card" style={{ padding: "15px 18px" }}>
          <div className="between" style={{ gap: 14 }}>
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
                {s.defaults?.length ? ` · defaults: ${s.defaults.join(", ")}` : " · no defaults"}
              </div>
            </div>
            <div className="row gap-8">
              <button
                className="btn secondary sm"
                onClick={() => setEditing(editing === s.id ? "" : s.id)}
              >
                {editing === s.id ? "Close" : "Edit"}
              </button>
              {s.state !== "published" && (
                <button
                  className="btn sm"
                  disabled={busy === s.id}
                  onClick={() => act(s.id, () => api.publishSnapshot(s.id))}
                >
                  Publish
                </button>
              )}
              <button
                className="btn secondary sm"
                disabled={busy === s.id}
                style={{ color: "var(--path-fg)" }}
                onClick={() => {
                  if (
                    confirm(
                      `Delete snapshot "${s.id}"?\n\nExisting job results stay readable — they keep their own column model — but new annotation against this name will fail.`,
                    )
                  ) {
                    act(s.id, () => api.deleteSnapshot(s.id));
                  }
                }}
              >
                Delete
              </button>
            </div>
          </div>

          {editing === s.id && (
            <EditSnapshot
              snapshot={s}
              onDone={() => {
                setEditing("");
                onChange();
              }}
            />
          )}
        </div>
      ))}
    </div>
  );
}

/**
 * Edits a snapshot's title and default fields.
 *
 * Available on a published snapshot too. Publishing fixes the pinned source
 * *versions* — that is what reproducibility rests on — not the label or which
 * fields are pre-checked. A job records the annotations it actually ran with, so
 * changing a default does not rewrite history.
 */
function EditSnapshot({ snapshot, onDone }: { snapshot: Snapshot; onDone: () => void }) {
  const [title, setTitle] = useState(snapshot.title ?? "");
  const [defaults, setDefaults] = useState((snapshot.defaults ?? []).join(", "));
  const [fields, setFields] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  useEffect(() => {
    api
      .snapshot(snapshot.id)
      .then((d) => setFields((d.annotations ?? []).map((a) => a.name)))
      .catch(() => {});
  }, [snapshot.id]);

  const chosen = defaults.split(",").map((d) => d.trim()).filter(Boolean);

  return (
    <div style={{ marginTop: 14, paddingTop: 14, borderTop: "1px solid var(--hairline)" }}>
      <div className="row gap-14" style={{ flexWrap: "wrap", alignItems: "flex-end" }}>
        <div style={{ flex: 1, minWidth: 240 }}>
          <label className="label">Title</label>
          <input className="input" value={title} onChange={(e) => setTitle(e.target.value)} />
        </div>
        <div style={{ flex: 2, minWidth: 280 }}>
          <label className="label">Default fields (comma-separated)</label>
          <input
            className="input mono"
            style={{ fontSize: 12 }}
            value={defaults}
            onChange={(e) => setDefaults(e.target.value)}
          />
        </div>
      </div>

      {fields.length > 0 && (
        <div className="row gap-8" style={{ flexWrap: "wrap", marginTop: 10 }}>
          {fields.map((f) => {
            const on = chosen.includes(f);
            return (
              <button
                key={f}
                className="tag"
                style={{
                  cursor: "pointer",
                  border: "none",
                  background: on ? "var(--accent-tint)" : "var(--neutral-fill)",
                  color: on ? "var(--accent-text)" : "var(--text-2)",
                }}
                onClick={() =>
                  setDefaults(
                    (on ? chosen.filter((c) => c !== f) : [...chosen, f]).join(", "),
                  )
                }
              >
                {on ? "✓ " : "+ "}
                {f}
              </button>
            );
          })}
        </div>
      )}

      {err && <p className="err" style={{ fontSize: 12.5, marginTop: 10 }}>{err}</p>}

      <div className="row gap-8" style={{ marginTop: 12, justifyContent: "flex-end" }}>
        <button
          className="btn sm"
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            setErr("");
            try {
              await api.updateSnapshotMeta(snapshot.id, { title, defaults: chosen });
              onDone();
            } catch (e) {
              setErr(e instanceof Error ? e.message : String(e));
            } finally {
              setBusy(false);
            }
          }}
        >
          {busy ? "Saving…" : "Save"}
        </button>
      </div>
      <p style={{ fontSize: 11.5, color: "var(--text-3)", marginTop: 8 }}>
        Pinned source versions cannot be changed once published — that is what makes
        a snapshot reproducible. Create a new snapshot to change them.
      </p>
    </div>
  );
}

function AddSource({ onCancel, onDone }: { onCancel: () => void; onDone: () => void }) {
  const [toml, setToml] = useState(DEFAULT_TOML);
  const [check, setCheck] = useState<{ valid: boolean; error?: string; id?: string; kind?: string } | null>(null);
  const [priv, setPriv] = useState(false);
  const [origin, setOrigin] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const [registries, setRegistries] = useState<Registry[]>([]);
  const [regID, setRegID] = useState("");
  const [entries, setEntries] = useState<RegistryEntry[] | null>(null);
  const [regErr, setRegErr] = useState("");
  const [loadingRef, setLoadingRef] = useState("");
  const [addingReg, setAddingReg] = useState(false);

  async function loadRegistries(select?: string) {
    try {
      const r = await api.registries();
      setRegistries(r.registries ?? []);
      const pick = select ?? r.registries?.[0]?.id ?? "";
      setRegID(pick);
    } catch (e) {
      setRegErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    loadRegistries();
  }, []);

  // Fetching a registry's catalog hits a remote; keep it out of the render path
  // and show its own error, since an unreachable registry is not a fault of this
  // form and must not block writing a manifest by hand.
  useEffect(() => {
    if (!regID) return;
    setEntries(null);
    setRegErr("");
    api
      .registryDatasets(regID)
      .then((d) => setEntries(d.sources ?? []))
      .catch((e) => setRegErr(e instanceof Error ? e.message : String(e)));
  }, [regID]);

  useEffect(() => {
    const t = window.setTimeout(() => {
      api
        .validateSource(toml)
        .then(setCheck)
        .catch(() => setCheck(null));
    }, 350);
    return () => window.clearTimeout(t);
  }, [toml]);

  async function use(entry: RegistryEntry) {
    const ref = entry.version ? `${entry.name}:${entry.version}` : entry.name;
    setLoadingRef(ref);
    setErr("");
    try {
      const d = await api.registryFetch(regID, ref);
      setToml(d.toml);
      setOrigin(d.origin);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoadingRef("");
    }
  }

  async function register() {
    setBusy(true);
    setErr("");
    try {
      await api.createSource({
        toml,
        visibility: priv ? "private" : "public",
        origin: origin || undefined,
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
        ← Back to sources
      </button>
      <h1 style={{ fontSize: 24, fontWeight: 600, margin: "14px 0 6px" }}>
        Register a source
      </h1>
      <p className="lede" style={{ fontSize: 13.5 }}>
        Pull a dataset from a registry, or write the manifest directly. Everything
        resolves to a TOML manifest that pins where the file comes from and how it
        is indexed — stored verbatim and handed to the engine unchanged.
      </p>

      <div
        style={{
          display: "grid",
          gridTemplateColumns: "340px 1fr",
          gap: 20,
          alignItems: "start",
          marginTop: 20,
        }}
      >
        <div>
          <div className="between" style={{ marginBottom: 6 }}>
            <span className="label" style={{ margin: 0 }}>
              Registry
            </span>
            <button
              className="btn link"
              style={{ fontSize: 12 }}
              onClick={() => setAddingReg(!addingReg)}
            >
              {addingReg ? "Cancel" : "+ Add registry"}
            </button>
          </div>

          {addingReg ? (
            <AddRegistry
              onCancel={() => setAddingReg(false)}
              onDone={(id) => {
                setAddingReg(false);
                loadRegistries(id);
              }}
            />
          ) : (
            <select
              className="select mono"
              style={{ marginBottom: 14 }}
              value={regID}
              onChange={(e) => setRegID(e.target.value)}
            >
              {registries.length === 0 && <option value="">(no registries)</option>}
              {registries.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.name}
                  {r.builtin ? " (default)" : ""}
                </option>
              ))}
            </select>
          )}

          <div className="card">
            <div className="thead">Available datasets</div>
            {regErr && (
              <div className="empty err" style={{ padding: "16px 13px", fontSize: 12.5 }}>
                {regErr}
              </div>
            )}
            {!regErr && entries === null && <div className="empty">Loading…</div>}
            {!regErr && entries?.length === 0 && (
              <div className="empty">This registry lists no sources.</div>
            )}
            {entries?.map((e) => {
              const ref = e.version ? `${e.name}:${e.version}` : e.name;
              return (
                <button
                  key={ref}
                  className="trow between"
                  style={{ cursor: "pointer", padding: "11px 13px" }}
                  disabled={!!loadingRef}
                  onClick={() => use(e)}
                >
                  <span>
                    <span style={{ fontSize: 13, fontWeight: 500 }}>
                      {e.title || e.name}
                    </span>
                    <br />
                    <span className="mono" style={{ fontSize: 10.5, color: "var(--text-3)" }}>
                      {ref}
                      {e.assembly ? ` · ${e.assembly}` : ""}
                      {e.non_commercial ? " · non-commercial" : ""}
                    </span>
                  </span>
                  <span style={{ fontSize: 12, fontWeight: 500, color: "var(--accent-text)" }}>
                    {loadingRef === ref ? "…" : "Use →"}
                  </span>
                </button>
              );
            })}
          </div>
        </div>

        <div>
          <div className="between" style={{ marginBottom: 6 }}>
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
            onChange={(e) => {
              setToml(e.target.value);
              setOrigin(""); // hand-edited: no longer straight from the registry
            }}
          />

          <label className="row gap-8" style={{ marginTop: 14, fontSize: 13 }}>
            <input type="checkbox" checked={priv} onChange={(e) => setPriv(e.target.checked)} />
            Private (licensed data — access grants are not implemented yet)
          </label>
          {origin && (
            <p className="mono" style={{ fontSize: 11, color: "var(--text-3)", marginTop: 6 }}>
              origin: {origin}
            </p>
          )}

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
      </div>
    </div>
  );
}

function AddRegistry({
  onCancel,
  onDone,
}: {
  onCancel: () => void;
  onDone: (id: string) => void;
}) {
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [warn, setWarn] = useState("");

  async function save() {
    setBusy(true);
    setErr("");
    setWarn("");
    try {
      const r = await api.addRegistry({ name: name.trim(), url: url.trim() });
      // Saved but unreadable is a warning, not a failure: the registry may be
      // temporarily down, and refusing to save would lose the operator's input.
      if (r.warning) {
        setWarn(r.warning);
        return;
      }
      onDone(r.id);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card" style={{ padding: 14, marginBottom: 14 }}>
      <label className="label">Name</label>
      <input
        className="input"
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder="Lab registry"
      />
      <label className="label" style={{ marginTop: 10 }}>
        registry.toml URL
      </label>
      <input
        className="input mono"
        style={{ fontSize: 12 }}
        value={url}
        onChange={(e) => setUrl(e.target.value)}
        placeholder="https://example.org/registry.toml"
      />
      {err && <p className="err" style={{ fontSize: 12, marginTop: 8 }}>{err}</p>}
      {warn && (
        <p style={{ fontSize: 12, marginTop: 8, color: "var(--vus-fg)" }}>{warn}</p>
      )}
      <div className="row gap-8" style={{ marginTop: 12, justifyContent: "flex-end" }}>
        <button className="btn secondary sm" onClick={onCancel}>
          Cancel
        </button>
        <button className="btn sm" disabled={busy || !name.trim() || !url.trim()} onClick={save}>
          {busy ? "Checking…" : "Add"}
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

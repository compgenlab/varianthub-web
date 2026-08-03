import { useEffect, useState } from "react";
import { Check, Cloud, Globe, HardDrive, Plus, Trash2, X } from "lucide-react";

import {
  api,
  type Registry,
  type RegistryEntry,
  type Snapshot,
  type Source,
  type SourceFile,
  type SourceAsset,
  type StorageLocation,
  type Team,
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
          <SnapshotList snapshots={snapshots} allSources={sources} onChange={load} />
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
  // Which source's manifest is expanded, if any.
  const [showConfig, setShowConfig] = useState("");
  const [showGrants, setShowGrants] = useState("");

  // A source is "provisioned" when files are recorded for it. Summarized per
  // source *and* per location: a source can be in two places at once, and
  // "1.2 GB somewhere" does not answer the question the row is being read for.
  const provisioned = new Map<string, Map<string, { bytes: number; count: number }>>();
  for (const f of files) {
    const byLoc = provisioned.get(f.source_id) ?? new Map();
    const cur = byLoc.get(f.storage_id) ?? { bytes: 0, count: 0 };
    cur.bytes += f.size_bytes;
    cur.count += 1;
    byLoc.set(f.storage_id, cur);
    provisioned.set(f.source_id, byLoc);
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
            <button
              onClick={() => setShowConfig(showConfig === s.id ? "" : s.id)}
              title="Show this source's manifest"
              style={{
                background: "none",
                border: "none",
                padding: 0,
                cursor: "pointer",
                textAlign: "left",
                fontSize: 14,
                fontWeight: 500,
                color: "var(--accent-text)",
              }}
            >
              {s.title || s.name}
            </button>
            <br />
            <span className="mono" style={{ fontSize: 11, color: "var(--text-3)" }}>
              {s.origin || s.id}
            </span>
          </span>
          <span className="mono" style={{ fontSize: 12, color: "var(--accent-text)" }}>
            {s.version}
          </span>
          <span className="row gap-8" style={{ flexWrap: "wrap" }}>
            <span className="tag">{s.kind}</span>
            {/* A remote source is read from its origin over the network. It
                needs no download, so without this it reads as one that has
                simply not been fetched yet. */}
            {s.stream && (
              <span className="tag tag-remote" title="Read from its origin over the network">
                <Globe size={10} /> remote
              </span>
            )}
          </span>
          <span
            style={{
              fontSize: 12.5,
              color: s.visibility === "private" ? "var(--private)" : "var(--text-2)",
            }}
          >
            {s.visibility === "private" ? (
              // Private is the default, so most sources land here; the button
              // is what decides who can actually see them.
              <button
                className="btn link"
                style={{ fontSize: 12.5, padding: 0, color: "var(--private)" }}
                onClick={() => setShowGrants(showGrants === s.id ? "" : s.id)}
              >
                Private ▾
              </button>
            ) : (
              "Public"
            )}
          </span>
          <ProvisionCell
            source={s}
            have={provisioned.get(s.id)}
            storage={storage}
            targets={targets}
            onChange={onChange}
          />
          <DeleteSource source={s} onChange={onChange} />
          {showConfig === s.id && <SourceConfig id={s.id} />}
          {showGrants === s.id && <SourceGrants id={s.id} />}
        </div>
      ))}
    </div>
  );
}

/**
 * Shows a source's stored manifest.
 *
 * The manifest is the source of truth — the listed columns are a projection of
 * it — so this is how an admin checks what a source actually declares instead
 * of inferring it from the row.
 */
function SourceConfig({ id }: { id: string }) {
  const [toml, setToml] = useState("");
  const [err, setErr] = useState("");

  useEffect(() => {
    let live = true;
    api
      .sourceConfig(id)
      .then((r) => live && setToml(r.config))
      .catch((e) => live && setErr(String(e.message ?? e)));
    return () => {
      live = false;
    };
  }, [id]);

  return (
    <pre
      className="mono"
      style={{
        gridColumn: "1 / -1",
        margin: "10px 0 0",
        padding: 12,
        fontSize: 12,
        lineHeight: 1.5,
        background: "var(--surface-2)",
        border: "1px solid var(--hairline)",
        borderRadius: 6,
        overflowX: "auto",
        whiteSpace: "pre",
        color: err ? "var(--danger)" : "var(--text-1)",
      }}
    >
      {err || toml || "Loading…"}
    </pre>
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
 * One place a source's data is stored.
 *
 * Names the location rather than just the kind: a deployment can have several
 * buckets, and "in S3" does not say which one to look in — or which one to
 * clean up when the disk bill arrives.
 */
function StoredAt({
  location,
  bytes,
  count,
}: {
  location?: StorageLocation;
  bytes: number;
  count: number;
}) {
  const s3 = location?.kind === "s3";
  // The bucket, for an s3 location: the URI is the operational identifier, and
  // the friendly name may be anything someone typed.
  const where = s3 ? (location?.uri ?? "").replace(/^s3:\/\//, "") : location?.name;
  return (
    <span className="row gap-8" style={{ fontSize: 12.5 }}>
      <i className="status-dot" style={{ background: "var(--benign-dot)" }} />
      {s3 ? <Cloud size={12} /> : <HardDrive size={12} />}
      <span>
        {humanSize(bytes)}{" "}
        <span style={{ color: "var(--text-3)" }}>
          ({count} file{count === 1 ? "" : "s"}) · {where ?? "unknown location"}
        </span>
      </span>
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
  storage,
  targets,
  onChange,
}: {
  source: Source;
  have?: Map<string, { bytes: number; count: number }>;
  storage: StorageLocation[];
  targets: StorageLocation[];
  onChange: () => void;
}) {
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState("");
  const [err, setErr] = useState("");

  // Nothing to fetch: a builtin computes from the variant, a streamed source is
  // read from its url. Both are usable the moment they are registered, so say
  // which rather than offering a control that would provision nothing.
  if (source.needs_data === false && !source.stream) {
    return (
      <span style={{ fontSize: 12.5, color: "var(--text-3)" }}>no data required</span>
    );
  }

  // A streamed source reads from its url, so it needs nothing — but a copy is
  // worth having for whole-genome runs, or to pin results to bytes that cannot
  // change upstream. Offered, not pressed: the default stays "no copy".
  const streamed = source.needs_data === false && source.stream;

  if (have && have.size > 0) {
    return (
      <span style={{ display: "block" }}>
        {[...have].map(([id, v]) => (
          <StoredAt key={id} location={storage.find((l) => l.id === id)} {...v} />
        ))}
        {/* A streamed source that also has a copy: say so, or the copy looks
            like the only way it is read. */}
        {streamed && (
          <span style={{ fontSize: 11, color: "var(--text-3)", display: "block", marginTop: 2 }}>
            also readable from its origin
          </span>
        )}
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
        include_streamed: streamed,
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
      {/* Without this a working remote source looks exactly like a broken
          un-provisioned one — same dropdown, same Download button. It is
          already usable; the copy is an optimisation, so say that before
          offering the control rather than only in a tooltip. */}
      {streamed && (
        <span
          className="row gap-8"
          style={{ fontSize: 11.5, color: "var(--text-2)", marginBottom: 4 }}
        >
          <Globe size={11} /> reads from origin — copy optional
        </span>
      )}
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
          title={
            streamed
              ? "This source is read from its url. Download a copy anyway — useful for whole-genome runs, or to pin results to bytes that cannot change upstream."
              : undefined
          }
        >
          {busy ? "…" : streamed ? "Copy locally" : "Download"}
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
  allSources,
  onChange,
}: {
  snapshots: Snapshot[];
  allSources: Source[];
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
            <>
              <EditSnapshot
                snapshot={s}
                onDone={() => {
                  setEditing("");
                  onChange();
                }}
              />
              <EditSnapshotSources snapshot={s} sources={allSources} onChange={onChange} />
            </>
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
  // Derived from the client rather than restated: this had drifted from what
  // the endpoint actually returns, so fields the server sent were invisible here.
  const [check, setCheck] = useState<Awaited<ReturnType<typeof api.validateSource>> | null>(null);
  const [priv, setPriv] = useState(false);
  const [origin, setOrigin] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  // Where the data should go, asked here rather than after registering.
  //
  // A source with files is not usable until they are fetched, and the previous
  // flow ended at "registered" — leaving a source that looks present and fails
  // at annotate time with "sources not downloaded". Asking now means one
  // decision instead of a second trip through a different screen.
  const [targets, setTargets] = useState<StorageLocation[]>([]);
  const [target, setTarget] = useState("");
  // Off for a streamed source: it reads from its origin, so a copy is a
  // deliberate extra rather than the thing that makes it work.
  const [alsoCopy, setAlsoCopy] = useState(false);

  // Helper files that came with a registry fragment. Held here and posted with
  // the manifest, so what is stored is what was on screen.
  const [assets, setAssets] = useState<SourceAsset[]>([]);
  const [assetErr, setAssetErr] = useState("");

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
    api
      .storage()
      .then((r) => {
        const usable = (r.storage ?? []).filter((l) => l.usable);
        setTargets(usable);
        setTarget(usable.find((l) => l.is_default)?.id ?? usable[0]?.id ?? "");
      })
      .catch(() => setTargets([]));
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
      setAssets(d.assets ?? []);
      setAssetErr(d.asset_error ?? "");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setLoadingRef("");
    }
  }

  // What this source will need after it is registered.
  const needsData = check?.valid === true && check.needs_data === true;
  const streamed = check?.valid === true && check.stream === true;
  const willDownload = (needsData || (streamed && alsoCopy)) && !!target;

  async function register() {
    setBusy(true);
    setErr("");
    try {
      const created = await api.createSource({
        toml,
        visibility: priv ? "private" : "public",
        origin: origin || undefined,
        assets: assets.length > 0 ? assets : undefined,
      });
      if (willDownload) {
        // Queued immediately, and separately: the source is registered either
        // way. A queue that refuses the job should not roll back a manifest
        // that is already correct — it should say so and leave the download to
        // be retried from the sources list.
        try {
          await api.download({
            sources: [created.id],
            storage_id: target,
            include_streamed: streamed,
          });
        } catch (e) {
          setErr(
            `Registered ${created.ref}, but the download could not be queued: ` +
              (e instanceof Error ? e.message : String(e)) +
              ". Start it from the sources list.",
          );
          setBusy(false);
          return;
        }
      }
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

          {(assets.length > 0 || assetErr) && (
            <AssetList assets={assets} error={assetErr} />
          )}

          {(needsData || streamed) && (
            <DownloadTarget
              needsData={needsData}
              streamed={streamed}
              targets={targets}
              target={target}
              setTarget={setTarget}
              alsoCopy={alsoCopy}
              setAlsoCopy={setAlsoCopy}
            />
          )}

          {err && <p className="err" style={{ fontSize: 13, marginTop: 12 }}>{err}</p>}

          <div className="row gap-10" style={{ marginTop: 14, justifyContent: "flex-end" }}>
            <button className="btn secondary" onClick={onCancel}>
              Cancel
            </button>
            <button
              className="btn"
              disabled={busy || !check?.valid || (needsData && targets.length > 0 && !target)}
              onClick={register}
            >
              {busy
                ? willDownload
                  ? "Registering & queueing…"
                  : "Registering…"
                : willDownload
                  ? "Register & download"
                  : "Validate & register"}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

/**
 * The helper files that arrived with a registry fragment.
 *
 * Shown, and expandable, rather than imported quietly. A build recipe executes
 * these — the same reason the fragment itself goes into an editor instead of
 * being one-click imported applies more strongly to a script than to the TOML
 * that names it.
 */
function AssetList({ assets, error }: { assets: SourceAsset[]; error?: string }) {
  const [open, setOpen] = useState("");
  if (error) {
    return (
      <div className="card" style={{ padding: 12, marginTop: 14, borderColor: "var(--vus-fg)" }}>
        <p style={{ fontSize: 12.5, color: "var(--vus-fg)", margin: 0 }}>
          This source names helper files that could not be fetched: {error}. It can
          still be registered, but a build recipe will fail without them.
        </p>
      </div>
    );
  }
  return (
    <div className="card" style={{ padding: 12, marginTop: 14 }}>
      <div className="label" style={{ marginBottom: 8 }}>
        Helper files ({assets.length})
      </div>
      <p style={{ fontSize: 11.5, color: "var(--text-3)", margin: "0 0 8px" }}>
        Scripts this source&apos;s build recipe runs. They are stored with the
        manifest and written beside it when a job runs.
      </p>
      {assets.map((a) => (
        <div key={a.name} style={{ marginBottom: 6 }}>
          <button
            className="btn link mono"
            style={{ fontSize: 12, padding: 0 }}
            onClick={() => setOpen(open === a.name ? "" : a.name)}
          >
            {open === a.name ? "▾" : "▸"} {a.name}{" "}
            <span style={{ color: "var(--text-3)" }}>
              ({a.content.split("\n").length} lines)
            </span>
          </button>
          {open === a.name && (
            <pre
              className="mono"
              style={{
                margin: "6px 0 0",
                padding: 10,
                fontSize: 11.5,
                lineHeight: 1.5,
                maxHeight: 260,
                overflow: "auto",
                background: "var(--neutral-fill)",
                borderRadius: 6,
                whiteSpace: "pre",
              }}
            >
              {a.content}
            </pre>
          )}
        </div>
      ))}
    </div>
  );
}

/**
 * Where a newly registered source's data should go.
 *
 * Shown before registering rather than after, because "registered" and "usable"
 * are different states and the gap between them is invisible: a source with no
 * data looks fine in the list and fails at annotate time. Asking here collapses
 * the two into one decision.
 */
function DownloadTarget({
  needsData,
  streamed,
  targets,
  target,
  setTarget,
  alsoCopy,
  setAlsoCopy,
}: {
  needsData: boolean;
  streamed: boolean;
  targets: StorageLocation[];
  target: string;
  setTarget: (v: string) => void;
  alsoCopy: boolean;
  setAlsoCopy: (v: boolean) => void;
}) {
  if (needsData && targets.length === 0) {
    return (
      <div
        className="card"
        style={{ padding: 12, marginTop: 14, borderColor: "var(--vus-fg)" }}
      >
        <p style={{ fontSize: 12.5, color: "var(--vus-fg)", margin: 0 }}>
          This source has data to download, but no storage location is configured.
          It can still be registered — add a location under{" "}
          <strong>Storage &amp; files</strong>, then download it from the sources
          list.
        </p>
      </div>
    );
  }

  return (
    <div className="card" style={{ padding: 12, marginTop: 14 }}>
      <div className="label" style={{ marginBottom: 8 }}>
        {needsData ? "Download to" : "Local copy"}
      </div>

      {streamed && (
        <label className="row gap-8" style={{ fontSize: 13, marginBottom: alsoCopy ? 10 : 0 }}>
          <input
            type="checkbox"
            checked={alsoCopy}
            onChange={(e) => setAlsoCopy(e.target.checked)}
          />
          {/* Unchecked by default: this source already works without a copy. */}
          Also keep a local copy — this source reads from its origin, so a copy
          is optional
        </label>
      )}

      {(needsData || alsoCopy) && (
        <>
          <select
            className="select"
            style={{ maxWidth: 320 }}
            value={target}
            onChange={(e) => setTarget(e.target.value)}
            aria-label="Download location"
          >
            {targets.map((l) => (
              <option key={l.id} value={l.id}>
                {l.name} — {l.uri}
              </option>
            ))}
          </select>
          <p style={{ fontSize: 11.5, color: "var(--text-3)", margin: "8px 0 0" }}>
            The download is queued as soon as the source is registered. Watch it
            under <strong>System jobs</strong>.
          </p>
        </>
      )}
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
              <span className="tag">{s.kind}</span>{" "}
              {/* Pinning a remote source makes the snapshot depend on somebody
                  else's server staying up, which is worth knowing before it is
                  published rather than after a run fails. */}
              {s.stream && (
                <span className="tag tag-remote" title="Read from its origin over the network">
                  <Globe size={10} /> remote
                </span>
              )}
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

/**
 * Adds and removes a snapshot's sources.
 *
 * Refused server-side once published: a published snapshot is a reproducibility
 * claim, so changing which sources it contains would silently change what every
 * past result meant. The checkboxes are disabled rather than the request being
 * allowed to fail, since there is nothing the user could do to make it succeed.
 *
 * Sources whose assembly differs from the snapshot's are also disabled, with the
 * mismatch named. A wrong assembly does not error at annotate time — it returns
 * plausible answers at coordinates that mean something else — so it is worth
 * refusing at the point of choosing rather than explaining afterwards.
 */
function EditSnapshotSources({
  snapshot,
  sources,
  onChange,
}: {
  snapshot: Snapshot;
  sources: Source[];
  onChange: () => void;
}) {
  const pinned = new Set((snapshot.sources ?? []).map((s) => s.id));
  const [picked, setPicked] = useState<Set<string>>(new Set(pinned));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const frozen = snapshot.state === "published";

  const dirty =
    picked.size !== pinned.size || [...picked].some((id) => !pinned.has(id));

  function toggle(id: string) {
    const next = new Set(picked);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    setPicked(next);
  }

  async function save() {
    setBusy(true);
    setErr("");
    try {
      await api.setSnapshotSources(snapshot.id, [...picked]);
      onChange();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div style={{ marginTop: 12, paddingTop: 12, borderTop: "1px solid var(--hairline)" }}>
      <div className="between" style={{ marginBottom: 8 }}>
        <span style={{ fontSize: 13, fontWeight: 600 }}>Sources</span>
        {!frozen && dirty && (
          <button className="btn sm" disabled={busy || picked.size === 0} onClick={save}>
            {busy ? "Saving…" : "Save sources"}
          </button>
        )}
      </div>
      {frozen && (
        <p style={{ fontSize: 12, color: "var(--text-3)", margin: "0 0 8px" }}>
          Published — its sources are fixed. Duplicate it to change them.
        </p>
      )}
      {err && <p className="err">{err}</p>}
      <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
        {sources.map((src) => {
          const clash = !!src.build && !!snapshot.build && src.build !== snapshot.build;
          return (
            <label
              key={src.id}
              className="row gap-8"
              style={{
                fontSize: 13,
                opacity: frozen || clash ? 0.55 : 1,
                cursor: frozen || clash ? "not-allowed" : "pointer",
              }}
            >
              <input
                type="checkbox"
                checked={picked.has(src.id)}
                disabled={frozen || clash}
                onChange={() => toggle(src.id)}
              />
              <span>{src.title || src.name}</span>
              <span className="mono" style={{ fontSize: 11, color: "var(--text-3)" }}>
                {src.id}
              </span>
              {clash && (
                <span style={{ fontSize: 11, color: "var(--path-fg)" }}>
                  {src.build}, not {snapshot.build}
                </span>
              )}
            </label>
          );
        })}
      </div>
    </div>
  );
}

/**
 * Which groups may see a private source.
 *
 * Grants attach to groups rather than to individuals so that access survives
 * membership changes: adding someone to the group is one action, not one per
 * source they need. (The API still calls these teams.)
 */
function SourceGrants({ id }: { id: string }) {
  const [granted, setGranted] = useState<Team[]>([]);
  const [teams, setTeams] = useState<Team[]>([]);
  const [err, setErr] = useState("");

  async function load() {
    try {
      const [g, all] = await Promise.all([api.grants(id), api.teams()]);
      setGranted(g.teams ?? []);
      setTeams(all.teams ?? []);
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, [id]);

  async function act<T>(fn: () => Promise<T>) {
    try {
      await fn();
      await load();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <div
      style={{
        gridColumn: "1 / -1",
        padding: "12px 4px 4px",
        borderTop: "1px solid var(--hairline)",
        marginTop: 10,
      }}
    >
      <div className="label" style={{ marginBottom: 8 }}>
        Groups with access
      </div>
      {err && <p className="err" style={{ fontSize: 12.5 }}>{err}</p>}
      <div className="row gap-8" style={{ flexWrap: "wrap" }}>
        {granted.map((t) => (
          <span key={t.id} className="tag" style={{ display: "inline-flex", gap: 6 }}>
            {t.name}
            <button
              className="btn link"
              style={{ padding: 0, fontSize: 11 }}
              onClick={() => act(() => api.revokeGrant(id, t.id))}
              title="Revoke access"
            >
              ×
            </button>
          </span>
        ))}
        {granted.length === 0 && (
          <span style={{ fontSize: 12.5, color: "var(--text-3)" }}>
            No groups — only administrators can see this source.
          </span>
        )}
      </div>
      <select
        className="select"
        style={{ fontSize: 12.5, padding: "4px 8px", marginTop: 10, maxWidth: 260 }}
        value=""
        onChange={(e) => {
          if (e.target.value) act(() => api.grant(id, e.target.value));
        }}
      >
        <option value="">Grant to a group…</option>
        {teams
          .filter((t) => !granted.some((g) => g.id === t.id))
          .map((t) => (
            <option key={t.id} value={t.id}>
              {t.name}
            </option>
          ))}
      </select>
      {teams.length === 0 && (
        <p style={{ fontSize: 11.5, color: "var(--text-3)", marginTop: 8 }}>
          No groups exist yet — create one under <strong>Users &amp; groups</strong>.
        </p>
      )}
    </div>
  );
}

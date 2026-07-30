import { useEffect, useState } from "react";
import { Download, HardDrive, Trash2, TriangleAlert } from "lucide-react";

import { api, type Source, type SourceFile, type StorageLocation } from "../api";

export function humanSize(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

export default function Files({ sources }: { sources: Source[] }) {
  const [storage, setStorage] = useState<StorageLocation[]>([]);
  const [files, setFiles] = useState<SourceFile[]>([]);
  const [total, setTotal] = useState(0);
  const [picked, setPicked] = useState<Record<string, boolean>>({});
  const [storageID, setStorageID] = useState("");
  const [msg, setMsg] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [addingS3, setAddingS3] = useState(false);

  async function load() {
    try {
      const [s, f] = await Promise.all([api.storage(), api.files()]);
      setStorage(s.storage ?? []);
      setFiles(f.files ?? []);
      setTotal(f.total_bytes ?? 0);
      const usable = (s.storage ?? []).find((l) => l.usable);
      setStorageID((cur) => cur || usable?.id || "");
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
    // Downloads run as jobs; refresh so files appear as they land.
    const t = window.setInterval(load, 6000);
    return () => window.clearInterval(t);
  }, []);

  async function startDownload() {
    setBusy(true);
    setErr("");
    setMsg("");
    try {
      const r = await api.download({ sources: chosen, storage_id: storageID });
      setMsg(
        `Queued as job ${r.job_id.slice(0, 8)} → ${r.storage.name}. ` +
          `Watch it under Results; files appear here when it finishes.`,
      );
      setPicked({});
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  // Group by source so a source's footprint is legible at a glance.
  const bySource = new Map<string, SourceFile[]>();
  for (const f of files) {
    const list = bySource.get(f.source_id) ?? [];
    list.push(f);
    bySource.set(f.source_id, list);
  }
  const provisioned = new Set(bySource.keys());
  const chosen = Object.entries(picked).filter(([, v]) => v).map(([k]) => k);

  return (
    <>
      <div className="between" style={{ marginBottom: 14 }}>
        <p className="lede" style={{ fontSize: 13.5, margin: 0 }}>
          Source data has to be downloaded before it can annotate. Provisioning is
          per source; the same source can be downloaded into more than one location.
        </p>
      </div>

      <div className="card" style={{ padding: 16, marginBottom: 18 }}>
        <label className="label">Sources to provision</label>
        <div
          style={{
            maxHeight: 220,
            overflow: "auto",
            border: "1px solid var(--border-card)",
            borderRadius: 8,
            marginBottom: 14,
          }}
        >
          {sources.length === 0 && <div className="empty">No sources registered.</div>}
          {sources.map((s) => {
            const on = !!picked[s.id];
            const have = provisioned.has(s.id);
            return (
              <button
                key={s.id}
                className="trow row gap-10"
                aria-pressed={on}
                style={{ cursor: "pointer", padding: "9px 14px" }}
                onClick={() => setPicked({ ...picked, [s.id]: !on })}
              >
                <span className={`check sm ${on ? "on" : ""}`}>
                  {on && <span style={{ fontSize: 11, color: "#fff" }}>✓</span>}
                </span>
                <span style={{ flex: 1 }}>
                  <span style={{ fontWeight: 500 }}>{s.title || s.name}</span>{" "}
                  <span className="mono" style={{ fontSize: 11.5, color: "var(--accent-text)" }}>
                    {s.version}
                  </span>
                  {s.build && <span className="tag" style={{ marginLeft: 6 }}>{s.build}</span>}
                </span>
                <span className="tag">{s.kind}</span>
                <span
                  style={{
                    fontSize: 11.5,
                    color: have ? "var(--benign-fg)" : "var(--text-4)",
                    minWidth: 90,
                    textAlign: "right",
                  }}
                >
                  {have ? "downloaded" : "not downloaded"}
                </span>
              </button>
            );
          })}
        </div>
        <div className="row gap-14" style={{ flexWrap: "wrap", alignItems: "flex-end" }}>
          <div style={{ minWidth: 220, flex: 1 }}>
            <label className="label">Download to</label>
            <select
              className="select"
              value={storageID}
              onChange={(e) => setStorageID(e.target.value)}
            >
              {storage.map((l) => (
                <option key={l.id} value={l.id} disabled={!l.usable}>
                  {l.name} — {l.uri}
                  {l.usable ? "" : " (not yet supported)"}
                </option>
              ))}
            </select>
          </div>
          <button
            className="btn"
            disabled={busy || chosen.length === 0 || !storageID}
            onClick={startDownload}
          >
            <Download size={15} />{" "}
            {busy ? "Queueing…" : `Download ${chosen.length || ""}`.trim()}
          </button>
        </div>
        {msg && <p style={{ fontSize: 13, color: "var(--accent-text)", marginTop: 12 }}>{msg}</p>}
        {err && <p className="err" style={{ fontSize: 13, marginTop: 12 }}>{err}</p>}
      </div>

      <div className="between" style={{ marginBottom: 10 }}>
        <span className="label" style={{ margin: 0 }}>
          Storage locations
        </span>
        <button className="btn link" style={{ fontSize: 12.5 }} onClick={() => setAddingS3(!addingS3)}>
          {addingS3 ? "Cancel" : "+ Add S3 bucket"}
        </button>
      </div>
      {addingS3 && <AddS3 onDone={() => { setAddingS3(false); load(); }} />}
      <div className="card" style={{ marginBottom: 22 }}>
        {storage.map((l) => (
          <div
            key={l.id}
            className="row gap-14"
            style={{ padding: "12px 18px", borderBottom: "1px solid var(--hairline)" }}
          >
            <HardDrive size={16} color="var(--text-3)" />
            <div style={{ flex: 1 }}>
              <div className="row gap-8">
                <span style={{ fontWeight: 500 }}>{l.name}</span>
                <span className="tag">{l.kind}</span>
                {l.is_default && <span className="tag">default</span>}
                {l.from_config && <span className="tag">from config</span>}
              </div>
              <div className="mono" style={{ fontSize: 11.5, color: "var(--text-3)" }}>
                {l.uri}
              </div>
              {!l.usable && (
                <div className="row gap-8" style={{ fontSize: 11.5, color: "var(--vus-fg)", marginTop: 2 }}>
                  <TriangleAlert size={11} /> {l.unusable_reason}
                </div>
              )}
            </div>
            {!l.from_config && (
              <button
                className="icon-btn"
                title="Remove location"
                onClick={async () => {
                  if (confirm(`Remove storage location "${l.name}"? Files already there are not deleted.`)) {
                    await api.deleteStorage(l.id).catch((e) => setErr(String(e)));
                    load();
                  }
                }}
              >
                <Trash2 size={14} />
              </button>
            )}
          </div>
        ))}
      </div>

      <div className="between" style={{ marginBottom: 10 }}>
        <span className="label" style={{ margin: 0 }}>
          Downloaded files
        </span>
        <span style={{ fontSize: 12.5, color: "var(--text-2)" }}>
          {files.length} files · {humanSize(total)}
        </span>
      </div>
      <div className="card">
        {files.length === 0 && (
          <div className="empty">
            Nothing downloaded yet. Provision a snapshot above — until then,
            annotating with a source that needs data fails with{" "}
            <span className="mono">sources not downloaded</span>.
          </div>
        )}
        {[...bySource.entries()].map(([sourceID, list]) => {
          const sum = list.reduce((a, f) => a + f.size_bytes, 0);
          return (
            <div key={sourceID} style={{ borderBottom: "1px solid var(--hairline)" }}>
              <div className="between" style={{ padding: "10px 18px", background: "var(--bg)" }}>
                <span className="mono" style={{ fontSize: 12.5, fontWeight: 500 }}>
                  {sourceID}
                </span>
                <span style={{ fontSize: 12, color: "var(--text-2)" }}>
                  {list.length} files · {humanSize(sum)}
                </span>
              </div>
              {list.map((f) => (
                <div
                  key={f.path}
                  className="between"
                  style={{ padding: "7px 18px 7px 34px", fontSize: 12.5 }}
                >
                  <span className="mono" style={{ color: "var(--text-2)" }}>
                    {f.path}
                  </span>
                  <span className="mono" style={{ color: "var(--text-3)" }}>
                    {humanSize(f.size_bytes)}
                  </span>
                </div>
              ))}
            </div>
          );
        })}
      </div>
      <p style={{ fontSize: 11.5, color: "var(--text-3)", marginTop: 10 }}>
        Files are not individually deletable: each belongs to a source, and removing
        it would break that source. Delete the source instead.
      </p>
    </>
  );
}

function AddS3({ onDone }: { onDone: () => void }) {
  const [name, setName] = useState("");
  const [uri, setUri] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  return (
    <div className="card" style={{ padding: 14, marginBottom: 14 }}>
      <div className="row gap-14" style={{ flexWrap: "wrap", alignItems: "flex-end" }}>
        <div style={{ flex: 1, minWidth: 200 }}>
          <label className="label">Name</label>
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="Cold storage" />
        </div>
        <div style={{ flex: 2, minWidth: 260 }}>
          <label className="label">Bucket URI</label>
          <input
            className="input mono"
            style={{ fontSize: 12 }}
            value={uri}
            onChange={(e) => setUri(e.target.value)}
            placeholder="s3://my-bucket/varianthub"
          />
        </div>
        <button
          className="btn sm"
          disabled={busy || !name.trim() || !uri.trim()}
          onClick={async () => {
            setBusy(true);
            setErr("");
            try {
              await api.addStorage({ name: name.trim(), kind: "s3", uri: uri.trim() });
              onDone();
            } catch (e) {
              setErr(e instanceof Error ? e.message : String(e));
            } finally {
              setBusy(false);
            }
          }}
        >
          Add
        </button>
      </div>
      {err && <p className="err" style={{ fontSize: 12.5, marginTop: 8 }}>{err}</p>}
      <p style={{ fontSize: 11.5, color: "var(--text-3)", marginTop: 10 }}>
        Filesystem paths are declared in the deployment config
        (<span className="mono">VHW_STORAGE_PATHS</span>) rather than here — a path
        only means something if the worker mounts it. S3 buckets need no mount, so
        they can be added at runtime. Note the CLI cannot download to S3 yet, so an
        S3 location is configurable but not yet selectable as a target.
      </p>
    </div>
  );
}

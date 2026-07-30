import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { HardDrive, Trash2, TriangleAlert } from "lucide-react";

import { api, type SourceFile, type StorageLocation } from "../api";

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

export default function Files() {
  const nav = useNavigate();
  const [storage, setStorage] = useState<StorageLocation[]>([]);
  const [files, setFiles] = useState<SourceFile[]>([]);

  const [err, setErr] = useState("");
  const [addingS3, setAddingS3] = useState(false);

  async function load() {
    try {
      const [s, f] = await Promise.all([api.storage(), api.files()]);
      setStorage(s.storage ?? []);
      setFiles(f.files ?? []);
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

  // Per-location totals for the rows; the file listing itself lives in the
  // browser behind View.
  const byStorage = new Map<string, { bytes: number; count: number }>();
  for (const f of files) {
    const cur = byStorage.get(f.storage_id) ?? { bytes: 0, count: 0 };
    cur.bytes += f.size_bytes;
    cur.count += 1;
    byStorage.set(f.storage_id, cur);
  }

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <h1 className="title">Storage &amp; files</h1>
      <div className="between" style={{ margin: "6px 0 18px" }}>
        <p className="lede" style={{ fontSize: 13.5, margin: 0 }}>
          Where downloaded source data lives, and what is on disk. Provision a
          source from <strong>Sources &amp; snapshots → Sources</strong>; each
          unprovisioned source has a download control on its row.
        </p>
      </div>

      {err && <p className="err" style={{ fontSize: 13 }}>{err}</p>}

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
              {byStorage.has(l.id) && (
                <div style={{ fontSize: 11.5, color: "var(--text-2)", marginTop: 2 }}>
                  {byStorage.get(l.id)!.count} files ·{" "}
                  {humanSize(byStorage.get(l.id)!.bytes)}
                </div>
              )}
            </div>
            <button
              className="btn secondary sm"
              onClick={() => nav(`/admin/storage/${encodeURIComponent(l.id)}`)}
            >
              View
            </button>
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

      <p style={{ fontSize: 11.5, color: "var(--text-3)", marginTop: 10 }}>
        Files are not individually deletable: each belongs to a source, and removing
        it would break that source. Delete the source instead.
      </p>
    </div>
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

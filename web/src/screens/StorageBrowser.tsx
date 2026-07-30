import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { ChevronDown, ChevronRight, File, Folder, RefreshCw } from "lucide-react";

import { api, type Source, type SourceFile, type StorageLocation } from "../api";
import { humanSize } from "./Files";

/** A directory in the browsed tree. Files sit at the leaves. */
interface Node {
  name: string;
  path: string;
  dirs: Map<string, Node>;
  files: SourceFile[];
  bytes: number;
  count: number;
}

function emptyNode(name: string, path: string): Node {
  return { name, path, dirs: new Map(), files: [], bytes: 0, count: 0 };
}

/**
 * Builds a directory tree from the flat relative paths the worker recorded.
 *
 * The listing comes from the catalog rather than the disk: only the worker is
 * guaranteed to mount the storage volume, so asking the API server to walk it
 * would work in compose and fail in a deployment where the two are separate
 * pods. What is shown is therefore what VariantHub put there — which is the
 * question being asked.
 */
function buildTree(files: SourceFile[]): Node {
  const root = emptyNode("", "");
  for (const f of files) {
    const parts = f.path.split("/");
    let node = root;
    root.bytes += f.size_bytes;
    root.count += 1;
    for (let i = 0; i < parts.length - 1; i++) {
      const name = parts[i];
      let next = node.dirs.get(name);
      if (!next) {
        next = emptyNode(name, [node.path, name].filter(Boolean).join("/"));
        node.dirs.set(name, next);
      }
      next.bytes += f.size_bytes;
      next.count += 1;
      node = next;
    }
    node.files.push(f);
  }
  return root;
}

function Dir({ node, depth, open }: { node: Node; depth: number; open: Set<string> }) {
  const [expanded, setExpanded] = useState(depth < 2 || open.has(node.path));
  const indent = 18 + depth * 16;

  return (
    <>
      {node.name !== "" && (
        <button
          className="trow row gap-8"
          style={{ paddingLeft: indent, cursor: "pointer", borderBottom: "none" }}
          onClick={() => setExpanded(!expanded)}
        >
          {expanded ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
          <Folder size={14} color="var(--accent)" />
          <span className="mono" style={{ flex: 1, fontSize: 13 }}>
            {node.name}
          </span>
          <span style={{ fontSize: 11.5, color: "var(--text-3)" }}>
            {node.count} file{node.count === 1 ? "" : "s"} · {humanSize(node.bytes)}
          </span>
        </button>
      )}
      {expanded && (
        <>
          {[...node.dirs.values()]
            .sort((a, b) => a.name.localeCompare(b.name))
            .map((d) => (
              <Dir key={d.path} node={d} depth={node.name === "" ? depth : depth + 1} open={open} />
            ))}
          {node.files
            .slice()
            .sort((a, b) => a.path.localeCompare(b.path))
            .map((f) => (
              <div
                key={f.path}
                className="row gap-8"
                style={{
                  paddingLeft: indent + (node.name === "" ? 0 : 16),
                  paddingRight: 18,
                  paddingTop: 6,
                  paddingBottom: 6,
                }}
              >
                <File size={13} color="var(--text-4)" />
                <span className="mono" style={{ flex: 1, fontSize: 12.5, color: "var(--text-2)" }}>
                  {f.path.split("/").pop()}
                </span>
                <span className="mono" style={{ fontSize: 12, color: "var(--text-3)" }}>
                  {humanSize(f.size_bytes)}
                </span>
              </div>
            ))}
        </>
      )}
    </>
  );
}

export default function StorageBrowser() {
  const { id = "" } = useParams();
  const nav = useNavigate();
  const [loc, setLoc] = useState<StorageLocation | null>(null);
  const [files, setFiles] = useState<SourceFile[] | null>(null);
  const [sources, setSources] = useState<Source[]>([]);
  const [err, setErr] = useState("");

  async function load() {
    try {
      const [st, f, src] = await Promise.all([
        api.storage(),
        api.files({ storage: id }),
        api.sources(),
      ]);
      setLoc((st.storage ?? []).find((l) => l.id === id) ?? null);
      setFiles(f.files ?? []);
      setSources(src.sources ?? []);
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
    // Keep the view live: a download in flight adds files as it completes.
    const t = window.setInterval(load, 5000);
    return () => window.clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id]);

  const tree = files ? buildTree(files) : null;
  const bySource = new Map<string, number>();
  for (const f of files ?? []) {
    bySource.set(f.source_id, (bySource.get(f.source_id) ?? 0) + f.size_bytes);
  }
  const title = (sid: string) => {
    const s = sources.find((x) => x.id === sid);
    return s ? s.title || s.name : sid;
  };

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <button className="btn link" style={{ fontSize: 13 }} onClick={() => nav("/admin/storage")}>
        ← Storage locations
      </button>

      <div className="between" style={{ margin: "12px 0 6px", alignItems: "flex-end" }}>
        <div>
          <h1 style={{ fontSize: 24, fontWeight: 600 }}>{loc?.name ?? id}</h1>
          <div className="mono" style={{ fontSize: 12, color: "var(--text-3)" }}>
            {loc?.uri}
          </div>
        </div>
        <div className="row gap-10">
          <span style={{ fontSize: 12.5, color: "var(--text-2)" }}>
            {tree ? `${tree.count} files · ${humanSize(tree.bytes)}` : ""}
          </span>
          <button className="icon-btn" onClick={load} title="Refresh">
            <RefreshCw size={15} />
          </button>
        </div>
      </div>

      {err && <p className="err">{err}</p>}

      {bySource.size > 0 && (
        <div className="row gap-8" style={{ flexWrap: "wrap", margin: "14px 0" }}>
          {[...bySource.entries()].map(([sid, bytes]) => (
            <span key={sid} className="tag">
              {title(sid)} · {humanSize(bytes)}
            </span>
          ))}
        </div>
      )}

      <div className="card" style={{ padding: "6px 0" }}>
        {files === null && !err && <div className="empty">Loading…</div>}
        {files?.length === 0 && (
          <div className="empty">
            This location is empty. Provision a source from{" "}
            <strong>Sources &amp; snapshots</strong>.
          </div>
        )}
        {tree && tree.count > 0 && <Dir node={tree} depth={0} open={new Set()} />}
      </div>

      <p style={{ fontSize: 11.5, color: "var(--text-3)", marginTop: 10 }}>
        Lists what VariantHub downloaded here, recorded when each provisioning job
        finished. Files placed by hand are not shown.
      </p>
    </div>
  );
}

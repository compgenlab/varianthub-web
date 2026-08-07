import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Trash2 } from "lucide-react";

import {
  api,
  type Source,
  type SourceFile,
  type StorageLocation,
  type Team,
} from "../api";
import { humanSize } from "./Files";

/**
 * One source, in full: what its manifest says, what this deployment decided
 * about it, who can see it, and where its data is.
 *
 * A page rather than a row that expands. The list answers "what is registered";
 * these are four different questions about one entry, and answering them inside
 * a table row meant the row grew to several screens and the table stopped being
 * scannable.
 */
export default function SourceDetail() {
  const { sourceId = "" } = useParams();
  const nav = useNavigate();
  const [source, setSource] = useState<Source | null>(null);
  const [files, setFiles] = useState<SourceFile[]>([]);
  const [storage, setStorage] = useState<StorageLocation[]>([]);
  const [err, setErr] = useState("");

  async function load() {
    try {
      const [s, f, st] = await Promise.all([api.sources(), api.files(), api.storage()]);
      const found = (s.sources ?? []).find((x) => x.id === sourceId) ?? null;
      setSource(found);
      setFiles((f.files ?? []).filter((x) => x.source_id === sourceId));
      setStorage(st.storage ?? []);
      setErr(found ? "" : `No source "${sourceId}".`);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, [sourceId]);

  if (err && !source) {
    return (
      <div className="page page-wide" style={{ paddingTop: 30 }}>
        <button className="btn link" style={{ fontSize: 13 }} onClick={() => nav("/admin")}>
          ← Sources
        </button>
        <p className="err" style={{ marginTop: 16, fontSize: 13 }}>
          {err}
        </p>
      </div>
    );
  }
  if (!source) {
    return (
      <div className="page page-wide" style={{ paddingTop: 30 }}>
        <p className="lede">Loading…</p>
      </div>
    );
  }

  // Where the data actually is, per location — a source can be in two places.
  const byLoc = new Map<string, { bytes: number; count: number }>();
  for (const f of files) {
    const cur = byLoc.get(f.storage_id) ?? { bytes: 0, count: 0 };
    cur.bytes += f.size_bytes;
    cur.count += 1;
    byLoc.set(f.storage_id, cur);
  }

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <button className="btn link" style={{ fontSize: 13 }} onClick={() => nav("/admin")}>
        ← Sources
      </button>

      <div className="between" style={{ margin: "14px 0 4px" }}>
        <h1 className="title">{source.title || source.name}</h1>
        <DeleteSource source={source} onDeleted={() => nav("/admin")} />
      </div>
      <p className="lede mono" style={{ fontSize: 12.5, margin: "0 0 20px" }}>
        {source.ref ?? `${source.name}:${source.version}`} · {source.kind}
        {source.build ? ` · ${source.build}` : ""} · {source.visibility}
        {source.stream ? " · read from its origin" : ""}
      </p>

      {err && (
        <p className="err" style={{ fontSize: 13, marginBottom: 12 }}>
          {err}
        </p>
      )}

      <h2 className="label" style={{ marginBottom: 8 }}>
        Stored data
      </h2>
      <div className="card" style={{ padding: 14, marginBottom: 22 }}>
        {byLoc.size === 0 ? (
          <p style={{ fontSize: 12.5, color: "var(--text-3)", margin: 0 }}>
            {source.stream
              ? "Read from its origin — nothing is stored here."
              : source.needs_data === false
                ? "Computed from the variant; there is nothing to store."
                : "Not downloaded yet. Provision it from the sources list."}
          </p>
        ) : (
          [...byLoc].map(([id, v]) => {
            const loc = storage.find((l) => l.id === id);
            return (
              <div key={id} className="between" style={{ fontSize: 13, marginBottom: 4 }}>
                <span>{loc ? `${loc.name} — ${loc.uri}` : id}</span>
                <span className="mono" style={{ fontSize: 12 }}>
                  {humanSize(v.bytes)} ({v.count} file{v.count === 1 ? "" : "s"})
                </span>
              </div>
            );
          })
        )}
        <StorageActions
          source={source}
          storage={storage}
          have={byLoc}
          onChange={load}
        />
      </div>

      <SourceSettingsPanel source={source} />
      {source.visibility === "private" && <SourceGrants id={source.id} />}
      <SourceConfig id={source.id} />
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
        margin: "0 0 22px",
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
 * What this deployment decided about a source.
 *
 * Separate from the manifest, which belongs to whoever published it: a prefix
 * chosen here has to survive re-fetching that manifest from a registry, so the
 * two are stored apart and shown apart.
 */
function SourceSettingsPanel({ source }: { source: Source }) {
  const [prefix, setPrefix] = useState("");
  const [cacheSetup, setCacheSetup] = useState(false);
  const [manifestPrefix, setManifestPrefix] = useState("");
  const [isTool, setIsTool] = useState(false);
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [err, setErr] = useState("");

  useEffect(() => {
    let live = true;
    api
      .sourceSettings(source.id)
      .then((r) => {
        if (!live) return;
        setPrefix(r.settings.annotation_prefix ?? "");
        setCacheSetup(!!r.settings.cache_setup);
        setManifestPrefix(r.manifest_prefix ?? "");
        setIsTool(r.is_tool);
      })
      .catch((e) => live && setErr(e instanceof Error ? e.message : String(e)));
    return () => {
      live = false;
    };
  }, [source.id]);

  async function save() {
    setBusy(true);
    setErr("");
    try {
      await api.setSourceSettings(source.id, {
        annotation_prefix: prefix.trim(),
        cache_setup: cacheSetup,
      });
      setSaved(true);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div
      className="card"
      style={{ padding: 14, marginBottom: 22 }}
    >
      <div className="label" style={{ marginBottom: 8 }}>
        Settings
      </div>

      <label className="label" style={{ marginBottom: 4 }}>
        Annotation prefix
      </label>
      <div className="row gap-8" style={{ flexWrap: "wrap" }}>
        <input
          className="input mono"
          style={{ width: 220 }}
          value={prefix}
          placeholder={manifestPrefix || "(none)"}
          onChange={(e) => {
            setPrefix(e.target.value);
            setSaved(false);
          }}
        />
        <label className="row gap-8" style={{ fontSize: 12.5 }}>
          <input
            type="checkbox"
            checked={cacheSetup}
            disabled={!isTool}
            onChange={(e) => {
              setCacheSetup(e.target.checked);
              setSaved(false);
            }}
          />
          {/* Only a tool has setup output, and only an object-store target has
              anywhere to put it. */}
          Reuse this install on other machines{isTool ? "" : " (tools only)"}
        </label>
        <button className="btn" disabled={busy} onClick={save}>
          {busy ? "Saving…" : saved ? "Saved" : "Save"}
        </button>
      </div>
      <p style={{ fontSize: 11.5, color: "var(--text-3)", margin: "8px 0 0", lineHeight: 1.55 }}>
        {/* An empty box and "no prefix" look the same but are not, so say which
            this is. */}
        {manifestPrefix
          ? `Blank falls back to the manifest's own prefix, ${manifestPrefix}. Enter "-" for no prefix at all.`
          : `This source's manifest declares no prefix, so blank means its fields keep the names it gives them.`}{" "}
        Renaming affects output field names only, not what is read from the file.
      </p>
      {isTool && (
        <p style={{ fontSize: 11.5, color: "var(--text-3)", margin: "6px 0 0", lineHeight: 1.55 }}>
          Reuse copies this tool&apos;s installed data to the storage location it
          was downloaded to, so a machine that has none unpacks it instead of
          repeating the install. It stays within this deployment — nothing is
          made public. It does nothing unless that location is an S3 bucket: a
          filesystem location already holds the data where other machines
          reading that path can see it.
        </p>
      )}
      {err && (
        <p className="err" style={{ fontSize: 12.5, marginTop: 8 }}>
          {err}
        </p>
      )}
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
      className="card"
      style={{ padding: 14, marginBottom: 22 }}
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

/**
 * Removes a source. Refused server-side while a snapshot pins it — removing it
 * would silently change what those snapshots mean — so the error names the
 * snapshots to detach rather than the button being pre-disabled on a guess.
 */
function DeleteSource({ source, onDeleted }: { source: Source; onDeleted: () => void }) {
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
            onDeleted();
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
 * Downloading a source, and moving it between storage locations.
 *
 * On the detail page rather than the list: both are decisions about one source
 * that need its current placement in view, and a dropdown per row made the list
 * a control panel rather than a summary.
 */
function StorageActions({
  source,
  storage,
  have,
  onChange,
}: {
  source: Source;
  storage: StorageLocation[];
  have: Map<string, { bytes: number; count: number }>;
  onChange: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState("");
  const [target, setTarget] = useState("");

  // Nothing to place: a builtin computes from the variant, and a streamed source
  // is read from its origin.
  if (source.needs_data === false && !source.stream) return null;

  const usable = storage.filter((l) => l.usable !== false);
  const elsewhere = usable.filter((l) => !have.has(l.id));
  const stored = have.size > 0;

  async function run(fn: () => Promise<unknown>, note: string) {
    setBusy(true);
    setMsg("");
    try {
      await fn();
      setMsg(note);
      onChange();
    } catch (e) {
      setMsg(String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div style={{ marginTop: stored ? 12 : 0, paddingTop: stored ? 12 : 0,
                  borderTop: stored ? "1px solid var(--border)" : "none" }}>
      <div className="row gap-8" style={{ flexWrap: "wrap" }}>
        <select
          value={target}
          disabled={busy || usable.length === 0}
          onChange={(e) => setTarget(e.target.value)}
          style={{ fontSize: 12.5, height: 28, padding: "0 6px", minWidth: 190 }}
        >
          <option value="">
            {usable.length === 0 ? "no usable storage configured" : "choose a location…"}
          </option>
          {(stored ? elsewhere : usable).map((l) => (
            <option key={l.id} value={l.id}>
              {l.name} — {l.uri}
            </option>
          ))}
        </select>

        {stored ? (
          <button
            className="btn sm"
            disabled={busy || !target}
            onClick={() =>
              // Copy, record, then delete: the source stays readable where it is
              // until the new copy has landed, so this is safe to interrupt.
              run(() => api.moveSource(source.id, target), "moving…")
            }
          >
            Move
          </button>
        ) : (
          <button
            className="btn sm"
            disabled={busy || !target}
            onClick={() => run(() => api.download({ sources: [source.id], storage_id: target }), "downloading…")}
          >
            Copy locally
          </button>
        )}
      </div>
      {msg && (
        <p style={{ fontSize: 12, color: "var(--accent-text)", margin: "8px 0 0" }}>{msg}</p>
      )}
      {stored && elsewhere.length === 0 && (
        <p style={{ fontSize: 11.5, color: "var(--text-3)", margin: "8px 0 0" }}>
          Stored in every configured location; there is nowhere to move it.
        </p>
      )}
    </div>
  );
}

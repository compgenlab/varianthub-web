import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { Check, Globe } from "lucide-react";

import {
  api,
  type Annotation,
  type Snapshot,
  type Source,
  type Visibility,
} from "../api";
import { LEVEL_HELP, LEVEL_LABEL, LevelIcon } from "./Visibility";

/**
 * One snapshot: what it pins, what it publishes, and what it is called.
 *
 * A page rather than a row that expands. Editing which sources a snapshot pins
 * is a task with its own state — a picker, a dirty set, a save — and doing it
 * inside a list row meant the list was never quite what it appeared to be.
 */
export default function SnapshotDetail() {
  const { snapshotId = "" } = useParams();
  const nav = useNavigate();
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [annotations, setAnnotations] = useState<Annotation[]>([]);
  const [sources, setSources] = useState<Source[]>([]);
  // Derived server-side from the pinned sources, so it arrives beside the
  // snapshot rather than on it.
  const [visibility, setVisibility] = useState<Visibility>("public");
  const [limitedBy, setLimitedBy] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  async function load() {
    try {
      const [d, s] = await Promise.all([api.snapshot(snapshotId), api.sources()]);
      setSnapshot(d.snapshot);
      setAnnotations(d.annotations ?? []);
      setSources(s.sources ?? []);
      setVisibility(d.visibility);
      setLimitedBy(d.constrained_by ?? []);
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, [snapshotId]);

  async function act(fn: () => Promise<unknown>) {
    setBusy(true);
    try {
      await fn();
      await load();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  if (!snapshot) {
    return (
      <div className="page page-wide" style={{ paddingTop: 30 }}>
        <button className="btn link" style={{ fontSize: 13 }} onClick={() => nav("/admin")}>
          ← Snapshots
        </button>
        {err ? (
          <p className="err" style={{ marginTop: 16, fontSize: 13 }}>
            {err}
          </p>
        ) : (
          <p className="lede" style={{ marginTop: 16 }}>
            Loading…
          </p>
        )}
      </div>
    );
  }

  const published = snapshot.state === "published";

  return (
    <div className="page page-wide" style={{ paddingTop: 30 }}>
      <button className="btn link" style={{ fontSize: 13 }} onClick={() => nav("/admin")}>
        ← Snapshots
      </button>

      <div className="between" style={{ margin: "14px 0 4px" }}>
        <h1 className="title">{snapshot.title || snapshot.id}</h1>
        <div className="row gap-8">
          {!published && (
            <button
              className="btn"
              disabled={busy}
              onClick={() => act(() => api.publishSnapshot(snapshot.id))}
            >
              Publish
            </button>
          )}
          <button
            className="btn secondary"
            disabled={busy}
            style={{ color: "var(--path-fg)" }}
            onClick={() => {
              if (
                confirm(
                  `Delete snapshot "${snapshot.id}"?\n\nExisting job results stay readable — they keep their own column model — but new annotation against this name will fail.`,
                )
              ) {
                act(async () => {
                  await api.deleteSnapshot(snapshot.id);
                  nav("/admin");
                });
              }
            }}
          >
            Delete
          </button>
        </div>
      </div>
      <p className="lede mono" style={{ fontSize: 12.5, margin: "0 0 6px" }}>
        {snapshot.id} · {snapshot.build} · {snapshot.state}
      </p>
      <div className="row gap-14" style={{ marginBottom: 20, fontSize: 12 }}>
        {/* Shown, not set — a snapshot's level follows from what it pins, so
            the sources named here are the only thing that can change it. */}
        <span
          className="row gap-8"
          style={{ color: "var(--text-2)" }}
          title={
            limitedBy.length
              ? `Limited by ${limitedBy.join(", ")}`
              : LEVEL_HELP[visibility]
          }
        >
          <LevelIcon level={visibility} /> {LEVEL_LABEL[visibility]}
          {limitedBy.length ? ` — limited by ${limitedBy.join(", ")}` : ""}
        </span>
        {snapshot.contains_remote && (
          <span className="row gap-8" style={{ color: "var(--text-2)" }}>
            <Globe size={11} /> reads some sources over the network
          </span>
        )}
      </div>

      {err && (
        <p className="err" style={{ fontSize: 13, marginBottom: 12 }}>
          {err}
        </p>
      )}

      <EditSnapshot snapshot={snapshot} onDone={load} />
      <EditSnapshotSources snapshot={snapshot} sources={sources} onChange={load} />

      <h2 className="label" style={{ margin: "24px 0 8px" }}>
        Fields this snapshot contributes ({annotations.length})
      </h2>
      <div className="card" style={{ padding: 14 }}>
        {annotations.length === 0 ? (
          <p style={{ fontSize: 12.5, color: "var(--text-3)", margin: 0 }}>
            No annotations — its sources declare none.
          </p>
        ) : (
          <div className="row gap-8" style={{ flexWrap: "wrap" }}>
            {annotations.map((a) => (
              <span
                key={a.name}
                className="tag mono"
                title={`${a.source ?? ""}${a.description ? " — " + a.description : ""}`}
              >
                {/* The default set is what runs when nobody chooses, so it is
                    worth seeing at a glance rather than in a separate list. */}
                {a.default && <Check size={10} />} {a.name}
              </span>
            ))}
          </div>
        )}
      </div>
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

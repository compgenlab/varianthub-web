import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ArrowRight, Check, Lock, TriangleAlert, Globe } from "lucide-react";

import { api, type Annotation, type Build, type Snapshot, type Source } from "../api";
import { useFlow } from "../flow";

type Mode = "snapshot" | "sources";

export default function ChooseSources() {
  const nav = useNavigate();
  const flow = useFlow();
  const [mode, setMode] = useState<Mode>("snapshot");
  const [snapshots, setSnapshots] = useState<Snapshot[] | null>(null);
  const [sources, setSources] = useState<(Source & { annotations: Annotation[] })[] | null>(null);
  const [builds, setBuilds] = useState<Build[] | null>(null);
  const [fields, setFields] = useState<Annotation[]>([]);
  const [err, setErr] = useState("");

  useEffect(() => {
    api
      .snapshots()
      .then((r) => setSnapshots(r.snapshots ?? []))
      .catch((e) => setErr(e.message));
    api
      .sources()
      .then((r) => setSources(r.sources ?? []))
      .catch((e) => setErr(e.message));
    // The builds come from the catalog rather than a constant in this file, so
    // an administrator can add one and it appears here without a rebuild.
    api
      .builds()
      .then((r) => {
        setBuilds(r.builds ?? []);
        // The flow starts on GRCh38 because something has to be selected before
        // the list arrives. If this installation does not offer it, move to one
        // it does — otherwise the form opens on a build with no sources and
        // nothing says why.
        if (r.builds?.length && !r.builds.some((b) => b.name === flow.build)) {
          flow.setBuild(r.builds[0].name);
        }
      })
      .catch((e) => setErr(e.message));
  }, []);

  // A source belongs to the chosen build, or declares no build at all — the
  // builtins compute from the variant itself and are correct against any
  // assembly. Comparison is exact and deliberately not normalized: GRCh38 and
  // hg38 are the same genome in real life but different strings here, and a
  // false match would annotate against the wrong coordinates and say nothing
  // about it.
  const visible = useMemo(
    () => (sources ?? []).filter((s) => !s.build || s.build === flow.build),
    [sources, flow.build],
  );

  // Selecting a build the current picks do not belong to would submit sources
  // from another assembly, which PutSnapshot rejects — drop them as the build
  // changes rather than failing at submit with a list the user can no longer see.
  useEffect(() => {
    if (!sources) {
      return;
    }
    const ok = new Set(visible.map((s) => s.id));
    const kept = flow.sources.filter((id) => ok.has(id));
    if (kept.length !== flow.sources.length) {
      flow.setSources(kept);
    }
  }, [visible, sources]);

  // Snapshot mode: the fields come from the snapshot, along with which it
  // applies by default. Fetched on selection so the picker below is always the
  // chosen snapshot's, never a stale one.
  useEffect(() => {
    if (mode !== "snapshot" || !flow.snapshot) {
      return;
    }
    let cancelled = false;
    api
      .snapshot(flow.snapshot)
      .then((d) => {
        if (cancelled) return;
        setFields(d.annotations ?? []);
        flow.setAnnotations(
          (d.annotations ?? []).filter((a) => a.default).map((a) => a.name),
        );
      })
      .catch((e) => !cancelled && setErr(e.message));
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, flow.snapshot]);

  // Individual mode: the fields are the union of the chosen sources'.
  const sourceFields = useMemo(() => {
    if (mode !== "sources" || !sources) return [];
    const seen = new Set<string>();
    const out: Annotation[] = [];
    for (const s of sources) {
      if (!flow.sources.includes(s.id)) continue;
      for (const a of s.annotations ?? []) {
        if (seen.has(a.name)) continue;
        seen.add(a.name);
        out.push({ ...a, source: s.title || s.name });
      }
    }
    return out;
  }, [mode, sources, flow.sources]);

  useEffect(() => {
    if (mode !== "sources") return;
    setFields(sourceFields);
    // Everything a chosen source offers is checked by default — the user picked
    // the source, so its fields are the point.
    flow.setAnnotations(sourceFields.map((a) => a.name));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, sourceFields]);

  const selectedSnapshot = snapshots?.find((s) => s.id === flow.snapshot);
  const isDraft = selectedSnapshot?.state === "draft";
  const ready =
    (mode === "snapshot" && !!flow.snapshot) ||
    (mode === "sources" && flow.sources.length > 0 && !!flow.build);

  function toggleField(name: string) {
    flow.setAnnotations(
      flow.annotations.includes(name)
        ? flow.annotations.filter((n) => n !== name)
        : [...flow.annotations, name],
    );
  }

  function next() {
    const p = new URLSearchParams();
    if (mode === "snapshot") p.set("snapshot", flow.snapshot);
    else {
      p.set("sources", flow.sources.join(","));
      p.set("build", flow.build);
    }
    if (flow.annotations.length) p.set("annotations", flow.annotations.join(","));
    nav(`/annotate/variants?${p}`);
  }

  return (
    <div className="page">
      <h1 className="title">Choose annotations</h1>
      <p className="lede">
        Annotate with a curated <strong>snapshot</strong> — a versioned, pinned
        bundle — or hand-pick <strong>individual sources</strong>. Then choose
        which fields to apply.
      </p>

      <div className="segmented" style={{ margin: "22px 0" }}>
        <button aria-pressed={mode === "snapshot"} onClick={() => setMode("snapshot")}>
          Snapshots
        </button>
        <button aria-pressed={mode === "sources"} onClick={() => setMode("sources")}>
          Individual sources
        </button>
      </div>

      {err && <p className="err">{err}</p>}

      {mode === "snapshot" ? (
        <>
          {snapshots === null && !err && <p style={{ color: "var(--text-3)" }}>Loading…</p>}
          {snapshots?.length === 0 && (
            <div className="card empty">
              No snapshots yet. Build one under <strong>Sources &amp; snapshots</strong>,
              or pick individual sources.
            </div>
          )}
          <div className="grid-2">
            {snapshots?.map((s) => {
              const on = s.id === flow.snapshot;
              return (
                <button
                  key={s.id}
                  className="snapcard"
                  aria-pressed={on}
                  onClick={() => flow.setSnapshot(s.id)}
                >
                  <div className="between">
                    <h3>{s.title || s.id}</h3>
                    <span
                      className={`check ${on ? "on" : ""}`}
                      style={{ visibility: on ? "visible" : "hidden" }}
                    >
                      <Check size={13} strokeWidth={3} />
                    </span>
                  </div>
                  <div className="meta">
                    {s.build} · {s.source_count ?? 0} sources
                  </div>
                  {s.description && <p>{s.description}</p>}
                  {s.state === "draft" && (
                    <div
                      className="row gap-8"
                      style={{ marginTop: 4, fontSize: 11.5, color: "var(--vus-fg)" }}
                    >
                      <TriangleAlert size={12} />
                      Draft — its pinned versions can still change
                    </div>
                  )}
                  {s.contains_private && (
                    <div
                      className="row gap-8"
                      style={{ marginTop: 8, fontSize: 11.5, color: "var(--text-2)" }}
                    >
                      <Lock size={11} /> Contains private sources
                    </div>
                  )}
                  {/* Says what it costs, not that something is wrong: a remote
                      source is a supported way to run, just one that reaches
                      across the network on every query. */}
                  {s.contains_remote && (
                    <div
                      className="row gap-8"
                      style={{ marginTop: 6, fontSize: 11.5, color: "var(--text-2)" }}
                    >
                      <Globe size={11} /> Reads some sources over the network
                    </div>
                  )}
                </button>
              );
            })}
          </div>
        </>
      ) : (
        <>
          <div style={{ maxWidth: 220, marginBottom: 14 }}>
            <label className="label">Reference build</label>
            <select
              className="select mono"
              value={flow.build}
              onChange={(e) => flow.setBuild(e.target.value)}
            >
              {/* Whatever the flow already holds stays selectable even if no
                  build record matches it, so a snapshot pinned to a build an
                  administrator later removed still shows what it is set to. */}
              {builds && !builds.some((b) => b.name === flow.build) && (
                <option key={flow.build}>{flow.build}</option>
              )}
              {(builds ?? []).map((b) => (
                <option key={b.name} value={b.name}>
                  {b.label && b.label !== b.name ? `${b.label} (${b.name})` : b.name}
                </option>
              ))}
            </select>
          </div>
          <div className="card">
            <div className="rowgrid thead" style={{ gridTemplateColumns: "26px 1.7fr 1fr .8fr 1fr" }}>
              <span />
              <span>Source</span>
              <span>Version</span>
              <span>Kind</span>
              <span>Access</span>
            </div>
            {sources && visible.length === 0 && (
              <div className="empty">
                {sources.length === 0
                  ? "No sources registered yet."
                  : `No sources are registered for ${flow.build}.`}
              </div>
            )}
            {visible.map((s) => {
              const on = flow.sources.includes(s.id);
              return (
                <button
                  key={s.id}
                  className="trow rowgrid"
                  aria-pressed={on}
                  style={{ gridTemplateColumns: "26px 1.7fr 1fr .8fr 1fr", cursor: "pointer" }}
                  onClick={() =>
                    flow.setSources(
                      on ? flow.sources.filter((id) => id !== s.id) : [...flow.sources, s.id],
                    )
                  }
                >
                  <span className={`check sm ${on ? "on" : ""}`}>
                    {on && <Check size={12} strokeWidth={3} />}
                  </span>
                  <span>
                    <span style={{ fontWeight: 500 }}>{s.title || s.name}</span>
                    <br />
                    <span style={{ fontSize: 11.5, color: "var(--text-3)" }}>
                      {s.detail || `${(s.annotations ?? []).length} fields`}
                    </span>
                  </span>
                  <span className="mono" style={{ fontSize: 12, color: "var(--accent-text)" }}>
                    {s.version}
                  </span>
                  <span className="row gap-8" style={{ flexWrap: "wrap" }}>
                    <span className="tag">{s.kind}</span>
                    {/* Worth surfacing here, not only in admin: a remote source
                        is fetched over the network at query time, so a run that
                        includes one can be slower and depends on somebody
                        else's server being up. */}
                    {s.stream && (
                      <span className="tag tag-remote" title="Read from its origin over the network">
                        <Globe size={10} /> remote
                      </span>
                    )}
                    {/* Registered is not the same as usable: a tool needs its
                        image and setup, a build source needs its recipe to have
                        run. Choosing one before that fails the whole run, so it
                        is worth seeing at the point of choosing. */}
                    {s.state?.state === "installing" && (
                      <span className="tag" title="Being downloaded and set up">
                        installing…
                      </span>
                    )}
                    {s.state?.state === "failed" && (
                      <span
                        className="tag"
                        style={{ background: "var(--path-bg)", color: "var(--path-fg)" }}
                        title={s.state.error ?? "Provisioning failed"}
                      >
                        not installed
                      </span>
                    )}
                  </span>
                  <span
                    className="mono"
                    style={{
                      fontSize: 11,
                      color: s.visibility === "private" ? "var(--private)" : "var(--text-3)",
                    }}
                  >
                    {s.visibility}
                  </span>
                </button>
              );
            })}
          </div>
        </>
      )}

      {isDraft && mode === "snapshot" && (
        <div
          className="row gap-10"
          style={{
            marginTop: 18,
            padding: "12px 14px",
            background: "var(--vus-bg)",
            color: "var(--vus-fg)",
            borderRadius: 9,
            fontSize: 13,
          }}
        >
          <TriangleAlert size={16} />
          <span>
            <strong>{selectedSnapshot?.title || flow.snapshot}</strong> is a draft.
            Its pinned source versions are not fixed yet, so a run today may not be
            reproducible later. Publish it to freeze the versions.
          </span>
        </div>
      )}

      {/* Field selection — the step between choosing what to annotate with and
          entering variants. */}
      {ready && (
        <div style={{ marginTop: 26 }}>
          <div className="between" style={{ marginBottom: 10 }}>
            <span className="label" style={{ margin: 0 }}>
              Fields to apply
            </span>
            <span className="row gap-10">
              <button
                className="btn link"
                style={{ fontSize: 12.5 }}
                onClick={() => flow.setAnnotations(fields.map((f) => f.name))}
              >
                Select all
              </button>
              <button
                className="btn link"
                style={{ fontSize: 12.5 }}
                onClick={() => flow.setAnnotations([])}
              >
                Select none
              </button>
            </span>
          </div>

          {fields.length === 0 ? (
            <div className="card empty">
              These sources declare no annotation fields.
            </div>
          ) : (
            <div className="card" style={{ padding: "6px 0" }}>
              {fields.map((f) => {
                const on = flow.annotations.includes(f.name);
                return (
                  <button
                    key={f.name}
                    className="trow row gap-10"
                    aria-pressed={on}
                    style={{ cursor: "pointer", borderBottom: "none", padding: "8px 18px" }}
                    onClick={() => toggleField(f.name)}
                  >
                    <span className={`check sm ${on ? "on" : ""}`}>
                      {on && <Check size={12} strokeWidth={3} />}
                    </span>
                    <span style={{ flex: 1 }}>
                      <span className="mono" style={{ fontSize: 13 }}>
                        {f.name}
                      </span>
                      {f.description && (
                        <span style={{ fontSize: 12.5, color: "var(--text-2)" }}>
                          {" — "}
                          {f.description}
                        </span>
                      )}
                    </span>
                    {f.type && <span className="tag">{f.type}</span>}
                    {f.source && (
                      <span className="src-tag" style={{ fontSize: 11 }}>
                        {f.source}
                      </span>
                    )}
                  </button>
                );
              })}
            </div>
          )}
        </div>
      )}

      <div className="between" style={{ marginTop: 26 }}>
        <span style={{ fontSize: 13, color: "var(--text-2)" }}>
          <strong style={{ fontSize: 15, color: "var(--text)" }}>
            {flow.annotations.length}
          </strong>{" "}
          of {fields.length} fields selected
          {mode === "sources" && ` · ${flow.sources.length} sources`}
        </span>
        <button
          className="btn"
          disabled={!ready || flow.annotations.length === 0}
          onClick={next}
        >
          Continue to variants <ArrowRight size={16} />
        </button>
      </div>
    </div>
  );
}

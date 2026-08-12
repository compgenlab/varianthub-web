import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ArrowRight, Check, Globe, Lock, Minus, TriangleAlert } from "lucide-react";

import { api, type Annotation, type Build, type Snapshot, type Source } from "../api";
import { useFlow } from "../flow";
import { LEVEL_LABEL } from "./Visibility";

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
    () =>
      (sources ?? []).filter(
        // References are excluded rather than listed: they contribute no
        // annotations, so offering one here invites picking it for something it
        // cannot do. It is chosen below, and only when something needs it.
        (s) => !s.is_reference && (!s.build || s.build === flow.build),
      ),
    [sources, flow.build],
  );

  const references = useMemo(
    () => (sources ?? []).filter((s) => s.is_reference && s.build === flow.build),
    [sources, flow.build],
  );

  // Only what is actually selected matters. A source that requires a genome and
  // is not picked should not make anyone choose one.
  const needsReference = useMemo(
    () => visible.some((s) => flow.sources.includes(s.id) && s.requires_reference),
    [visible, flow.sources],
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

  // The reference belongs to the build for the same reason, and with exactly one
  // to choose from there is no choice to make — so make it rather than asking.
  useEffect(() => {
    if (!sources) {
      return;
    }
    if (flow.reference && !references.some((r) => r.id === flow.reference)) {
      flow.setReference("");
      return;
    }
    if (!flow.reference && references.length === 1) {
      flow.setReference(references[0].id);
    }
  }, [references, sources, flow.reference]);

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
    (mode === "sources" &&
      flow.sources.length > 0 &&
      !!flow.build &&
      // Submitting without one fails server-side in withDefaultReference, with a
      // message about a default the person choosing sources cannot set.
      (!needsReference || !!flow.reference));

  function toggleField(name: string) {
    flow.setAnnotations(
      flow.annotations.includes(name)
        ? flow.annotations.filter((n) => n !== name)
        : [...flow.annotations, name],
    );
  }

  // Counted against the fields on offer rather than against flow.annotations
  // itself, which can still name a field from a source since deselected —
  // otherwise the header reads "13 of 12" and its checkbox never fills.
  const chosen = fields.filter((f) => flow.annotations.includes(f.name)).length;
  const allChosen = fields.length > 0 && chosen === fields.length;
  const someChosen = chosen > 0 && !allChosen;

  // Clearing sets the empty list rather than subtracting the visible fields, so
  // "select none" also drops any such leftover.
  function toggleAllFields() {
    flow.setAnnotations(allChosen ? [] : fields.map((f) => f.name));
  }

  function next() {
    const p = new URLSearchParams();
    if (mode === "snapshot") p.set("snapshot", flow.snapshot);
    else {
      // Appended to the sources rather than sent separately: a reference is an
      // ordinary pinned source to everything downstream, and pinning it
      // explicitly is what stops the server picking the build's default.
      const picked = needsReference && flow.reference
        ? [...flow.sources, flow.reference]
        : flow.sources;
      p.set("sources", picked.join(","));
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
                      <Lock size={11} /> Not offered to everyone
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
          {/* Only when the selection needs one. Most annotation opens no genome,
              and asking every time would be a question with no consequence. */}
          {needsReference && (
            <div style={{ marginBottom: 14 }}>
              <label className="label">Reference FASTA</label>
              {references.length === 0 ? (
                <p className="err" style={{ fontSize: 13, margin: "4px 0 0" }}>
                  {visible
                    .filter((s) => flow.sources.includes(s.id) && s.requires_reference)
                    .map((s) => s.title || s.name)
                    .join(", ")}{" "}
                  needs a reference genome, and none is registered for {flow.build}.
                  An administrator can add one from the sources page.
                </p>
              ) : (
                <>
                  <select
                    className="select mono"
                    value={flow.reference}
                    onChange={(e) => flow.setReference(e.target.value)}
                  >
                    {/* Absent once one is chosen, so the control cannot be put
                        back into a state the form will not accept. */}
                    {!flow.reference && <option value="">— choose a reference —</option>}
                    {references.map((r) => (
                      <option key={r.id} value={r.id}>
                        {r.name} {r.version}
                        {r.is_default_reference ? " (default)" : ""}
                      </option>
                    ))}
                  </select>
                  <p className="lede" style={{ fontSize: 12, margin: "6px 0 0" }}>
                    Required by{" "}
                    {visible
                      .filter((s) => flow.sources.includes(s.id) && s.requires_reference)
                      .map((s) => s.title || s.name)
                      .join(", ")}
                    . Only references for {flow.build} are offered — a snapshot cannot
                    mix assemblies.
                  </p>
                </>
              )}
            </div>
          )}

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
                      color:
                        s.visibility === "public" ? "var(--text-3)" : "var(--private)",
                    }}
                  >
                    {LEVEL_LABEL[s.visibility] ?? s.visibility}
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
          </div>

          {fields.length === 0 ? (
            <div className="card empty">
              These sources declare no annotation fields.
            </div>
          ) : (
            <div className="card" style={{ padding: "6px 0" }}>
              {/* Select-all as the first row of the list rather than as a pair of
                  links above it, so its box sits in the same column as the boxes
                  it controls — which is what makes it read as "all of these"
                  rather than as an unrelated control that happens to be nearby. */}
              <button
                className="trow row gap-10"
                // "mixed" is the state a partial selection is actually in.
                // Reporting it as unchecked would tell a screen reader that
                // nothing is selected while the page plainly shows otherwise.
                aria-pressed={allChosen ? "true" : someChosen ? "mixed" : "false"}
                style={{
                  cursor: "pointer",
                  padding: "8px 18px",
                  borderBottom: "1px solid var(--hairline)",
                  // Never the selected-row tint, whatever its state. It is a
                  // control over the list rather than a member of it, and taking
                  // the tint made it blend into the rows when everything was
                  // checked and stand apart from them when only some was — the
                  // header changing character as the selection changed.
                  background: "var(--surface)",
                }}
                onClick={toggleAllFields}
              >
                <span
                  className={`check sm ${allChosen ? "on" : someChosen ? "some" : ""}`}
                >
                  {allChosen && <Check size={12} strokeWidth={3} />}
                  {someChosen && <Minus size={12} strokeWidth={3} />}
                </span>
                <span style={{ flex: 1, fontWeight: 500 }}>Select all</span>
                <span style={{ fontSize: 12.5, color: "var(--text-2)" }}>
                  {chosen} of {fields.length}
                </span>
              </button>
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

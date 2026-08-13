import { useEffect, useMemo, useState } from "react";
import { Check, Plus, TriangleAlert } from "lucide-react";
import {
  api,
  type GeneListCheck,
  type GeneModel,
  type Source,
  type Visibility,
} from "../api";
import {
  LEVEL_HELP,
  LEVEL_LABEL,
  LEVELS,
  VisibilityPicker,
} from "./Visibility";

// The gene-list builder.
//
// A gene list is an ordinary source with type = "genelist", and one could always
// be registered by writing the manifest by hand on the Sources tab. The part a
// person cannot do by hand is get the gene names right: a symbol the gene model
// does not have contributes nothing at annotate time and reports nothing either,
// so a typo is indistinguishable from a gene no variant landed in. The list looks
// like it worked.
//
// So this screen is a paste box, a gene model to check against, and a refusal to
// save until every gene resolves.

export default function GeneLists({
  sources,
  onChange,
}: {
  sources: Source[];
  onChange: () => void;
}) {
  const [adding, setAdding] = useState(false);
  // The list being edited, or "" for none. One form serves both: editing is the
  // same screen with the genes already in it, so a correction is the same work
  // as the first entry rather than a different feature.
  const [editing, setEditing] = useState("");
  const lists = useMemo(
    () => sources.filter((s) => s.kind === "genelist"),
    [sources],
  );

  if (adding || editing)
    return (
      <BuildGeneList
        editID={editing}
        onCancel={() => {
          setAdding(false);
          setEditing("");
        }}
        onDone={() => {
          setAdding(false);
          setEditing("");
          onChange();
        }}
      />
    );

  return (
    <>
      <div className="between" style={{ marginBottom: 14 }}>
        <p className="lede" style={{ fontSize: 13.5, margin: 0 }}>
          A gene list flags a variant when the gene it falls in is one you named
          — a cancer panel, an actionable set, a drug-target list. Genes are
          checked against a GTF gene model, which is also what resolves a
          variant to its gene when the list runs.
        </p>
        <button className="btn sm" onClick={() => setAdding(true)}>
          <Plus size={15} /> New gene list
        </button>
      </div>

      <div className="card">
        <div className="thead">
          <span>Gene lists</span>
        </div>
        {lists.length === 0 && (
          <div className="empty">
            No gene lists yet. A new one needs a provisioned GTF source to check
            its genes against.
          </div>
        )}
        {lists.map((s) => (
          <div
            key={s.id}
            className="trow between"
            style={{ padding: "11px 13px" }}
          >
            <span>
              <span style={{ fontSize: 13, fontWeight: 500 }}>
                {s.title || s.name}
              </span>
              <br />
              <span
                className="mono"
                style={{ fontSize: 10.5, color: "var(--text-3)" }}
              >
                {s.name}:{s.version}
                {s.build ? ` · ${s.build}` : ""}
                {/* The gene model is not a detail — the list cannot answer
                    without it, and a snapshot that pins one without the other
                    fails at annotate time rather than at save. */}
                {s.genelist_gtf ? ` · via ${s.genelist_gtf}` : ""}
              </span>
            </span>
            <span className="row gap-8">
              {/* A gene list is a source, so this is the same toggle the sources
                  table uses and the same endpoint behind it. */}
              <VisibilityPicker
                level={s.visibility}
                onChange={async (next) => {
                  await api.setSourceVisibility(s.id, next);
                  onChange();
                }}
              />
              <button
                className="btn secondary sm"
                onClick={() => setEditing(s.id)}
              >
                Edit
              </button>
            </span>
          </div>
        ))}
      </div>
    </>
  );
}

function BuildGeneList({
  editID,
  onCancel,
  onDone,
}: {
  /** The list being edited, or "" to create a new one. */
  editID: string;
  onCancel: () => void;
  onDone: () => void;
}) {
  const editingExisting = !!editID;
  const [models, setModels] = useState<GeneModel[] | null>(null);
  const [gtf, setGtf] = useState("");
  const [text, setText] = useState("");
  const [byID, setByID] = useState(false);

  const [name, setName] = useState("");
  const [version, setVersion] = useState("1");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [annName, setAnnName] = useState("");
  // Closed by default: the two mistakes are not symmetric. A list that should
  // have been public is one change away from being fixed; one that should have
  // been restricted has already been readable by everyone who could reach the
  // server.
  const [visibility, setVisibility] = useState<Visibility>("restricted");

  // Why an existing list cannot be changed, when it cannot. Known before the
  // form is touched, so nobody retypes a panel and learns on save.
  const [pinnedBy, setPinnedBy] = useState<string[]>([]);
  const [loaded, setLoaded] = useState(!editID);
  const [wantGTF, setWantGTF] = useState("");

  const [check, setCheck] = useState<GeneListCheck | null>(null);
  const [checkErr, setCheckErr] = useState("");
  const [checking, setChecking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  useEffect(() => {
    api
      .geneModels()
      .then(setModels)
      .catch((e) => {
        setModels([]);
        setErr(e instanceof Error ? e.message : String(e));
      });
  }, []);

  // Load the stored list into the form. The genes come back as an array and go
  // into the textarea one per line, which is also the shape somebody editing
  // them wants to see — a 300-gene panel on one comma-joined line is not
  // reviewable.
  useEffect(() => {
    if (!editID) return;
    api
      .geneList(editID)
      .then((g) => {
        setText(g.genes.join("\n"));
        setByID(g.gene_field === "gene_id");
        setName(g.name);
        setVersion(g.version);
        setTitle(g.title ?? "");
        setDescription(g.description ?? "");
        setAnnName(g.annotation_name ?? "");
        setVisibility(g.visibility);
        setWantGTF(g.gtf);
        setPinnedBy(g.editable ? [] : (g.pinned_by ?? []));
        setLoaded(true);
      })
      .catch((e) => {
        setErr(e instanceof Error ? e.message : String(e));
        setLoaded(true);
      });
  }, [editID]);

  // A stored list names its gene model by ref ("gencode:48"); the picker holds a
  // source id. Resolve one to the other once both have arrived.
  useEffect(() => {
    if (!models || !wantGTF) return;
    const found = models.find((m) => m.ref === wantGTF);
    if (found) setGtf(found.id);
    setWantGTF("");
  }, [models, wantGTF]);

  // Pick the first model that actually has genes. One with none cannot validate
  // anything, and defaulting to it would make a correct list look entirely wrong.
  // Skipped while an edit is still resolving its own model, or the default would
  // win the race and silently repoint the list.
  useEffect(() => {
    if (!models || gtf || wantGTF || !loaded) return;
    setGtf(models.find((m) => m.genes > 0)?.id ?? models[0]?.id ?? "");
  }, [models, gtf, wantGTF, loaded]);

  // Validate as they type, debounced. Checking on submit alone would mean the
  // first thing a long paste tells you is that it failed.
  useEffect(() => {
    if (!gtf || !text.trim()) {
      setCheck(null);
      setCheckErr("");
      return;
    }
    setChecking(true);
    const t = window.setTimeout(() => {
      api
        .validateGeneList({
          gtf_source_id: gtf,
          genes: text,
          gene_field: byID ? "gene_id" : "gene_name",
        })
        .then((c) => {
          setCheck(c);
          setCheckErr("");
        })
        .catch((e) => {
          setCheck(null);
          setCheckErr(e instanceof Error ? e.message : String(e));
        })
        .finally(() => setChecking(false));
    }, 400);
    return () => {
      window.clearTimeout(t);
      setChecking(false);
    };
  }, [gtf, text, byID]);

  const model = models?.find((m) => m.id === gtf);
  const frozen = pinnedBy.length > 0;
  const clean =
    !!check && (check.unknown?.length ?? 0) === 0 && check.total > 0;
  const canSave =
    clean && !!name.trim() && !!version.trim() && !busy && !frozen && loaded;

  async function save() {
    setBusy(true);
    setErr("");
    try {
      if (editingExisting) {
        // Name and version are not sent: they identify the source, and the
        // server takes them from the stored row so a form echoing them back
        // cannot rename anything. Visibility is left alone too — changing which
        // genes are in a list says nothing about who may use it.
        await api.updateGeneList(editID, {
          gtf_source_id: gtf,
          genes: text,
          gene_field: byID ? "gene_id" : "gene_name",
          title: title.trim() || undefined,
          description: description.trim() || undefined,
          annotation_name: annName.trim() || undefined,
        });
      } else {
        await api.createGeneList({
          gtf_source_id: gtf,
          genes: text,
          gene_field: byID ? "gene_id" : "gene_name",
          name: name.trim(),
          version: version.trim(),
          title: title.trim() || undefined,
          description: description.trim() || undefined,
          annotation_name: annName.trim() || undefined,
          visibility,
        });
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
        ← Back to gene lists
      </button>
      <h1 style={{ fontSize: 24, fontWeight: 600, margin: "14px 0 6px" }}>
        {editingExisting ? `Edit ${name || "gene list"}` : "New gene list"}
      </h1>
      <p className="lede" style={{ fontSize: 13.5 }}>
        Paste the genes and pick the gene model to resolve them through. Every
        gene has to exist in that model before the list can be saved — a name it
        does not have would silently match nothing.
      </p>

      {err && <p className="err">{err}</p>}

      {/* Said before the form is touched, not on save. A snapshot is a promise
          about what an annotation ran against, so a pinned list is frozen — and
          finding that out after retyping a panel is the wrong moment. */}
      {frozen && (
        <p className="err" style={{ fontSize: 13 }}>
          This list is pinned by {pinnedBy.join(", ")} and cannot be changed —
          a snapshot records what an annotation ran against. Remove it from
          those snapshots, or register a new version alongside it.
        </p>
      )}

      <div
        style={{
          display: "grid",
          gridTemplateColumns: "1fr 340px",
          gap: 20,
          alignItems: "start",
          marginTop: 20,
        }}
      >
        <div>
          <label className="label">Genes</label>
          <textarea
            className="input mono"
            style={{ minHeight: 260, resize: "vertical", width: "100%" }}
            placeholder={"TP53\nMYC\nKRAS\nMDM2\nCDK4"}
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
          <p
            style={{ fontSize: 12, color: "var(--text-3)", margin: "6px 0 0" }}
          >
            One per line, or separated by commas, tabs or spaces — paste a
            column straight out of a spreadsheet. A{" "}
            <span className="mono">#</span> comments out the rest of its line.
            Case does not matter.
          </p>

          <CheckReport
            check={check}
            checking={checking}
            error={checkErr}
            byID={byID}
            hasText={!!text.trim()}
          />
        </div>

        <div>
          <label className="label">Gene model</label>
          <select
            className="select mono"
            value={gtf}
            onChange={(e) => setGtf(e.target.value)}
          >
            {models === null && <option value="">Loading…</option>}
            {models?.length === 0 && (
              <option value="">(no GTF sources registered)</option>
            )}
            {models?.map((m) => (
              <option key={m.id} value={m.id}>
                {m.ref}
                {m.build ? ` · ${m.build}` : ""}
                {m.genes > 0
                  ? ` · ${m.genes.toLocaleString()} genes`
                  : " · not provisioned"}
              </option>
            ))}
          </select>
          {model && model.genes === 0 && (
            // Said plainly, because otherwise the form reports every gene as
            // unknown and the obvious conclusion is that the genes are wrong.
            <p className="err" style={{ fontSize: 12.5, marginTop: 8 }}>
              {model.ref} has not been provisioned yet, so there are no genes to
              check against. Download it from the Sources tab; its genes become
              available when that job finishes.
            </p>
          )}
          <p
            style={{ fontSize: 12, color: "var(--text-3)", margin: "6px 0 0" }}
          >
            Selecting this list for a run pins the gene model too — it cannot
            resolve a variant to a gene without one.
          </p>

          <label className="label" style={{ marginTop: 14 }}>
            <input
              type="checkbox"
              checked={byID}
              onChange={(e) => setByID(e.target.checked)}
            />{" "}
            Match on gene IDs
          </label>
          <p
            style={{ fontSize: 12, color: "var(--text-3)", margin: "2px 0 0" }}
          >
            Ensembl/GENCODE identifiers rather than symbols. Version suffixes
            are ignored, so <span className="mono">ENSG00000141510</span> and{" "}
            <span className="mono">ENSG00000141510.17</span> are the same gene.
          </p>

          <label className="label" style={{ marginTop: 16 }}>
            Name
          </label>
          <input
            className="input mono"
            placeholder="cancer_genes"
            value={name}
            disabled={editingExisting}
            onChange={(e) => setName(e.target.value)}
          />
          <p
            style={{ fontSize: 12, color: "var(--text-3)", margin: "4px 0 0" }}
          >
            {editingExisting
              ? "Fixed. Name and version identify the list and are where its files would live, so changing either would make it a different one — register that alongside instead."
              : "Letters, digits and underscores. This becomes the source name."}
          </p>

          <label className="label" style={{ marginTop: 12 }}>
            Version
          </label>
          <input
            className="input mono"
            value={version}
            disabled={editingExisting}
            onChange={(e) => setVersion(e.target.value)}
          />

          <label className="label" style={{ marginTop: 12 }}>
            Annotation name
          </label>
          <input
            className="input mono"
            placeholder={name || "cancer_gene"}
            value={annName}
            onChange={(e) => setAnnName(e.target.value)}
          />
          <p
            style={{ fontSize: 12, color: "var(--text-3)", margin: "4px 0 0" }}
          >
            The column a match is reported under. Defaults to the name.
          </p>

          <label className="label" style={{ marginTop: 12 }}>
            Title
          </label>
          <input
            className="input"
            placeholder="Cancer gene panel"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
          />

          <label className="label" style={{ marginTop: 12 }}>
            Description
          </label>
          <input
            className="input"
            placeholder="Variant falls in a cancer-related gene"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />

          <label className="label" style={{ marginTop: 14 }}>
            Who can use it
          </label>
          <select
            className="select"
            value={visibility}
            onChange={(e) => setVisibility(e.target.value as Visibility)}
          >
            {LEVELS.map((l) => (
              <option key={l} value={l}>
                {LEVEL_LABEL[l]}
              </option>
            ))}
          </select>
          <p
            style={{ fontSize: 12, color: "var(--text-3)", margin: "4px 0 0" }}
          >
            {LEVEL_HELP[visibility]}
          </p>

          <button
            className="btn"
            style={{ marginTop: 18, width: "100%" }}
            disabled={!canSave}
            onClick={save}
          >
            {busy
              ? "Saving…"
              : editingExisting
                ? "Save changes"
                : "Create gene list"}
          </button>
          {!clean && !!text.trim() && (
            <p
              style={{
                fontSize: 12,
                color: "var(--text-3)",
                margin: "8px 0 0",
              }}
            >
              Every gene has to resolve before this can be saved.
            </p>
          )}
        </div>
      </div>
    </div>
  );
}

// CheckReport is the whole of the feedback: how many genes were read, and which
// of them do not exist.
//
// The unknown ones are listed rather than counted. "3 genes were not found" is
// not actionable — the user has to be told which three, and told them in the
// spelling they typed, or they cannot find them in the box they just pasted into.
function CheckReport({
  check,
  checking,
  error,
  byID,
  hasText,
}: {
  check: GeneListCheck | null;
  checking: boolean;
  error: string;
  byID: boolean;
  hasText: boolean;
}) {
  if (error)
    return (
      <p className="err" style={{ fontSize: 12.5, marginTop: 12 }}>
        {error}
      </p>
    );
  if (!hasText) return null;
  if (!check)
    return (
      <p style={{ fontSize: 12.5, color: "var(--text-3)", marginTop: 12 }}>
        {checking ? "Checking…" : ""}
      </p>
    );

  const unknown = check.unknown ?? [];
  const noun = byID ? "gene IDs" : "genes";

  return (
    <div style={{ marginTop: 14 }}>
      {unknown.length === 0 ? (
        <p
          style={{
            fontSize: 13,
            margin: 0,
            display: "flex",
            alignItems: "center",
            gap: 6,
            color: "var(--ok, #2f7a4d)",
          }}
        >
          <Check size={15} /> All {check.total} {noun} found in {check.gtf}.
        </p>
      ) : (
        <>
          <p
            style={{
              fontSize: 13,
              margin: 0,
              display: "flex",
              alignItems: "center",
              gap: 6,
              fontWeight: 500,
            }}
            className="err"
          >
            <TriangleAlert size={15} /> {unknown.length} of {check.total} {noun}{" "}
            are not in {check.gtf}.
          </p>
          <div
            className="card"
            style={{
              marginTop: 8,
              padding: "10px 12px",
              display: "flex",
              flexWrap: "wrap",
              gap: 6,
            }}
          >
            {unknown.map((g) => (
              <span key={g} className="pill-mono" style={{ fontSize: 11.5 }}>
                {g}
              </span>
            ))}
          </div>
          <p
            style={{ fontSize: 12, color: "var(--text-3)", margin: "6px 0 0" }}
          >
            Fix or remove these to continue. Deprecated symbols are not resolved
            for you — {check.gtf} is the authority on what a gene is called, and
            guessing at an alias would put a different gene in the list than the
            one that was asked for.
          </p>
        </>
      )}
    </div>
  );
}

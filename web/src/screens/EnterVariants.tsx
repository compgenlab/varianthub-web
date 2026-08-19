import { useState } from "react";
import { Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { Play, Plus, Upload, X, FileText } from "lucide-react";

import { api } from "../api";
import { useFlow } from "../flow";

type Mode = "single" | "batch" | "vcf";

const PLACEHOLDER = "chr17:7676154:C:T · chr17-7676154-C-T";

export default function EnterVariants() {
  const nav = useNavigate();
  const flow = useFlow();
  const [params] = useSearchParams();
  const [mode, setMode] = useState<Mode>("batch");
  const [draft, setDraft] = useState("");
  const [batch, setBatch] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  // The chosen snapshot comes from the URL, not context. Context does not survive
  // a reload or a back-navigation into this step from a fresh page load, so a
  // context-only guard bounced the user to step 1 — which is what "the back
  // button doesn't work" looked like. With it in the URL the step is a real,
  // reloadable location and browser history behaves.
  const snapshot = params.get("snapshot") ?? "";
  const sources = (params.get("sources") ?? "").split(",").filter(Boolean);
  const build = params.get("build") ?? "";
  const annotations = (params.get("annotations") ?? "").split(",").filter(Boolean);

  // Declarative redirect: calling navigate() during render is not supported and
  // fights the history stack.
  if (!snapshot && sources.length === 0) return <Navigate to="/annotate/sources" replace />;

  const chips = flow.variants;
  const batchLines = batch.split("\n").map((l) => l.trim()).filter(Boolean);
  const count = mode === "single" ? chips.length : mode === "batch" ? batchLines.length : 0;

  // Behind a disclosure, because it is for a service rather than a person: the
  // page it submits from is already watching the job, so anyone filling this in
  // wants something *else* told. Putting it in front of everyone would ask most
  // of them a question that does not apply.
  const [showCallback, setShowCallback] = useState(false);
  const [callback, setCallback] = useState("");

  function addChip() {
    const v = draft.trim();
    if (!v) return;
    flow.setVariants([...chips, v]);
    setDraft("");
  }

  async function run() {
    setErr("");
    setBusy(true);
    try {
      let job: { job_id: string };
      if (mode === "vcf") {
        if (!flow.file) throw new Error("Choose a VCF file first.");
        job = await api.annotateVCF(flow.file, {
          snapshot: snapshot || undefined,
          sources: sources.length ? sources : undefined,
          build: build || undefined,
          annotations: annotations.length ? annotations.join(",") : undefined,
          callback_url: callback.trim() || undefined,
        });
      } else {
        const variants = mode === "single" ? chips : batchLines;
        if (variants.length === 0) throw new Error("Enter at least one variant.");
        job = await api.annotate({
          snapshot: snapshot || undefined,
          sources: sources.length ? sources : undefined,
          build: build || undefined,
          variants,
          annotations: annotations.length ? annotations : undefined,
          callback_url: callback.trim() || undefined,
        });
      }
      nav(`/annotate/running/${job.job_id}`);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="page page-narrow">
      <h1 className="title">Enter variants</h1>
      <p className="lede">
        Accepts VCF-style coordinates — <span className="mono">chr:pos:ref:alt</span>,
        as varhub uses, or the dash form. HGVS and rsIDs are not resolved yet.
      </p>

      <div className="segmented" style={{ margin: "24px 0 22px" }}>
        {(["single", "batch", "vcf"] as Mode[]).map((m) => (
          <button key={m} aria-pressed={mode === m} onClick={() => setMode(m)}>
            {m === "single" ? "Single" : m === "batch" ? "Batch paste" : "VCF upload"}
          </button>
        ))}
      </div>

      {mode === "single" && (
        <>
          <div className="row gap-10" style={{ alignItems: "flex-end" }}>
            <div style={{ flex: 1, minWidth: 300 }}>
              <label className="label" htmlFor="v">
                Variant
              </label>
              <input
                id="v"
                className="input mono"
                value={draft}
                placeholder={PLACEHOLDER}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    addChip();
                  }
                }}
              />
            </div>
            <button className="btn secondary" onClick={addChip}>
              <Plus size={15} /> Add
            </button>
          </div>
          {chips.length > 0 && (
            <div className="row gap-8" style={{ flexWrap: "wrap", marginTop: 16 }}>
              {chips.map((c, i) => (
                <span
                  key={`${c}-${i}`}
                  className="row mono"
                  style={{
                    gap: 6,
                    padding: "6px 8px 6px 12px",
                    background: "var(--surface)",
                    border: "1px solid var(--border-input-soft)",
                    borderRadius: 7,
                    fontSize: 12,
                  }}
                >
                  {c}
                  <button
                    aria-label={`Remove ${c}`}
                    style={{ background: "none", border: "none", color: "var(--text-4)", padding: 0 }}
                    onClick={() => flow.setVariants(chips.filter((_, j) => j !== i))}
                  >
                    <X size={13} />
                  </button>
                </span>
              ))}
            </div>
          )}
        </>
      )}

      {mode === "batch" && (
        <>
          <label className="label" htmlFor="b">
            One variant per line
          </label>
          <textarea
            id="b"
            className="textarea"
            value={batch}
            onChange={(e) => setBatch(e.target.value)}
            placeholder={"chr7:140753336:A:T\nchr17:7676154:C:T\nchr13-32340301-G-A"}
          />
          <p style={{ fontSize: 12.5, color: "var(--text-2)", marginTop: 8 }}>
            <strong style={{ color: "var(--accent-text)" }}>{batchLines.length}</strong>{" "}
            variants detected
          </p>
        </>
      )}

      {mode === "vcf" && (
        <>
          <label className="dropzone" style={{ display: "block" }}>
            <input
              type="file"
              accept=".vcf,.vcf.gz"
              style={{ display: "none" }}
              onChange={(e) => flow.setFile(e.target.files?.[0] ?? null)}
            />
            <Upload size={30} color="var(--accent)" />
            <div style={{ fontSize: 16, fontWeight: 600, marginTop: 8 }}>
              Choose a .vcf or .vcf.gz file
            </div>
            <div style={{ fontSize: 13, color: "var(--text-2)" }}>
              bgzipped + tabix accepted · 64 MiB max
            </div>
          </label>
          {flow.file && (
            <div
              className="row gap-14"
              style={{
                marginTop: 14,
                background: "var(--surface)",
                border: "1px solid rgba(22,24,29,.1)",
                borderRadius: 9,
                padding: "13px 15px",
              }}
            >
              <FileText size={18} color="var(--accent)" />
              <div style={{ flex: 1 }}>
                <div className="mono" style={{ fontSize: 13, fontWeight: 500 }}>
                  {flow.file.name}
                </div>
                <div style={{ fontSize: 11.5, color: "var(--text-3)" }}>
                  {(flow.file.size / 1024 / 1024).toFixed(2)} MB
                </div>
              </div>
              <button
                className="btn link"
                onClick={() => flow.setFile(null)}
                style={{ fontSize: 12.5 }}
              >
                Remove
              </button>
            </div>
          )}
        </>
      )}

      {err && (
        <p className="err" style={{ marginTop: 16, fontSize: 13 }}>
          {err}
        </p>
      )}

      <div style={{ marginTop: 24 }}>
        {!showCallback ? (
          <button
            className="btn link"
            style={{ fontSize: 12.5, padding: 0 }}
            onClick={() => setShowCallback(true)}
          >
            + Notify a server when this finishes
          </button>
        ) : (
          <div>
            <label
              style={{ display: "block", fontSize: 12.5, marginBottom: 6, color: "var(--text-2)" }}
            >
              Callback URL
            </label>
            <input
              className="input mono"
              style={{ width: "100%", fontSize: 12.5 }}
              placeholder="https://your-service.example.org/varianthub-hook"
              value={callback}
              onChange={(e) => setCallback(e.target.value)}
              spellCheck={false}
            />
            <p style={{ fontSize: 11.5, color: "var(--text-3)", margin: "8px 0 0", lineHeight: 1.5 }}>
              When the job finishes we POST{" "}
              <code>{`{"job_id", "status"}`}</code> here — nothing more, and it is
              not signed, so treat the status as a hint and fetch the job when you
              need certainty. It may arrive more than once; the{" "}
              <code>X-VariantHub-Delivery</code> header is the job id to
              deduplicate on. The address must be reachable from the internet;
              this page will keep showing you the job either way.
            </p>
          </div>
        )}
      </div>

      <div
        className="between"
        style={{ marginTop: 30, paddingTop: 20, borderTop: "1px solid var(--border)" }}
      >
        <button className="btn link" onClick={() => nav("/annotate/sources")}>
          ← Back to sources
        </button>
        <button
          className="btn"
          disabled={busy || (mode === "vcf" ? !flow.file : count === 0)}
          onClick={run}
        >
          <Play size={14} fill="currentColor" /> {busy ? "Submitting…" : "Run annotation"}
        </button>
      </div>
    </div>
  );
}

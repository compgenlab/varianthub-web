import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ArrowRight, Check, Lock } from "lucide-react";

import { api, type Snapshot } from "../api";
import { useFlow } from "../flow";

export default function ChooseSources() {
  const nav = useNavigate();
  const flow = useFlow();
  const [snapshots, setSnapshots] = useState<Snapshot[] | null>(null);
  const [err, setErr] = useState("");

  useEffect(() => {
    api
      .snapshots()
      .then((r) => {
        const list = r.snapshots ?? [];
        setSnapshots(list);
        // Preselect when there is exactly one, which is the common deployment.
        if (!flow.snapshot && list.length === 1) flow.setSnapshot(list[0].id);
      })
      .catch((e) => setErr(e.message));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const selected = snapshots?.find((s) => s.id === flow.snapshot);

  return (
    <div className="page">
      <h1 className="title">Choose annotations</h1>
      <p className="lede">
        Annotate with a curated <strong>snapshot</strong> — a versioned, pinned
        bundle of sources. Every source is tabix-indexed.
      </p>

      {err && (
        <p className="err" style={{ marginTop: 20 }}>
          {err}
        </p>
      )}

      {snapshots === null && !err && (
        <p style={{ marginTop: 24, color: "var(--text-3)" }}>Loading…</p>
      )}

      {snapshots?.length === 0 && (
        <div className="card empty" style={{ marginTop: 24 }}>
          No snapshots are registered yet. Add one with{" "}
          <code className="mono">varianthub-web seed</code> or the CLI.
        </div>
      )}

      {snapshots && snapshots.length > 0 && (
        <div className="grid-2" style={{ marginTop: 24 }}>
          {snapshots.map((s) => {
            const on = s.id === flow.snapshot;
            return (
              <button
                key={s.id}
                className="snapcard"
                aria-pressed={on}
                onClick={() => {
                  flow.setSnapshot(s.id);
                  flow.setAnnotations([]); // defaults follow the snapshot
                }}
              >
                <div className="between">
                  <h3>{s.title || s.id}</h3>
                  <span className={`check ${on ? "on" : ""}`} style={{ visibility: on ? "visible" : "hidden" }}>
                    <Check size={13} strokeWidth={3} />
                  </span>
                </div>
                <div className="meta">
                  {s.build} · {s.sources?.length ?? s.source_count ?? 0} sources
                </div>
                {s.description && <p>{s.description}</p>}
                {!!s.tags?.length && (
                  <div className="tags">
                    {s.tags.map((t) => (
                      <span className="tag" key={t}>
                        {t}
                      </span>
                    ))}
                  </div>
                )}
                {s.contains_private && (
                  <div
                    className="row gap-8"
                    style={{ marginTop: 11, fontSize: 11.5, color: "var(--text-2)" }}
                  >
                    <Lock size={11} /> Contains private sources
                  </div>
                )}
              </button>
            );
          })}
        </div>
      )}

      <div className="between" style={{ marginTop: 26 }}>
        <span style={{ fontSize: 13, color: "var(--text-2)" }}>
          {selected ? (
            <>
              <strong style={{ fontSize: 15, color: "var(--text)" }}>
                {selected.sources?.length ?? selected.source_count ?? 0}
              </strong>{" "}
              sources selected
            </>
          ) : (
            "No snapshot selected"
          )}
        </span>
        <button
          className="btn"
          disabled={!flow.snapshot}
          onClick={() => nav("/annotate/variants")}
        >
          Continue to variants <ArrowRight size={16} />
        </button>
      </div>
    </div>
  );
}

import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import {
  ArrowDown,
  ArrowUp,
  ChevronLeft,
  ChevronRight,
  Download,
  Search,
} from "lucide-react";

import { api, type Column, type ResultPage } from "../api";

const PER_PAGE = 50;

/** Maps a ClinVar-style significance string onto the handoff's pill classes. */
function sigClass(v: string): string | null {
  const s = v.toLowerCase();
  if (s.includes("likely pathogenic")) return "sig sig-likely-pathogenic";
  if (s.includes("pathogenic")) return "sig sig-pathogenic";
  if (s.includes("likely benign")) return "sig sig-likely-benign";
  if (s.includes("benign")) return "sig sig-benign";
  if (s === "vus" || s.includes("uncertain")) return "sig sig-vus";
  return null;
}

function renderCell(col: Column, raw: unknown) {
  if (raw === null || raw === undefined || raw === "") {
    return <span style={{ color: "var(--text-5)" }}>—</span>;
  }
  const text = typeof raw === "object" ? JSON.stringify(raw) : String(raw);
  const cls = sigClass(text);
  if (cls) return <span className={cls}>{text}</span>;
  if (col.type === "numeric") return <span className="mono">{text}</span>;
  return <span className="mono" style={{ fontSize: 12.5 }}>{text}</span>;
}

export default function JobResults() {
  const { jobId = "" } = useParams();
  const nav = useNavigate();

  const [page, setPage] = useState(1);
  const [sort, setSort] = useState("");
  const [desc, setDesc] = useState(false);
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  const [data, setData] = useState<ResultPage | null>(null);
  const [err, setErr] = useState("");
  const [menu, setMenu] = useState(false);
  const [busy, setBusy] = useState(false);

  // Debounce the search box: every keystroke would otherwise be a round trip,
  // and the server filters with a scan.
  useEffect(() => {
    const t = window.setTimeout(() => {
      setQuery(search);
      setPage(1);
    }, 250);
    return () => window.clearTimeout(t);
  }, [search]);

  useEffect(() => {
    let cancelled = false;
    setErr("");
    api
      .results(jobId, {
        page,
        per_page: PER_PAGE,
        sort: sort || undefined,
        order: desc ? "desc" : undefined,
        q: query || undefined,
      })
      .then((d) => !cancelled && setData(d))
      .catch((e) => !cancelled && setErr(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
    };
  }, [jobId, page, sort, desc, query]);

  const cols = data?.columns ?? [];
  const total = data?.total ?? 0;
  const pages = Math.max(1, Math.ceil(total / PER_PAGE));
  const from = total === 0 ? 0 : (page - 1) * PER_PAGE + 1;
  const to = Math.min(page * PER_PAGE, total);

  function toggleSort(key: string) {
    if (sort === key) setDesc(!desc);
    else {
      setSort(key);
      setDesc(false);
    }
    setPage(1);
  }

  async function download(format: "json" | "tsv" | "csv") {
    setMenu(false);
    setBusy(true);
    try {
      await api.downloadExport(jobId, format, {
        sort: sort || undefined,
        order: desc ? "desc" : undefined,
        q: query || undefined,
      });
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  const sortIcon = (key: string) =>
    sort === key ? desc ? <ArrowDown size={11} /> : <ArrowUp size={11} /> : null;

  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100%" }}>
      <div style={{ flex: "none", padding: "24px var(--gutter) 16px" }}>
        <button className="btn link" style={{ fontSize: 13 }} onClick={() => nav("/jobs")}>
          ← All jobs
        </button>

        <div
          className="between"
          style={{ alignItems: "flex-end", flexWrap: "wrap", marginTop: 12, gap: 12 }}
        >
          <div>
            <div className="eyebrow" style={{ color: "var(--accent)", letterSpacing: ".06em" }}>
              Job #{jobId.slice(0, 8)}
            </div>
            <h1 style={{ fontSize: 24, fontWeight: 600 }}>Annotated variants</h1>
          </div>

          <div className="row gap-8" style={{ position: "relative" }}>
            <div
              className="row"
              style={{
                height: 38,
                padding: "0 12px",
                gap: 7,
                background: "var(--surface)",
                border: "1px solid var(--border-input-soft)",
                borderRadius: 8,
              }}
            >
              <Search size={14} color="var(--text-4)" />
              <input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="Filter variants…"
                style={{ border: "none", outline: "none", width: 214, fontSize: 13 }}
              />
            </div>
            <button className="btn sm" disabled={busy} onClick={() => setMenu(!menu)}>
              <Download size={15} /> {busy ? "Preparing…" : "Download"}
            </button>
            {menu && (
              <div className="menu">
                <div
                  className="thead"
                  style={{ padding: "9px 13px", fontSize: 10 }}
                >
                  Export {total} variants
                </div>
                {(["json", "tsv", "csv"] as const).map((f) => (
                  <button key={f} onClick={() => download(f)}>
                    <span className="mono" style={{ fontWeight: 500 }}>
                      variants.{f}
                    </span>
                    <span style={{ fontSize: 11, color: "var(--text-3)" }}>
                      {f.toUpperCase()}
                    </span>
                  </button>
                ))}
              </div>
            )}
          </div>
        </div>

        <div
          className="row gap-14"
          style={{ marginTop: 14, fontSize: 12.5, color: "var(--text-2)" }}
        >
          <span>
            Showing <strong style={{ color: "var(--text)" }}>{from}–{to}</strong> of{" "}
            <strong style={{ color: "var(--text)" }}>{total}</strong>
          </span>
          {query && <span>· filtered</span>}
        </div>
        {err && <p className="err" style={{ fontSize: 13, marginTop: 10 }}>{err}</p>}
      </div>

      <div className="results">
        <table className="rt">
          <thead>
            <tr>
              <th>
                <button onClick={() => toggleSort("locus")}>
                  Variant {sortIcon("locus")}
                </button>
              </th>
              {cols.map((c) => (
                // The header is the field name — it is what the manifest
                // declares, what the export writes, and what a filter refers
                // to. The description is prose and belongs on hover.
                <th key={c.key} title={c.description || undefined}>
                  <button onClick={() => toggleSort(c.key)}>
                    <span className="mono">{c.label || c.key}</span>
                    {c.source && <span className="src-tag">{c.source}</span>}
                    {sortIcon(c.key)}
                  </button>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {data?.rows.map((v, i) => (
              <tr key={`${v.chrom}-${v.pos}-${v.ref}-${v.alt}-${i}`}>
                <td className="mono" style={{ fontSize: 12.5, whiteSpace: "nowrap" }}>
                  <span style={{ color: "var(--accent)" }}>{v.chrom}</span>-{v.pos}-
                  {v.ref}-{v.alt}
                </td>
                {cols.map((c) => (
                  <td key={c.key}>{renderCell(c, v.annotations[c.key])}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
        {data?.rows.length === 0 && (
          <div className="empty">
            {query ? "No variants match that filter." : "This job produced no variants."}
          </div>
        )}
      </div>

      <div className="between" style={{ flex: "none", padding: "13px var(--gutter)" }}>
        <span style={{ fontSize: 12.5, color: "var(--text-2)" }}>
          Page <strong style={{ color: "var(--text)" }}>{page}</strong> of{" "}
          <strong style={{ color: "var(--text)" }}>{pages}</strong> · {PER_PAGE} rows/page
        </span>
        <div className="row gap-8">
          <button
            className="icon-btn"
            disabled={page <= 1}
            onClick={() => setPage(page - 1)}
            aria-label="Previous page"
          >
            <ChevronLeft size={15} />
          </button>
          <button
            className="icon-btn"
            disabled={page >= pages}
            onClick={() => setPage(page + 1)}
            aria-label="Next page"
          >
            <ChevronRight size={15} />
          </button>
        </div>
      </div>
    </div>
  );
}

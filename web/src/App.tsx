import { useEffect, useState } from "react";
import {
  BrowserRouter,
  NavLink,
  Navigate,
  Route,
  Routes,
  useLocation,
} from "react-router-dom";
import { FilePlus2, Table2, KeyRound } from "lucide-react";

import { api, getToken, setToken } from "./api";
import { AnnotateProvider } from "./flow";
import ChooseSources from "./screens/ChooseSources";
import EnterVariants from "./screens/EnterVariants";
import Running from "./screens/Running";
import JobsList from "./screens/JobsList";
import JobResults from "./screens/JobResults";
import TokenGate from "./screens/TokenGate";

/** Steps are shown only during the annotation flow, per the handoff. */
function StepHeader() {
  const { pathname } = useLocation();
  const idx = pathname.startsWith("/annotate/sources")
    ? 0
    : pathname.startsWith("/annotate/variants")
      ? 1
      : pathname.startsWith("/annotate/running")
        ? 2
        : -1;
  if (idx < 0) return null;

  const steps = ["Sources", "Variants", "Results"];
  return (
    <div className="steps">
      {steps.map((label, i) => (
        <div key={label} style={{ display: "flex", alignItems: "center" }}>
          <div className={`step ${i === idx ? "active" : i < idx ? "done" : ""}`}>
            <span className="dot">{i < idx ? "✓" : i + 1}</span>
            <span className="step-label">{label}</span>
          </div>
          {i < steps.length - 1 && (
            <span className={`connector ${i < idx ? "done" : ""}`} />
          )}
        </div>
      ))}
    </div>
  );
}

function Shell({ children }: { children: React.ReactNode }) {
  const [version, setVersion] = useState("");
  const [snapshots, setSnapshots] = useState<number | null>(null);

  useEffect(() => {
    api.version().then((v) => setVersion(v.version)).catch(() => {});
    api
      .snapshots()
      .then((r) => setSnapshots(r.snapshots?.length ?? 0))
      .catch(() => {});
  }, []);

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="wordmark">
          <span className="mark" />
          VariantHub
        </div>
        <nav className="nav">
          <NavLink to="/annotate/sources">
            <FilePlus2 /> New annotation
          </NavLink>
          <NavLink to="/jobs">
            <Table2 /> Results
          </NavLink>
        </nav>
        <div
          style={{ marginTop: "auto", padding: 14, borderTop: "1px solid var(--border)" }}
        >
          <button
            className="btn link"
            style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12.5 }}
            onClick={() => {
              setToken("");
              location.reload();
            }}
          >
            <KeyRound size={14} /> Change API token
          </button>
        </div>
      </aside>

      <div className="main">
        <div className="topbar">
          <div className="row gap-10">
            <span className="eyebrow">Snapshots</span>
            <span className="pill-mono">{snapshots ?? "—"} available</span>
          </div>
          <span style={{ fontSize: 12.5, color: "var(--text-3)" }} className="mono">
            {version}
          </span>
        </div>
        <StepHeader />
        <div className="content">{children}</div>
      </div>
    </div>
  );
}

export default function App() {
  // Auth is a single shared bearer token today. Rather than fake a login screen
  // against an endpoint that does not exist, ask for the token once and keep it
  // for the session — replaced by real sign-in when accounts land.
  const [authed, setAuthed] = useState(!!getToken());
  if (!authed) return <TokenGate onDone={() => setAuthed(true)} />;

  return (
    <BrowserRouter>
      <AnnotateProvider>
        <Shell>
          <Routes>
            <Route path="/" element={<Navigate to="/annotate/sources" replace />} />
            <Route path="/annotate/sources" element={<ChooseSources />} />
            <Route path="/annotate/variants" element={<EnterVariants />} />
            <Route path="/annotate/running/:jobId" element={<Running />} />
            <Route path="/jobs" element={<JobsList />} />
            <Route path="/jobs/:jobId" element={<JobResults />} />
            <Route path="*" element={<Navigate to="/annotate/sources" replace />} />
          </Routes>
        </Shell>
      </AnnotateProvider>
    </BrowserRouter>
  );
}

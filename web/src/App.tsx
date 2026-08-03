import { useEffect, useState } from "react";
import {
  BrowserRouter,
  NavLink,
  Navigate,
  Route,
  Routes,
  useLocation,
} from "react-router-dom";
import {
  FilePlus2,
  Table2,
  ShieldCheck,
  ServerCog,
  HardDrive,
  ChartColumn,
  LogOut,
  LogIn,
  UserRound,
  UsersRound,
} from "lucide-react";

import { api, setToken, type Me } from "./api";
import { AnnotateProvider } from "./flow";
import ChooseSources from "./screens/ChooseSources";
import EnterVariants from "./screens/EnterVariants";
import Running from "./screens/Running";
import JobsList from "./screens/JobsList";
import JobResults from "./screens/JobResults";
import SignIn from "./screens/SignIn";
import Account from "./screens/Account";
import Groups from "./screens/Groups";
import Admin from "./screens/Admin";
import Metrics from "./screens/Metrics";
import SystemJobs from "./screens/SystemJobs";
import Files from "./screens/Files";
import StorageBrowser from "./screens/StorageBrowser";

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

/** What an unidentified caller looks like before the server has said otherwise. */
const anonymousMe: Me = {
  anonymous: true,
  admin: false,
  label: "anonymous",
  bootstrap: false,
};

function Shell({
  me,
  onSignIn,
  children,
}: {
  me: Me;
  onSignIn: () => void;
  children: React.ReactNode;
}) {
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

          {!me.anonymous && (
            <NavLink to="/account">
              <UserRound /> Account
            </NavLink>
          )}

          {/* Role-gated. Hidden rather than disabled for a non-admin: the server
              refuses these routes anyway, and showing them would only advertise
              actions the person cannot take. */}
          {me.admin && (
          <>
          <div style={{ padding: "14px 12px 5px" }}>
            <span
              className="row gap-8"
              style={{
                fontSize: 10,
                letterSpacing: ".09em",
                textTransform: "uppercase",
                color: "rgba(22,24,29,.35)",
              }}
            >
              Administration
              <span
                className="mono"
                style={{
                  fontSize: 9,
                  padding: "1px 6px",
                  borderRadius: 4,
                  background: "#f6eaea",
                  color: "#8f2f2f",
                  letterSpacing: 0,
                }}
              >
                ADMIN
              </span>
            </span>
          </div>
          <NavLink to="/admin">
            <ShieldCheck /> Sources &amp; snapshots
          </NavLink>
          <NavLink to="/admin/storage">
            <HardDrive /> Storage &amp; files
          </NavLink>
          <NavLink to="/admin/jobs">
            <ServerCog /> System jobs
          </NavLink>
          <NavLink to="/admin/metrics">
            <ChartColumn /> Metrics
          </NavLink>
          <NavLink to="/admin/groups">
            <UsersRound /> Users &amp; groups
          </NavLink>
          </>
          )}
        </nav>
        <div
          style={{ marginTop: "auto", padding: 14, borderTop: "1px solid var(--border)" }}
        >
          <div style={{ fontSize: 12, color: "var(--text-3)", marginBottom: 8 }}>
            {me.anonymous ? "Not signed in" : (me.user?.email ?? me.label)}
          </div>
          {me.anonymous ? (
            <button
              className="btn link"
              style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12.5 }}
              onClick={onSignIn}
            >
              <LogIn size={14} /> Sign in
            </button>
          ) : (
            <button
              className="btn link"
              style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12.5 }}
              onClick={async () => {
                // Ends the session server-side too; clearing the cookie alone
                // would leave a credential that still works if it is replayed.
                try {
                  await api.logout();
                } catch {
                  /* signing out locally matters more than the round trip */
                }
                setToken("");
                location.reload();
              }}
            >
              <LogOut size={14} /> Sign out
            </button>
          )}
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
  // Who the caller is comes from the server, not from whether a token happens to
  // be in storage: a stale token, an expired session and a signed-in cookie are
  // indistinguishable here, and only the server can tell them apart.
  const [me, setMe] = useState<Me | null>(null);
  const [ready, setReady] = useState(false);
  // Set when an anonymous visitor asks to sign in, so the wall can be reached
  // deliberately on an open instance rather than only by being turned away.
  const [signIn, setSignIn] = useState(false);

  useEffect(() => {
    api
      .me()
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setReady(true));
  }, []);

  if (!ready) return null;
  // An anonymous visitor gets the app when the server allows it, and the login
  // wall otherwise. Deciding here from what the server reported keeps the two
  // in agreement — a UI that gates on its own opinion would show a login screen
  // for endpoints that would have answered.
  const anon = !me || me.anonymous;
  if (anon && !(me?.allow_anonymous && !signIn)) {
    return (
      <SignIn
        me={me ?? { anonymous: true, admin: false, label: "anonymous", bootstrap: false }}
        onDone={() => location.reload()}
      />
    );
  }

  return (
    <BrowserRouter>
      <AnnotateProvider>
        <Shell me={me ?? anonymousMe} onSignIn={() => setSignIn(true)}>
          <Routes>
            <Route path="/" element={<Navigate to="/annotate/sources" replace />} />
            <Route path="/annotate/sources" element={<ChooseSources />} />
            <Route path="/annotate/variants" element={<EnterVariants />} />
            <Route path="/annotate/running/:jobId" element={<Running />} />
            <Route path="/jobs" element={<JobsList />} />
            <Route path="/jobs/:jobId" element={<JobResults />} />
            <Route path="/admin" element={<Admin />} />
            <Route path="/admin/storage" element={<Files />} />
            <Route path="/admin/storage/:id" element={<StorageBrowser />} />
            <Route path="/admin/jobs" element={<SystemJobs />} />
            <Route path="/admin/metrics" element={<Metrics />} />
            <Route path="/admin/groups" element={<Groups me={me ?? anonymousMe} />} />
            <Route path="/account" element={<Account me={me ?? anonymousMe} />} />
            <Route path="*" element={<Navigate to="/annotate/sources" replace />} />
          </Routes>
        </Shell>
      </AnnotateProvider>
    </BrowserRouter>
  );
}

import { ArrowRight, Boxes, GitBranch, LogIn, Server, Terminal } from "lucide-react";

import type { Me } from "../api";

/**
 * The public front door.
 *
 * An anonymous visitor used to arrive at a bare sign-in form: a password box on
 * an installation that never said what it was, what signing in would give them,
 * or whether they could run their own. This says those three things and then
 * gets out of the way.
 *
 * Shown instead of the wall, not in front of it — sign-in is a button here
 * rather than a step, so someone who already has an account is one click from
 * where they were going.
 */
export default function Landing({ me, onSignIn }: { me: Me; onSignIn: () => void }) {
  return (
    <div style={{ height: "100%", overflowY: "auto", background: "var(--bg)" }}>
      <header
        className="appbar"
        style={{ position: "sticky", top: 0, zIndex: 10, background: "var(--bg)" }}
      >
        {/* Links to "/" like the logo everywhere else. Here that is the page you
            are already on, which is the point: it behaves the same before and
            after signing in. */}
        <a href="/" className="wordmark" style={{ textDecoration: "none", color: "inherit" }}>
          <span className="mark" />
          VariantHub
        </a>
        <button className="btn" onClick={onSignIn} style={{ display: "flex", gap: 7 }}>
          <LogIn size={15} /> Sign in
        </button>
      </header>

      <main style={{ maxWidth: 860, margin: "0 auto", padding: "56px 24px 80px" }}>
        <h1 style={{ fontSize: 34, fontWeight: 600, lineHeight: 1.2, marginBottom: 14 }}>
          Annotate variants against sources you can name, pin, and re-run.
        </h1>
        <p style={{ fontSize: 16.5, lineHeight: 1.65, color: "var(--text-2)", marginBottom: 30 }}>
          VariantHub runs variant annotation against versioned bundles of reference data.
          A bundle — a <em>snapshot</em> — pins each source at a specific version, so the
          same input gives the same answer next month, and a result can say exactly what
          produced it.
        </p>

        <div className="row gap-14" style={{ marginBottom: 52 }}>
          <button className="btn primary" onClick={onSignIn} style={{ display: "flex", gap: 8 }}>
            <LogIn size={16} /> Sign in
          </button>
          {/* Only when the server actually permits it, rather than offering a
              door that turns out to be locked. */}
          {me.allow_anonymous && (
            <a className="btn" href="/annotate/sources" style={{ display: "flex", gap: 8 }}>
              Try it without an account <ArrowRight size={15} />
            </a>
          )}
        </div>

        <Section icon={<Boxes size={17} />} title="What it does">
          <p>
            You choose a snapshot, give it variants, and get a table back — one row per
            variant, one column per annotation, with the source of each column named.
            Input is <code className="mono">chrom:pos:ref:alt</code> or a VCF; a VCF in
            gives a VCF out, with the annotations added as INFO fields and everything else
            about the record left as it was.
          </p>
          <p>
            Sources are ordinary published data — gene models, population frequencies,
            pathogenicity predictions, external tools like VEP. Some are public, some are
            restricted to the groups granted them.
          </p>
        </Section>

        <Section icon={<GitBranch size={17} />} title="Why snapshots">
          <p>
            An annotation is only meaningful if you can say what it was made against.
            A snapshot pins every source at a version and records the assembly it belongs
            to, and a source of a mismatched assembly can never be added to it — a wrong
            genome build does not error, it returns plausible answers at coordinates that
            mean something else, which is the one failure invisible in the output.
          </p>
        </Section>

        <Section icon={<Terminal size={17} />} title="Using the API">
          <p>
            Everything the web app does, a token can do. Sign in, mint a token from your
            account, and submit jobs over HTTP — the API explorer in the app runs real
            requests against the documented surface, so what you try there is what a script
            will get.
          </p>
        </Section>

        <Section icon={<Server size={17} />} title="Running your own">
          <p>
            VariantHub is open source and meant to be self-hosted. Your data stays on your
            infrastructure; source files can live on local disk or in object storage and be
            read in place, so a server does not need room for the whole catalog.
          </p>
          <p>
            A Docker Compose stack brings up the API, a worker, and Postgres. The
            repository has the compose file and the configuration it expects.
          </p>
          <p>
            <a
              className="btn"
              href="https://github.com/compgenlab/varianthub-web"
              target="_blank"
              rel="noreferrer"
              style={{ display: "inline-flex", gap: 8, marginTop: 4 }}
            >
              Source and setup instructions <ArrowRight size={15} />
            </a>
          </p>
        </Section>

        <p style={{ fontSize: 12.5, color: "var(--text-3)", marginTop: 56 }}>
          Compgen Lab · <a href="https://github.com/compgenlab/varianthub-web">varianthub-web</a>
        </p>
      </main>
    </div>
  );
}

function Section({
  icon,
  title,
  children,
}: {
  icon: React.ReactNode;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <section style={{ marginBottom: 38 }}>
      <div className="row gap-8" style={{ marginBottom: 10, color: "var(--text-2)" }}>
        {icon}
        <h2 style={{ fontSize: 17, fontWeight: 600, margin: 0 }}>{title}</h2>
      </div>
      <div
        style={{ fontSize: 14.5, lineHeight: 1.7, color: "var(--text-2)" }}
        className="landing-prose"
      >
        {children}
      </div>
    </section>
  );
}

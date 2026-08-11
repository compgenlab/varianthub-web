import { useEffect, useState } from "react";
import { AlertTriangle, RotateCcw, Trash2 } from "lucide-react";
import { api, type SiteSettings as Settings } from "../api";

/**
 * The deployment's own settings.
 *
 * Each field shows its effective value and, when it differs from the file, says
 * so — with a way back. That distinction is the point of the screen: an
 * administrator debugging "why is anonymous access on?" needs to know whether to
 * edit config.toml or clear an override, and an effective value alone cannot
 * tell them.
 */
export default function SiteSettingsTab() {
  const [defaults, setDefaults] = useState<Settings | null>(null);
  const [overrides, setOverrides] = useState<Record<string, string>>({});
  const [form, setForm] = useState<Record<string, string>>({});
  const [cacheAvailable, setCacheAvailable] = useState(false);
  const [err, setErr] = useState("");
  const [msg, setMsg] = useState("");
  const [busy, setBusy] = useState(false);
  const [confirmClear, setConfirmClear] = useState(false);

  async function load() {
    try {
      const s = await api.settings();
      setDefaults(s.defaults);
      setOverrides(s.overrides ?? {});
      setCacheAvailable(s.cache_available);
      setForm(effectiveForm(s.effective));
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }
  useEffect(() => {
    load();
  }, []);

  async function save() {
    setBusy(true);
    setMsg("");
    try {
      // Sent whole, not per field: the server validates the set and writes all
      // or none, so a mistyped duration cannot leave half a form applied.
      await api.saveSettings(form);
      setMsg("Saved. New jobs pick this up within a few seconds.");
      await load();
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  // Reverting is sending an empty value: the row goes and the file's value
  // applies again. There is no third state meaning "same as the file".
  async function revert(key: string) {
    setBusy(true);
    try {
      await api.saveSettings({ [key]: "" });
      await load();
      setMsg("");
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function clearCache() {
    setBusy(true);
    setConfirmClear(false);
    try {
      await api.clearCache();
      setMsg("Cache cleared. The next run of each query recomputes.");
      setErr("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  if (!defaults) return <p className="lede">{err || "Loading…"}</p>;

  const overridden = (k: string) => k in overrides;

  return (
    <div style={{ maxWidth: 620 }}>
      <p className="lede" style={{ fontSize: 13.5, marginTop: 0 }}>
        Settings for this installation. config.toml supplies the defaults; a
        change here overrides one until you revert it.
      </p>

      {err && <p className="err">{err}</p>}
      {msg && (
        <p style={{ fontSize: 13, color: "var(--accent)", marginTop: 10 }}>{msg}</p>
      )}

      <Section title="Access">
        <Toggle
          label="Allow anonymous use"
          hint="Lets a visitor with no account submit jobs. The REST API still needs a token."
          checked={form.allow_anonymous === "true"}
          overridden={overridden("allow_anonymous")}
          configured={defaults.allow_anonymous ? "on" : "off"}
          onRevert={() => revert("allow_anonymous")}
          onChange={(v) => setForm({ ...form, allow_anonymous: String(v) })}
        />
      </Section>

      <Section title="Annotation cache">
        {!cacheAvailable && (
          <p className="lede" style={{ fontSize: 13, margin: "0 0 12px" }}>
            <AlertTriangle size={14} style={{ verticalAlign: "-2px" }} /> No
            database is configured, so there is nowhere to cache. These settings
            will have no effect.
          </p>
        )}
        <Toggle
          label="Cache annotations"
          hint="Variant annotations do not change, so a value computed once can be reused. Turning this off leaves what is already cached alone."
          checked={form.cache_enabled === "true"}
          overridden={overridden("cache_enabled")}
          configured={defaults.cache_enabled ? "on" : "off"}
          onRevert={() => revert("cache_enabled")}
          onChange={(v) => setForm({ ...form, cache_enabled: String(v) })}
        />
        <Field
          label="Keep at most"
          hint="Cached variant-source entries. Blank is unlimited. The oldest go first, at the end of each run."
          suffix="entries"
          value={form.cache_max_entries === "0" ? "" : (form.cache_max_entries ?? "")}
          placeholder={String(defaults.cache_max_entries || "unlimited")}
          overridden={overridden("cache_max_entries")}
          onRevert={() => revert("cache_max_entries")}
          onChange={(v) => setForm({ ...form, cache_max_entries: v })}
        />
        <Field
          label="Discard after"
          hint='Entries unused for longer than this are dropped. A Go duration — "2160h" is 90 days. Blank is never.'
          value={form.cache_max_age ?? ""}
          placeholder={defaults.cache_max_age || "never"}
          overridden={overridden("cache_max_age")}
          onRevert={() => revert("cache_max_age")}
          onChange={(v) => setForm({ ...form, cache_max_age: v })}
        />
      </Section>

      <div className="row gap-8" style={{ marginTop: 22 }}>
        <button className="btn" disabled={busy} onClick={save}>
          Save settings
        </button>
      </div>

      <Section title="Danger zone">
        <div className="between">
          <div>
            <div style={{ fontSize: 13.5, fontWeight: 500 }}>Clear the cache</div>
            <div style={{ fontSize: 12.5, color: "var(--text-2)", maxWidth: 400 }}>
              Removes every cached annotation. Nothing is lost that cannot be
              recomputed — the next run of each query is just slow again.
            </div>
          </div>
          {confirmClear ? (
            <div className="row gap-8">
              <button className="btn sm" disabled={busy} onClick={clearCache}>
                Yes, clear it
              </button>
              <button className="btn sm link" onClick={() => setConfirmClear(false)}>
                Cancel
              </button>
            </div>
          ) : (
            <button
              className="btn sm"
              disabled={busy || !cacheAvailable}
              onClick={() => setConfirmClear(true)}
            >
              <Trash2 size={14} /> Clear cache
            </button>
          )}
        </div>
      </Section>
    </div>
  );
}

/** The effective settings as form strings — the shape the API takes back. */
function effectiveForm(e: Settings): Record<string, string> {
  return {
    allow_anonymous: String(e.allow_anonymous),
    cache_enabled: String(e.cache_enabled),
    cache_max_entries: e.cache_max_entries ? String(e.cache_max_entries) : "",
    cache_max_age: e.cache_max_age ?? "",
  };
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div style={{ marginTop: 26 }}>
      <div
        className="eyebrow"
        style={{ color: "var(--text-3)", marginBottom: 12, letterSpacing: ".06em" }}
      >
        {title}
      </div>
      <div
        style={{
          border: "1px solid var(--hairline)",
          borderRadius: 8,
          padding: "4px 16px",
        }}
      >
        {children}
      </div>
    </div>
  );
}

/** "Overridden — config says X" plus a way back, shown only when it applies. */
function OverrideNote({
  configured,
  onRevert,
}: {
  configured?: string;
  onRevert: () => void;
}) {
  return (
    <div style={{ fontSize: 11.5, color: "var(--text-3)", marginTop: 3 }}>
      Overridden{configured ? ` — config.toml says ${configured}` : ""}.{" "}
      <button
        className="btn link"
        style={{ fontSize: 11.5, padding: 0 }}
        onClick={onRevert}
      >
        <RotateCcw size={11} style={{ verticalAlign: "-1px" }} /> revert
      </button>
    </div>
  );
}

function Toggle({
  label,
  hint,
  checked,
  overridden,
  configured,
  onChange,
  onRevert,
}: {
  label: string;
  hint: string;
  checked: boolean;
  overridden: boolean;
  configured: string;
  onChange: (v: boolean) => void;
  onRevert: () => void;
}) {
  return (
    <div style={{ padding: "14px 0", borderBottom: "1px solid var(--hairline)" }}>
      <label className="between" style={{ cursor: "pointer", gap: 16 }}>
        <div>
          <div style={{ fontSize: 13.5, fontWeight: 500 }}>{label}</div>
          <div style={{ fontSize: 12.5, color: "var(--text-2)", maxWidth: 420 }}>{hint}</div>
          {overridden && <OverrideNote configured={configured} onRevert={onRevert} />}
        </div>
        <input
          type="checkbox"
          checked={checked}
          onChange={(e) => onChange(e.target.checked)}
          style={{ width: 18, height: 18, flex: "none" }}
        />
      </label>
    </div>
  );
}

function Field({
  label,
  hint,
  value,
  placeholder,
  suffix,
  overridden,
  onChange,
  onRevert,
}: {
  label: string;
  hint: string;
  value: string;
  placeholder: string;
  suffix?: string;
  overridden: boolean;
  onChange: (v: string) => void;
  onRevert: () => void;
}) {
  return (
    <div style={{ padding: "14px 0", borderBottom: "1px solid var(--hairline)" }}>
      <div className="between" style={{ gap: 16 }}>
        <div>
          <div style={{ fontSize: 13.5, fontWeight: 500 }}>{label}</div>
          <div style={{ fontSize: 12.5, color: "var(--text-2)", maxWidth: 420 }}>{hint}</div>
          {overridden && <OverrideNote onRevert={onRevert} />}
        </div>
        <div className="row gap-8" style={{ flex: "none" }}>
          <input
            value={value}
            placeholder={placeholder}
            onChange={(e) => onChange(e.target.value)}
            style={{ width: 130, fontSize: 13, textAlign: "right" }}
          />
          {suffix && (
            <span style={{ fontSize: 12, color: "var(--text-3)" }}>{suffix}</span>
          )}
        </div>
      </div>
    </div>
  );
}

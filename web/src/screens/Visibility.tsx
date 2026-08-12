import { useState } from "react";
import { Globe, Lock, Users } from "lucide-react";
import type { Visibility } from "../api";

// Who may use a source, a gene list or a snapshot.
//
// Three levels, and the middle one is the reason this exists. "Not for anonymous
// visitors" used to be sayable only by granting a source to a team, one source at
// a time — administration that grew with the catalog to express something that is
// really a property of the deployment.

export const LEVELS: Visibility[] = ["public", "signed_in", "restricted"];

export const LEVEL_LABEL: Record<Visibility, string> = {
  public: "Public",
  signed_in: "Signed in",
  restricted: "Restricted",
};

export const LEVEL_HELP: Record<Visibility, string> = {
  public: "Anyone who can reach the server, including anonymous visitors",
  signed_in: "Anyone with an account — no group membership needed",
  restricted: "Only members of a group this is granted to",
};

export function LevelIcon({
  level,
  size = 11,
}: {
  level: Visibility;
  size?: number;
}) {
  if (level === "public") return <Globe size={size} />;
  if (level === "signed_in") return <Users size={size} />;
  return <Lock size={size} />;
}

/** A read-only badge, for rows that are not being edited. */
export function VisibilityBadge({ level }: { level: Visibility }) {
  return (
    <span
      title={LEVEL_HELP[level]}
      style={{
        fontSize: 12.5,
        display: "inline-flex",
        alignItems: "center",
        gap: 5,
        color: level === "public" ? "var(--text-2)" : "var(--private)",
      }}
    >
      <LevelIcon level={level} />
      {LEVEL_LABEL[level]}
    </span>
  );
}

/**
 * The picker. Saves on change rather than behind a button: there is one field,
 * and a separate save step for a single dropdown is a step that gets forgotten
 * — leaving the screen showing a level that was never stored.
 *
 * `note` carries the server's explanation when what was stored is not what takes
 * effect, which happens for a snapshot whose sources are more restrictive than
 * it is. Without it, setting a snapshot to public and seeing nothing change looks
 * like the control is broken.
 */
export function VisibilityPicker({
  level,
  onChange,
  disabled,
}: {
  level: Visibility;
  onChange: (next: Visibility) => Promise<{ note?: string } | void>;
  disabled?: boolean;
}) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [note, setNote] = useState("");

  async function pick(next: Visibility) {
    if (next === level) return;
    setBusy(true);
    setErr("");
    setNote("");
    try {
      const res = await onChange(next);
      if (res && "note" in res && res.note) setNote(res.note);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <span style={{ display: "inline-flex", flexDirection: "column", gap: 3 }}>
      <select
        className="select"
        style={{
          fontSize: 12,
          padding: "3px 6px",
          height: "auto",
          width: "auto",
        }}
        value={level}
        disabled={busy || disabled}
        title={LEVEL_HELP[level]}
        onChange={(e) => pick(e.target.value as Visibility)}
      >
        {LEVELS.map((l) => (
          <option key={l} value={l}>
            {LEVEL_LABEL[l]}
          </option>
        ))}
      </select>
      {err && (
        <span className="err" style={{ fontSize: 11 }}>
          {err}
        </span>
      )}
      {note && (
        <span style={{ fontSize: 11, color: "var(--text-3)", maxWidth: 260 }}>
          {note}
        </span>
      )}
    </span>
  );
}

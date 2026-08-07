import { createContext, useContext, useMemo, useState } from "react";

/**
 * State carried across the three annotation steps.
 *
 * Held in React context rather than the URL: the variant list can be large
 * (thousands of lines pasted in Step 2) and does not belong in a query string.
 * The consequence is that a hard reload on Step 2 sends you back to Step 1,
 * which is the right tradeoff for a wizard whose output is a durable job.
 */
export interface AnnotateState {
  snapshot: string;
  setSnapshot: (s: string) => void;
  /** Individual-source selection, used instead of a snapshot. */
  sources: string[];
  setSources: (s: string[]) => void;
  /** Reference genome, chosen separately because it contributes no annotations.
   *  Empty unless something in the selection requires one. */
  reference: string;
  setReference: (r: string) => void;
  /** Assembly, required when selecting individual sources. */
  build: string;
  setBuild: (b: string) => void;
  /** Selected annotation field names. */
  annotations: string[];
  setAnnotations: (a: string[]) => void;
  variants: string[];
  setVariants: (v: string[]) => void;
  file: File | null;
  setFile: (f: File | null) => void;
}

const Ctx = createContext<AnnotateState | null>(null);

export function AnnotateProvider({ children }: { children: React.ReactNode }) {
  const [snapshot, setSnapshot] = useState("");
  const [sources, setSources] = useState<string[]>([]);
  const [reference, setReference] = useState("");
  const [build, setBuild] = useState("GRCh38");
  const [annotations, setAnnotations] = useState<string[]>([]);
  const [variants, setVariants] = useState<string[]>([]);
  const [file, setFile] = useState<File | null>(null);

  const value = useMemo(
    () => ({
      snapshot,
      setSnapshot,
      sources,
      setSources,
      reference,
      setReference,
      build,
      setBuild,
      annotations,
      setAnnotations,
      variants,
      setVariants,
      file,
      setFile,
    }),
    [snapshot, sources, reference, build, annotations, variants, file],
  );
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useFlow(): AnnotateState {
  const v = useContext(Ctx);
  if (!v) throw new Error("useFlow outside AnnotateProvider");
  return v;
}

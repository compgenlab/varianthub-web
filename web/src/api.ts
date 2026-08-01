// Typed client for /api/v1. Shapes mirror docs/api.md — treat a change here as a
// change to a shared contract.

export interface Snapshot {
  id: string;
  title?: string;
  description?: string;
  build: string;
  state: string;
  source_count?: number;
  contains_private?: boolean;
  defaults?: string[];
  tags?: string[];
  sources?: Source[];
}

export interface Source {
  id: string;
  /** False for builtins, which compute from the variant and have no files. */
  needs_data?: boolean;
  name: string;
  version: string;
  ref?: string;
  title?: string;
  detail?: string;
  kind: string;
  build?: string;
  visibility: string;
  index_status: string;
  origin?: string;
  stream?: boolean;
}

export interface JobStats {
  total: number;
  succeeded: number;
  failed: number;
  queued: number;
  running: number;
  /** Creation time of the longest-waiting queued job; absent when none wait. */
  oldest_queued_at?: number;
  /** Variants across successful jobs only. */
  variants: number;
  last_24h: number;
  last_7d: number;
}

export interface StorageUsage {
  storage_id: string;
  name: string;
  kind: string;
  uri: string;
  /** Set for S3 locations, so usage reads per bucket. */
  bucket?: string;
  bytes: number;
  files: number;
  sources: number;
  is_default: boolean;
}

export interface RemoteUsage {
  source_id: string;
  name: string;
  host: string;
  files: number;
  bytes: number;
  /** Files whose origin reported no length, making bytes a floor. */
  unmeasured?: number;
}

export interface Metrics {
  jobs: JobStats;
  sources: {
    total: number;
    provisioned: number;
    streamed: number;
    builtin: number;
    pending: number;
  };
  storage: StorageUsage[];
  /** Only what this deployment stores; remote bytes are counted separately. */
  storage_bytes: number;
  remote: RemoteUsage[];
  remote_bytes: number;
  remote_measured: boolean;
  generated_at: number;
}

export interface Registry {
  id: string;
  name: string;
  url: string;
  builtin: boolean;
}

export interface RegistryEntry {
  name: string;
  version?: string;
  title?: string;
  assembly?: string;
  file: string;
  description?: string;
  non_commercial?: boolean;
  latest?: boolean;
}

export interface Annotation {
  name: string;
  field?: string;
  type?: string;
  description?: string;
  builtin?: string;
  source?: string;
  source_ref?: string;
  default?: boolean;
}

export interface StorageLocation {
  id: string;
  name: string;
  kind: "path" | "s3";
  uri: string;
  from_config: boolean;
  is_default: boolean;
  usable: boolean;
  unusable_reason?: string;
}

export interface SourceFile {
  source_id: string;
  storage_id: string;
  path: string;
  size_bytes: number;
  modified_at: number;
}

export interface Job {
  job_id: string;
  kind: string;
  snapshot: string;
  selection?: string;
  status: "queued" | "running" | "done" | "error";
  error?: string;
  n_variants: number;
  label?: string;
  created_at: number;
  started_at?: number;
  finished_at?: number;
  results?: Variant[];
}

export interface Column {
  key: string;
  label: string;
  description?: string;
  type?: string;
  source?: string;
  source_ref?: string;
  default: boolean;
}

export interface Variant {
  chrom: string;
  pos: number;
  ref: string;
  alt: string;
  annotations: Record<string, unknown>;
}

export interface ResultPage {
  columns: Column[] | null;
  rows: Variant[];
  total: number;
  limit: number;
  offset: number;
}

/** Thrown for any non-2xx response, carrying the server's message. */
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

// The API base is per-deployment, never hardcoded: same-origin by default, or
// VITE_API_BASE when the dev server runs separately from the API.
const BASE = (import.meta.env.VITE_API_BASE ?? "").replace(/\/$/, "");

// The token is held in sessionStorage rather than localStorage so it does not
// outlive the browser session. Auth is a single shared token until per-user
// tokens land; see docs/api.md.
const TOKEN_KEY = "vh_token";

export function getToken(): string {
  return sessionStorage.getItem(TOKEN_KEY) ?? "";
}

export function setToken(t: string) {
  if (t) sessionStorage.setItem(TOKEN_KEY, t);
  else sessionStorage.removeItem(TOKEN_KEY);
}

function headers(extra: Record<string, string> = {}): Record<string, string> {
  const h: Record<string, string> = { ...extra };
  const t = getToken();
  if (t) h.Authorization = `Bearer ${t}`;
  return h;
}

async function req<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(`${BASE}/api/v1${path}`, {
    ...init,
    headers: headers(init.headers as Record<string, string>),
  });
  if (!res.ok) {
    // The server sends {"error": "..."} and promises it is safe to display.
    let msg = `${res.status} ${res.statusText}`;
    try {
      const body = await res.json();
      if (body?.error) msg = body.error;
      if (body?.detail) msg += `: ${body.detail}`;
    } catch {
      /* non-JSON error body; keep the status line */
    }
    throw new ApiError(res.status, msg);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

export const api = {
  // includeDrafts is for the admin view only: the annotation flow must not offer
  // a snapshot that can still be re-pinned.
  snapshots: (includeDrafts = false) =>
    req<{ snapshots: Snapshot[] }>(`/snapshots${includeDrafts ? "?state=all" : ""}`),
  snapshot: (id: string) =>
    req<{ snapshot: Snapshot; contains_private: boolean; annotations: Annotation[] }>(
      `/snapshots/${encodeURIComponent(id)}`,
    ),
  sources: () => req<{ sources: (Source & { annotations: Annotation[] })[] }>("/sources"),

  annotate: (body: {
    snapshot?: string;
    sources?: string[];
    build?: string;
    variants: string[];
    annotations?: string | string[];
    wait?: number;
  }) => {
    const { wait, ...rest } = body;
    const qs = wait ? `?wait=${wait}` : "";
    return req<{ job_id: string } & Partial<Job>>(`/annotate${qs}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(rest),
    });
  },

  annotateVCF: (
    file: File,
    opts: { snapshot?: string; sources?: string[]; build?: string; annotations?: string },
  ) => {
    const fd = new FormData();
    fd.append("vcf", file);
    if (opts.snapshot) fd.append("snapshot", opts.snapshot);
    if (opts.sources?.length) fd.append("sources", opts.sources.join(","));
    if (opts.build) fd.append("build", opts.build);
    if (opts.annotations) fd.append("annotations", opts.annotations);
    return req<{ job_id: string } & Partial<Job>>("/annotate/vcf", {
      method: "POST",
      body: fd,
    });
  },

  // kind defaults to annotation jobs; the admin job log asks for "download".
  jobs: (
    params: {
      status?: string;
      kind?: "annotation" | "download" | "all";
      limit?: number;
      offset?: number;
    } = {},
  ) => {
    const q = new URLSearchParams();
    if (params.status) q.set("status", params.status);
    if (params.kind) q.set("kind", params.kind);
    if (params.limit) q.set("limit", String(params.limit));
    if (params.offset) q.set("offset", String(params.offset));
    const qs = q.toString();
    return req<{ jobs: Job[]; total?: number; scoped?: boolean }>(
      `/jobs${qs ? `?${qs}` : ""}`,
    );
  },

  job: (id: string) => req<Job>(`/jobs/${encodeURIComponent(id)}`),

  results: (
    id: string,
    p: { page?: number; per_page?: number; sort?: string; order?: string; q?: string } = {},
  ) => {
    const q = new URLSearchParams();
    if (p.page) q.set("page", String(p.page));
    if (p.per_page) q.set("per_page", String(p.per_page));
    if (p.sort) q.set("sort", p.sort);
    if (p.order) q.set("order", p.order);
    if (p.q) q.set("q", p.q);
    const qs = q.toString();
    return req<ResultPage>(`/jobs/${encodeURIComponent(id)}/results${qs ? `?${qs}` : ""}`);
  },

  /**
   * Downloads a job's full export, honoring the active filter and sort.
   *
   * Fetched with the Authorization header and saved as a blob rather than
   * navigating to the URL. A plain navigation cannot carry a header, which would
   * force the token into the query string — where it lands in browser history and
   * every intermediary's access log. Buffering the body in the tab is the cost;
   * at the scale the design targets (thousands of rows) that is a few MB.
   */
  downloadExport: async (
    id: string,
    format: "json" | "tsv" | "csv",
    p: { sort?: string; order?: string; q?: string } = {},
  ) => {
    const q = new URLSearchParams({ format });
    if (p.sort) q.set("sort", p.sort);
    if (p.order) q.set("order", p.order);
    if (p.q) q.set("q", p.q);

    const res = await fetch(
      `${BASE}/api/v1/jobs/${encodeURIComponent(id)}/export?${q}`,
      { headers: headers() },
    );
    if (!res.ok) {
      let msg = `${res.status} ${res.statusText}`;
      try {
        const body = await res.json();
        if (body?.error) msg = body.error;
      } catch {
        /* keep the status line */
      }
      throw new ApiError(res.status, msg);
    }
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `variants-${id.slice(0, 8)}.${format}`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  },

  // --- admin ---
  //
  // Any valid token can administer today: there are no roles yet. See
  // internal/api/admin.go — a registered manifest is executed by varhub, so the
  // token is effectively an administrative credential.

  validateSource: (toml: string) =>
    req<{ valid: boolean; error?: string; id?: string; name?: string; version?: string; kind?: string }>(
      "/admin/sources/validate",
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ toml }),
      },
    ),

  createSource: (body: {
    toml: string;
    id?: string;
    title?: string;
    detail?: string;
    visibility?: "public" | "private";
    origin?: string;
  }) =>
    req<{ id: string; ref: string; kind: string; visibility: string }>("/admin/sources", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),

  createSnapshot: (body: {
    id: string;
    title?: string;
    description?: string;
    build: string;
    defaults?: string[];
    tags?: string[];
    sources: string[];
    publish?: boolean;
  }) =>
    req<Snapshot>("/admin/snapshots", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),

  updateSnapshotMeta: (
    id: string,
    body: { title?: string; description?: string; defaults?: string[]; tags?: string[] },
  ) =>
    req<Snapshot>(`/admin/snapshots/${encodeURIComponent(id)}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),

  sourceConfig: (id: string) =>
    req<{ id: string; ref: string; format: string; config: string }>(
      `/admin/sources/${encodeURIComponent(id)}/config`,
    ),

  setSnapshotSources: (id: string, sources: string[]) =>
    req<{ snapshot: Snapshot }>(`/admin/snapshots/${encodeURIComponent(id)}/sources`, {
      method: "PUT",
      body: JSON.stringify({ sources }),
    }),

  deleteSource: (id: string) =>
    req<{ id: string; ref: string; cleanup_jobs: string[] }>(
      `/admin/sources/${encodeURIComponent(id)}`,
      { method: "DELETE" },
    ),

  deleteSnapshot: (id: string) =>
    req<void>(`/admin/snapshots/${encodeURIComponent(id)}`, { method: "DELETE" }),

  publishSnapshot: (id: string) =>
    req<{ id: string; state: string }>(`/admin/snapshots/${encodeURIComponent(id)}/publish`, {
      method: "POST",
    }),

  metrics: () => req<Metrics>("/admin/metrics"),

  storage: () => req<{ storage: StorageLocation[] }>("/admin/storage"),

  addStorage: (body: { name: string; kind: "s3"; uri: string }) =>
    req<{ id: string }>("/admin/storage", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),

  deleteStorage: (id: string) =>
    req<void>(`/admin/storage/${encodeURIComponent(id)}`, { method: "DELETE" }),

  files: (p: { source?: string; storage?: string } = {}) => {
    const q = new URLSearchParams();
    if (p.source) q.set("source", p.source);
    if (p.storage) q.set("storage", p.storage);
    const qs = q.toString();
    return req<{ files: SourceFile[]; total_bytes: number; count: number }>(
      `/admin/files${qs ? `?${qs}` : ""}`,
    );
  },

  // Provisioning is per source: a source is the unit of data, and a newly
  // registered one must be downloadable before anyone bundles it in a snapshot.
  download: (body: {
    sources: string[];
    storage_id?: string;
    force?: boolean;
    include_streamed?: boolean;
  }) =>
    req<{ job_id: string; sources: string[]; storage: StorageLocation }>("/admin/downloads", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),

  registries: () => req<{ registries: Registry[] }>("/admin/registries"),

  addRegistry: (body: { name: string; url: string; id?: string }) =>
    req<{ id: string; name: string; url: string; warning?: string }>("/admin/registries", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),

  deleteRegistry: (id: string) =>
    req<void>(`/admin/registries/${encodeURIComponent(id)}`, { method: "DELETE" }),

  registryDatasets: (id: string) =>
    req<{ registry: Registry; sources: RegistryEntry[]; snapshots: RegistryEntry[] }>(
      `/admin/registries/${encodeURIComponent(id)}/datasets`,
    ),

  // Returns the entry's manifest for review. Not a one-click import: the
  // fragment is executed by varhub, so it goes into the editor to be read first.
  registryFetch: (id: string, ref: string) =>
    req<{ ref: string; entry: RegistryEntry; toml: string; origin: string }>(
      `/admin/registries/${encodeURIComponent(id)}/fetch?ref=${encodeURIComponent(ref)}`,
    ),

  health: () => fetch(`${BASE}/healthz`).then((r) => r.json()),
  version: () => fetch(`${BASE}/version`).then((r) => r.json()),
};

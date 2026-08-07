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
  /** True when a pinned source is read from its origin rather than our storage. */
  contains_remote?: boolean;
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
  /** Set on the reference genome ad-hoc snapshots pin for this assembly. */
  is_default_reference?: boolean;
  origin?: string;
  stream?: boolean;
  /** Whether the source can be annotated with yet, and what is happening to it. */
  state?: {
    state?: "installing" | "ready" | "failed";
    error?: string;
    updated_at?: number;
    /** The download job working on it, when one is. */
    job?: string;
  };
}

export interface JobStats {
  total: number;
  succeeded: number;
  failed: number;
  queued: number;
  running: number;
  /** Creation time of the longest-waiting queued job; absent when none wait. */
  oldest_queued_at?: number;
  /** Stopped on purpose — counted apart from failed. */
  cancelled: number;
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

export interface User {
  id: string;
  email: string;
  name?: string;
  role: string;
  disabled?: boolean;
  /** No password is stored here — the account signs in through an identity provider. */
  sso?: boolean;
  created_at: number;
  updated_at: number;
}

export interface Me {
  anonymous: boolean;
  admin: boolean;
  label: string;
  bootstrap: boolean;
  teams?: string[];
  user?: User;
  /** True while the installation has no administrator and the bootstrap token works. */
  needs_bootstrap?: boolean;
  /** False for an SSO account, which has no password here to change. */
  can_change_password?: boolean;
  /** True when institutional (CILogon) sign-in is configured. */
  sso_enabled?: boolean;
  /** True when the server lets callers with no account annotate. */
  allow_anonymous?: boolean;
}

export interface ExternalIdentity {
  provider: string;
  subject: string;
  email?: string;
  created_at: number;
  last_seen_at?: number;
}

export interface ApiToken {
  id: string;
  name?: string;
  prefix: string;
  created_at: number;
  last_used_at?: number;
  revoked_at?: number;
}

export interface Team {
  id: string;
  name: string;
  created_at: number;
  members?: { user: User; role: string }[];
}

export interface Registry {
  id: string;
  name: string;
  url: string;
  builtin: boolean;
}

/** A helper file a build recipe or tool step needs, shipped with the source. */
/** A genome assembly this installation offers. */
export interface Build {
  /** The assembly string itself, matched exactly against a source's build. */
  name: string;
  label?: string;
  description?: string;
  sort_order: number;
  /** How many sources are registered against it. */
  sources: number;
  created_at: number;
  updated_at: number;
}

export interface SourceAsset {
  name: string;
  content: string;
}

/** What this deployment decided about a source, as opposed to what its manifest says. */
export interface SourceSettings {
  /** Renames the source's output fields. "-" means no prefix at all. */
  annotation_prefix?: string;
  /** Publish a tool's setup output so another machine can restore it. */
  cache_setup?: boolean;
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
  status: "queued" | "running" | "done" | "error" | "cancelled";
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

// A bearer token is held in sessionStorage rather than localStorage so it does
// not outlive the browser tab. It is the *secondary* path: signing in with a
// password sets an HttpOnly cookie the browser sends automatically, which is
// what a session should be. This exists for the bootstrap credential and for
// pasting a personal token, neither of which has a cookie.
const TOKEN_KEY = "vh_token";

export function getToken(): string {
  return sessionStorage.getItem(TOKEN_KEY) ?? "";
}

export function setToken(t: string) {
  if (t) sessionStorage.setItem(TOKEN_KEY, t);
  else sessionStorage.removeItem(TOKEN_KEY);
}

// A per-browser id scoping an anonymous visitor's own job history.
//
// This is not a credential and grants nothing: the server treats it as a
// self-asserted history scope, and a job that has a real account behind it
// ignores it entirely. It exists because without one an anonymous caller can
// submit a job and then get a 404 reading it back — there would be nothing to
// scope the result to, and returning everyone's jobs instead is not an option.
//
// localStorage rather than sessionStorage so a reload or a new tab still finds
// yesterday's results, which is the whole point of a history.
const SESSION_KEY = "vh_history";

function historyID(): string {
  let id = localStorage.getItem(SESSION_KEY);
  if (!id) {
    id = randomID();
    localStorage.setItem(SESSION_KEY, id);
  }
  return id;
}

/**
 * A random identifier that works outside a secure context.
 *
 * Deliberately not crypto.randomUUID(): that is secure-context-only, so it is
 * undefined over plain http on anything but localhost — and because this runs
 * inside headers(), a throw there fails *every* request, including the one the
 * app uses to find out who you are. The symptom is a login page on
 * http://host:18080 and none through an ssh tunnel to localhost, which looks
 * like a server problem and is not.
 *
 * getRandomValues has no such restriction. Math.random is the last resort: this
 * id scopes a history and is not a credential, so uniqueness is the only
 * requirement, and a browser too old for getRandomValues should still work.
 */
function randomID(): string {
  const bytes = new Uint8Array(16);
  if (globalThis.crypto?.getRandomValues) {
    globalThis.crypto.getRandomValues(bytes);
  } else {
    for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256);
  }
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

function headers(extra: Record<string, string> = {}): Record<string, string> {
  const h: Record<string, string> = { ...extra };
  const t = getToken();
  if (t) h.Authorization = `Bearer ${t}`;
  h["X-Varhub-Session"] = historyID();
  return h;
}

async function req<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(`${BASE}/api/v1${path}`, {
    ...init,
    // Send the session cookie. Without this a cross-origin dev server would
    // authenticate every request as anonymous with no visible reason why.
    credentials: "include",
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
    req<{
      snapshot: Snapshot;
      contains_private: boolean;
      contains_remote: boolean;
      annotations: Annotation[];
    }>(
      `/snapshots/${encodeURIComponent(id)}`,
    ),
  sources: () => req<{ sources: (Source & { annotations: Annotation[] })[] }>("/sources"),
  builds: () => req<{ builds: Build[] }>("/builds"),

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
    req<{
      valid: boolean;
      error?: string;
      id?: string;
      name?: string;
      version?: string;
      kind?: string;
      /** True when the source has files to fetch before it can be annotated with. */
      needs_data?: boolean;
      /** True when it is read from its origin instead. */
      stream?: boolean;
    }>(
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
    assets?: SourceAsset[];
    settings?: SourceSettings;
  }) =>
    req<{
      id: string;
      ref: string;
      kind: string;
      visibility: string;
      assets?: number;
      /** Files the manifest names that nobody supplied. */
      missing_assets?: string[] | null;
    }>("/admin/sources", {
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

  me: () => req<Me>("/auth/me"),

  login: (email: string, password: string) =>
    req<{ user: User }>("/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ email, password }),
    }),

  logout: () => req<void>("/auth/logout", { method: "POST" }),

  changePassword: (current: string, next: string) =>
    req<void>("/auth/password", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ current_password: current, new_password: next }),
    }),

  identities: () => req<{ identities: ExternalIdentity[] }>("/auth/identities"),

  tokens: () => req<{ tokens: ApiToken[] }>("/auth/tokens"),

  createToken: (name: string) =>
    req<{ token: ApiToken; secret: string }>("/auth/tokens", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    }),

  revokeToken: (id: string) =>
    req<void>(`/auth/tokens/${encodeURIComponent(id)}`, { method: "DELETE" }),

  users: () => req<{ users: User[] }>("/admin/users"),

  createUser: (body: { email: string; name?: string; role: string; password: string }) =>
    req<{ user: User }>("/admin/users", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),

  updateUser: (id: string, body: { role?: string; disabled?: boolean; password?: string }) =>
    req<{ user: User }>(`/admin/users/${encodeURIComponent(id)}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),

  teams: () => req<{ teams: Team[] }>("/admin/teams"),

  createTeam: (name: string) =>
    req<{ team: Team }>("/admin/teams", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    }),

  deleteTeam: (id: string) =>
    req<void>(`/admin/teams/${encodeURIComponent(id)}`, { method: "DELETE" }),

  addMember: (teamId: string, userId: string, role = "member") =>
    req<void>(`/admin/teams/${encodeURIComponent(teamId)}/members`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user_id: userId, role }),
    }),

  removeMember: (teamId: string, userId: string) =>
    req<void>(
      `/admin/teams/${encodeURIComponent(teamId)}/members/${encodeURIComponent(userId)}`,
      { method: "DELETE" },
    ),

  grants: (sourceId: string) =>
    req<{ teams: Team[] }>(`/admin/sources/${encodeURIComponent(sourceId)}/grants`),

  grant: (sourceId: string, teamId: string) =>
    req<void>(`/admin/sources/${encodeURIComponent(sourceId)}/grants`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ team_id: teamId }),
    }),

  revokeGrant: (sourceId: string, teamId: string) =>
    req<void>(
      `/admin/sources/${encodeURIComponent(sourceId)}/grants/${encodeURIComponent(teamId)}`,
      { method: "DELETE" },
    ),

  cancelJob: (id: string) =>
    req<{ job: Job; cancelled: boolean; detail?: string }>(
      `/jobs/${encodeURIComponent(id)}/cancel`,
      { method: "POST" },
    ),

  jobLog: (id: string) =>
    req<{ job_id: string; output: string; recorded: boolean }>(
      `/jobs/${encodeURIComponent(id)}/log`,
    ),

  sourceSettings: (id: string) =>
    req<{ settings: SourceSettings; manifest_prefix: string; is_tool: boolean }>(
      `/admin/sources/${encodeURIComponent(id)}/settings`,
    ),

  setSourceSettings: (id: string, body: SourceSettings) =>
    req<{ settings: SourceSettings }>(`/admin/sources/${encodeURIComponent(id)}/settings`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),

  metrics: () => req<Metrics>("/admin/metrics"),

  storage: () => req<{ storage: StorageLocation[] }>("/admin/storage"),

  moveSource: (id: string, storageID: string) =>
    req<{ job_id: string; from: string; to: string }>(
      `/admin/sources/${encodeURIComponent(id)}/move`,
      { method: "POST", body: JSON.stringify({ storage_id: storageID }) },
    ),

  setDefaultReference: (id: string) =>
    req<void>(`/admin/sources/${encodeURIComponent(id)}/default-reference`, {
      method: "POST",
    }),


  addStorage: (body: { name: string; kind: "s3"; uri: string }) =>
    req<{ id: string }>("/admin/storage", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }),

  deleteStorage: (id: string) =>
    req<void>(`/admin/storage/${encodeURIComponent(id)}`, { method: "DELETE" }),

  putBuild: (b: { name: string; label?: string; description?: string; sort_order?: number }) =>
    req<{ name: string }>("/admin/builds", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(b),
    }),

  // Refused with 409 while a source or snapshot still declares it, rather than
  // cascading: those keep their assembly strings and keep working, so removing
  // the build would only stop it being offered.
  deleteBuild: (name: string) =>
    req<void>(`/admin/builds/${encodeURIComponent(name)}`, { method: "DELETE" }),

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
    req<{
      ref: string;
      entry: RegistryEntry;
      toml: string;
      origin: string;
      assets?: SourceAsset[];
      /** Set when the manifest arrived but its helper files could not be fetched. */
      asset_error?: string;
    }>(
      `/admin/registries/${encodeURIComponent(id)}/fetch?ref=${encodeURIComponent(ref)}`,
    ),

  health: () => fetch(`${BASE}/healthz`).then((r) => r.json()),
  version: () => fetch(`${BASE}/version`).then((r) => r.json()),
};

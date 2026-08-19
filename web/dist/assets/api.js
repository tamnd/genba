// The API client.
//
// Every request carries the caller's identity headers, because the server has
// no session of its own: it authenticates each request and applies the
// principal inside the storage driver. That is what makes the identity switcher
// in the rail a real test of the permission model rather than a cosmetic one.

const BASE = "/api/v1";

/**
 * identity is who the browser says it is.
 *
 * This is a development identity, held in localStorage, and the server only
 * trusts it because the header authenticator is configured. A deployment puts
 * a proxy in front that authenticates properly and strips these headers, which
 * is written down in the authenticator's own documentation.
 */
const STORAGE_KEY = "genba.identity";

const DEFAULT_IDENTITY = {
  subject: "dev",
  tenant: "",
  groups: [],
  identities: [],
};

export function identity() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return { ...DEFAULT_IDENTITY };
    return { ...DEFAULT_IDENTITY, ...JSON.parse(raw) };
  } catch {
    return { ...DEFAULT_IDENTITY };
  }
}

export function setIdentity(next) {
  localStorage.setItem(STORAGE_KEY, JSON.stringify({ ...identity(), ...next }));
}

function headers() {
  const who = identity();
  const out = { "X-Genba-Subject": who.subject };
  if (who.tenant) out["X-Genba-Tenant"] = who.tenant;
  if (who.groups.length) out["X-Genba-Groups"] = who.groups.join(",");
  if (who.identities.length) out["X-Genba-Identities"] = who.identities.join(",");
  return out;
}

/** ApiError carries the server's own error code, so a caller can branch on it. */
export class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

async function get(path, params, signal) {
  const url = new URL(BASE + path, location.origin);
  for (const [key, value] of Object.entries(params || {})) {
    if (value === undefined || value === null || value === "") continue;
    for (const v of Array.isArray(value) ? value : [value]) {
      if (v !== "" && v !== undefined && v !== null) url.searchParams.append(key, v);
    }
  }

  const res = await fetch(url, { headers: headers(), signal });
  if (!res.ok) {
    let code = "error";
    let message = `the server returned ${res.status}`;
    try {
      const body = await res.json();
      if (body && body.error) {
        code = body.error.code || code;
        message = body.error.message || message;
      }
    } catch {
      // A response that is not JSON is still a failure, and the status line
      // above is a better message than a parse error.
    }
    throw new ApiError(res.status, code, message);
  }
  return res.json();
}

export const api = {
  me: (signal) => get("/me", {}, signal),
  stats: (signal) => get("/stats", {}, signal),
  search: (query, signal) => get("/search", query, signal),
  suggest: (q, signal) => get("/suggest", { q }, signal),
  document: (id, signal) => get(`/documents/${encodeURIComponent(id)}`, {}, signal),
  health: (signal) => fetch("/healthz", { signal }).then((r) => r.json()),
};

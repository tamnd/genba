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

/**
 * get reads one JSON endpoint and reports whether the answer moved.
 *
 * Every read returns an envelope rather than the body, because every one of
 * these responses is revalidated rather than refetched. Passing back the entity
 * tag lets the next call ask whether what the client holds is still current,
 * and a server that has not changed its mind answers that in a few hundred
 * bytes with no body at all, which is the whole point of cache.js.
 */
async function get(path, params, opts = {}) {
  const url = new URL(BASE + path, location.origin);
  for (const [key, value] of Object.entries(params || {})) {
    if (value === undefined || value === null || value === "") continue;
    for (const v of Array.isArray(value) ? value : [value]) {
      if (v !== "" && v !== undefined && v !== null) url.searchParams.append(key, v);
    }
  }

  const sent = headers();
  if (opts.etag) sent["If-None-Match"] = opts.etag;
  const res = await fetch(url, { headers: sent, signal: opts.signal });
  if (res.status === 304) return { modified: false, etag: opts.etag };
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
  return { modified: true, etag: res.headers.get("ETag") || "", data: await res.json() };
}

/**
 * events reads the index change stream.
 *
 * EventSource would be the obvious way to do this and cannot be used, because
 * it sends no headers of its own and every request here carries the caller's
 * identity in one. So the stream is read from a normal fetch, which means the
 * frame parsing is ours: frames are separated by a blank line, a line starting
 * with a colon is a comment and is how the server keeps the connection alive,
 * and anything we cannot parse is ignored rather than thrown, because a
 * malformed frame is not a reason to tear down a working stream.
 *
 * It resolves when the server closes the stream, so the caller reconnects.
 */
async function events(onEvent, signal) {
  const res = await fetch(BASE + "/events", { headers: headers(), signal });
  if (!res.ok || !res.body) {
    throw new ApiError(res.status, "error", `the event stream returned ${res.status}`);
  }
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    buffer += decoder.decode(value, { stream: true });
    for (let cut = buffer.indexOf("\n\n"); cut >= 0; cut = buffer.indexOf("\n\n")) {
      const frame = buffer.slice(0, cut);
      buffer = buffer.slice(cut + 2);
      const data = frame
        .split("\n")
        .filter((line) => line.startsWith("data:"))
        .map((line) => line.slice(5).trim())
        .join("");
      if (!data) continue;
      try {
        onEvent(JSON.parse(data));
      } catch {
        // A frame this client does not understand is a frame it does not need.
      }
    }
  }
}

/**
 * bytes fetches a document's raw content.
 *
 * An img tag cannot carry the identity headers, so the bytes come through fetch
 * and become an object URL. That keeps one rule for who may read what: the
 * server decides, from the same headers as every other request, and a document
 * somebody may not read is a 404 here exactly as it is everywhere else.
 */
async function bytes(path, opts = {}) {
  const res = await fetch(BASE + path, { headers: headers(), signal: opts.signal });
  if (!res.ok) throw new ApiError(res.status, "error", `the server returned ${res.status}`);
  const dims = (res.headers.get("X-Content-Dimensions") || "").split("x");
  return {
    blob: await res.blob(),
    type: res.headers.get("Content-Type") || "",
    width: Number(dims[0]) || 0,
    height: Number(dims[1]) || 0,
  };
}

// Every JSON read takes the same options, { signal, etag }, and returns the
// same envelope. The two that do not are content, which is bytes rather than a
// document, and the stream, which never finishes.
export const api = {
  me: (opts) => get("/me", {}, opts),
  stats: (opts) => get("/stats", {}, opts),
  search: (query, opts) => get("/search", query, opts),
  suggest: (q, opts) => get("/suggest", { q }, opts),
  document: (id, opts) => get(`/documents/${encodeURIComponent(id)}`, {}, opts),
  content: (id, opts) => bytes(`/documents/${encodeURIComponent(id)}/content`, opts),
  // The version goes in the URL rather than in a header, because it is what
  // makes the address of a thumbnail stand for one picture forever. The server
  // reads it only to decide how long the browser may keep the answer, so a
  // document that never said which revision it is still gets a thumbnail, it
  // just gets one the browser revalidates.
  thumbnail: (id, size, version, opts) =>
    bytes(
      `/documents/${encodeURIComponent(id)}/thumbnail?size=${encodeURIComponent(size)}` +
        (version ? `&v=${encodeURIComponent(version)}` : ""),
      opts,
    ),
  events,
  health: (signal) => fetch("/healthz", { signal }).then((r) => r.json()),
};

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

/**
 * ApiError carries the server's own error code, so a caller can branch on it.
 *
 * The request id is whatever names this request in a log, when something in
 * front of the server names it. Nothing in this program sends one and a proxy
 * in front of it may, and where there is one it is the only thread between what
 * somebody saw on screen and the line that explains it.
 */
export class ApiError extends Error {
  constructor(status, code, message, requestID = "") {
    super(message);
    this.status = status;
    this.code = code;
    this.requestID = requestID;
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
  if (!res.ok) throw await failure(res);
  return { modified: true, etag: res.headers.get("ETag") || "", data: await res.json() };
}

/**
 * failure turns a refusal into the error a screen prints.
 *
 * The server's own sentence is used wherever there is one, because it is the
 * only part of this that knows why. A connector that was configured on the
 * command line comes back from here saying where to change it, and no message
 * written in this file could have known that.
 */
async function failure(res) {
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
  return new ApiError(res.status, code, message, res.headers.get("X-Request-Id") || "");
}

/**
 * send is a write, and returns the state that resulted from it.
 *
 * These endpoints answer with the state that resulted rather than with an
 * acknowledgement, so a screen paints what is true after the change instead of
 * guessing and then asking. Nothing here is cached and nothing here is retried:
 * a retried start is a second crawler.
 */
async function send(method, path, body) {
  const sent = headers();
  if (body) sent["Content-Type"] = "application/json";
  const res = await fetch(BASE + path, {
    method,
    headers: sent,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) throw await failure(res);
  // A write that removes something has nothing to say back, and reading a body
  // that is not there is a parse error on a request that worked.
  if (res.status === 204) return null;
  return res.json();
}

/** verifyPath is one document's verification, which is written and withdrawn. */
function verifyPath(id) {
  return `/documents/${encodeURIComponent(id)}/verify`;
}

/** ownerPath is one document's owner, which is corrected and put back. */
function ownerPath(id) {
  return `/documents/${encodeURIComponent(id)}/owner`;
}

/** stalePath is what has been said about one document, which is written and cleared. */
function stalePath(id) {
  return `/documents/${encodeURIComponent(id)}/stale`;
}

/** answerPath is one written answer, which is saved and taken down. */
function answerPath(id) {
  return `/admin/answers/${encodeURIComponent(id)}`;
}

/** connector is one source's path under the administration endpoints. */
function connector(source, action = "") {
  return `/admin/connectors/${encodeURIComponent(source)}${action}`;
}

/**
 * recordOpen tells the server that this document was read.
 *
 * keepalive, because this fires on the way to somewhere else and a plain fetch
 * is cancelled when the page that started it goes away, which is exactly the
 * navigation being recorded. Nothing waits for the answer and nothing reports a
 * failure: a lost entry in a recency list is not worth interrupting a read for,
 * and the read itself has already happened by the time this is called.
 */
function recordOpen(id) {
  if (!id) return;
  fetch(BASE + "/recent", {
    method: "POST",
    headers: { ...headers(), "Content-Type": "application/json" },
    body: JSON.stringify({ id }),
    keepalive: true,
  }).catch(() => {});
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
  if (!res.ok) {
    throw new ApiError(res.status, "error", `the server returned ${res.status}`, res.headers.get("X-Request-Id") || "");
  }
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
  // One request for the whole administration screen, because every number on
  // it is a snapshot of the same moment and three requests would let the count
  // of held documents disagree with the list under it.
  admin: (opts) => get("/admin/operations", {}, opts),
  // What one named person can see. The counts are asked for rather than always
  // returned, because they are an aggregate over the whole tenant and the check
  // beside them is two indexed reads, so a screen that wants the check does not
  // wait on the aggregate.
  access: (question, opts) => get("/admin/access", question, opts),
  // The five writes on the administration screen. The tenant is not sent with
  // any of them: the server uses the operator's own, so a form cannot be used
  // to reach into somebody else's corpus.
  addConnector: (c) => send("POST", "/admin/connectors", c),
  dropConnector: (source) => send("DELETE", connector(source)),
  startConnector: (source) => send("POST", connector(source, "/start")),
  stopConnector: (source) => send("POST", connector(source, "/stop")),
  syncConnector: (source) => send("POST", connector(source, "/sync")),
  // The written answers, and the two writes that maintain them. Saving one is
  // the same call whether it is new or not, and it is also how an answer is
  // confirmed: the date under it is the date somebody last stood behind the
  // words, and there is no separate act that produces one.
  answers: (opts) => get("/admin/answers", {}, opts),
  curate: (id, answer) => send("PUT", answerPath(id), answer),
  retract: (id) => send("DELETE", answerPath(id)),
  // Titles for a handful of ids, for the one screen that holds ids and has to
  // print something a person recognises. It resolves through the caller, so an
  // id this person cannot read is simply not in the answer, and what comes back
  // is for display only: the editor saves the ids it was given, because saving
  // what came back would drop every source it happens not to have access to.
  documents: (ids, opts) => get("/documents", { id: ids }, opts),
  search: (query, opts) => get("/search", query, opts),
  // One request for both halves of the recent screen, because the screen asks
  // both questions at once and a screen that paints in two stages paints twice.
  recent: (limit, opts) => get("/recent", { limit }, opts),
  recordOpen,
  suggest: (q, opts) => get("/suggest", { q }, opts),
  document: (id, opts) => get(`/documents/${encodeURIComponent(id)}`, {}, opts),
  // Putting a name to a document and taking it off again. Neither sends who is
  // making the claim: the server takes that from the same headers it
  // authenticates with, so a request cannot put somebody else's name on a
  // badge. The note and the expiry are optional and the interface sends
  // neither, which is what makes the common case one click.
  verify: (id, claim) => send("POST", verifyPath(id), claim),
  unverify: (id) => send("DELETE", verifyPath(id)),
  // Correcting who owns a document, and undoing that. Both answer with the
  // owner that stands afterwards, including the undo, because what a connector
  // says is on the server and asking again would be a second round trip to find
  // out what the first one just did.
  setOwner: (id, who) => send("PUT", ownerPath(id), who),
  clearOwner: (id) => send("DELETE", ownerPath(id)),
  // Saying a document is out of date, and saying that has been dealt with. The
  // first takes no permission but being able to read the document and the
  // second is held to the same rule as verifying, which is why they are two
  // calls rather than one toggle. Reporting answers with the count, because how
  // many other people had already said the same thing is on the server.
  report: (id, said) => send("POST", stalePath(id), said),
  resolve: (id) => send("DELETE", stalePath(id)),
  // The owner's side of the same feature: the documents this person owns or
  // wrote that somebody has complained about. A deployment whose driver cannot
  // remember a report answers with an empty list rather than a refusal, so the
  // panel this feeds simply does not draw.
  reported: (limit, opts) => get("/reported", { limit }, opts),
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

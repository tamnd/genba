// Query state lives in the URL.
//
// Every filter, the sort, the page and the open document are in the address
// bar, so a search can be linked, bookmarked and reloaded, and the back button
// does what a back button should. There is no second copy of this state in
// memory that could disagree with the address bar.

const LIST_KEYS = ["source", "kind", "container", "author", "owner"];

// The five screens that are a path rather than a query string.
//
// A document is the thing somebody pastes into a message, so it gets an address
// that survives being read out loud and does not carry the state of whoever
// found it. The words they searched for are the one exception, and the reason
// is written on documentPath below. Recent is a path because it is not a view
// of a search: it answers what has been going on rather than what matches, and
// a rail entry that ran a recency sorted search was answering the wrong
// question with the right rows. Settings is a path because none of the query
// state means anything on it. Administration is a path for the same reason and
// for one more: it is not about the corpus at all, it is about the process, and
// carrying a search filter onto it would be carrying it onto a screen where it
// means nothing.
const DOCUMENT = "/d/";
export const RECENT = "/recent";
export const SETTINGS = "/settings";
export const ADMIN = "/admin";

// The answers screen is a path for the same two reasons administration is, and
// it is a path of its own rather than a tab on that screen because what it
// maintains is part of the corpus rather than part of the process.
export const ANSWERS = "/answers";

/**
 * route says which screen the path names.
 *
 * The server serves the shell for any path it does not recognise, so a reload
 * of a document page reaches this function rather than a 404.
 */
export function route(pathname = location.pathname) {
  if (pathname === RECENT) return { name: "recent", id: "" };
  if (pathname === SETTINGS) return { name: "settings", id: "" };
  if (pathname === ADMIN) return { name: "admin", id: "" };
  if (pathname === ANSWERS) return { name: "answers", id: "" };
  if (!pathname.startsWith(DOCUMENT)) return { name: "search", id: "" };
  const id = decode(pathname.slice(DOCUMENT.length));
  return id ? { name: "document", id } : { name: "search", id: "" };
}

/**
 * documentPath is the address of one document as a page of its own.
 *
 * The words somebody searched for are the one piece of the state around a
 * result that belongs on this address, and every link on a result row carries
 * them. They are not how that person found the document, which is what this
 * address deliberately leaves out: no filters, no sort, no page and no cursor.
 * They are where in the document to start reading, and a middle click on a
 * result has to land in the same place a plain click does.
 *
 * Nothing that offers this address to be copied passes them, because a link
 * somebody sends is about the document rather than about the search behind it.
 */
export function documentPath(id, q = "") {
  const path = DOCUMENT + encodeURIComponent(id);
  return q ? `${path}?q=${encodeURIComponent(q)}` : path;
}

// A path somebody typed or a link somebody mangled can hold a percent sign that
// is not an escape, and that is a request for a document we do not have rather
// than an exception on the way to the first paint.
function decode(value) {
  try {
    return decodeURIComponent(value);
  } catch {
    return "";
  }
}

/** read parses the current location into a query. */
export function read(search = location.search) {
  const params = new URLSearchParams(search);
  const query = {
    q: params.get("q") || "",
    sort: params.get("sort") || "",
    offset: Number(params.get("offset") || 0) || 0,
    limit: Number(params.get("limit") || 20) || 20,
    tab: params.get("tab") || "all",
    open: params.get("open") || "",
    // The passage inside the open document to land on, which is what a citation
    // under an answer carries. It is the quoted text rather than an offset,
    // because an offset into a body is a promise the renderer breaks as soon as
    // it takes the markup out, and because a link somebody pastes into a message
    // should still work when the document has had a paragraph added to the top.
    at: params.get("at") || "",
    // Empty means the view has not been chosen, which is different from having
    // chosen the list. A page of nothing but images opens as a grid, and that
    // has to stop happening the moment somebody says they want the list, so the
    // absence of an answer and the answer list cannot be the same value.
    view: params.get("view") === "grid" || params.get("view") === "list" ? params.get("view") : "",
    // Which row the eye is on. Minus one is no row, which is what a search
    // starts as and what anything typed into the box goes back to.
    cursor: cursorOf(params.get("cursor")),
  };
  for (const key of LIST_KEYS) query[key] = params.getAll(key).filter(Boolean);
  return query;
}

/**
 * cursorOf reads the row index out of the address bar.
 *
 * Anything that is not a whole number at or above zero is no cursor at all,
 * because this parameter is as editable as the rest of the URL and a cursor of
 * "four" or of minus six would otherwise reach the roving tabindex.
 */
function cursorOf(value) {
  if (value === null) return -1;
  const n = Number(value);
  return Number.isInteger(n) && n >= 0 ? n : -1;
}

/**
 * write turns a query back into an address, leaving out the defaults.
 *
 * The path is a parameter because two screens carry this state now. Opening a
 * preview from the recent screen has to stay on the recent screen, and a
 * function that always wrote a slash would have quietly moved somebody to the
 * search every time they pressed Enter on a row.
 */
export function write(query, path = "/") {
  const params = new URLSearchParams();
  if (query.q) params.set("q", query.q);
  for (const key of LIST_KEYS) {
    for (const value of query[key] || []) params.append(key, value);
  }
  if (query.sort) params.set("sort", query.sort);
  if (query.offset) params.set("offset", String(query.offset));
  if (query.tab && query.tab !== "all") params.set("tab", query.tab);
  if (query.view) params.set("view", query.view);
  if (query.cursor >= 0) params.set("cursor", String(query.cursor));
  if (query.open) params.set("open", query.open);
  // Only ever alongside the document it points into. A passage with nothing open
  // is a parameter naming a place in a document nobody is looking at.
  if (query.open && query.at) params.set("at", query.at);
  // Rooted rather than relative, because a search reached from a document page
  // is a search and not a document with a query string stuck on the end of it.
  const s = params.toString();
  return s ? `${path}?${s}` : path;
}

/**
 * toggle adds or removes one value of a list filter and resets paging.
 *
 * Paging resets because keeping the offset across a filter change lands
 * somebody on page four of a result set that now has two pages, which reads as
 * an empty result rather than as a filter that worked.
 */
export function toggle(query, key, value) {
  const current = query[key] || [];
  const next = current.includes(value)
    ? current.filter((v) => v !== value)
    : [...current, value];
  return { ...query, [key]: next, offset: 0 };
}

/** params turns a query into what the search endpoint expects. */
export function params(query, verticals) {
  const out = {
    q: query.q,
    source: query.source,
    container: query.container,
    author: query.author,
    owner: query.owner,
    sort: query.sort,
    offset: query.offset || undefined,
    limit: query.limit,
  };
  const vertical = verticals.find((v) => v.id === query.tab);
  // A vertical is a preset over kinds, and a kind ticked in the sidebar is the
  // same filter. Intersecting them would let the two disagree, so the vertical
  // wins while it is selected and the sidebar shows the kinds inside it.
  out.kind = vertical && vertical.kinds.length ? vertical.kinds : query.kind;
  return out;
}

/** same reports whether two queries would produce the same request. */
export function same(a, b) {
  return JSON.stringify(a) === JSON.stringify(b);
}

/** count is how many filters are active, for the badge on the filter bar. */
export function count(query) {
  return LIST_KEYS.reduce((n, key) => n + (query[key] || []).length, 0);
}

/** clear drops every filter but keeps the text. */
export function clear(query) {
  const next = { ...query, offset: 0, tab: "all" };
  for (const key of LIST_KEYS) next[key] = [];
  return next;
}

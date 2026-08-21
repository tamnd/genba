// Query state lives in the URL.
//
// Every filter, the sort, the page and the open document are in the address
// bar, so a search can be linked, bookmarked and reloaded, and the back button
// does what a back button should. There is no second copy of this state in
// memory that could disagree with the address bar.

const LIST_KEYS = ["source", "kind", "container", "author", "owner"];

// The one screen that is a path rather than a query string.
//
// A document is the thing somebody pastes into a message, so it gets an address
// that survives being read out loud and does not carry the state of whoever
// found it. Everything else is a view of a search and belongs in the query.
const DOCUMENT = "/d/";

/**
 * route says which screen the path names.
 *
 * The server serves the shell for any path it does not recognise, so a reload
 * of a document page reaches this function rather than a 404.
 */
export function route(pathname = location.pathname) {
  if (!pathname.startsWith(DOCUMENT)) return { name: "search", id: "" };
  const id = decode(pathname.slice(DOCUMENT.length));
  return id ? { name: "document", id } : { name: "search", id: "" };
}

/** documentPath is the address of one document as a page of its own. */
export function documentPath(id) {
  return DOCUMENT + encodeURIComponent(id);
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
    // Empty means the view has not been chosen, which is different from having
    // chosen the list. A page of nothing but images opens as a grid, and that
    // has to stop happening the moment somebody says they want the list, so the
    // absence of an answer and the answer list cannot be the same value.
    view: params.get("view") === "grid" || params.get("view") === "list" ? params.get("view") : "",
  };
  for (const key of LIST_KEYS) query[key] = params.getAll(key).filter(Boolean);
  return query;
}

/** write turns a query back into a query string, leaving out the defaults. */
export function write(query) {
  const params = new URLSearchParams();
  if (query.q) params.set("q", query.q);
  for (const key of LIST_KEYS) {
    for (const value of query[key] || []) params.append(key, value);
  }
  if (query.sort) params.set("sort", query.sort);
  if (query.offset) params.set("offset", String(query.offset));
  if (query.tab && query.tab !== "all") params.set("tab", query.tab);
  if (query.view) params.set("view", query.view);
  if (query.open) params.set("open", query.open);
  // Rooted rather than relative, because a search reached from a document page
  // is a search and not a document with a query string stuck on the end of it.
  const s = params.toString();
  return s ? `/?${s}` : "/";
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

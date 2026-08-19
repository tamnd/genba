// Query state lives in the URL.
//
// Every filter, the sort, the page and the open document are in the address
// bar, so a search can be linked, bookmarked and reloaded, and the back button
// does what a back button should. There is no second copy of this state in
// memory that could disagree with the address bar.

const LIST_KEYS = ["source", "kind", "container", "author", "owner"];

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
  if (query.open) params.set("open", query.open);
  const s = params.toString();
  return s ? `?${s}` : location.pathname;
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

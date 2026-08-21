// The browser's copy of what the server already answered.
//
// The server budget makes a response fast. This makes the common case need no
// response at all. Going back, changing a sort, unticking a facet, reopening a
// preview: every one of those asks for something this tab already had.
//
// Nothing here is written to disk. Search results are titles and excerpts from
// a permissioned corpus, and localStorage would leave them on the machine after
// the session that was allowed to see them ended. Memory is the right lifetime
// because it is the session's lifetime.
//
// This module is the store and the stale while revalidate flow over it.
// Deciding when to revalidate is live.js.

// TTL is how old an entry may be and still be worth painting. Beyond it the
// entry is a miss, because painting a several minute old answer while a new one
// loads is not a fast interface, it is a wrong one.
//
// STALE is the line between an answer that needs no request and one that is
// painted and then checked. Somebody arrow keying through results asks the same
// question repeatedly and none of those needs a round trip.
const TTL = 30_000;
const STALE = 5_000;
const MAX_ENTRIES = 200;

export class Store {
  constructor({ maxEntries = MAX_ENTRIES, ttl = TTL, staleAfter = STALE, now = Date.now } = {}) {
    this.max = maxEntries;
    this.ttl = ttl;
    this.staleAfter = staleAfter;
    this.now = now;
    this.view = "";
    this.entries = new Map();
    this.running = new Map();
  }

  /**
   * as names whose results these are, and empties everything when that changes.
   *
   * The value comes from the me endpoint rather than being derived here, so it
   * is the fingerprint the server keys its own caches by and the two cannot
   * drift. Prepending it is what makes the identity switcher safe: one tab
   * holds results for more than one identity over its life, and serving one
   * identity's results to another is the same bug in a browser as in a server.
   */
  as(view) {
    if (view === this.view) return;
    this.view = view;
    this.clear();
  }

  /**
   * key canonicalises a request so two spellings of one question share an entry.
   *
   * Parameter order and the order of a repeated parameter carry no meaning, so
   * both are sorted. Case is left alone: the server matches a container or an
   * author exactly, so folding it here would merge two requests whose answers
   * differ.
   */
  key(name, params) {
    const out = new URLSearchParams();
    for (const field of Object.keys(params || {}).sort()) {
      const value = params[field];
      for (const one of Array.isArray(value) ? [...value].sort() : [value]) {
        if (one === "" || one === undefined || one === null) continue;
        out.append(field, String(one));
      }
    }
    return `${this.view}|${name}|${out}`;
  }

  /**
   * read reports what is held for a key and how far it can be trusted. Three
   * states rather than two, because cached is not one condition: "miss" is
   * nothing to paint, "fresh" is paint it and send nothing, "stale" is paint it
   * and check behind it.
   */
  read(k) {
    const entry = this.entries.get(k);
    if (!entry) return { state: "miss" };
    const age = this.now() - entry.at;
    if (age > this.ttl) {
      this.entries.delete(k);
      return { state: "miss" };
    }
    // Reading is use, so the entry moves to the end and eviction takes the one
    // nobody has looked at.
    this.entries.delete(k);
    this.entries.set(k, entry);
    return {
      state: entry.stale || age > this.staleAfter ? "stale" : "fresh",
      data: entry.data,
      etag: entry.etag,
      at: entry.at,
    };
  }

  /** write records a response and evicts the least recently used if it has to. */
  write(k, data, etag = "") {
    this.entries.delete(k);
    this.entries.set(k, { data, etag, at: this.now(), stale: false });
    while (this.entries.size > this.max) {
      this.entries.delete(this.entries.keys().next().value);
    }
  }

  /** touch marks an entry current again, which is what a 304 means. */
  touch(k) {
    const entry = this.entries.get(k);
    if (!entry) return undefined;
    entry.at = this.now();
    entry.stale = false;
    return entry.data;
  }

  /**
   * once runs a fetch for a key, or joins the one already running. A prefetch
   * and the click it predicted ask for the same key seconds apart, and the
   * second should cost nothing. Same single flight as the server, same reason.
   */
  once(k, run) {
    const running = this.running.get(k);
    if (running) return running.promise;
    const controller = new AbortController();
    const record = { controller };
    record.promise = run(controller.signal).finally(() => {
      if (this.running.get(k) === record) this.running.delete(k);
    });
    this.running.set(k, record);
    return record.promise;
  }

  /**
   * swr paints what is cached and checks it behind the paint.
   *
   * paint is called at most twice, and the second time only if the answer is
   * really different. That is what the entity tag buys: a revalidation that
   * confirms the page costs a few hundred bytes and produces no repaint, so
   * the scroll position and the keyboard cursor survive it.
   */
  async swr(k, run, paint) {
    const held = this.read(k);
    if (held.data !== undefined) paint(held.data, held.state);
    if (held.state === "fresh") return held.data;

    const res = await this.once(k, (signal) => run({ signal, etag: held.etag }));
    if (!res.modified) {
      const kept = this.touch(k);
      return kept === undefined ? held.data : kept;
    }
    // Changed is measured against what this caller painted rather than against
    // the map, because a second caller joined to the same request arrives after
    // the first has already written it. Without a tag on either side there is
    // nothing to compare, so the answer is assumed to be new.
    const changed = !res.etag || !held.etag || res.etag !== held.etag;
    this.write(k, res.data, res.etag);
    if (changed) paint(res.data, "fresh");
    return res.data;
  }

  /**
   * invalidate marks entries stale rather than dropping them. Dropping would be
   * correct and would also mean the next thing anybody looks at is a skeleton.
   * Stale keeps the answer paintable and keeps its tag, so the revalidation it
   * triggers is usually a 304.
   */
  invalidate(prefix = "") {
    for (const [k, entry] of this.entries) {
      if (!prefix || k.startsWith(prefix)) entry.stale = true;
    }
  }

  /**
   * cancel abandons the request for one key, because somebody asked it to stop.
   *
   * Nothing held is dropped. A request being abandoned says nothing about the
   * answer that was already cached, and the next thing to ask for this key
   * should get that answer rather than a fresh wait. Anything joined to the
   * same request stops with it, which is single flight working as intended:
   * there was only ever one request and this is it.
   */
  cancel(k) {
    const running = this.running.get(k);
    if (!running) return false;
    running.controller.abort();
    this.running.delete(k);
    return true;
  }

  /** clear drops everything and abandons what is in flight. */
  clear() {
    this.entries.clear();
    for (const { controller } of this.running.values()) controller.abort();
    this.running.clear();
  }
}

/** cache is the one store the interface shares. */
export const cache = new Store();

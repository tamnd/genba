// The administration screen.
//
// It answers one question, which is whether this deployment is working, and it
// answers it for somebody who can do something about the answer. Four parts:
// what each connector is doing and which of its syncs failed, what the corpus
// holds, which documents are being held back and why, and what one named person
// can actually see.
//
// Nothing on it writes. That is deliberate for now and it is not a permanent
// shape: adding and starting connectors from here is the other half of the
// screen and it is a change to the server as much as to this file, so it is a
// separate piece of work rather than a button that half exists.
//
// The one rule this screen keeps that no other screen has to is that the
// numbers on it have to agree with each other. That is why it is one request:
// the count of held documents above the table and the table itself are read at
// the same moment, so a sync finishing between two fetches cannot make the
// screen contradict itself.

import { h, replace, svg } from "genba/dom.js";
import { api } from "genba/api.js";
import { cache } from "genba/cache.js";
import { bytes, duration, exact, icon, label, number, when } from "genba/format.js";
import { failed as failedState } from "genba/states.js";
import { Access } from "genba/access.js";

// REFRESH is how often the screen asks again while somebody is looking at it.
//
// Five seconds, which is short enough that a sync starting is something you
// watch happen rather than something you reload to find out about, and long
// enough that a screen left open on a wall does not make a request a second
// forever. The endpoint reads memory and one aggregate, so the cost of being
// wrong here is small in one direction and a stale operations screen in the
// other.
const REFRESH = 5000;

export class Admin {
  constructor({ onBack }) {
    this.onBack = onBack;
    this.data = null;
    this.error = null;
    this.timer = 0;
    this.title = null;
    // Built once and reused by every paint, because it holds a form and this
    // screen repaints itself on a timer. See paint.
    this.access = new Access();
    this.el = h("div", { class: "admin" });
  }

  /** key is the entry this screen paints from, which the offline banner names. */
  key() {
    return cache.key("admin", {});
  }

  /**
   * render paints what was held and then what the server says.
   *
   * The held copy is painted first for the same reason every other screen does
   * it, and it matters slightly less here: an operator coming back to this
   * screen wants the current answer, and the current answer is a request away.
   * What the cache buys is that the layout is on screen while that request is
   * in flight, so the numbers land in place rather than after a blank.
   */
  async render() {
    const k = this.key();
    const held = cache.read(k).data || null;
    if (held) {
      this.data = held;
      this.paint();
    } else {
      this.paint();
    }
    await this.refresh();
    this.watch();
  }

  /** refresh reads the endpoint once and repaints. */
  async refresh() {
    try {
      const res = await api.admin();
      if (!this.el.isConnected && this.data) return;
      if (res.modified) cache.write(this.key(), res.data, res.etag);
      this.data = res.data || this.data;
      this.error = null;
    } catch (err) {
      if (err.name === "AbortError") return;
      // A failed check behind a screen that is already painted keeps the
      // screen. What is on it was true when it was painted, and an operator
      // reading a sync failure should not lose it because the next poll
      // happened to arrive during a restart.
      this.error = err;
      if (this.data) {
        this.paint();
        return;
      }
    }
    this.paint();
  }

  /**
   * watch keeps the screen current while it is on the page.
   *
   * The timer checks whether the screen is still in the document rather than
   * being cancelled by whatever navigated away, because there is no unmount
   * hook to hang that on and a poll that keeps running behind another screen is
   * a request nobody asked for.
   */
  watch() {
    clearTimeout(this.timer);
    this.timer = setTimeout(async () => {
      if (!this.el.isConnected) return;
      await this.refresh();
      this.watch();
    }, REFRESH);
  }

  /** stop ends the polling, for a screen being taken off the page. */
  stop() {
    clearTimeout(this.timer);
    this.timer = 0;
  }

  paint() {
    // Whether anything on this screen already has focus, read before the paint
    // takes it away. This screen repaints every five seconds on its own, so
    // pulling focus back to the heading each time would make it impossible to
    // read the table with a keyboard.
    const held = this.el.contains(document.activeElement) && document.activeElement !== this.title;
    // Which element, and where the caret was in it. The access panel below is
    // the same node across repaints, so its contents survive, but a node that
    // is detached and reattached loses focus and a five second timer that takes
    // the caret out of a half typed group name is a form nobody can use.
    const focused = held ? document.activeElement : null;
    const caret = selectionOf(focused);

    this.title = h("h1", { class: "admin__title", tabindex: "-1" }, "Administration");
    // Nothing arrived and nothing was held, so there is nothing to draw. The
    // three zeroes this screen would otherwise print are not the state of the
    // deployment, they are the state of this browser, and on the one screen
    // somebody reads to decide whether anything is wrong that is the worst
    // thing it could say. It is also what somebody who typed this address
    // without the role sees, and the server's own refusal is a better sentence
    // than anything written here.
    const failed = !this.data && this.error;
    const data = this.data || {};
    replace(
      this.el,
      this.backLink(),
      h(
        "header",
        { class: "admin__head" },
        this.title,
        h(
          "p",
          { class: "admin__lead" },
          "What this deployment is doing, and what it is holding back.",
        ),
      ),
      failed ? failedState(this.error, () => this.retry()) : null,
      failed ? null : this.banner(),
      failed ? null : this.corpus(data),
      failed ? null : this.connectors(data),
      failed ? null : this.quarantine(data),
      failed ? null : this.access.el,
    );
    if (!held) {
      this.title.focus();
      return;
    }
    if (focused && focused.isConnected) restore(focused, caret);
  }

  /** retry reads again after a failure, from the button the failure offers. */
  async retry() {
    await this.refresh();
    this.watch();
  }

  /**
   * banner says the screen is not current, and only once it is not.
   *
   * An operations screen that quietly keeps showing the last good answer while
   * the server is down is the one screen where that is actively dangerous, so
   * the failure is on the screen rather than in the console.
   */
  banner() {
    if (!this.error) return null;
    return h(
      "p",
      { class: "admin__stale", role: "status" },
      svg(icon("clock"), 16),
      `This is the last answer that arrived. The server said: ${this.error.message}`,
    );
  }

  backLink() {
    const to = this.onBack();
    return h(
      "div",
      { class: "page__back" },
      h(
        "a",
        {
          class: "page__back-link",
          href: to.href,
          onClick: (e) => {
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
            e.preventDefault();
            this.stop();
            to.go();
          },
        },
        svg(icon("arrow-left"), 16),
        to.title,
      ),
    );
  }

  /**
   * corpus is the two numbers the whole screen hangs off.
   *
   * Servable and held rather than one total, because they are the two halves of
   * what was indexed and the second one is the number nobody has a way of
   * seeing anywhere else. A corpus with eleven hundred held documents and a
   * healthy sync log looks perfect from every other screen in the product.
   */
  corpus(data) {
    return h(
      "section",
      { class: "panel admin__corpus" },
      h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, "Corpus")),
      h(
        "div",
        { class: "admin__figures" },
        figure("Servable", number(data.documents || 0), "Documents a search can reach"),
        figure(
          "Held back",
          number(data.quarantined || 0),
          "Documents whose permissions did not resolve",
          (data.quarantined || 0) > 0,
        ),
      ),
    );
  }

  connectors(data) {
    const list = data.connectors || [];
    return h(
      "section",
      { class: "panel" },
      h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, "Connectors")),
      list.length
        ? list.map((c) => this.connector(c))
        : note(
            "This process runs no connectors.",
            "Its index is filled by something else, so there is nothing to report here.",
          ),
    );
  }

  connector(c) {
    const runs = c.runs || [];
    const last = runs[0] || null;
    const failing = Boolean(last && last.error);
    return h(
      "article",
      { class: "connector" },
      h(
        "div",
        { class: "connector__head" },
        h(
          "div",
          { class: "connector__name" },
          h("h3", { class: "connector__title" }, c.source || "unnamed"),
          h("span", { class: "connector__kind" }, label(c.kind || "")),
        ),
        // The state of the connector as a word rather than a colour, because a
        // colour is not a fact and half the people reading this cannot use it.
        c.syncing
          ? h("span", { class: "pill pill--busy" }, "Syncing")
          : failing
            ? h("span", { class: "pill pill--bad" }, "Last sync failed")
            : runs.length
              ? h("span", { class: "pill pill--good" }, "Healthy")
              : h("span", { class: "pill" }, "Not run yet"),
      ),
      facts([
        ["Reads", c.target],
        ["Tenant", c.tenant],
        ["Refreshes", c.refresh ? `every ${c.refresh}` : "once at startup"],
      ]),
      this.mapping(c.permissions),
      runs.length ? this.runs(runs) : null,
    );
  }

  /**
   * mapping is what the permission mapping made of this source, by reason.
   *
   * Three separate numbers because they want three different actions. A foreign
   * domain is a decision somebody has to make about the tenant, an unmappable
   * deny is usually a feature of the source nobody has written the mapping for
   * yet, and a malformed grant is a bug in the connector.
   */
  mapping(m) {
    if (!m) return null;
    const held = (m.foreign_domain || 0) + (m.unmappable_deny || 0) + (m.malformed || 0);
    if (!m.mapped && !held) return null;
    return h(
      "div",
      { class: "connector__mapping" },
      h("h4", { class: "connector__subtitle" }, "Permissions"),
      h(
        "div",
        { class: "admin__figures admin__figures--tight" },
        figure("Mapped", number(m.mapped || 0), "Access control lists this source resolved"),
        figure("Foreign domain", number(m.foreign_domain || 0), "Grants to somebody outside the tenant", Boolean(m.foreign_domain)),
        figure("Unmappable deny", number(m.unmappable_deny || 0), "A deny this model cannot carry", Boolean(m.unmappable_deny)),
        figure("Malformed", number(m.malformed || 0), "Grants the source wrote in a way nothing understood", Boolean(m.malformed)),
        figure("Ignored roles", number(m.ignored || 0), "Statements whose role does not confer read"),
      ),
    );
  }

  /**
   * runs is the sync history, newest first.
   *
   * A single last run cannot answer the question somebody comes here with,
   * which is not whether a sync failed but whether the failures started this
   * morning or have been there since the process came up.
   */
  runs(runs) {
    return h(
      "div",
      { class: "connector__runs" },
      h("h4", { class: "connector__subtitle" }, "Recent syncs"),
      h(
        "table",
        { class: "admin__table" },
        h(
          "thead",
          {},
          h(
            "tr",
            {},
            h("th", { scope: "col" }, "Started"),
            h("th", { scope: "col" }, "Took"),
            h("th", { scope: "col", class: "admin__num" }, "Indexed"),
            h("th", { scope: "col", class: "admin__num" }, "Held back"),
            h("th", { scope: "col", class: "admin__num" }, "Deleted"),
            h("th", { scope: "col", class: "admin__num" }, "Read"),
            h("th", { scope: "col" }, "Outcome"),
          ),
        ),
        h(
          "tbody",
          {},
          runs.map((r) =>
            h(
              "tr",
              { class: r.error ? "admin__row admin__row--bad" : "admin__row" },
              h("td", {}, h("time", { datetime: r.started, title: exact(r.started) }, when(r.started))),
              h("td", {}, r.duration_ms ? duration(r.duration_ms) : "still going"),
              h("td", { class: "admin__num" }, number(r.indexed || 0)),
              h("td", { class: "admin__num" }, number(r.quarantined || 0)),
              h("td", { class: "admin__num" }, number(r.deleted || 0)),
              h("td", { class: "admin__num" }, bytes(r.bytes || 0)),
              // The whole message rather than a code. The failures here are a
              // directory that disappeared, a bucket refusing a signature and a
              // token that expired, and the sentence is the entire value.
              h("td", { class: "admin__outcome" }, r.error || "Finished"),
            ),
          ),
        ),
      ),
    );
  }

  /**
   * quarantine is which documents are being held back and why.
   *
   * It is the only place in the product where a document nobody may read is
   * named, and it names nothing about the contents: the title, where it came
   * from, when it last changed and the reason it did not resolve. That is what
   * an operator needs to go and fix the access control list at the source, and
   * nothing beyond it.
   */
  quarantine(data) {
    const list = data.held || [];
    const total = data.quarantined || 0;
    return h(
      "section",
      { class: "panel" },
      h(
        "div",
        { class: "panel__head" },
        h("h2", { class: "panel__title" }, "Held back"),
        // Said out loud when the list is a sample, so nobody reads a hundred
        // rows as the whole of a corpus of fourteen hundred.
        total > list.length && list.length
          ? h("span", { class: "panel__note" }, `${number(list.length)} of ${number(total)}`)
          : null,
      ),
      !total
        ? note(
            "Nothing is being held back.",
            "Every document that was indexed resolved to a permission this deployment understands.",
          )
        : !data.listable
          ? note(
              "This storage driver cannot list what it is holding back.",
              `The count is ${number(total)}. The sync log names the reasons in aggregate.`,
            )
          : this.heldTable(list),
    );
  }

  heldTable(list) {
    return h(
      "table",
      { class: "admin__table" },
      h(
        "thead",
        {},
        h(
          "tr",
          {},
          h("th", { scope: "col" }, "Document"),
          h("th", { scope: "col" }, "Source"),
          h("th", { scope: "col" }, "Changed"),
          h("th", { scope: "col" }, "Why it did not resolve"),
        ),
      ),
      h(
        "tbody",
        {},
        list.map((d) =>
          h(
            "tr",
            { class: "admin__row" },
            h(
              "td",
              {},
              h("span", { class: "admin__doc" }, d.title || d.id),
              d.title ? h("span", { class: "admin__id" }, d.id) : null,
            ),
            h("td", {}, label(d.source || "")),
            h(
              "td",
              {},
              d.modified_at
                ? h("time", { datetime: d.modified_at, title: exact(d.modified_at) }, when(d.modified_at))
                : "",
            ),
            // The reason is the only thing on this screen anybody acts on, so
            // it gets the room. A list of held documents with no reasons is the
            // count again, drawn as a table.
            h("td", { class: "admin__reason" }, d.reason || "The connector did not say."),
          ),
        ),
      ),
    );
  }
}

/**
 * selectionOf reads where the caret is, for an element that has one.
 *
 * Null for anything else, including a button or a link, because those are put
 * back by focusing them and have no position to keep.
 */
function selectionOf(el) {
  if (!el || typeof el.selectionStart !== "number") return null;
  return { start: el.selectionStart, end: el.selectionEnd, direction: el.selectionDirection };
}

/**
 * restore puts focus and the caret back after a repaint.
 *
 * Guarded, because setSelectionRange throws on an input whose type does not
 * carry a selection, and an email field somebody is typing into is worth
 * keeping focus on even if the caret has to go to the end of it.
 */
function restore(el, caret) {
  el.focus();
  if (!caret) return;
  try {
    el.setSelectionRange(caret.start, caret.end, caret.direction || "none");
  } catch {
    // Focus is the part that matters and it is already back.
  }
}

/**
 * figure is one number with its name and what it counts.
 *
 * The same element the home screen prints a corpus size with, because a number
 * on a screen is a number on a screen and a second way of drawing one is a
 * second thing to keep in step. warn colours it, and only ever for a count that
 * is not zero, because a warning that is always on is not a warning.
 */
function figure(name, value, hint, warn = false) {
  return h(
    "div",
    { class: warn ? "stat admin__stat--warn" : "stat" },
    h("span", { class: "stat__value" }, value),
    h("span", { class: "stat__label" }, name),
    h("span", { class: "admin__hint" }, hint),
  );
}

/** note is an empty state, which on this screen is usually the good news. */
function note(title, body) {
  return h(
    "div",
    { class: "admin__note" },
    h("p", { class: "admin__note-title" }, title),
    h("p", { class: "admin__note-body" }, body),
  );
}

/**
 * facts is a list of named values, with the ones nobody answered left out.
 *
 * An empty row would be a fact this screen claims to know and does not, which
 * on the screen somebody comes to for the truth about a deployment is the worst
 * thing it could print.
 */
function facts(rows) {
  return h(
    "dl",
    { class: "facts" },
    rows
      .filter(([, value]) => value)
      .map(([name, value]) => [
        h("dt", { class: "facts__name" }, name),
        h("dd", { class: "facts__value" }, value),
      ]),
  );
}

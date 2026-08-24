// The answers screen.
//
// One screen for the prose somebody here wrote and signed, which is the only
// thing in this product that is added to the corpus rather than found in it.
// The three endpoints behind it have existed for a while and the only way to
// reach them was curl, which made the feature available to whoever was willing
// to write JSON by hand.
//
// Three things about it are worth writing down, and they are all consequences
// of decisions taken further down the stack.
//
// The sources are ids and they stay ids. An answer's sources are stored that
// way so that an answer written by somebody who can read everything does not
// become a list of documents the reader in front of it cannot open, and the
// price of that is a screen holding ids where it wants titles. It resolves them
// for display through an endpoint of their own and saves back the ids it was
// given, because saving what came back would quietly drop every source the
// editor happens not to have access to.
//
// Saving is also confirming. There is no separate act that re-verifies an
// answer: the date under it is the date somebody last stood behind the words,
// and pressing Save is somebody standing behind them. The form says so rather
// than leaving it to be found out.
//
// It does not poll. The administration screen next to this one does, because it
// is watching a process that changes on its own. This one is watching a form,
// and a five second timer running through a half typed answer is a screen
// nobody can write in.

import { h, replace, svg } from "genba/dom.js";
import { api } from "genba/api.js";
import { cache } from "genba/cache.js";
import { exact, icon, label, sourceColor, when } from "genba/format.js";
import { render as markdown } from "genba/markdown.js";
import { failed as failedState } from "genba/states.js";

// The three words the server sends with an answer, which are the three a
// verification badge uses, because a reader who has learnt what an amber mark
// on a document means has learnt what it means here.
const STATES = new Set(["fresh", "expiring", "expired"]);

// SOURCES is how many documents one answer may cite.
//
// Six, because six is what the card under an answer draws. A seventh would be
// stored, would never be seen, and would leave whoever added it believing they
// had cited it. The server is the one that truncates, so this is the screen
// declining to write something it knows will not be shown.
const SOURCES = 6;

// FOUND is how many results the source picker offers, and TYPING is how long it
// waits before asking. Five results is a short list somebody reads rather than
// scans, and a sixth of a second is long enough that a typed word is one
// request rather than six.
const FOUND = 5;
const TYPING = 160;

// EXCERPT is how much of an answer the list shows under its question. Enough to
// recognise which answer it is, and not so much that a list of ten is a page of
// prose nobody scrolls past.
const EXCERPT = 180;

export class Answers {
  constructor({ onBack, onSay }) {
    this.onBack = onBack;
    this.onSay = onSay || (() => {});

    this.data = null;
    // Two errors rather than one, because they mean different things and are
    // printed in different places. broken is a list that could not be read, and
    // it takes over the screen. error is a write that was refused, and it goes
    // on the status line above the form.
    this.broken = null;
    this.error = null;
    this.busy = "";
    this.said = "";
    // The answer whose removal has been asked for once. Taking one down cannot
    // be undone from here, so it is asked twice, and the confirmation replaces
    // the button that opened it so a keyboard lands on it without going looking.
    this.confirming = "";

    // The answer being written, or null when this screen is only a list. It
    // holds the id and nothing else. The words live in the fields below, which
    // are the same nodes for the life of the screen, so that repainting the list
    // underneath somebody never empties what they are typing.
    this.editing = null;
    this.sources = [];
    // Titles for the ids above, filled in behind the screen. What is saved is
    // the ids, never this.
    this.titles = new Map();
    this.finding = 0;
    this.looking = "";

    this.title = h("h1", { class: "answers__title", tabindex: "-1" }, "Answers");
    this.output = h("p", {
      class: "answers__output",
      role: "status",
      "aria-live": "polite",
      tabindex: "-1",
    });
    this.back = h("div", { class: "page__back" });
    this.editorEl = h("section", { class: "panel answers__editor", hidden: true });
    this.listEl = h("section", { class: "panel" });
    this.el = h("div", { class: "answers" });
    this.build();
  }

  /** key is the entry this screen paints from, which the offline banner names. */
  key() {
    return cache.key("answers", {});
  }

  /**
   * build makes the screen once.
   *
   * Once rather than on every paint, because half of it is a form. The list
   * repaints on its own into a node that outlives it, and the fields never
   * repaint at all: their values are set when an answer is opened and read back
   * when it is saved.
   */
  build() {
    this.editorTitle = h("h2", { class: "panel__title" }, "Write an answer");
    this.question = field({
      id: "answer-question",
      name: "Question",
      hint: "The question in the words somebody would type into the box",
    });
    this.variants = area({
      id: "answer-variants",
      name: "Other phrasings",
      hint: "One a line. Each one finds this answer too, so a question that gets asked three ways is answered once",
      rows: 3,
    });
    this.body = area({
      id: "answer-body",
      name: "Answer",
      hint: "Markdown, drawn by the same renderer that draws a document",
      rows: 12,
    });
    this.body.input.addEventListener("input", () => this.repreview());
    this.preview = h("div", { class: "answers__preview prose" });

    this.find = field({
      id: "answer-source",
      name: "Add a source",
      hint: "Search the corpus for it. A document is cited by id, so a reader who cannot open it simply does not see the citation",
    });
    this.find.input.addEventListener("input", () => this.look());
    this.found = h("div", { class: "answers__found" });
    this.chosen = h("ul", { class: "answers__chosen" });

    const form = h(
      "form",
      {
        class: "answers__form",
        onSubmit: (e) => {
          e.preventDefault();
          this.save();
        },
      },
      this.question.el,
      this.variants.el,
      h(
        "div",
        { class: "answers__writing" },
        this.body.el,
        h(
          "div",
          { class: "answers__side" },
          h("span", { class: "answers__label" }, "Preview"),
          this.preview,
        ),
      ),
      h(
        "div",
        { class: "answers__block" },
        h("span", { class: "answers__label" }, "Sources"),
        this.chosen,
        this.find.el,
        this.found,
      ),
      // Said on the form rather than left to be discovered, because it is the
      // part of this screen that is not obvious: there is no button anywhere
      // that re-verifies an answer, and this is why there does not need to be.
      h(
        "p",
        { class: "answers__rule" },
        "Saving is also confirming. The date under the answer becomes today, and the review date moves out again.",
      ),
      h(
        "div",
        { class: "answers__actions" },
        h("button", { class: "button button--primary", type: "submit" }, "Save answer"),
        h(
          "button",
          { class: "button", type: "button", onClick: () => this.close() },
          "Cancel",
        ),
      ),
    );
    replace(this.editorEl, h("div", { class: "panel__head" }, this.editorTitle), form);
    replace(
      this.el,
      this.back,
      h(
        "header",
        { class: "answers__head" },
        this.title,
        h(
          "p",
          { class: "answers__lead" },
          "Answers written here stand above the results for the questions they answer, signed by whoever wrote them.",
        ),
      ),
      // Above both panels rather than inside the form, because the form is not
      // on the screen when an answer is taken down and a sentence nobody can
      // see is a write that reports nothing.
      this.output,
      this.editorEl,
      this.listEl,
    );
    this.repreview();
    this.say();
  }

  /**
   * render paints what was held and then what the server says.
   *
   * The held copy goes up first for the reason every other screen does it: the
   * layout is on the page while the request is in flight, so the rows land in
   * place rather than after a blank.
   *
   * This runs again every time the tab comes back to the front, which is what
   * makes the focus check matter. Arriving here from the rail has to land
   * somewhere, and the heading is where, but the same call while somebody is
   * halfway through an answer must leave the cursor where their hands are.
   */
  async render() {
    const working = this.el.contains(document.activeElement) && document.activeElement !== this.title;
    replace(this.back, this.backLink());
    const held = cache.read(this.key()).data;
    if (held) this.data = held;
    this.paintList();
    if (!working) this.title.focus();
    await this.refresh();
  }

  /** refresh reads the list again and repaints only if it moved. */
  async refresh() {
    try {
      await cache.swr(
        this.key(),
        (opts) => api.answers(opts),
        (data) => {
          this.data = data;
          this.paintList();
        },
      );
      if (this.broken) {
        this.broken = null;
        this.paintList();
      }
    } catch (err) {
      if (err.name === "AbortError") return;
      // A check that failed behind a list already on screen keeps the list.
      // What is on it was true when it was painted, and somebody halfway
      // through writing an answer should not lose the screen because a poll
      // arrived during a restart.
      this.broken = err;
      this.paintList();
    }
  }

  backLink() {
    const to = this.onBack();
    return h(
      "a",
      {
        class: "page__back-link",
        href: to.href,
        onClick: (e) => {
          if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
          e.preventDefault();
          to.go();
        },
      },
      svg(icon("arrow-left"), 16),
      to.title,
    );
  }

  /**
   * open puts an answer in the form, or empties it for a new one.
   *
   * The fields are set here and nowhere else. Nothing repaints them afterwards,
   * which is what makes it safe for the list below to redraw itself while
   * somebody is typing.
   */
  open(record) {
    this.editing = { id: (record && record.id) || "" };
    this.question.input.value = (record && record.question) || "";
    this.variants.input.value = ((record && record.variants) || []).join("\n");
    this.body.input.value = (record && record.body) || "";
    this.sources = [...((record && record.sources) || [])];
    this.find.input.value = "";
    this.error = null;
    this.said = "";
    this.confirming = "";

    replace(this.editorTitle, record ? "Edit this answer" : "Write an answer");
    replace(this.found);
    this.editorEl.hidden = false;
    this.repreview();
    this.paintChosen();
    this.say();
    this.paintList();
    this.resolve();
    this.question.input.focus();
  }

  /** close takes the form off the screen and empties it. */
  close() {
    this.editing = null;
    this.sources = [];
    this.question.input.value = "";
    this.variants.input.value = "";
    this.body.input.value = "";
    this.find.input.value = "";
    replace(this.found);
    this.editorEl.hidden = true;
    this.paintList();
    const back = this.listEl.querySelector('[data-focus-key="new"]');
    if (back) back.focus();
  }

  /** repreview draws the answer as it is being typed. */
  repreview() {
    const text = this.body.input.value;
    replace(
      this.preview,
      text.trim()
        ? markdown(text)
        : h("p", { class: "answers__blank" }, "What is typed on the left is drawn here."),
    );
  }

  /**
   * look asks for documents matching what has been typed into the picker.
   *
   * Debounced, because this fires on every keystroke and the server is being
   * asked a real search each time. The answer is thrown away if the box has
   * moved on, so a slow request for three letters cannot overwrite the list for
   * six.
   */
  look() {
    clearTimeout(this.finding);
    const q = this.find.input.value.trim();
    this.looking = q;
    if (!q) {
      replace(this.found);
      return;
    }
    this.finding = setTimeout(() => this.search(q), TYPING);
  }

  async search(q) {
    let hits = [];
    try {
      const res = await cache.swr(
        cache.key("search", { q, limit: FOUND }),
        (opts) => api.search({ q, limit: FOUND }, opts),
        () => {},
      );
      hits = (res && res.hits) || [];
    } catch (err) {
      if (err.name === "AbortError") return;
      if (this.looking !== q) return;
      replace(this.found, h("p", { class: "answers__blank" }, err.message));
      return;
    }
    if (this.looking !== q) return;
    if (!hits.length) {
      replace(this.found, h("p", { class: "answers__blank" }, `Nothing here matches ${q}.`));
      return;
    }
    replace(this.found, hits.map((hit) => this.result(hit)));
  }

  /** result is one document the picker is offering. */
  result(hit) {
    const already = this.sources.includes(hit.id);
    return h(
      "button",
      {
        class: "answers__result",
        type: "button",
        disabled: already || this.sources.length >= SOURCES,
        onClick: () => this.add(hit),
      },
      h("span", { class: "source__dot", style: { background: sourceColor(hit.source) } }),
      h("span", { class: "answers__result-title" }, hit.title || hit.id),
      h("span", { class: "answers__result-where" }, label(hit.source)),
      already ? h("span", { class: "answers__result-note" }, "Already cited") : null,
    );
  }

  add(hit) {
    if (this.sources.includes(hit.id) || this.sources.length >= SOURCES) return;
    this.sources.push(hit.id);
    this.titles.set(hit.id, hit);
    this.find.input.value = "";
    this.looking = "";
    replace(this.found);
    this.paintChosen();
    this.find.input.focus();
  }

  drop(id) {
    this.sources = this.sources.filter((one) => one !== id);
    this.paintChosen();
    this.find.input.focus();
  }

  /**
   * resolve fills in the titles of the sources this answer already carries.
   *
   * Behind the screen, because the ids are what makes the form correct and the
   * titles are only what makes it readable. An id that does not come back is
   * either a document this editor may not open or one that has gone, and the
   * two are deliberately not distinguished. Either way it stays on the answer.
   */
  async resolve() {
    const want = this.sources.filter((id) => !this.titles.has(id));
    if (!want.length) return;
    try {
      const res = await cache.swr(
        cache.key("documents", { id: want }),
        (opts) => api.documents(want, opts),
        () => {},
      );
      for (const hit of (res && res.documents) || []) this.titles.set(hit.id, hit);
    } catch {
      // A lookup that failed costs a nicer label and nothing else.
    }
    this.paintChosen();
  }

  /** paintChosen draws the sources on the answer being written. */
  paintChosen() {
    const rows = this.sources.map((id) => {
      const hit = this.titles.get(id);
      return h(
        "li",
        { class: "answers__source" },
        h("span", {
          class: "source__dot",
          style: { background: sourceColor(hit ? hit.source : "") },
        }),
        h("span", { class: "answers__source-title" }, hit ? hit.title || hit.id : id),
        hit
          ? h("span", { class: "answers__source-where" }, label(hit.source))
          : h(
              "span",
              { class: "answers__source-where" },
              "Did not resolve for you, and stays on the answer",
            ),
        h(
          "button",
          {
            class: "button button--small",
            type: "button",
            onClick: () => this.drop(id),
          },
          "Remove",
        ),
      );
    });
    if (this.sources.length >= SOURCES) {
      rows.push(
        h(
          "li",
          { class: "answers__full" },
          `Six is what a reader sees under an answer, so this one is full.`,
        ),
      );
    }
    replace(this.chosen, rows.length ? rows : h("li", { class: "answers__blank" }, "None yet."));
  }

  /** save writes the answer, which is the same call whether it is new or not. */
  async save() {
    const question = this.question.input.value.trim();
    if (!question) {
      this.complain("Write the question this answers.", this.question.input);
      return;
    }
    const body = this.body.input.value.trim();
    if (!body) {
      this.complain("Write the answer.", this.body.input);
      return;
    }
    // A new answer is named here rather than by the server, because the id is
    // in the path of the write. It is derived from the question so that
    // anybody reading a log can tell which answer it is, and it carries a
    // suffix so that two differently worded questions about the same subject
    // cannot silently replace one another.
    const id = this.editing.id || idFor(question);
    const record = { question, variants: lines(this.variants.input.value), body, sources: this.sources };
    const ok = await this.write("save", () => api.curate(id, record), "Saved.");
    if (ok) this.close();
  }

  /** ask opens or closes the confirmation on one answer's removal. */
  ask(id) {
    this.confirming = id;
    this.error = null;
    this.said = "";
    this.say();
    this.paintList();
  }

  async remove(id) {
    const editing = this.editing && this.editing.id === id;
    const ok = await this.write(`remove:${id}`, () => api.retract(id), "Taken down.");
    if (ok && editing) this.close();
  }

  /**
   * write does one change and repaints from what is true afterwards.
   *
   * Nothing is retried and a second press while one is in flight is dropped
   * here rather than by disabling the button, because a disabled button loses
   * focus and a keyboard that pressed Save would be thrown to the top of the
   * page for it.
   */
  async write(about, run, said) {
    if (this.busy) return false;
    this.busy = about;
    this.error = null;
    this.said = "";
    this.confirming = "";
    this.say();
    this.paintList();

    try {
      await run();
      this.said = said;
    } catch (err) {
      this.error = err;
    }
    this.busy = "";
    this.say();
    if (this.error) {
      this.paintList();
      return false;
    }
    // Every search this tab is holding may have a card on it that this write
    // just changed, and the search screen has no way of knowing that. Marking
    // them stale rather than dropping them means the next visit revalidates in
    // a few hundred bytes instead of painting a skeleton.
    cache.invalidate();
    this.onSay(said);
    await this.refresh();
    return true;
  }

  /** complain refuses a form that is not finished, and says which field. */
  complain(message, where) {
    this.error = new Error(message);
    this.said = "";
    this.say();
    if (where) where.focus();
  }

  /**
   * say prints the outcome of the last write.
   *
   * One line above the form for all of them, rather than a message beside each
   * button, because it is the one node on this screen that does not move when
   * an answer is saved or taken down.
   */
  say() {
    if (this.error) {
      replace(this.output, svg(icon("alert"), 16), this.error.message);
      this.output.className = "answers__output answers__output--bad";
      return;
    }
    if (this.busy) {
      replace(this.output, this.busy === "save" ? "Saving it." : "Taking it down.");
      this.output.className = "answers__output answers__output--busy";
      return;
    }
    if (this.said) {
      replace(this.output, svg(icon("check"), 16), this.said);
      this.output.className = "answers__output answers__output--good";
      return;
    }
    replace(this.output);
    this.output.className = "answers__output";
  }

  paintList() {
    // Nothing arrived and nothing was held, so there is nothing to draw. The
    // empty list this would otherwise print is the state of this browser rather
    // than the state of the tenant, and it is also what somebody who typed this
    // address without the role sees, where the server's own refusal is a better
    // sentence than anything written here.
    if (!this.data && this.broken) {
      this.editorEl.hidden = true;
      replace(this.listEl, failedState(this.broken, () => this.refresh()));
      return;
    }

    const data = this.data || {};
    const rows = data.answers || [];
    replace(
      this.listEl,
      h(
        "div",
        { class: "panel__head" },
        h("h2", { class: "panel__title" }, "Written answers"),
        rows.length
          ? h("span", { class: "panel__note" }, rows.length === 1 ? "One answer" : `${rows.length} answers`)
          : null,
        data.writable && !this.editing
          ? h(
              "button",
              {
                class: "button button--primary answers__new",
                type: "button",
                dataset: { focusKey: "new" },
                onClick: () => this.open(null),
              },
              "Write an answer",
            )
          : null,
      ),
      this.broken
        ? h(
            "p",
            { class: "answers__stale", role: "status" },
            svg(icon("clock"), 16),
            `This is the last list that arrived. The server said: ${this.broken.message}`,
          )
        : null,
      // A deployment whose storage driver cannot keep an answer gets the
      // sentence rather than the form, because a form whose answers disappear
      // at the next restart is worse than no form at all.
      data.writable
        ? null
        : note(
            "This deployment cannot keep a written answer.",
            "The storage driver it is running on has nowhere to put one, so there is nothing here that could write one. Sqlite and Postgres both can.",
          ),
      rows.length ? rows.map((a) => this.row(a)) : null,
      rows.length || !data.writable
        ? null
        : note(
            "Nothing has been answered yet.",
            "An answer is worth writing when the same question keeps being asked and the documents that answer it are spread across four places.",
          ),
    );
  }

  /** row is one answer as the person who maintains it sees it. */
  row(a) {
    const state = STATES.has(a.state) ? a.state : "expired";
    const editing = Boolean(this.editing && this.editing.id === a.id);
    const confirming = this.confirming === a.id;
    const variants = (a.variants || []).length;
    const sources = (a.sources || []).length;
    return h(
      "article",
      { class: editing ? "answer-row answer-row--editing" : "answer-row" },
      h(
        "div",
        { class: "answer-row__head" },
        h("h3", { class: "answer-row__question" }, a.question || a.id),
        pill(state, a.until),
      ),
      h("p", { class: "answer-row__excerpt" }, excerpt(a.body || "")),
      h(
        "p",
        { class: "answer-row__meta" },
        h(
          "span",
          { title: exact(a.at) },
          `${a.by || "somebody"} wrote this ${when(a.at)}`,
        ),
        variants ? h("span", {}, variants === 1 ? "One other phrasing" : `${variants} other phrasings`) : null,
        sources ? h("span", {}, sources === 1 ? "One source" : `${sources} sources`) : null,
      ),
      h(
        "div",
        { class: "answer-row__controls" },
        action(editing ? "Editing it" : "Edit", `edit:${a.id}`, () => this.open(a)),
        confirming
          ? action("Take it down", `remove:${a.id}`, () => this.remove(a.id), "button--danger")
          : action("Take down", `remove:${a.id}`, () => this.ask(a.id)),
        confirming ? action("Keep it", `keep:${a.id}`, () => this.ask("")) : null,
        confirming
          ? h(
              "p",
              { class: "answer-row__warn" },
              "This cannot be undone from here. The documents it cited stay where they are.",
            )
          : null,
      ),
    );
  }
}

/**
 * pill is where the answer stands today, in the words a verification uses.
 *
 * An expired answer is still on the screen and still says so, because taking it
 * off the list on the day it runs out would leave the person who wrote it with
 * no way to find out it needs looking at.
 *
 * A current one carries no date, the same way a verification badge does not. Six
 * months away is not a date anybody acts on, and the two states that do want
 * acting on are the ones that print how long is left.
 */
function pill(state, until) {
  if (state === "fresh") {
    return h(
      "span",
      { class: "verified", title: `Due to be confirmed again ${exact(until)}` },
      svg(icon("check"), 14),
      "Current",
    );
  }
  return h(
    "span",
    {
      class: `verified verified--${state}`,
      title:
        state === "expired"
          ? `Nobody has confirmed this since ${exact(until)}`
          : `Due to be confirmed again ${exact(until)}`,
    },
    svg(icon("alert"), 14),
    state === "expired" ? "Not confirmed recently" : `Due for review ${when(until)}`,
  );
}

/**
 * excerpt is enough of an answer to recognise which one it is.
 *
 * The markdown is not rendered, because this is a line in a list of answers and
 * a rendered heading in it would be a heading in the middle of somebody else's
 * list. It is not printed as typed either. The markers that carry no meaning
 * once the structure is gone read as typing mistakes to whoever is scanning the
 * list for the answer they came to fix, so the inline ones come off and what is
 * left is the sentence underneath.
 */
function excerpt(body) {
  const flat = body
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/^\s*[>#]+\s*/gm, "")
    .replace(/^\s*[-*+]\s+/gm, "")
    .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/(\*\*|__|[*_`])/g, "")
    .replace(/\s+/g, " ")
    .trim();
  return flat.length > EXCERPT ? `${flat.slice(0, EXCERPT).trimEnd()}...` : flat;
}

/** lines splits a textarea into the list of phrasings it holds. */
function lines(value) {
  return value
    .split("\n")
    .map((one) => one.trim())
    .filter(Boolean);
}

/**
 * idFor names a new answer.
 *
 * The question, so that a log line or a stored row says which answer it is,
 * with a short suffix on the end. Without the suffix two questions that reduce
 * to the same words would be the same id, and writing the second would replace
 * the first with no warning anywhere. Nobody reads this: an answer is addressed
 * by the question a reader typed, never by its id.
 */
function idFor(question) {
  const slug = question
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48);
  const random = crypto.getRandomValues(new Uint8Array(4));
  const suffix = Array.from(random, (b) => b.toString(36).padStart(2, "0")).join("").slice(0, 6);
  return slug ? `${slug}-${suffix}` : `answer-${suffix}`;
}

/**
 * field and area are one labelled control each, returned with the control so it
 * can be read back.
 *
 * The label, the hint and the identifier that ties them together are the same in
 * both, which is why they are two shapes of one function rather than two
 * functions: a hint that is a label's sibling in one and something else in the
 * other is two things to get right in the accessibility tree instead of one.
 */
function field(spec) {
  const hint = `${spec.id}-hint`;
  const input = h("input", {
    class: "answers__input",
    id: spec.id,
    type: "text",
    autocomplete: "off",
    spellcheck: "false",
    "aria-describedby": hint,
  });
  return { input, el: labelled(spec, hint, input) };
}

function area(spec) {
  const hint = `${spec.id}-hint`;
  const input = h("textarea", {
    class: "answers__area",
    id: spec.id,
    rows: String(spec.rows || 4),
    "aria-describedby": hint,
  });
  return { input, el: labelled(spec, hint, input) };
}

function labelled(spec, hint, input) {
  return h(
    "div",
    { class: "answers__field" },
    h("label", { class: "answers__label", for: spec.id }, spec.name),
    input,
    h("span", { class: "answers__hint", id: hint }, spec.hint),
  );
}

/**
 * action is one button on one answer.
 *
 * The key names the answer as well as the verb, because every row has a Take
 * down button and focus belongs on the one it was on.
 */
function action(name, key, onClick, extra = "") {
  return h(
    "button",
    {
      class: extra ? `button button--small ${extra}` : "button button--small",
      type: "button",
      dataset: { focusKey: key },
      onClick,
    },
    name,
  );
}

/** note is an empty state, which on this screen is usually just news. */
function note(title, body) {
  return h(
    "div",
    { class: "answers__note" },
    h("p", { class: "answers__note-title" }, title),
    h("p", { class: "answers__note-body" }, body),
  );
}

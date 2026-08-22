// The access check on the administration screen.
//
// It answers the question every operator eventually asks, which arrives in one
// of two shapes: why can that person find this document, or why can that person
// find nothing. Both are usually answered today by borrowing somebody's account,
// which is not audited, needs their password and reads their documents. This
// asks the server the same question the search path asks, as them, and records
// that it was asked.
//
// Two things it does not do, both on purpose. It never lists documents, only
// counts them, because the administrator role grants nothing over documents and
// a list here would be a way of reading a corpus through somebody else's eyes.
// And it explains a yes and never a no: a document held back, a document in
// another tenant, a document nobody listed them on and a document that does not
// exist all come back as the same refusal, because an operator who can tell
// those apart can use the difference to prove a document exists.

import { h, replace, svg } from "genba/dom.js";
import { api } from "genba/api.js";
import { icon, label, number } from "genba/format.js";

export class Access {
  constructor() {
    // The root is built once and handed back to every paint of the screen
    // around it, which repaints itself every five seconds. A panel rebuilt on
    // that timer would be a form that empties itself while somebody is typing
    // into it.
    this.el = h("section", { class: "panel access" });
    this.answer = null;
    this.error = null;
    this.busy = "";
    this.form = null;
    this.output = h("div", { class: "access__answer", role: "status", "aria-live": "polite" });
    this.paint();
  }

  paint() {
    this.form = this.form || this.buildForm();
    replace(
      this.el,
      h(
        "div",
        { class: "panel__head" },
        h("h2", { class: "panel__title" }, "What one person can see"),
        h("span", { class: "panel__note" }, "Every check is recorded"),
      ),
      h(
        "p",
        { class: "access__lead" },
        "Answered as that person, by the same rule a search applies. It reports counts and never documents.",
      ),
      this.form,
      this.output,
    );
    this.fill();
    return this.el;
  }

  /**
   * buildForm is the question, kept alive across repaints.
   *
   * The groups field is the one that matters and the one people get wrong, so
   * it says what a group looks like rather than leaving somebody to guess: the
   * keys are compared exactly as the directory wrote them, and a name typed in
   * the wrong case is a different group.
   */
  buildForm() {
    const form = h("form", {
      class: "access__form",
      onSubmit: (e) => {
        e.preventDefault();
        this.ask(false);
      },
    });
    this.subject = field("Subject", "access-subject", "The account identifier, as the login gives it");
    this.groups = field("Groups", "access-groups", "Comma separated, as the directory writes them, like gdrive:eng@acme.com");
    this.identities = field("Identities", "access-identities", "Comma separated, like slack:U04AB");
    this.document = field("Document", "access-document", "Optional, to ask about one document");

    this.check = h("button", { class: "button button--primary", type: "submit" }, "Check");
    this.count = h(
      "button",
      {
        class: "button",
        type: "button",
        onClick: () => this.ask(true),
      },
      "Count what they can reach",
    );
    replace(
      form,
      h("div", { class: "access__fields" }, this.subject.el, this.groups.el, this.identities.el, this.document.el),
      h(
        "div",
        { class: "access__actions" },
        this.check,
        this.count,
        // Said next to the button rather than in a tooltip, because the reason
        // it is a separate button is a cost somebody is about to pay.
        h("span", { class: "access__aside" }, "Counting reads every document in the tenant, so it takes a moment."),
      ),
    );
    return form;
  }

  /** ask puts the question, with or without the counts. */
  async ask(counts) {
    const subject = this.subject.input.value.trim();
    if (!subject) {
      this.error = new Error("Name the subject to ask about.");
      this.answer = null;
      this.fill();
      this.subject.input.focus();
      return;
    }
    this.busy = counts ? "count" : "check";
    this.error = null;
    this.fill();
    try {
      const res = await api.access({
        subject,
        groups: this.groups.input.value.trim(),
        identities: this.identities.input.value.trim(),
        id: this.document.input.value.trim(),
        counts: counts ? "1" : "",
      });
      this.answer = res.data;
      this.error = null;
    } catch (err) {
      if (err.name === "AbortError") return;
      this.error = err;
      this.answer = null;
    } finally {
      this.busy = "";
      this.fill();
    }
  }

  /** fill paints the answer, leaving the form above it alone. */
  fill() {
    this.check.disabled = Boolean(this.busy);
    this.count.disabled = Boolean(this.busy) || (this.answer ? !this.answer.countable : false);
    if (this.error) {
      replace(
        this.output,
        h("p", { class: "access__error" }, svg(icon("alert"), 16), this.error.message),
      );
      return;
    }
    if (this.busy) {
      replace(
        this.output,
        h(
          "p",
          { class: "access__busy" },
          this.busy === "count" ? "Counting what they can reach." : "Asking.",
        ),
      );
      return;
    }
    if (!this.answer) {
      replace(this.output, h("p", { class: "access__idle" }, "Nothing asked yet."));
      return;
    }
    replace(this.output, this.asked(this.answer), this.verdict(this.answer), this.reach(this.answer));
  }

  /**
   * asked echoes the question back.
   *
   * An operator who typed a group name wrongly gets an answer of nothing, and
   * the only way to tell that apart from somebody who genuinely has no access
   * is to see what was asked. The tenant is on here for the same reason: it is
   * the operator's own and never what they typed.
   */
  asked(a) {
    const groups = a.groups || [];
    return h(
      "div",
      { class: "access__asked" },
      h("h3", { class: "access__subtitle" }, "Asked about"),
      h(
        "dl",
        { class: "facts" },
        h("dt", { class: "facts__name" }, "Subject"),
        h("dd", { class: "facts__value" }, a.subject),
        h("dt", { class: "facts__name" }, "Tenant"),
        h(
          "dd",
          { class: "facts__value" },
          // A deployment that never named a tenant is one tenant, and an empty
          // row here would be a fact this screen claims to know and does not.
          a.tenant || "Not named, so the whole deployment",
        ),
        h("dt", { class: "facts__name" }, "Groups"),
        h(
          "dd",
          { class: "facts__value" },
          groups.length ? groups.join(", ") : "None, so only what the whole tenant can read",
        ),
      ),
    );
  }

  /**
   * verdict is the answer about one document.
   *
   * A yes says which clause admitted them and which group or account it
   * matched, which is the whole of what somebody needs to go and change it at
   * the source. A no says nothing further, and the sentence says so out loud so
   * that a bare no does not read as a bug.
   */
  verdict(a) {
    if (!a.document) return null;
    const d = a.document;
    return h(
      "div",
      { class: d.visible ? "access__verdict access__verdict--yes" : "access__verdict" },
      h("h3", { class: "access__subtitle" }, "That document"),
      h(
        "p",
        { class: "access__call" },
        svg(icon(d.visible ? "check" : "lock"), 18),
        h("span", { class: "access__doc" }, d.id),
        d.visible ? "can be read by them" : "cannot be read by them",
      ),
      d.visible
        ? h(
            "dl",
            { class: "facts" },
            h("dt", { class: "facts__name" }, "Because"),
            h("dd", { class: "facts__value" }, rule(d.rule)),
            d.matched ? h("dt", { class: "facts__name" }, "Matched") : null,
            d.matched ? h("dd", { class: "facts__value" }, d.matched) : null,
          )
        : h(
            "p",
            { class: "access__aside" },
            "Held back, in another tenant, not on the list, or not indexed. This screen does not say which, because the difference would tell you whether the document exists.",
          ),
    );
  }

  /** reach is how much of the corpus they can read, once somebody asked. */
  reach(a) {
    if (!a.counted) {
      return a.countable
        ? null
        : h("p", { class: "access__aside" }, "This storage driver cannot count what somebody can reach.");
    }
    const sources = a.sources || [];
    return h(
      "div",
      { class: "access__reach" },
      h("h3", { class: "access__subtitle" }, "Can reach"),
      h(
        "p",
        { class: "access__total" },
        h("strong", {}, number(a.documents || 0)),
        a.documents === 1 ? " document" : " documents",
      ),
      sources.length
        ? h(
            "table",
            { class: "admin__table" },
            h(
              "thead",
              {},
              h(
                "tr",
                {},
                h("th", { scope: "col" }, "Source"),
                h("th", { scope: "col", class: "admin__num" }, "Documents"),
              ),
            ),
            h(
              "tbody",
              {},
              sources.map((s) =>
                h(
                  "tr",
                  { class: "admin__row" },
                  h("td", {}, label(s.source || "")),
                  h("td", { class: "admin__num" }, number(s.documents || 0)),
                ),
              ),
            ),
          )
        : h("p", { class: "access__aside" }, "Nothing at all, from any connector."),
    );
  }
}

/** field is one labelled input, returned with the input so it can be read. */
function field(name, id, hint) {
  const input = h("input", { class: "access__input", id, type: "text", autocomplete: "off", spellcheck: "false" });
  const el = h(
    "div",
    { class: "access__field" },
    h("label", { class: "access__label", for: id }, name),
    input,
    h("span", { class: "access__hint" }, hint),
  );
  return { el, input };
}

/**
 * rule turns the clause the server named into a sentence.
 *
 * The wire values are the permission rule's own names and they are the right
 * thing to send, but "owner-only" on a screen is a fragment rather than an
 * answer. Anything unrecognised is printed as it arrived, because a server
 * ahead of this file should still say something true.
 */
function rule(name) {
  switch (name) {
    case "owner":
      return "They own it";
    case "tenant":
      return "It is readable by everybody in the tenant";
    case "listed":
      return "They are on its access control list";
    default:
      return name || "The server did not say";
  }
}

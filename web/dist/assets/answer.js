// The answer above the results.
//
// There are two kinds and the region holds one of them at a time. Usually it
// holds passages taken out of the documents on the page below it, word for word,
// each one with the document it came from underneath it. Nothing in that kind is
// written by the product and nothing is paraphrased, which is a smaller claim
// than the one an assistant makes and the only one this build can keep: there is
// no model behind it yet.
//
// The other kind is prose a person in this company wrote and signed, for a
// question somebody asked often enough to be worth answering once. It takes the
// place of the quotes rather than sitting above them, because this region is the
// answer to the question in the box and two answers to one question is a reader
// deciding which of the product's own answers to believe. The server decides
// which arrives, and it never sends both.
//
// That is deliberate rather than a placeholder. The part of an answer surface
// that a model does not fix is where it sits, what a citation does when somebody
// clicks it, and how a reader tells a document's words from the product's. Those
// are settled here, against real documents, before there is anything generated
// to get them wrong.
//
// Two rules govern the region and both are about the list underneath it.
//
// It is absent when there is no answer, and absent means not rendered rather
// than rendered empty, so a query with nothing worth quoting gets the page it
// had before this existed.
//
// It never grows on its own. Every quote arrives in the same response as the
// results and in the same paint, its length is bounded by the server, and
// nothing inside it loads late. A region above a list that reflows after the
// list has painted moves the row somebody was about to click, and there is no
// amount of usefulness that pays for that.

import { h, replace, svg } from "genba/dom.js";
import { icon, label, sourceColor, when, exact } from "genba/format.js";
import { render as markdown } from "genba/markdown.js";

// The three states the server sends with a written answer, in the words the
// verification badge uses, since a reader who has learnt what an amber mark on a
// document means has learnt what it means here.
const STATES = new Set(["fresh", "expiring", "expired"]);

export class Answer {
  constructor({ onCite }) {
    this.onCite = onCite;
    this.el = h("div", { class: "answer", hidden: true });
  }

  /**
   * render draws the answer in a response, or takes the region away.
   *
   * The quotes name their documents by id and the documents are the hits of the
   * same response, so the title and the source under a quote are the same
   * strings as the result row it points at. A second copy on the wire would be a
   * second copy to disagree with the first.
   */
  render(res) {
    if (res && res.curated) {
      this.written(res.curated);
      return;
    }
    const answer = res && res.answer;
    const quotes = (answer && answer.quotes) || [];
    const hits = new Map((res.hits || []).map((hit) => [hit.id, hit]));
    const cited = quotes
      .map((q) => ({ quote: q, hit: hits.get(q.id) }))
      .filter((c) => c.hit);

    if (!cited.length) {
      this.clear();
      return;
    }

    this.el.hidden = false;
    replace(
      this.el,
      h(
        "section",
        { class: "answer__panel", "aria-labelledby": "answer-title" },
        h(
          "div",
          { class: "answer__head" },
          h("h2", { class: "answer__title", id: "answer-title" }, "From your documents"),
          // Said once, in the region, rather than left to the styling to imply.
          // A reader who assumes a paragraph above a list of results was written
          // for them has assumed the normal thing, and the styling that says
          // otherwise is the styling of a product they have never used before.
          h(
            "p",
            { class: "answer__note" },
            "Passages quoted from the documents below, word for word. Nothing here was written for you.",
          ),
        ),
        h(
          "ol",
          { class: "answer__list" },
          cited.map(({ quote, hit }) => this.renderQuote(quote, hit)),
        ),
      ),
    );
  }

  /**
   * written draws the answer somebody in this company wrote down.
   *
   * The three things it has to carry are the words, the name and the date. The
   * words are why a reader stops here instead of opening four documents, and the
   * name and the date are the whole reason they are allowed to. An unsigned
   * paragraph above a list of results is the product asserting something, and
   * this product does not assert things about a corpus it did not write.
   *
   * The body is markdown, rendered by the same renderer that draws a document,
   * so a list in an answer looks like a list in the document it came from. The
   * source of it never came from a source outside this deployment: somebody with
   * the administrator role typed it into this product.
   */
  written(a) {
    this.el.hidden = false;
    const state = STATES.has(a.state) ? a.state : "expired";
    const sources = a.sources || [];
    replace(
      this.el,
      h(
        "section",
        { class: "answer__panel answer__panel--written", "aria-labelledby": "answer-title" },
        h(
          "div",
          { class: "answer__head" },
          h("h2", { class: "answer__title", id: "answer-title" }, "Answer"),
          // The question as it was written down, which is not always the words
          // that were typed into the box. A reader who searched for a phrasing
          // somebody thought of as the same question deserves to see which
          // question they have been given the answer to.
          h("p", { class: "answer__question" }, a.question || ""),
        ),
        h("div", { class: "answer__body prose" }, markdown(a.body || "")),
        this.byline(a, state),
        sources.length
          ? h(
              "div",
              { class: "answer__sources" },
              h("h3", { class: "answer__sources-title" }, "Sources"),
              h(
                "ul",
                { class: "answer__source-list" },
                sources.map((hit) => this.renderSource(hit)),
              ),
            )
          : null,
      ),
    );
  }

  /**
   * byline is who wrote it and when they last stood behind it.
   *
   * An answer that has run out still says so and is still drawn. Taking it down
   * on the day it expires would leave the reader with silence, which tells them
   * nothing, and would leave the person who wrote it with no way to find out it
   * needs looking at.
   */
  byline(a, state) {
    const stale = state !== "fresh";
    return h(
      "p",
      { class: `answer__by${stale ? " answer__by--stale" : ""}` },
      // The name and the date are one sentence, so they are one element. Two
      // elements in the flex row below would be two things with a gap between
      // them, and the gap is meant to separate the sentence from the mark
      // beside it rather than a person from the day they wrote something.
      h(
        "span",
        { class: "answer__wrote", title: exact(a.at) },
        a.email
          ? h("a", { class: "answer__author", href: `mailto:${a.email}` }, a.by || a.email)
          : h("span", { class: "answer__author" }, a.by || "somebody"),
        ` wrote this ${when(a.at)}`,
      ),
      stale
        ? h(
            "span",
            {
              class: `verified verified--${state}`,
              title:
                state === "expired"
                  ? `Nobody has confirmed this since ${exact(a.until)}`
                  : `Due to be confirmed again ${exact(a.until)}`,
            },
            svg(icon("alert"), 14),
            state === "expired" ? "Not confirmed recently" : `Due for review ${when(a.until)}`,
          )
        : null,
    );
  }

  /**
   * renderSource is one of the documents the answer was drawn from.
   *
   * They arrive resolved through the reader asking, so a source this person
   * cannot open is simply not in the list. It is the same link a citation under
   * a quote is, opening the same preview, because there is one document viewer
   * in this product.
   */
  renderSource(hit) {
    return h(
      "li",
      { class: "answer__source" },
      h(
        "a",
        {
          class: "quote__cite",
          href: sourceHref(hit.id),
          "aria-label": `Open ${hit.title || hit.id}`,
          onClick: (e) => {
            if (e.metaKey || e.ctrlKey || e.shiftKey) return;
            e.preventDefault();
            this.onCite(hit.id, "");
          },
        },
        h("span", {
          class: "source__dot",
          style: { background: sourceColor(hit.source) },
        }),
        h("span", { class: "quote__title" }, hit.title || hit.id),
        h("span", { class: "quote__where" }, label(hit.source)),
        svg(icon("arrow-right"), 16),
      ),
    );
  }

  /**
   * clear takes the region off the page.
   *
   * Hidden and emptied, in that order and both of them. Hidden alone leaves the
   * quotes of the previous search in the tree for a screen reader to walk past
   * and for the node budget to count, and emptied alone leaves a box with the
   * margins of a panel and nothing in it.
   */
  clear() {
    this.el.hidden = true;
    replace(this.el);
  }

  renderQuote(quote, hit) {
    return h(
      "li",
      { class: "answer__quote" },
      h(
        "blockquote",
        { class: "quote" },
        h("p", { class: "quote__text" }, textOf(quote)),
      ),
      h(
        "a",
        {
          class: "quote__cite",
          href: citeHref(quote),
          // One action, and the same action from the pointer, the keyboard and
          // the middle button. It is a real anchor so that a citation can be
          // opened in a tab and sent to somebody, and the click is intercepted
          // so that the ordinary case does not reload the page.
          "aria-label": `Open ${hit.title || hit.id} at this passage`,
          onClick: (e) => {
            if (e.metaKey || e.ctrlKey || e.shiftKey) return;
            e.preventDefault();
            this.onCite(quote.id, quote.text);
          },
        },
        h("span", {
          class: "source__dot",
          style: { background: sourceColor(hit.source) },
        }),
        h("span", { class: "quote__title" }, hit.title || hit.id),
        h("span", { class: "quote__where" }, label(hit.source)),
        svg(icon("arrow-right"), 16),
      ),
    );
  }
}

/**
 * textOf builds the quote out of the runs the server split it into.
 *
 * The marked runs are the ones the index matched, which is the same claim a
 * result snippet makes and it is made in the same place. Searching the quote for
 * the words in the box here would highlight what a substring search found rather
 * than what was matched, and the two disagree on every stemmed word.
 */
function textOf(quote) {
  const parts = quote.passages && quote.passages.length ? quote.passages : [{ text: quote.text }];
  return parts.map((p) => (p.match ? h("mark", { class: "hit" }, p.text) : p.text));
}

/**
 * citeHref is the address a citation leads to: this search, with the document
 * open in the preview, at the passage.
 *
 * The same address the row for that document leads to, plus the passage, because
 * there is one document viewer in this product and a citation is a result. An
 * answer that opened its own reader would be a second place a document can be
 * looked at, and the second one is always the one that is a version behind.
 */
function citeHref(quote) {
  const params = new URLSearchParams(location.search);
  params.set("open", quote.id);
  params.set("at", quote.text);
  return `/?${params.toString()}`;
}

/**
 * sourceHref is the address a source of a written answer leads to.
 *
 * The same address without a passage, because the person who wrote the answer
 * cited a document rather than a sentence, and jumping the reader to a
 * highlighted line the author never pointed at would be the product inventing
 * the part of a citation that matters most.
 */
function sourceHref(id) {
  const params = new URLSearchParams(location.search);
  params.set("open", id);
  params.delete("at");
  return `/?${params.toString()}`;
}

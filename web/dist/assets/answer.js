// The answer above the results.
//
// It holds passages taken out of the documents on the page below it, word for
// word, each one with the document it came from underneath it. Nothing in here
// is written by the product and nothing is paraphrased, which is a smaller claim
// than the one an assistant makes and the only one this build can keep: there is
// no model behind it yet.
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
import { icon, label, sourceColor } from "genba/format.js";

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

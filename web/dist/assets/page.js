// A document as a page of its own.
//
// The drawer and this render the same document through the same function, and
// the difference is the frame around it. A drawer sits over a list somebody is
// coming back to, so it is narrow, it is modal on a phone and it closes. A page
// is what somebody was sent a link to, so it has the measure to read at, a way
// back to a list it may never have had, and no modality at all.
//
// It is the reason the title of a result can be an anchor. A middle click, a
// command click and the context menu all lead here, which is what makes the
// primary target on a row behave like a link on the web rather than like a
// widget that happens to look like one.

import { h, replace, svg } from "genba/dom.js";
import { api } from "genba/api.js";
import { cache } from "genba/cache.js";
import { copies } from "genba/clipboard.js";
import {
  icon,
  label,
  sourceColor,
  when,
  exact,
  followable,
  copyable,
} from "genba/format.js";
import { body as renderBody, shapeOf, detailOf } from "genba/content.js";
import { badge, note, control } from "genba/verify.js";
import { owner, reassign } from "genba/own.js";
import { reveal, toLine } from "genba/marks.js";
import { notPermitted, failedBody, failureTitle, NOT_AVAILABLE } from "genba/states.js";
import { documentPath } from "genba/state.js";

export class Page {
  constructor({ onBack, onSay }) {
    this.onBack = onBack;
    this.onSay = onSay || (() => {});
    this.currentKey = "";
    this.currentId = "";
    this.query = "";

    this.back = h("div", { class: "page__back" });
    this.meta = h("div", { class: "page__meta" });
    this.title = h("h1", { class: "page__title", tabindex: "-1" });
    this.content = h("div", { class: "page__body" });
    this.foot = h("div", { class: "page__foot" });

    this.el = h(
      "article",
      { class: "page" },
      this.back,
      h("header", { class: "page__head" }, this.meta, this.title),
      this.content,
      this.foot,
    );
  }

  async show(id, query = "") {
    this.query = query;
    this.currentId = id;
    const k = cache.key("document", { id });
    if (this.currentKey === k) return;
    this.currentKey = k;

    this.renderBack();
    // A page reached from a result row is usually already in the cache, because
    // the pointer rested on that row on the way to clicking it. The skeleton is
    // for the ones that are not, which is a reload and a link somebody sent.
    if (cache.read(k).state === "miss") this.skeleton();

    let painted = false;
    try {
      await cache.swr(
        k,
        (opts) => api.document(id, opts),
        (d) => {
          if (this.currentKey !== k) return;
          painted = true;
          this.render(d);
        },
      );
    } catch (err) {
      if (err.name === "AbortError") return;
      if (!painted && this.currentKey === k) this.renderError(err);
    }
  }

  /**
   * toLine moves to a line named by an address that arrived while this page was
   * already open.
   *
   * Following a link to a line in the file being read is not a navigation as far
   * as the browser is concerned. The path does not change, so nothing is fetched
   * and nothing is painted, and without this the page would sit exactly where it
   * was while the address bar claimed otherwise.
   */
  toLine() {
    toLine(this.content);
  }

  /**
   * renderBack draws the way out.
   *
   * Where it goes depends on how this page was reached, and the shell is what
   * knows that. A page opened in a new tab has no results to go back to and no
   * history to walk, so it offers the search instead of a button that would do
   * nothing.
   */
  renderBack() {
    const to = this.onBack();
    replace(
      this.back,
      h(
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
      ),
    );
  }

  skeleton() {
    replace(this.title, "Loading");
    replace(this.meta);
    replace(
      this.content,
      h("div", { class: "skeleton", style: { width: "100%", height: "14px", marginBottom: "8px" } }),
      h("div", { class: "skeleton", style: { width: "92%", height: "14px", marginBottom: "8px" } }),
      h("div", { class: "skeleton", style: { width: "70%", height: "14px" } }),
    );
    replace(this.foot);
  }

  render(d) {
    const heading = d.title || d.id;
    replace(this.title, heading);
    document.title = `${heading} · genba`;
    this.stamp(d);

    this.content.dataset.shape = shapeOf(d);
    replace(this.content, renderBody(d, { query: this.query }));

    // Focus goes to the heading rather than to the first control, because this
    // is a document somebody came to read and the top of it is where reading
    // starts. It carries tabindex="-1" so it can take focus without joining the
    // tab order.
    //
    // Without preventScroll it would also put the top of the document on the
    // screen, which is the one place a page opened at line four hundred must
    // not be. Focus and the scroll position are two different questions here:
    // reading starts at the heading and the eye starts at the match.
    this.title.focus({ preventScroll: true });
    if (!reveal(this.content)) this.el.scrollIntoView({ block: "start" });
  }

  /**
   * stamp draws everything about the document that is not the document.
   *
   * Verifying one redraws this and nothing else. Redrawing the body would lose
   * the scroll position and the marked words, and a page opened at line four
   * hundred would jump back to the top because somebody put their name to it.
   */
  stamp(d) {
    const detail = detailOf(d);
    replace(
      this.meta,
      h(
        "span",
        { class: "source" },
        h("span", { class: "source__dot", style: { background: sourceColor(d.source) } }),
        label(d.source),
      ),
      h("span", { class: "crumbs__sep" }, "·"),
      h("span", {}, label(d.kind)),
      detail && h("span", { class: "crumbs__sep" }, "·"),
      detail && h("span", {}, detail),
      d.container && h("span", { class: "crumbs__sep" }, "·"),
      d.container && h("span", { class: "crumbs" }, d.container),
      d.modified_at && h("span", { class: "crumbs__sep" }, "·"),
      d.modified_at &&
        h("time", { title: exact(d.modified_at), datetime: d.modified_at }, `Updated ${when(d.modified_at)}`),
      d.owner && h("span", { class: "crumbs__sep" }, "·"),
      owner(d.owner),
      d.verified && h("span", { class: "crumbs__sep" }, "·"),
      badge(d.verified, { by: true }),
    );
    replace(
      this.foot,
      actions(d, this.onSay),
      control(d, {
        onSay: this.onSay,
        onChange: (next) => {
          d.verified = next;
          this.stamp(d);
        },
      }),
      // Handing the document over is drawn beside vouching for it because the
      // two are the same decision from either end: the owner is who may vouch,
      // so the person reading this line is either the one who should put their
      // name to it or the one who knows whose name belongs there.
      reassign(d, {
        onSay: this.onSay,
        // The write answers with the owner and with what this reader may do
        // next, under the same names the document carries them, so merging it
        // in is the whole update. Somebody who has just given away a document
        // they did not write loses both controls here.
        onChange: (next) => {
          Object.assign(d, next);
          this.stamp(d);
        },
      }),
      note(d.verified),
    );
  }

  /**
   * renderError says the same thing for a document that is not there and for
   * one this viewer may not read.
   *
   * That is the whole permission model showing through the interface. A message
   * that distinguished the two would let anybody enumerate what exists by
   * reading the difference, which is exactly what the storage driver refuses to
   * tell the API.
   */
  renderError(err) {
    const missing = err.status === 404 || err.status === 403;
    replace(this.title, missing ? NOT_AVAILABLE : failureTitle(err));
    document.title = "genba";
    replace(this.meta);
    this.content.dataset.shape = "text";
    // A way out of both. Somebody who followed a link to a document they cannot
    // read is somewhere they did not choose to be, and an apology with no exit
    // is a dead end with good typography.
    replace(
      this.content,
      missing ? notPermitted(this.onBack()) : failedBody(err, () => this.retry()),
    );
    replace(this.foot);
    this.title.focus();
  }

  /** retry reads the document again, as if the address had just been opened. */
  retry() {
    const id = this.currentId;
    this.currentKey = "";
    if (id) this.show(id, this.query);
  }
}

/**
 * actions is the row under a document.
 *
 * Opening at the source is offered where a browser would go there. Where it
 * would not, which is every document the file connector read, the path takes
 * its place, because a path somebody can paste into a terminal is worth
 * something and a link that does nothing is worth less than no link.
 */
function actions(d, say) {
  const here = new URL(documentPath(d.id), location.origin).href;
  return [
    followable(d.url) &&
      h(
        "a",
        { class: "button button--primary", href: d.url, target: "_blank", rel: "noreferrer noopener" },
        "Open in source",
        svg(icon("external"), 16),
      ),
    h(
      "button",
      {
        class: "button",
        type: "button",
        onClick: (e) => copies(e.currentTarget, here, say),
      },
      svg(icon("link"), 16),
      "Copy link",
    ),
    d.url &&
      !followable(d.url) &&
      h(
        "button",
        {
          class: "button",
          type: "button",
          onClick: (e) => copies(e.currentTarget, copyable(d.url), say),
        },
        svg(icon("copy"), 16),
        "Copy path",
      ),
  ];
}

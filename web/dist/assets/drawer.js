// The preview drawer.
//
// Opening a result in its source is a context switch and a page load. Most of
// the time somebody wants to check whether this is the right document, and the
// drawer answers that without leaving the results.
//
// It is a modal dialog as far as assistive technology is concerned: focus goes
// in, Tab stays inside, Escape closes it and focus goes back where it was.

import { h, replace, svg } from "./dom.js";
import { api } from "./api.js";
import { icon, label, sourceColor, when, exact } from "./format.js";

export class Drawer {
  constructor({ onClose }) {
    this.onClose = onClose;
    this.returnTo = null;
    this.pending = null;

    this.title = h("h2", { class: "drawer__title", id: "drawer-title" });
    this.meta = h("div", { class: "drawer__meta" });
    this.body = h("div", { class: "drawer__body" });
    this.foot = h("div", { class: "drawer__foot" });

    this.scrim = h("div", { class: "scrim", hidden: true, onClick: () => this.close() });
    this.el = h(
      "div",
      {
        class: "drawer",
        role: "dialog",
        "aria-modal": "true",
        "aria-labelledby": "drawer-title",
        hidden: true,
        onKeydown: (e) => this.trap(e),
      },
      h(
        "div",
        { class: "drawer__head" },
        h("div", { class: "drawer__heading" }, this.meta, this.title),
        h(
          "button",
          { class: "icon-button", type: "button", "aria-label": "Close preview", onClick: () => this.close() },
          svg(icon("close"), 20),
        ),
      ),
      this.body,
      this.foot,
    );
  }

  get open() {
    return !this.el.hidden;
  }

  async show(id) {
    this.returnTo = document.activeElement;
    this.el.hidden = false;
    this.scrim.hidden = false;
    replace(this.title, "Loading");
    replace(this.meta);
    replace(
      this.body,
      h("div", { class: "skeleton", style: { width: "100%", height: "14px", marginBottom: "8px" } }),
      h("div", { class: "skeleton", style: { width: "92%", height: "14px", marginBottom: "8px" } }),
      h("div", { class: "skeleton", style: { width: "70%", height: "14px" } }),
    );
    replace(this.foot);
    this.el.focus();

    if (this.pending) this.pending.abort();
    const controller = new AbortController();
    this.pending = controller;
    try {
      const d = await api.document(id, controller.signal);
      if (controller.signal.aborted) return;
      this.render(d);
    } catch (err) {
      if (err.name === "AbortError") return;
      this.renderError(err);
    }
  }

  render(d) {
    replace(this.title, d.title || d.id);
    replace(
      this.meta,
      h(
        "span",
        { class: "source" },
        h("span", { class: "source__dot", style: { background: sourceColor(d.source) } }),
        label(d.source),
      ),
      h("span", { class: "crumbs__sep" }, "/"),
      h("span", {}, label(d.kind)),
      d.container && h("span", { class: "crumbs__sep" }, "/"),
      d.container && h("span", {}, d.container),
    );
    replace(this.body, d.body || "This document has no text body.");
    replace(
      this.foot,
      d.url &&
        h(
          "a",
          { class: "button button--primary", href: d.url, target: "_blank", rel: "noreferrer noopener" },
          "Open in source",
          svg(icon("external"), 16),
        ),
      d.modified_at &&
        h("span", { class: "meta", title: exact(d.modified_at) }, `Updated ${when(d.modified_at)}`),
    );
    // Focus lands on the first thing worth acting on once the content is in,
    // rather than on a spinner.
    const first = this.el.querySelector("a, button");
    if (first) first.focus();
  }

  renderError(err) {
    const missing = err.status === 404;
    replace(this.title, missing ? "Not available" : "Could not load this document");
    replace(this.meta);
    replace(
      this.body,
      missing
        ? "This document no longer exists, or you do not have access to it."
        : err.message,
    );
    replace(this.foot);
  }

  close() {
    if (!this.open) return;
    if (this.pending) this.pending.abort();
    this.el.hidden = true;
    this.scrim.hidden = true;
    if (this.returnTo && this.returnTo.focus) this.returnTo.focus();
    this.onClose();
  }

  /** trap keeps Tab inside the dialog while it is open. */
  trap(e) {
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      this.close();
      return;
    }
    if (e.key !== "Tab") return;
    const focusable = this.el.querySelectorAll(
      'a[href], button:not([disabled]), input, [tabindex]:not([tabindex="-1"])',
    );
    if (!focusable.length) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }
}

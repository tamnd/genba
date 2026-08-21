// The omnibox: one input for everything.
//
// There is one search box in the product and it is this one. It takes text,
// operators and a document you already know the name of, and it routes on what
// was typed rather than making somebody choose a mode first.
//
// The markup is a real ARIA combobox over a listbox, because this is a control
// that people drive with the keyboard and a screen reader has to be told what
// the arrow keys are doing.

import { h, replace, svg } from "./dom.js";
import { api } from "./api.js";
import { cache } from "./cache.js";
import { icon } from "./format.js";

// DEBOUNCE is how long the box waits before asking the server. The suggestion
// budget is 80ms at the server, and this is the other half of feeling instant:
// asking on every keystroke would spend that budget on requests nobody reads.
const DEBOUNCE = 90;

export class Omnibox {
  constructor({ onSearch, onOpen, onHighlight }) {
    this.onSearch = onSearch;
    this.onOpen = onOpen;
    this.onHighlight = onHighlight || (() => {});
    this.items = [];
    this.active = -1;
    this.timer = null;
    this.latest = "";

    this.input = h("input", {
      class: "omnibox__input",
      type: "text",
      role: "combobox",
      id: "omnibox-input",
      placeholder: "Search everything, or type app: to filter",
      autocomplete: "off",
      spellcheck: "false",
      "aria-expanded": "false",
      "aria-controls": "omnibox-listbox",
      "aria-autocomplete": "list",
      "aria-label": "Search",
      onInput: () => this.schedule(),
      onKeydown: (e) => this.key(e),
      onFocus: () => this.schedule(),
      onBlur: () => setTimeout(() => this.close(), 120),
    });

    this.list = h("ul", {
      class: "suggestions",
      id: "omnibox-listbox",
      role: "listbox",
      "aria-label": "Suggestions",
      hidden: true,
    });

    this.el = h(
      "div",
      { class: "omnibox", role: "search" },
      h(
        "div",
        { class: "omnibox__field" },
        h("span", { class: "omnibox__icon" }, svg(icon("search"))),
        this.input,
        h("kbd", { class: "kbd" }, shortcutLabel()),
      ),
      this.list,
    );
  }

  /** value keeps the box in step with the URL without firing a search. */
  set value(v) {
    if (this.input.value !== v) this.input.value = v;
  }

  get value() {
    return this.input.value;
  }

  focus() {
    this.input.focus();
    this.input.select();
  }

  schedule() {
    clearTimeout(this.timer);
    const q = this.input.value.trim();
    if (!q) {
      this.close();
      return;
    }
    this.timer = setTimeout(() => this.fetch(q), DEBOUNCE);
  }

  async fetch(q) {
    // Only the newest prefix may render. Without this a slow response for two
    // letters lands after the fast one for five and the list goes backwards
    // while somebody is still typing. The request itself is not cancelled: it
    // is on its way to the cache, and backspacing one letter is the single most
    // likely next thing anybody does in a search box.
    this.latest = q;
    try {
      await cache.swr(
        cache.key("suggest", { q }),
        (opts) => api.suggest(q, opts),
        (res) => {
          if (this.latest !== q) return;
          this.render(res.suggestions || []);
        },
      );
    } catch (err) {
      if (this.latest === q && err.name !== "AbortError") this.close();
    }
  }

  render(items) {
    this.items = items;
    this.active = -1;
    if (!items.length) {
      this.close();
      return;
    }
    replace(
      this.list,
      items.map((item, i) =>
        h(
          "li",
          {
            class: "suggestion",
            role: "option",
            id: `omnibox-option-${i}`,
            "aria-selected": "false",
            onMousedown: (e) => {
              // mousedown rather than click, because the input blurs first and
              // the blur handler would have closed the list by then.
              e.preventDefault();
              this.choose(i);
            },
            onMouseenter: () => this.highlight(i),
          },
          h("span", { class: "omnibox__icon" }, svg(icon(item.kind === "operator" ? "slider" : "doc"), 14)),
          h("span", { class: "suggestion__text" }, item.text),
          item.hint && h("span", { class: "suggestion__hint" }, item.hint),
          item.kind === "operator" && h("span", { class: "suggestion__badge" }, "filter"),
        ),
      ),
    );
    this.list.hidden = false;
    this.input.setAttribute("aria-expanded", "true");
  }

  close() {
    this.list.hidden = true;
    this.input.setAttribute("aria-expanded", "false");
    this.input.removeAttribute("aria-activedescendant");
    this.active = -1;
  }

  highlight(i) {
    const options = this.list.children;
    for (let n = 0; n < options.length; n++) {
      options[n].setAttribute("aria-selected", String(n === i));
    }
    this.active = i;
    if (i >= 0) {
      this.input.setAttribute("aria-activedescendant", `omnibox-option-${i}`);
      options[i].scrollIntoView({ block: "nearest" });
    } else {
      this.input.removeAttribute("aria-activedescendant");
    }
    // The shell decides whether a highlighted row is worth fetching ahead of
    // Enter being pressed, and how long it has to stay highlighted first.
    this.onHighlight(this.items[i] || null);
  }

  choose(i) {
    const item = this.items[i];
    if (!item) return;
    if (item.kind === "operator") {
      // An operator completes the box and leaves the caret in it, because the
      // next thing to type is the value and nobody wants to search for "app:".
      this.input.value = item.value || item.text;
      this.input.focus();
      this.schedule();
      return;
    }
    this.close();
    this.onOpen(item.id, item.url);
  }

  key(e) {
    const open = !this.list.hidden;
    switch (e.key) {
      case "ArrowDown":
        if (!open) return;
        e.preventDefault();
        this.highlight((this.active + 1) % this.items.length);
        break;
      case "ArrowUp":
        if (!open) return;
        e.preventDefault();
        this.highlight((this.active - 1 + this.items.length) % this.items.length);
        break;
      case "Enter":
        e.preventDefault();
        if (open && this.active >= 0) {
          this.choose(this.active);
          return;
        }
        this.close();
        this.onSearch(this.input.value);
        break;
      case "Escape":
        if (open) {
          e.preventDefault();
          e.stopPropagation();
          this.close();
        }
        break;
      default:
        break;
    }
  }
}

const MAC = navigator.platform.toLowerCase().includes("mac");

/** modifierLabel is the key a platform holds down for its own shortcuts. */
export function modifierLabel() {
  return MAC ? "⌘" : "Ctrl";
}

/** shortcutLabel matches the key the platform actually uses. */
export function shortcutLabel() {
  return MAC ? "⌘K" : "Ctrl K";
}

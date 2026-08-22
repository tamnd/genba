// The settings screen.
//
// Four sections and nothing else: how this looks, which keys do what, who the
// server thinks you are, and what it is running. Nothing on it writes to the
// server. Everything it changes is local to this browser, which is what keeps
// it a screen rather than a feature, and it is why there is no save button
// anywhere on it: a choice applies as it is made.
//
// It is also where somebody is sent when a document tells them they may not
// read it, because the first question then is which account they are and which
// groups came with it. That answer has to be the server's rather than whatever
// this browser last sent, so it is read from the me response rather than from
// local storage.

import { h, replace, svg } from "genba/dom.js";
import { api, identity } from "genba/api.js";
import { cache } from "genba/cache.js";
import { SHORTCUTS, keysOf } from "genba/keys.js";
import { DENSITIES, THEMES, density, setDensity, setTheme, theme } from "genba/prefs.js";
import { exact, icon, label, number, when } from "genba/format.js";

export class Settings {
  constructor({ onBack, onIdentity, onSay }) {
    this.onBack = onBack;
    this.onIdentity = onIdentity;
    this.onSay = onSay || (() => {});
    this.session = null;
    this.health = null;
    this.stats = null;
    this.title = null;
    this.el = h("div", { class: "settings" });
  }

  /**
   * render paints the screen and then fills in what the server says.
   *
   * The two halves that need a request are the last two sections, and neither
   * is worth a skeleton: the appearance and keyboard sections are complete
   * without the network, so the screen is useful at the first frame and the
   * facts arrive under it a moment later.
   */
  async render(session) {
    this.session = session;
    this.paint();

    const [health, stats] = await Promise.all([
      api.health().catch(() => null),
      cache
        .swr(cache.key("stats", {}), (opts) => api.stats(opts), () => {})
        .catch(() => null),
    ]);
    // The address bar may have moved on while those were in flight, and a
    // screen that has been taken off the page should not be repainted under
    // whatever replaced it.
    if (!this.el.isConnected) return;
    this.health = health;
    this.stats = stats;
    this.paint();
  }

  paint() {
    // Whether anything on this screen already has focus, read before the paint
    // takes it away. Arriving here from a key rather than from a click has to
    // land somewhere, and the heading is where, but a repaint underneath
    // somebody who is on a radio must not pull them back to the top.
    const held = this.el.contains(document.activeElement) && document.activeElement !== this.title;

    this.title = h("h1", { class: "settings__title", tabindex: "-1" }, "Settings");
    replace(
      this.el,
      this.backLink(),
      h(
        "header",
        { class: "settings__head" },
        this.title,
        h(
          "p",
          { class: "settings__lead" },
          "Everything here is kept in this browser and none of it is sent to the server.",
        ),
      ),
      h(
        "div",
        { class: "settings__grid" },
        this.appearance(),
        this.keyboard(),
        this.who(),
        this.server(),
      ),
    );
    if (!held) this.title.focus();
  }

  /**
   * backLink is the way out, which is wherever this screen was opened from.
   *
   * A tab that opened straight onto this address has nothing behind it, so it
   * is offered the search rather than a button that would land on whatever else
   * that tab was doing.
   */
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
            to.go();
          },
        },
        svg(icon("arrow-left"), 16),
        to.title,
      ),
    );
  }

  appearance() {
    return h(
      "section",
      { class: "panel" },
      h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, "Appearance")),
      // Nothing repaints on a pick. The radio the browser just ticked is
      // already showing the answer, and rebuilding the screen under somebody's
      // hand would take the focus off the control they are using.
      this.choice("Theme", THEMES, theme(), (value) => {
        setTheme(value);
        this.onSay(`Theme ${labelOf(THEMES, value).toLowerCase()}`);
      }),
      this.choice("Density", DENSITIES, density(), (value) => {
        setDensity(value);
        this.onSay(`Density ${labelOf(DENSITIES, value).toLowerCase()}`);
      }),
    );
  }

  /**
   * choice is one set of options, as radios.
   *
   * They are real radio inputs in a real fieldset rather than buttons dressed
   * up as one, because a group of options where exactly one is chosen is what a
   * radio group is, and the browser already knows how to walk one from the
   * keyboard and how to say what it is.
   */
  choice(title, options, chosen, onPick) {
    const name = `settings-${title.toLowerCase()}`;
    return h(
      "fieldset",
      { class: "choice" },
      h("legend", { class: "choice__legend" }, title),
      options.map((option) =>
        h(
          "label",
          { class: "choice__option" },
          h("input", {
            class: "choice__input",
            type: "radio",
            name,
            value: option.value,
            checked: option.value === chosen,
            onChange: () => onPick(option.value),
          }),
          h(
            "span",
            { class: "choice__text" },
            h("span", { class: "choice__label" }, option.label),
            option.hint ? h("span", { class: "choice__hint" }, option.hint) : null,
          ),
        ),
      ),
    );
  }

  /**
   * keyboard is the list of keys, generated from the table the handler reads.
   *
   * Not typed out again. A list of shortcuts written beside the code that
   * implements them drifts from it in about two commits, and a list that is
   * wrong about the keys is worse than no list.
   */
  keyboard() {
    return h(
      "section",
      { class: "panel" },
      h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, "Keyboard")),
      SHORTCUTS.map((row) => h("div", { class: "shortcut" }, row.what, keysOf(row))),
    );
  }

  who() {
    const session = this.session || {};
    const local = identity();
    return h(
      "section",
      { class: "panel" },
      h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, "Identity")),
      facts([
        ["Subject", session.subject || local.subject],
        ["Tenant", session.tenant || "the single tenant deployment"],
        ["Groups", (session.groups || []).join(", ")],
        ["Roles", (session.roles || []).join(", ")],
        ["Source identities", local.identities.join(", ")],
      ]),
      h(
        "p",
        { class: "meta" },
        "This is what the server resolved from the request, and it is what decides which documents come back.",
      ),
      h(
        "button",
        { class: "button", type: "button", onClick: () => this.onIdentity() },
        "Search as somebody else",
      ),
    );
  }

  server() {
    const health = this.health || {};
    const stats = this.stats || {};
    return h(
      "section",
      { class: "panel" },
      h("div", { class: "panel__head" }, h("h2", { class: "panel__title" }, "Server")),
      facts([
        ["Version", health.version],
        ["Commit", health.commit],
        ["Built", health.built === "unknown" ? "" : exact(health.built) || health.built],
        ["Store", stats.driver ? label(stats.driver) : ""],
        // Which of the two query paths this deployment is on. It is here rather
        // than left to be inferred from the clock because the two answer the
        // same queries with the same results, and on a few hundred documents
        // they are a hundred times apart.
        ["Queries", this.stats ? (stats.ranking ? "ranked in the store" : "scanned one at a time") : ""],
        ["Documents", this.stats ? number(stats.documents) : ""],
        ["Held back", this.stats ? number(stats.quarantined) : ""],
        ["Last indexed", stats.indexed_at ? when(stats.indexed_at) : "not since this process started"],
        ["Up", health.uptime],
      ]),
    );
  }
}

/**
 * facts is a list of named values, with the ones nobody answered left out.
 *
 * An empty row would be a fact this screen claims to know and does not, which
 * on the one screen somebody comes to for the truth about their session is the
 * worst thing it could print.
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

function labelOf(options, value) {
  const found = options.find((option) => option.value === value);
  return found ? found.label : value;
}

// The keyboard, as one table.
//
// Every key this interface answers is a row here: what it does, how it is
// written down, which press reaches it and the code that runs. The shell's
// listener dispatches through this table and both places that print a list of
// shortcuts read the same rows, which is the whole point of the file. A list
// kept next to the switch that implements it drifts from it in about two
// commits, and a list of keys that is wrong about the keys is worse than no
// list at all.
//
// A row's run is handed the shell and the event, and does its own guarding. A
// key that has nothing to act on is a key the browser should still get: j on
// the home screen is a j, not a silent no-op somewhere else, so a run that
// finds no list returns without preventing the default.

import { h } from "genba/dom.js";
import { documentPath, RECENT, SETTINGS } from "genba/state.js";
import { followable } from "genba/format.js";

const MAC = navigator.platform.toLowerCase().includes("mac");

/** modifierLabel is the key a platform holds down for its own shortcuts. */
export function modifierLabel() {
  return MAC ? "⌘" : "Ctrl";
}

/** shortcutLabel matches the key the platform actually uses. */
export function shortcutLabel() {
  return MAC ? "⌘K" : "Ctrl K";
}

/**
 * CHORD is the key that arms a two key sequence.
 *
 * It arms and disarms itself, so a stray press does nothing rather than waiting
 * forever for a second key.
 */
export const CHORD = "g";

/** CHORD_TIMEOUT is how long an armed sequence waits for its second key. */
export const CHORD_TIMEOUT = 1200;

/**
 * SHORTCUTS is every key, in the order the lists print them.
 *
 * keys is a sequence of chords, and a chord is the labels pressed together, so
 * [["g", "h"]] is g then h and [["j"], ["↓"]] is either of two ways to do the
 * same thing. on is the matching half: one descriptor per way in, where mod is
 * the platform's own modifier and typing says the press is answered even when
 * the caret is in a field.
 */
export const SHORTCUTS = [
  {
    what: "Focus search",
    keys: [[shortcutLabel()], ["/"]],
    on: [{ key: "k", mod: true, typing: true }, { key: "/" }],
    run: (app, e) => {
      e.preventDefault();
      app.omnibox.focus();
    },
  },
  {
    what: "Next result",
    keys: [["j"], ["↓"]],
    on: [{ key: "j" }, { key: "ArrowDown" }],
    run: (app, e) => move(app, e, 1),
  },
  {
    what: "Previous result",
    keys: [["k"], ["↑"]],
    on: [{ key: "k" }, { key: "ArrowUp" }],
    run: (app, e) => move(app, e, -1),
  },
  {
    what: "First result",
    keys: [["Home"]],
    on: [{ key: "Home" }],
    run: (app, e) => edge(app, e, "first"),
  },
  {
    what: "Last result",
    keys: [["End"]],
    on: [{ key: "End" }],
    run: (app, e) => edge(app, e, "last"),
  },
  {
    what: "Open preview",
    keys: [["Enter"], ["p"]],
    on: [{ key: "Enter" }, { key: "p" }],
    run: (app, e) => {
      const hit = current(app);
      if (!hit) return;
      e.preventDefault();
      app.open(hit.id);
    },
  },
  {
    what: "Next match in the preview",
    keys: [["n"]],
    on: [{ key: "n" }],
    run: (app, e) => match(app, e, 1),
  },
  {
    what: "Previous match in the preview",
    keys: [["shift", "n"]],
    on: [{ key: "N" }],
    run: (app, e) => match(app, e, -1),
  },
  {
    // The modifier means the same thing here as it does on a link: open it over
    // there and leave me where I am.
    what: "Open as a page, in a new tab",
    keys: [[modifierLabel(), "Enter"]],
    on: [{ key: "Enter", mod: true }],
    run: (app, e) => {
      const hit = current(app);
      if (!hit) return;
      e.preventDefault();
      window.open(documentPath(hit.id, app.query.q), "_blank", "noreferrer");
    },
  },
  {
    what: "Open in source",
    keys: [["o"]],
    on: [{ key: "o" }],
    run: (app, e) => {
      // Only where a browser would go there. Opening a file:// URL in a new tab
      // from an HTTP page leaves somebody looking at a blank tab, which is
      // worse than the key doing nothing.
      const hit = current(app);
      if (!hit || !followable(hit.url)) return;
      e.preventDefault();
      window.open(hit.url, "_blank", "noreferrer");
    },
  },
  {
    what: "Copy a link to this result",
    keys: [["y"]],
    on: [{ key: "y" }],
    run: (app, e) => {
      if (app.copyLink()) e.preventDefault();
    },
  },
  {
    what: "Previous page",
    keys: [["["]],
    on: [{ key: "[" }],
    run: (app, e) => {
      if (app.page(-1)) e.preventDefault();
    },
  },
  {
    what: "Next page",
    keys: [["]"]],
    on: [{ key: "]" }],
    run: (app, e) => {
      if (app.page(1)) e.preventDefault();
    },
  },
  {
    what: "Filters",
    keys: [["f"]],
    on: [{ key: "f" }],
    run: (app, e) => {
      // Not from inside the preview, which is modal. Moving focus behind a
      // modal is the one way out of a focus trap that nobody asked for, and not
      // from a screen with no filters on it either.
      if (app.drawer.open || !app.showingResults()) return;
      e.preventDefault();
      app.results.focusFilters();
    },
  },
  {
    what: "Go home",
    keys: [["g", "h"]],
    on: [{ key: "h", chord: CHORD }],
    run: (app, e) => {
      e.preventDefault();
      app.goHome();
    },
  },
  {
    what: "Recent",
    keys: [["g", "s"]],
    on: [{ key: "s", chord: CHORD }],
    run: (app, e) => {
      e.preventDefault();
      app.visit(RECENT);
    },
  },
  {
    what: "Settings",
    keys: [["g", ","]],
    on: [{ key: ",", chord: CHORD }],
    run: (app, e) => {
      e.preventDefault();
      app.visit(SETTINGS);
    },
  },
  {
    // Not one action but the way out of whatever is open, which is why it is
    // the one row that is answered while the caret is in a field.
    what: "Close",
    keys: [["Esc"]],
    on: [{ key: "Escape", typing: true }],
    run: (app, e) => {
      if (app.drawer.open) return; // the drawer handles its own Escape
      if (typing(e.target)) e.target.blur();
    },
  },
  {
    what: "This list",
    keys: [["?"]],
    on: [{ key: "?" }],
    run: (app, e) => {
      e.preventDefault();
      app.shortcuts();
    },
  },
];

/**
 * binding is the row a press reaches, or nothing.
 *
 * chord is the key already armed, if any. A row that names one is unreachable
 * without it and a row that names none is unreachable with it, so an armed g
 * cannot be followed by a stray j that scrolls the list.
 */
export function binding(e, chord = "") {
  // Alt is the one modifier nothing here claims. It is how a keyboard produces
  // characters this table does not know about, and swallowing those is how an
  // interface stops working in a language it was not tested in.
  if (e.altKey) return null;
  const inField = typing(e.target);
  for (const row of SHORTCUTS) {
    for (const on of row.on) {
      if ((on.chord || "") !== chord) continue;
      if (Boolean(on.mod) !== Boolean(e.metaKey || e.ctrlKey)) continue;
      if (inField && !on.typing) continue;
      if (on.mod ? e.key.toLowerCase() !== on.key : e.key !== on.key) continue;
      return row;
    }
  }
  return null;
}

/**
 * keysOf draws one row's keys, for whichever list is printing them.
 *
 * A chord is its keys side by side, which reads as press them in order, and two
 * chords are separated by a word, which reads as either of these.
 */
export function keysOf(row) {
  const out = [];
  for (const chord of row.keys) {
    if (out.length) out.push(h("span", { class: "shortcut__or" }, "or"));
    for (const key of chord) out.push(h("kbd", { class: "kbd" }, key));
  }
  return h("span", { class: "shortcut__keys" }, out);
}

/** arms reports whether a press should start a two key sequence. */
export function arms(e) {
  return e.key === CHORD && !e.metaKey && !e.ctrlKey && !e.altKey && !typing(e.target);
}

/** typing reports whether the caret is somewhere a key means a character. */
export function typing(target) {
  return Boolean(target && target.matches && target.matches("input, textarea, select"));
}

function current(app) {
  const rows = app.rows();
  return (rows && rows.current()) || null;
}

// The arrow keys move inside the list and only inside it. A page has one
// scrollbar and somebody reading a preview with the arrow keys is scrolling it,
// so these are answered where the pattern says they are: when focus is in the
// widget they belong to. j and k are not that, which is why they also move the
// preview.
function move(app, e, delta) {
  if (e.key.startsWith("Arrow") && !app.inList()) return;
  const rows = app.rows();
  if (!rows) return;
  e.preventDefault();
  // With the preview open these keys move the preview. Reading through five
  // candidates used to be five open and close cycles, each of which lost the
  // place in the list.
  const stepping = app.drawer.open && !e.key.startsWith("Arrow");
  rows.move(delta, { focus: !stepping });
  if (stepping) app.step(delta);
}

function edge(app, e, where) {
  const rows = app.rows();
  if (app.drawer.open || !rows || !rows.hits.length) return;
  e.preventDefault();
  rows.edge(where);
}

// The matches inside the preview, in the same direction j and k move between
// documents. It is answered by the shell rather than by the drawer so that it
// works wherever focus happens to be, which above the breakpoint is not
// necessarily inside the drawer.
function match(app, e, delta) {
  if (!app.drawer.open) return;
  e.preventDefault();
  app.drawer.toMatch(delta);
}

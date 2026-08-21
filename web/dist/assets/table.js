// The frame around a table.
//
// A table written in markdown and a table lifted out of a page of HTML are the
// same table by the time anybody looks at it, so the two rules that make a real
// one readable live here rather than in each renderer.
//
// Real tables are wider than the measure and longer than a preview. Widening
// the column to fit one is what pushes the text of every other document on the
// screen out of shape, and showing four hundred rows of a spreadsheet in a
// preview is showing somebody the spreadsheet instead of telling them whether
// it is the one they wanted.

import { h } from "./dom.js";

// How much of a long table the preview shows, and the length at which it counts
// as long. A table of thirty rows is read in one go and folding it would be an
// interruption. A table of four hundred is a document of its own.
const KEEP = 20;
const LONG = 50;

/**
 * frame wraps a table in the region that scrolls it.
 *
 * The region takes focus. A region that scrolls and cannot be focused is a
 * table whose right hand columns can only be reached with a pointer, which is
 * the finding every site with a wide table eventually gets written up for. It
 * is named for the same reason: a focus stop with no name is announced as a
 * group and nothing else, and a person tabbing through a document deserves to
 * be told what they have landed in.
 */
export function frame(table) {
  const rows = [...(table.tBodies[0] ? table.tBodies[0].rows : [])];
  const long = rows.length > LONG;

  const region = h(
    "div",
    {
      class: "prose__scroll",
      tabindex: "0",
      role: "group",
      "aria-label": rows.length ? `Table, ${rows.length} rows` : "Table",
    },
    table,
  );
  if (!long) return region;

  for (const row of rows.slice(KEEP)) row.hidden = true;
  const rest = rows.length - KEEP;
  const more = h(
    "button",
    {
      class: "button button--ghost prose__more",
      type: "button",
      onClick: (e) => {
        for (const row of rows) row.hidden = false;
        // The control is what said the table was folded, so it goes away when
        // it is not, and focus goes to the table rather than to the space the
        // button used to be in.
        e.currentTarget.remove();
        region.focus();
      },
    },
    `Show the remaining ${rest} rows`,
  );
  return h("div", { class: "prose__folded" }, region, more);
}

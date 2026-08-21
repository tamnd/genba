// Copying something, and saying that it happened.
//
// Three views offer a copy button and all three need the same two things after
// the write: a visible change on the control, because a copy that looks like
// nothing is a copy somebody does twice, and a spoken one, because a tick
// appearing is not something a screen reader announces.

import { replace, svg } from "./dom.js";
import { icon } from "./format.js";

// How long the tick stays. Long enough to be seen after the eye has moved back
// to the button, short enough that the control is itself again before anybody
// wants to press it a second time.
const CONFIRM = 1600;

/**
 * copies writes text to the clipboard and reports what happened.
 *
 * The button's own contents are held rather than rebuilt, so this works for the
 * icon buttons on a result row and for the labelled buttons under a document
 * without either caller describing itself twice.
 *
 * The clipboard is refused outside a secure context and by a browser that has
 * decided this was not a user gesture, and neither is an error worth a dialog.
 * What is worth saying is what did not get copied, so the failure names it and
 * the person can select it themselves.
 */
export async function copies(button, text, say = () => {}) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    say(`Could not copy ${text}`);
    return;
  }
  say(`Copied ${text}`);

  const held = Array.from(button.childNodes);
  const labelled = button.hasAttribute("aria-label");
  const label = button.getAttribute("aria-label");
  const worded = held.some((node) => node.nodeType === Node.TEXT_NODE);

  replace(button, svg(icon("check"), 18), worded && "Copied");
  if (labelled) button.setAttribute("aria-label", "Copied");

  setTimeout(() => {
    replace(button, held);
    if (labelled) button.setAttribute("aria-label", label);
  }, CONFIRM);
}

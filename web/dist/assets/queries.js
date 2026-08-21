// The searches this person has run, kept on their own machine.
//
// This is the one half of the recent screen that does not come from the server,
// and the split is deliberate. What somebody opened is a document, and the title
// that makes a list of documents useful is corpus content, so it is read back
// from the server under the same permission rule as everything else and it
// follows them from the laptop to the desk machine. A query is their own input.
// It is not corpus content, nobody else needs it, and it is the thing that makes
// an empty search box useful, so it stays here.
//
// localStorage is allowed to hold this for the same reason it is allowed to hold
// the theme and nothing else: there are no titles and no excerpts in it. It is
// emptied by the identity switcher along with the result cache, because the next
// person at this machine is not entitled to know what the last one was looking
// for.

const KEY = "genba.recent";

// Twenty, which is what the client cache specification says. It is a list
// somebody glances at rather than searches, and the entries below the twentieth
// are ones they would type again faster than they would find.
const LIMIT = 20;

/** queries is what was searched for, most recent first. */
export function queries() {
  try {
    const raw = localStorage.getItem(KEY);
    if (!raw) return [];
    const held = JSON.parse(raw);
    // Anything that is not a list of strings is something else's key or a half
    // written value, and an empty history is a better answer than a broken
    // screen.
    if (!Array.isArray(held)) return [];
    return held.filter((q) => typeof q === "string" && q.trim()).slice(0, LIMIT);
  } catch {
    return [];
  }
}

/**
 * remember records one search.
 *
 * Repeating a search moves it to the top rather than adding a second copy of it,
 * because a list of the last twenty searches that is nine copies of the one
 * somebody is iterating on is a list with eleven entries.
 */
export function remember(q) {
  const text = (q || "").trim();
  if (!text) return;
  const next = [text, ...queries().filter((held) => held !== text)].slice(0, LIMIT);
  try {
    localStorage.setItem(KEY, JSON.stringify(next));
  } catch {
    // A full or disabled store means no history, which costs a convenience and
    // nothing else. It is not worth an error on a screen.
  }
}

/** forget empties the history, which is what switching identity does. */
export function forget() {
  try {
    localStorage.removeItem(KEY);
  } catch {
    // Same reasoning as above, one step further: a store that will not delete
    // is a store that was never written to.
  }
}

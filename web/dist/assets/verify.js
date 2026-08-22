// Who put their name to a document, and when that stops counting.
//
// The badge is a claim by a named person with a date on it rather than a tick,
// because a tick nobody ever unsets is noise within a year. So there are three
// states and all three are drawn: current, running out, and run out. A document
// somebody verified in 2023 and not since keeps its badge and the badge says
// so, which is more useful than a document nobody ever looked at and reads as
// the warning it is.
//
// Which state a claim is in is decided by the server and sent down with it.
// Working it out here from the date would put the cadence and the warning
// window in two places, and the browser's copy would be the one drawing an
// amber badge a fortnight after the server thinks it should be red.

import { h, svg } from "genba/dom.js";
import { api } from "genba/api.js";
import { cache } from "genba/cache.js";
import { icon, when, exact } from "genba/format.js";

// The three states the server sends. Anything else is read as expired, which is
// the safe way to be wrong: a badge that overstates how current a document is
// costs somebody an hour, and one that understates it costs them a click.
const STATES = new Set(["fresh", "expiring", "expired"]);

function stateOf(v) {
  return STATES.has(v.state) ? v.state : "expired";
}

/**
 * badge is the mark on a document that somebody vouched for it.
 *
 * It takes the name only where there is room for it, which is a preview or a
 * page rather than a result row. On a row the name is in the tooltip: the row
 * already carries a source, a kind, a folder, an author and a date, and a sixth
 * name on that line is the line nobody reads.
 *
 * compact is the grid, where the label under a picture is one line that already
 * truncates. The words are still there and still read out, they are just not
 * drawn, because the alternative is a mark with no name on it at all and a
 * picture nobody can tell is stale.
 */
export function badge(v, opts = {}) {
  if (!v || !v.at) return null;
  const state = stateOf(v);
  const said = words(v, state, opts.by);
  return h(
    "span",
    { class: `verified verified--${state}`, title: sentence(v, state) },
    svg(icon(state === "expired" ? "alert" : "check"), 14),
    opts.compact ? h("span", { class: "visually-hidden" }, said) : said,
  );
}

/** words is the badge itself, which is short by the time it reaches a row. */
function words(v, state, withName) {
  const named = withName && v.by ? `Verified by ${v.by}` : "Verified";
  switch (state) {
    case "fresh":
      return named;
    case "expiring":
      return `${named}, expires ${when(v.until)}`;
    default:
      return withName && v.by
        ? `${named}, expired ${when(v.until)}`
        : `Verification expired ${when(v.until)}`;
  }
}

/**
 * sentence is the whole claim, for the tooltip.
 *
 * The address is in it because the point of a signal that is a person rather
 * than a flag is that a reader who disagrees with the claim has somebody to ask.
 */
function sentence(v, state) {
  const who = v.email ? `${v.by} (${v.email})` : v.by;
  const parts = [
    `Verified by ${who || "somebody"} on ${exact(v.at)}`,
    state === "expired" ? `Expired ${exact(v.until)}` : `Current until ${exact(v.until)}`,
  ];
  if (v.note) parts.push(v.note);
  return parts.join(". ");
}

/**
 * note is the verifier's own sentence, where they left one.
 *
 * It is the case a badge cannot express: verified except for the section about
 * the old cluster, which is the line that saves the next reader an hour. It is
 * shown on a preview and a page and not on a row, because a row has a snippet
 * from the document itself and this would compete with it.
 */
export function note(v) {
  return v && v.note ? h("p", { class: "verified__note" }, v.note) : null;
}

/**
 * control is the button offered to somebody who may vouch for this document.
 *
 * Whether they may is the server's answer, sent with the document, so a reader
 * who is not the owner is never shown a button that would be refused. It is a
 * button rather than a form: the note and the expiry are both worth having and
 * neither is worth a dialog in front of the one action anybody takes, so the
 * common case is one click and the endpoint takes the rest.
 */
export function control(d, opts = {}) {
  if (!d || !d.can_verify) return null;
  const claimed = Boolean(d.verified && d.verified.at);
  return [
    h(
      "button",
      {
        class: "button button--verify",
        type: "button",
        title: claimed
          ? "Say this document is still current, from today"
          : "Put your name to this document as current",
        onClick: (e) => act(e.currentTarget, () => api.verify(d.id), opts),
      },
      svg(icon("check"), 16),
      claimed ? "Verify again" : "Verify",
    ),
    claimed &&
      h(
        "button",
        {
          class: "button button--ghost",
          type: "button",
          title: "Take your name off this document",
          onClick: (e) => act(e.currentTarget, () => api.unverify(d.id), opts),
        },
        "Withdraw",
      ),
  ];
}

/**
 * act runs one of the two writes and tells the screen what came back.
 *
 * The button is disabled for the round trip rather than the whole screen, and
 * it is not enabled again on success because the screen that redraws replaces
 * it. A failure says why and gives the button back, because the most likely
 * reason is a network that was not there and the next likely thing is another
 * click.
 *
 * Focus follows the replacement. Withdrawing a claim removes the button that
 * did it and puts the one that undoes it in the same place, and without this
 * the focus a keyboard was holding would land on the body, which inside a modal
 * preview is a way out of the dialog that nobody asked for.
 */
async function act(button, run, { onSay = () => {}, onChange = () => {} } = {}) {
  const foot = button.parentNode;
  button.disabled = true;
  try {
    const next = await run();
    // Every list that carries a badge now carries the wrong one. Marking them
    // stale rather than dropping them keeps them paintable and keeps their
    // tags, so what this costs is a revalidation rather than a skeleton.
    cache.invalidate(cache.key("search", {}));
    cache.invalidate(cache.key("recent", {}));
    onChange(next);
    const moved = foot && foot.querySelector(".button--verify");
    if (moved) moved.focus();
    onSay(next ? "Verified" : "Verification withdrawn");
  } catch (err) {
    onSay(err.message);
    button.disabled = false;
  }
}

// Somebody saying a document is out of date, and the owner saying it is dealt
// with.
//
// This is the one write in the interface with no permission behind it beyond
// being able to read the page it is on. The person who has just lost an hour to
// a runbook naming a cluster that was turned off in March is the person who
// knows, and a corpus that only takes corrections from the people who own the
// documents takes very few of them.
//
// So the button is offered to everybody and clearing what was said is not. That
// asymmetry is the whole feature: if any reader could clear a report, the first
// thing that would happen to an inconvenient one is that somebody would clear
// it.
//
// The mark is drawn on a preview and on a document page and not on a result
// row. A row already carries a source, a kind, a folder, an author, a date and
// a verification badge, and the search path has ten milliseconds to answer in,
// which is not the place to be counting complaints.

import { h, replace, svg } from "genba/dom.js";
import { api } from "genba/api.js";
import { cache } from "genba/cache.js";
import { icon, exact } from "genba/format.js";

/**
 * mark is the line that says this document has been reported.
 *
 * It counts people rather than clicks, which is what makes the number worth
 * printing at all: a second report from the same reader replaces their first
 * one, so five means five different people gave up on the same page.
 *
 * The words carry the colour here, unlike an owner and unlike a current
 * verification. A document somebody has said is wrong is a warning, and the
 * point of drawing it is that it is read as one.
 */
export function mark(s) {
  if (!s || !s.at) return null;
  return h(
    "span",
    { class: "stale", title: sentence(s) },
    svg(icon("flag"), 14),
    words(s),
  );
}

/** words is the mark itself, which stays short enough for a crumb. */
function words(s) {
  if (s.count > 1) return `Reported out of date by ${s.count} people`;
  return s.by ? `Reported out of date by ${s.by}` : "Reported out of date";
}

/**
 * sentence is everything known about the most recent report, for the tooltip.
 *
 * The address is in it because the reader who disagrees with the report needs
 * somebody to ask, exactly as the reader who disagrees with a verification
 * does.
 */
function sentence(s) {
  const who = s.email ? `${s.by} (${s.email})` : s.by || "somebody";
  const parts = [`Reported out of date by ${who} on ${exact(s.at)}`];
  if (s.count > 1) parts.push(`${s.count} people have said so`);
  if (s.note) parts.push(s.note);
  return parts.join(". ");
}

/**
 * reason is what the reporter said was wrong, in their own words.
 *
 * It is the sentence that saves the next reader the hour the reporter lost, and
 * it is the reason a report is worth more than a flag. Only the most recent one
 * is drawn: a panel printing nine near identical complaints would bury the one
 * that says what is actually out of date.
 */
export function reason(s) {
  return s && s.note ? h("p", { class: "stale__note" }, s.note) : null;
}

/**
 * report is the pair of controls under a document.
 *
 * Whether either is offered is the server's answer, sent with the document, so
 * a deployment whose driver cannot remember a report never draws a button that
 * would be refused, and a reader who neither owns nor wrote the document is
 * never shown the one that clears it.
 *
 * Reporting starts as a button and becomes a field, the way handing a document
 * over does. A note box sitting open under every document invites a sentence
 * nobody meant to write, and the sentence is the part worth having.
 */
export function report(d, opts = {}) {
  if (!d || !d.can_report) return null;
  const box = h("div", { class: "stale-control" });
  return replace(box, closed(d, box, opts));
}

/** closed is the resting state: the button, and the way out when there is one. */
function closed(d, box, opts) {
  const reported = Boolean(d.stale && d.stale.at);
  return [
    h(
      "button",
      {
        class: "button button--ghost button--report",
        type: "button",
        title: reported
          ? "Say what else is out of date about this document"
          : "Tell whoever owns this document that it is out of date",
        onClick: () => open(d, box, opts),
      },
      svg(icon("flag"), 16),
      reported ? "Report as well" : "Report as out of date",
    ),
    reported &&
      d.can_resolve &&
      h(
        "button",
        {
          class: "button button--ghost",
          type: "button",
          title: "Say the reports on this document have been dealt with",
          onClick: (e) =>
            act(e.currentTarget, d, box, () => api.resolve(d.id), opts, "Reports cleared"),
        },
        "Mark as dealt with",
      ),
  ];
}

/**
 * open swaps the button for the field and puts the cursor in it.
 *
 * The note is optional and the field says so, because a reader who knows a page
 * is wrong and cannot say why in a sentence is still worth hearing from, and a
 * required field would turn that reader into no report at all.
 *
 * Escape closes the field and stops there rather than closing the preview
 * around it, because the innermost thing open is the thing Escape means.
 */
function open(d, box, opts) {
  const field = h("input", {
    class: "stale__field",
    type: "text",
    maxlength: "280",
    autocomplete: "off",
    placeholder: "What is out of date? (optional)",
    "aria-label": "What is out of date about this document",
  });
  const form = h(
    "form",
    {
      class: "stale__form",
      onSubmit: (e) => {
        e.preventDefault();
        const note = field.value.trim();
        const button = e.submitter || form.querySelector("[type=submit]");
        act(button, d, box, () => api.report(d.id, note ? { note } : null), opts, "Reported as out of date");
      },
      onKeyDown: (e) => {
        if (e.key !== "Escape") return;
        e.stopPropagation();
        cancel(d, box, opts);
      },
    },
    field,
    h("button", { class: "button button--primary", type: "submit" }, "Report"),
    h("button", { class: "button button--ghost", type: "button", onClick: () => cancel(d, box, opts) }, "Cancel"),
  );
  replace(box, form);
  field.focus();
}

/** cancel puts the button back and takes the focus with it. */
function cancel(d, box, opts) {
  replace(box, closed(d, box, opts));
  const back = box.querySelector(".button--report");
  if (back) back.focus();
}

/**
 * act runs one of the two writes and tells the screen what came back.
 *
 * The control is disabled for the round trip rather than the whole screen, and
 * it is not enabled again on success because the screen that redraws replaces
 * it. A failure says why and gives it back, because the most likely reason is a
 * network that was not there and the next likely thing is another try.
 *
 * Focus follows the replacement, the same way it does for a verification and a
 * handover and for the same reason: the element that had the focus is gone by
 * the time this returns, and focus that falls to the body inside a modal
 * preview is a way out of the dialog that nobody asked for.
 */
async function act(button, d, box, run, { onSay = () => {}, onChange = () => {} } = {}, said) {
  const foot = box.parentNode;
  if (button) button.disabled = true;
  try {
    const next = await run();
    // The lists that carry this document now carry the wrong thing about it.
    // Marking them stale rather than dropping them keeps them paintable, so
    // what this costs is a revalidation that is usually answered with a 304.
    cache.invalidate(cache.key("search", {}));
    cache.invalidate(cache.key("recent", {}));
    cache.invalidate(cache.key("document", { id: d.id }));
    // Reporting answers with what stands afterwards and clearing answers with
    // nothing at all, which is the same thing said two ways.
    onChange(next || { stale: null });
    const moved = foot && foot.querySelector(".button--report");
    if (moved) moved.focus();
    onSay(said);
  } catch (err) {
    onSay(err.message);
    if (button) button.disabled = false;
  }
}

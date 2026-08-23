// Who owns a document, and how somebody says the connector got it wrong.
//
// Ownership is derived, and what a connector derives is very often the account
// that ran the import. A corpus where half the documents are owned by Drive
// Sync is a corpus where the owner is not worth printing, and the owner is the
// one field on a stale document that tells a reader who to go and ask. So it is
// printed, and anybody the server says may hand the document over is offered a
// field to correct it in.
//
// The correction carries the name of the person who made it, which is what
// keeps it honest: an owner nobody can be shown to have chosen is a fact from
// nowhere, and the point of the whole feature is that there is somebody behind
// the name.

import { h, replace, svg } from "genba/dom.js";
import { api } from "genba/api.js";
import { cache } from "genba/cache.js";
import { icon, exact } from "genba/format.js";

/**
 * owner is the line that says who is accountable for a document.
 *
 * It draws the owner as it stands, corrected or not, because that is the
 * question a reader is asking and the provenance is the answer to a different
 * one. Where the answer came from is in the tooltip for a mouse and in a hidden
 * sentence for a screen reader, rather than in a second visible crumb: this
 * line already sits after a source, a kind, a folder and a date, and the reader
 * who wants to know who chose the owner is rarer than the reader who wants to
 * know who it is.
 */
export function owner(o) {
  if (!o || (!o.name && !o.email)) return null;
  return h(
    "span",
    { class: o.by ? "owner owner--corrected" : "owner", title: sentence(o) },
    svg(icon("people"), 14),
    `Owned by ${o.name || o.email}`,
    o.by && h("span", { class: "visually-hidden" }, `, ${provenance(o)}`),
  );
}

/** sentence is the whole of what is known about the owner, for the tooltip. */
function sentence(o) {
  const who = o.name && o.email ? `${o.name} (${o.email})` : o.name || o.email;
  return o.by ? `${who}. ${provenance(o)}` : `${who}, as reported by the source`;
}

function provenance(o) {
  return o.at ? `handed over by ${o.by} on ${exact(o.at)}` : `handed over by ${o.by}`;
}

/**
 * reassign is the control offered to somebody who may change the owner.
 *
 * Whether they may is the server's answer, sent with the document, so a reader
 * who neither owns nor wrote it is never shown a field that would be refused.
 *
 * It starts as a button and becomes a field when the button is pressed, rather
 * than being a field that is always there. An address box sitting open under
 * every document invites a change on a document nobody meant to change, and the
 * action here is one somebody takes about twice a year.
 */
export function reassign(d, opts = {}) {
  if (!d || !d.can_reassign) return null;
  const box = h("div", { class: "own" });
  return replace(box, closed(d, box, opts));
}

/** closed is the resting state: one button, and a way back when there is one. */
function closed(d, box, opts) {
  const corrected = Boolean(d.owner && d.owner.by);
  return [
    h(
      "button",
      {
        class: "button button--ghost button--reassign",
        type: "button",
        title: "Say who really owns this document",
        onClick: () => open(d, box, opts),
      },
      svg(icon("people"), 16),
      "Change owner",
    ),
    corrected &&
      h(
        "button",
        {
          class: "button button--ghost",
          type: "button",
          title: "Put back the owner the source reports",
          onClick: (e) => act(e.currentTarget, d.id, box, () => api.clearOwner(d.id), opts, "Owner put back"),
        },
        "Undo correction",
      ),
  ];
}

/**
 * open swaps the button for the field, and puts the cursor in it.
 *
 * The field is prefilled with the address that stands and selected, so the
 * common case is type over it and press return. Escape closes the field and
 * stops there rather than closing the preview around it, because the innermost
 * thing open is the thing Escape means.
 */
function open(d, box, opts) {
  const field = h("input", {
    class: "own__field",
    type: "email",
    required: true,
    autocomplete: "email",
    spellcheck: "false",
    placeholder: "name@example.com",
    "aria-label": "The address of the person who owns this document",
    value: (d.owner && d.owner.email) || "",
  });
  const form = h(
    "form",
    {
      class: "own__form",
      onSubmit: (e) => {
        e.preventDefault();
        const email = field.value.trim();
        if (!email) return field.focus();
        const button = e.submitter || form.querySelector("[type=submit]");
        act(button, d.id, box, () => api.setOwner(d.id, { email }), opts, `Owner changed to ${email}`);
      },
      onKeyDown: (e) => {
        if (e.key !== "Escape") return;
        e.stopPropagation();
        cancel(d, box, opts);
      },
    },
    field,
    h("button", { class: "button button--primary", type: "submit" }, "Save"),
    h("button", { class: "button button--ghost", type: "button", onClick: () => cancel(d, box, opts) }, "Cancel"),
  );
  replace(box, form);
  field.focus();
  field.select();
}

/** cancel puts the button back and takes the focus with it. */
function cancel(d, box, opts) {
  replace(box, closed(d, box, opts));
  const back = box.querySelector(".button--reassign");
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
 * Focus follows the replacement, the same way it does for a verification and
 * for the same reason: the element that had the focus is gone by the time this
 * returns, and focus that falls to the body inside a modal preview is a way out
 * of the dialog that nobody asked for.
 */
async function act(button, id, box, run, { onSay = () => {}, onChange = () => {} } = {}, said) {
  const foot = box.parentNode;
  if (button) button.disabled = true;
  try {
    const next = await run();
    // Every list that carries an owner now carries the wrong one, and so does
    // the held copy of this document. Marking them stale rather than dropping
    // them keeps them paintable, so what this costs is a revalidation that is
    // usually answered with a 304.
    cache.invalidate(cache.key("search", {}));
    cache.invalidate(cache.key("recent", {}));
    cache.invalidate(cache.key("document", { id }));
    onChange(next);
    const moved = foot && foot.querySelector(".button--reassign");
    if (moved) moved.focus();
    onSay(said);
  } catch (err) {
    onSay(err.message);
    if (button) button.disabled = false;
  }
}

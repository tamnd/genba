// The screens that are not the happy path.
//
// Every one of them says three things: what happened, what it means for what is
// on screen, and what to do next. A state missing the third is a dead end, a
// state missing the second is a puzzle, and a state missing the first is a
// spinner. That rule is the whole of the states specification and it is the
// reason each of these carries prose rather than a shrug and an icon.
//
// They live together in one module because two of them have to be compared. The
// answer for a document that is not there and the answer for one this viewer may
// not read are the same words, and the only way to keep them the same words is
// for there to be one copy of them.

import { h, svg } from "./dom.js";
import { icon, label, number } from "./format.js";
import * as urlState from "./state.js";

/**
 * NO_ACCESS is what a 403 and a 404 on a document both say.
 *
 * Not that the document does not exist, which is a lie when it does, and not
 * access denied, which reads as a failure of the system rather than a statement
 * about permissions. Saying which of the two happened would tell somebody
 * whether a document with that id exists, which is precisely what the
 * permission system is keeping from them. The client is not making a permission
 * decision here, it is declining to leak the difference between two decisions
 * the server already made.
 */
export const NO_ACCESS = "You do not have access to this document.";

/** NOT_AVAILABLE is the heading over that sentence, and says as little. */
export const NOT_AVAILABLE = "Not available";

/**
 * firstRun is the first screen anybody ever sees, and currently the only one
 * they get no second chance at.
 *
 * An index with nothing in it is not a search that failed, so it is not written
 * as one. It says what this program does, gives the one command that fills it,
 * and gets out of the way. A dashboard reading zero documents in three places
 * would be the same information arranged as a disappointment.
 */
export function firstRun(command = "genbad -corpus ~/notes -corpus-name notes") {
  return h(
    "div",
    { class: "state state--first" },
    h("span", { class: "state__icon" }, svg(icon("search"), 40)),
    h("h1", { class: "state__title" }, "Nothing indexed yet"),
    h("p", { class: "state__body" }, "genba searches what you point it at."),
    h("pre", { class: "state__command", tabindex: "0" }, h("code", {}, command)),
    h(
      "div",
      { class: "state__actions" },
      h(
        "a",
        {
          class: "button button--primary",
          href: "https://github.com/tamnd/genba#readme",
          target: "_blank",
          rel: "noreferrer noopener",
        },
        "Connectors",
      ),
      h(
        "a",
        {
          class: "button",
          href: "https://github.com/tamnd/genba/tree/main/docs",
          target: "_blank",
          rel: "noreferrer noopener",
        },
        "Documentation",
      ),
    ),
  );
}

/**
 * nothingMatched is a search with no results, which is three different states
 * wearing one heading.
 *
 * Nothing typed is not a failure. Nothing matched with filters on is almost
 * always the filters, and the count of what they removed is the difference
 * between a person removing one of them and a person deciding the product is
 * broken. Nothing matched with no filters is a query to change, or a source
 * that was never connected, and saying which is better than a shrug.
 */
export function nothingMatched(query, onQuery, context = {}) {
  const { removed = null, documents = null, sources = [], chips = [] } = context;

  if (documents === 0) return firstRun();

  if (!query.q && urlState.count(query) === 0) {
    return h(
      "div",
      { class: "state" },
      h("span", { class: "state__icon" }, svg(icon("search"), 40)),
      h("p", { class: "state__title" }, "Search your company"),
      h("p", { class: "state__body" }, "Start typing above. Add app:, type:, in: or from: to narrow it down."),
    );
  }

  const filtered = urlState.count(query) > 0 || (query.tab && query.tab !== "all");
  if (filtered) {
    return h(
      "div",
      { class: "state" },
      h("span", { class: "state__icon" }, svg(icon("slider"), 40)),
      h("p", { class: "state__title" }, `Nothing matches ${quoted(query)} with these filters`),
      // The count arrives after the empty state is already on screen, because it
      // is a second search and the first one is what somebody is waiting on. The
      // sentence appears when the answer does, and the state reads correctly
      // without it in the meantime.
      removed
        ? h(
            "p",
            { class: "state__body" },
            `Your filters removed ${number(removed)} ${removed === 1 ? "result" : "results"}.`,
          )
        : h("p", { class: "state__body" }, "The filters below are the reason. Remove one, or clear them all."),
      // The controls are the ones above the list rather than a second set that
      // reads the same query, because two sets of filter chips are two chances
      // to disagree about what is on.
      h("div", { class: "state__filters" }, chips),
      h(
        "div",
        { class: "state__actions" },
        h(
          "button",
          { class: "button button--primary", type: "button", onClick: () => onQuery(urlState.clear(query)) },
          "Clear filters",
        ),
      ),
    );
  }

  return h(
    "div",
    { class: "state" },
    h("span", { class: "state__icon" }, svg(icon("search"), 40)),
    h("p", { class: "state__title" }, `Nothing found for ${quoted(query)}`),
    h("p", { class: "state__body" }, "Try fewer words, or a word that would be written down rather than spoken."),
    sources.length
      ? h(
          "p",
          { class: "state__body state__body--quiet" },
          `Searching ${listOf(sources.map((s) => label(s.value)))}. Anything else is not connected yet.`,
        )
      : null,
  );
}

/**
 * slow is the line that appears when a request has taken long enough to be
 * worth apologising for.
 *
 * The cancel control is the point of it. An interface with no way to stop
 * something has taken the machine away from the person using it, and five
 * seconds of a spinner with no way out is exactly that.
 */
export function slow(onCancel) {
  return h(
    "div",
    { class: "state__slow", role: "status" },
    h("span", {}, "This is taking longer than usual."),
    h("button", { class: "button button--ghost", type: "button", onClick: onCancel }, "Cancel"),
  );
}

/** stopped is what is left after somebody cancels, which is a way back in. */
export function stopped(onRetry) {
  return h(
    "div",
    { class: "state" },
    h("span", { class: "state__icon" }, svg(icon("close"), 40)),
    h("p", { class: "state__title" }, "Search cancelled"),
    h("p", { class: "state__body" }, "Nothing was loaded. The words are still in the box."),
    h(
      "div",
      { class: "state__actions" },
      h("button", { class: "button button--primary", type: "button", onClick: onRetry }, "Try again"),
    ),
  );
}

/**
 * failed is a request that did not answer, with nothing on screen to keep.
 *
 * The shape of the failure rather than something went wrong, which has no verb
 * in it and tells nobody whether to wait, retry or go and look at the server.
 * The request id is shown when there is one, because it is the only thing that
 * connects what somebody saw to a line in a log.
 */
export function failed(err, onRetry) {
  return h(
    "div",
    { class: "state state--error" },
    h("span", { class: "state__icon" }, svg(icon("close"), 40)),
    h("p", { class: "state__title" }, failureTitle(err)),
    failedBody(err, onRetry),
  );
}

/**
 * failureTitle is the shape of the failure in five words.
 *
 * Which of the three it is decides what somebody should do next, and that is
 * the whole reason not to write something went wrong: a server that cannot be
 * reached is a wait, a server that answered with a five hundred is a look at
 * the logs, and a refusal is neither.
 */
export function failureTitle(err) {
  if (!err.status) return "The server could not be reached";
  return err.status >= 500 ? "The server could not answer" : "That request was refused";
}

/** failedBody is the failure under a heading that has already been written. */
export function failedBody(err, onRetry) {
  return h(
    "div",
    { class: "state__detail" },
    h("p", { class: "state__body" }, err.message),
    // Only when there is one. Nothing in this program sends one yet, and a
    // proxy in front of it may, which is the only thread between what somebody
    // saw and a line in a log.
    err.requestID ? h("p", { class: "state__body state__body--quiet" }, `Request ${err.requestID}`) : null,
    onRetry
      ? h(
          "div",
          { class: "state__actions" },
          h("button", { class: "button button--primary", type: "button", onClick: onRetry }, "Try again"),
        )
      : null,
  );
}

/**
 * notPermitted is the document somebody followed a link to and cannot read.
 *
 * The heading belongs to whatever is showing this, because both the drawer and
 * the document page already have one. What is here is the sentence and the way
 * out, and the sentence is the constant above rather than a copy of it.
 */
export function notPermitted(back) {
  return h(
    "div",
    { class: "state" },
    h("span", { class: "state__icon" }, svg(icon("lock"), 40)),
    h("p", { class: "state__body" }, NO_ACCESS),
    back
      ? h(
          "div",
          { class: "state__actions" },
          h("button", { class: "button button--primary", type: "button", onClick: back.go }, back.title),
        )
      : null,
  );
}

function quoted(query) {
  return query.q ? `"${query.q}"` : "this search";
}

/** listOf writes a list the way a person would say it out loud. */
function listOf(names) {
  if (names.length <= 1) return names[0] || "";
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

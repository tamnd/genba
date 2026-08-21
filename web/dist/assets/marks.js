// Finding the words somebody typed inside the document they opened.
//
// This is a different claim from the one a result row makes. The snippet on a
// row is the server's answer to why that document came back, and the runs
// marked in it are the ones the analyzer that built the index said matched.
// That answer belongs to the server and is never guessed at here.
//
// What this does is answer a reader's question instead: where in the page in
// front of me are the words I asked for. It is the browser's job because the
// body is already in the browser, and asking the server to find a word in a
// document it has just finished sending is a second request for something that
// takes a millisecond here.
//
// The two can disagree. A search for cache matches a document that only says
// caching, because the index stems and this does not. A word ending may follow
// a term for that reason and nothing else is allowed to, so when the two
// disagree the marks are missing rather than wrong, and a document with no
// marks in it opens at the top the way it always did.

import { h } from "genba/dom.js";

// The most marks worth drawing. A one word query against a file that repeats
// that word on every line is a thousand elements nobody is going to look at,
// and the first one is the only one that decides where the page opens.
const MAX = 400;

// Two characters, because a single letter appears in every document ever
// written and marking all of it marks nothing.
const MIN = 2;

// Words in a query that are the query language rather than the words somebody
// is looking for.
const OPERATORS = new Set(["AND", "OR", "NOT"]);

// Where a word is not a word. A line number is a coordinate, so a search for 42
// must not paint the gutter, and a block waiting to be highlighted is about to
// have its contents replaced.
const SKIP = "mark, .line__no, .code__head, [data-lazy]";

/**
 * terms picks the words out of a query that are worth looking for in a body.
 *
 * A quoted phrase stays whole, because somebody who asked for "index format"
 * meant the pair. A field filter and a negated word are dropped: neither is
 * text to find, and marking the word somebody asked not to see is the opposite
 * of what they asked for.
 */
export function terms(query) {
  const found = [];
  const rest = String(query || "").replace(/"([^"]+)"/g, (_, phrase) => {
    found.push(phrase);
    return " ";
  });
  for (const word of rest.split(/\s+/)) {
    if (!word || word.includes(":") || word.startsWith("-")) continue;
    if (OPERATORS.has(word.toUpperCase())) continue;
    found.push(word);
  }

  const out = [];
  for (const term of found) {
    const clean = term.trim().replace(/[*?]/g, "").toLowerCase();
    if (clean.length < MIN || out.includes(clean)) continue;
    out.push(clean);
  }
  // A query longer than this is a sentence somebody pasted, and marking every
  // word of a sentence marks the whole document.
  return out.slice(0, 8);
}

/**
 * mark wraps every occurrence of those words in the tree, and returns how many.
 *
 * The text nodes are collected before any of them is replaced. Walking and
 * mutating at the same time means walking into the nodes just built, which
 * finds the same word again inside the mark that was drawn around it.
 */
export function mark(root, words) {
  if (!words.length) return 0;
  const pattern = new RegExp(`\\b(${words.map(escape).join("|")})\\w*`, "gi");

  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const nodes = [];
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (!node.nodeValue.trim()) continue;
    if (node.parentElement && node.parentElement.closest(SKIP)) continue;
    nodes.push(node);
  }

  let count = 0;
  for (const node of nodes) {
    if (count >= MAX) break;
    const text = node.nodeValue;
    const out = document.createDocumentFragment();
    let at = 0;
    pattern.lastIndex = 0;
    for (let hit = pattern.exec(text); hit && count < MAX; hit = pattern.exec(text)) {
      if (hit.index > at) out.appendChild(document.createTextNode(text.slice(at, hit.index)));
      out.appendChild(h("mark", { class: "hit" }, hit[0]));
      at = hit.index + hit[0].length;
      count++;
    }
    if (!at) continue;
    if (at < text.length) out.appendChild(document.createTextNode(text.slice(at)));
    node.parentNode.replaceChild(out, node);
  }
  return count;
}

/**
 * reveal moves a document to the part of it somebody came for.
 *
 * A line in the address wins over a word in the query, because a link to line
 * forty two was written by a person who meant line forty two, and the query it
 * carries is only how they found the file.
 *
 * Nothing moves when there is neither, which is the case for every document
 * opened from the rail or from a link with nothing on the end of it, and the
 * top of a document is where reading starts.
 */
export function reveal(root, hash = location.hash) {
  const line = toLine(root, hash);
  const target = line || root.querySelector("mark.hit");
  if (!target) return null;
  // center rather than the default, so a match on the last line of a file is
  // not left one pixel above the fold with nothing around it to read.
  if (!line) target.scrollIntoView({ block: "center", inline: "nearest" });
  return target;
}

/**
 * toLine moves to the line an address names and says whether there was one.
 *
 * This is the half of reveal that an address arriving on a page already open
 * is allowed to do. A line is a place somebody asked for. The first match is
 * only where reading starts, and going back to it because somebody followed a
 * link to the top of the page would move them away from what they were reading.
 */
export function toLine(root, hash = location.hash) {
  const line = lineOf(root, hash);
  if (!line) return null;
  current(root, line);
  line.scrollIntoView({ block: "center", inline: "nearest" });
  return line;
}

/** lineOf finds the line an address names, if it names one that is there. */
export function lineOf(root, hash = location.hash) {
  const at = /^#L(\d+)$/.exec(String(hash || ""));
  return at ? root.querySelector(`[data-line="${at[1]}"]`) : null;
}

/** current marks one line as the one being pointed at, and unmarks the rest. */
export function current(root, line) {
  for (const was of root.querySelectorAll(".line--current")) was.classList.remove("line--current");
  if (line) line.classList.add("line--current");
}

function escape(term) {
  return term.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

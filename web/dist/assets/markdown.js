// A markdown renderer for the preview.
//
// It covers the subset people actually write in a wiki: headings, paragraphs,
// emphasis, links, images, lists, quotes, fenced code, pipe tables and rules.
// Anything outside that is printed as the characters that were typed, and raw
// HTML in a document is printed rather than parsed.
//
// Nothing here builds a node from a string of markup. Every element comes from
// createElement and every piece of text goes in through a text node, so there is
// no sanitiser to get wrong and a body containing a script tag is a body
// containing the text of a script tag. That is the whole security argument for
// this file and it is the reason it is hand written rather than pulled in.

import { h } from "genba/dom.js";
import { highlight, rows } from "genba/highlight.js";
import { current } from "genba/marks.js";
import { frame } from "genba/table.js";

/**
 * render turns markdown into nodes.
 *
 * options.imageNode is given the source of an image the renderer cannot resolve
 * on its own, which is every image written as a path relative to the document.
 * It returns a node to put there, or nothing to fall back to the alt text.
 * Without it, only absolute http and https images are drawn.
 */
export function render(src, options = {}) {
  const frag = document.createDocumentFragment();
  for (const node of blocks(String(src || "").replace(/\r\n?/g, "\n").split("\n"), options)) {
    frag.appendChild(node);
  }
  return frag;
}

/** blocks parses a run of lines into block level nodes. */
function blocks(lines, options) {
  const out = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];
    const trimmed = line.trim();

    if (!trimmed) {
      i++;
      continue;
    }

    const fence = fenceOf(trimmed);
    if (fence) {
      const lang = trimmed.slice(fence.length).trim().split(/\s+/)[0];
      const body = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith(fence)) {
        body.push(lines[i]);
        i++;
      }
      i++; // the closing fence, or the end of the document
      out.push(codeBlock(body.join("\n"), lang));
      continue;
    }

    if (isRule(trimmed)) {
      out.push(h("hr", { class: "prose__rule" }));
      i++;
      continue;
    }

    const heading = /^(#{1,6})\s+(.*)$/.exec(trimmed);
    if (heading) {
      const level = heading[1].length;
      out.push(h(`h${level}`, { class: "prose__h" }, inline(heading[2].replace(/\s+#+$/, ""), options)));
      i++;
      continue;
    }

    if (trimmed.startsWith(">")) {
      const quoted = [];
      while (i < lines.length && lines[i].trim().startsWith(">")) {
        quoted.push(lines[i].trim().replace(/^>\s?/, ""));
        i++;
      }
      // A quote can hold anything a document can hold, including another quote,
      // so its contents go back through the same parser.
      out.push(h("blockquote", { class: "prose__quote" }, ...blocks(quoted, options)));
      continue;
    }

    if (isTableAt(lines, i)) {
      const rows = [];
      while (i < lines.length && lines[i].trim().startsWith("|")) {
        rows.push(lines[i].trim());
        i++;
      }
      out.push(table(rows, options));
      continue;
    }

    if (itemOf(line)) {
      const collected = [];
      while (i < lines.length && (itemOf(lines[i]) || continuation(lines[i], collected.length))) {
        collected.push(lines[i]);
        i++;
      }
      out.push(list(collected, options));
      continue;
    }

    // A paragraph runs until a blank line or the start of another block. The
    // lines are joined with a space rather than kept, because a hard wrapped
    // paragraph is one sentence that somebody's editor broke, and showing the
    // break is showing their editor's settings.
    const para = [];
    while (i < lines.length && lines[i].trim() && !startsBlock(lines, i)) {
      para.push(lines[i].trim());
      i++;
    }
    if (!para.length) continue;
    const text = para.join(" ");

    // An image on a line of its own is a figure rather than a paragraph with a
    // picture in it, which is what lets it take the space it deserves and take
    // its title as a caption.
    if (text.startsWith("![")) {
      const link = linkAt(text.slice(1));
      if (link && link.length === text.length - 1) {
        out.push(
          h(
            "figure",
            { class: "prose__figure" },
            image(link, options),
            link.title && h("figcaption", {}, link.title),
          ),
        );
        continue;
      }
    }
    out.push(h("p", { class: "prose__p" }, inline(text, options)));
  }

  return out;
}

function startsBlock(lines, i) {
  const t = lines[i].trim();
  return (
    Boolean(fenceOf(t)) ||
    isRule(t) ||
    /^#{1,6}\s/.test(t) ||
    t.startsWith(">") ||
    Boolean(itemOf(lines[i])) ||
    isTableAt(lines, i)
  );
}

function fenceOf(line) {
  if (line.startsWith("```")) return "```";
  if (line.startsWith("~~~")) return "~~~";
  return "";
}

function isRule(line) {
  return /^(-{3,}|\*{3,}|_{3,})$/.test(line.replace(/\s/g, ""));
}

/** itemOf splits a list line into its indent, its marker and its text. */
function itemOf(line) {
  const m = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/.exec(line);
  if (!m) return null;
  return { indent: m[1].length, ordered: /\d/.test(m[2]), text: m[3] };
}

/** continuation reports whether a line belongs to the item above it. */
function continuation(line, started) {
  return started > 0 && Boolean(line.trim()) && /^\s{2,}/.test(line) && !isRule(line.trim());
}

/**
 * list builds a nested list from the flat lines that make it up.
 *
 * Nesting is by indentation, which is what people type, rather than by any of
 * the several other things markdown will accept.
 */
function list(lines, options) {
  const items = [];
  for (const line of lines) {
    const item = itemOf(line);
    if (item) {
      items.push({ ...item, lines: [item.text] });
    } else if (items.length) {
      items[items.length - 1].lines.push(line.trim());
    }
  }

  const build = (start, indent) => {
    const ordered = items[start].ordered;
    const el = h(ordered ? "ol" : "ul", { class: "prose__list" });
    let i = start;
    while (i < items.length && items[i].indent >= indent) {
      if (items[i].indent > indent) {
        const [child, next] = build(i, items[i].indent);
        const last = el.lastElementChild;
        if (last) last.appendChild(child);
        i = next;
        continue;
      }
      const item = items[i];
      const li = h("li", { class: "prose__item" });
      const task = /^\[([ xX])\]\s+(.*)$/.exec(item.lines.join(" "));
      if (task) {
        li.className = "prose__item prose__item--task";
        li.appendChild(
          h("input", { type: "checkbox", disabled: true, checked: task[1] !== " " }),
        );
        li.appendChild(inline(task[2], options));
      } else {
        li.appendChild(inline(item.lines.join(" "), options));
      }
      el.appendChild(li);
      i++;
    }
    return [el, i];
  };

  return items.length ? build(0, items[0].indent)[0] : h("ul", { class: "prose__list" });
}

function isTableAt(lines, i) {
  const row = lines[i].trim();
  const next = (lines[i + 1] || "").trim();
  return row.startsWith("|") && /^\|[\s:|-]+\|?$/.test(next) && next.includes("-");
}

function table(rows, options) {
  const cells = (row) =>
    row
      .replace(/^\||\|$/g, "")
      .split("|")
      .map((c) => c.trim());

  const align = cells(rows[1]).map((c) => {
    if (c.startsWith(":") && c.endsWith(":")) return "center";
    if (c.endsWith(":")) return "right";
    return null;
  });
  const cell = (tag, text, i) =>
    h(tag, align[i] ? { style: { textAlign: align[i] } } : {}, inline(text, options));

  return frame(
    h(
      "table",
      { class: "prose__table" },
      h("thead", {}, h("tr", {}, cells(rows[0]).map((c, i) => cell("th", c, i)))),
      h(
        "tbody",
        {},
        rows.slice(2).map((row) => h("tr", {}, cells(row).map((c, i) => cell("td", c, i)))),
      ),
    ),
  );
}

// A file longer than this is shown without a gutter.
//
// Every numbered line is two more elements and the whole file is built in one
// go, so this is where the one screen in the interface that could jank would
// be. It is well past what anybody reads in a preview, the copy button still
// hands over all of it, and a generated file of a hundred thousand lines is not
// worth making every other file wait for.
const NUMBERED_MAX = 4000;

/**
 * codeBlock renders a block of code, with its language named and a copy button.
 *
 * The head is only drawn when there is something to put in it, so a two line
 * snippet does not grow a bar of chrome taller than the code inside it.
 *
 * options.numbers draws the gutter, and only the shape that is a whole file
 * passes it. A fenced block inside a document is an excerpt somebody quoted and
 * its first line is not line one of anything, so a number beside it would be a
 * fact about the code block rather than about the file it came from.
 */
export function codeBlock(source, lang, options = {}) {
  const lines = source.split("\n");
  const numbered = Boolean(options.numbers) && lines.length <= NUMBERED_MAX;
  const code = h("code", { class: "code__text" }, numbered ? gutter(source, lang) : highlight(source, lang));
  // A code block scrolls sideways when a line is long, and a region that
  // scrolls has to be reachable by keyboard or the only way to read the end of
  // that line is a mouse.
  const pre = h("pre", { class: "code__pre", tabindex: "0" }, code);
  const long = lines.length > 6;
  if (!lang && !long && !numbered) return h("div", { class: "code" }, pre);

  const copy = h(
    "button",
    {
      class: "button button--ghost code__copy",
      type: "button",
      onClick: async (e) => {
        const button = e.currentTarget;
        try {
          await navigator.clipboard.writeText(source);
          button.textContent = "Copied";
        } catch {
          button.textContent = "Press ctrl C";
        }
        setTimeout(() => {
          button.textContent = "Copy";
        }, 1600);
      },
    },
    "Copy",
  );
  return h(
    "div",
    { class: numbered ? "code code--file" : "code" },
    h("div", { class: "code__head" }, h("span", { class: "code__lang" }, lang || "text"), copy),
    pre,
  );
}

/**
 * gutter is the file, one line at a time, each with its number beside it.
 *
 * The number is a link to itself. That is the whole of the feature: a line
 * somebody wants to talk about has an address, and pasting that address opens
 * the file there. The click is handled rather than followed because this
 * document was painted after a fetch, so by the time the browser would have
 * jumped to the fragment there was nothing on the page to jump to.
 */
function gutter(source, lang) {
  const out = document.createDocumentFragment();
  rows(source, lang).forEach((text, i) => {
    const at = String(i + 1);
    const number = h(
      "a",
      {
        class: "line__no",
        href: `#L${at}`,
        "aria-label": `Line ${at}`,
        onClick: (e) => {
          e.preventDefault();
          // replace rather than push, because reading down a file and tapping
          // numbers as you go should not fill the back button with lines.
          history.replaceState(null, "", `#L${at}`);
          const line = e.currentTarget.closest(".line");
          current(line.closest(".code"), line);
        },
      },
      at,
    );
    out.appendChild(h("span", { class: "line", "data-line": at }, number, text));
  });
  return out;
}

// Inline ------------------------------------------------------------------

const SPECIAL = /[\\`*_~[!<]|https?:\/\//;

/** inline renders one run of text into a fragment. */
function inline(text, options) {
  const frag = document.createDocumentFragment();
  let rest = String(text);

  while (rest) {
    const at = rest.search(SPECIAL);
    if (at < 0) {
      frag.appendChild(document.createTextNode(rest));
      break;
    }
    if (at > 0) {
      frag.appendChild(document.createTextNode(rest.slice(0, at)));
      rest = rest.slice(at);
    }

    const taken = take(rest, options);
    if (!taken) {
      // Nothing matched, so the character is a character. Consuming one keeps
      // the loop finite, which matters more here than anywhere else in the file.
      frag.appendChild(document.createTextNode(rest[0]));
      rest = rest.slice(1);
      continue;
    }
    frag.appendChild(taken.node);
    rest = rest.slice(taken.length);
  }
  return frag;
}

/** take matches one inline construct at the start of the string. */
function take(rest, options) {
  const c = rest[0];

  if (c === "\\" && rest.length > 1) {
    return { node: document.createTextNode(rest[1]), length: 2 };
  }

  if (c === "`") {
    const ticks = /^`+/.exec(rest)[0];
    const end = rest.indexOf(ticks, ticks.length);
    if (end > 0) {
      return {
        node: h("code", { class: "prose__code" }, rest.slice(ticks.length, end)),
        length: end + ticks.length,
      };
    }
    return null;
  }

  if (c === "!" && rest[1] === "[") {
    const link = linkAt(rest.slice(1));
    if (link) return { node: image(link, options), length: link.length + 1 };
    return null;
  }

  if (c === "[") {
    const link = linkAt(rest);
    if (!link) return null;
    const href = safeURL(link.href);
    if (!href) {
      // A scheme nobody vetted is not a link. The text survives, because the
      // words are what somebody wrote and only the target is suspect.
      return { node: inline(link.text, options), length: link.length };
    }
    return {
      node: h(
        "a",
        { class: "prose__link", href, target: "_blank", rel: "noreferrer noopener" },
        inline(link.text, options),
      ),
      length: link.length,
    };
  }

  if (c === "<") {
    const end = rest.indexOf(">");
    const inner = end > 0 ? rest.slice(1, end) : "";
    const href = inner && safeURL(inner);
    if (href) {
      return {
        node: h("a", { class: "prose__link", href, target: "_blank", rel: "noreferrer noopener" }, inner),
        length: end + 1,
      };
    }
    return null;
  }

  for (const [marker, tag, cls] of [
    ["**", "strong", "prose__strong"],
    ["__", "strong", "prose__strong"],
    ["~~", "del", "prose__del"],
    ["*", "em", "prose__em"],
    ["_", "em", "prose__em"],
  ]) {
    if (!rest.startsWith(marker)) continue;
    // An underscore inside a word is part of the word. store_id is a column
    // name, not the start of an emphasis run.
    if (marker === "_" && /\w/.test(rest[marker.length] || "")) {
      const closer = rest.indexOf(marker, marker.length);
      if (closer > 0 && /\w/.test(rest[closer - 1]) && /\w/.test(rest[closer + 1] || "")) continue;
    }
    const end = rest.indexOf(marker, marker.length);
    if (end < 0 || end === marker.length) continue;
    return {
      node: h(tag, { class: cls }, inline(rest.slice(marker.length, end), options)),
      length: end + marker.length,
    };
  }

  const bare = /^https?:\/\/[^\s<>()]+/.exec(rest);
  if (bare) {
    return {
      node: h(
        "a",
        { class: "prose__link", href: bare[0], target: "_blank", rel: "noreferrer noopener" },
        bare[0],
      ),
      length: bare[0].length,
    };
  }

  return null;
}

/** linkAt reads a [text](target) at the start of the string. */
function linkAt(rest) {
  if (rest[0] !== "[") return null;
  let depth = 0;
  let close = -1;
  for (let i = 0; i < rest.length; i++) {
    if (rest[i] === "[") depth++;
    else if (rest[i] === "]") {
      depth--;
      if (!depth) {
        close = i;
        break;
      }
    }
  }
  if (close < 0 || rest[close + 1] !== "(") return null;
  const end = rest.indexOf(")", close + 2);
  if (end < 0) return null;

  const target = rest.slice(close + 2, end).trim();
  const space = target.search(/\s/);
  return {
    text: rest.slice(1, close),
    href: space < 0 ? target : target.slice(0, space),
    title: space < 0 ? "" : target.slice(space).trim().replace(/^["']|["']$/g, ""),
    length: end + 1,
  };
}

function image(link, options) {
  const alt = link.text || link.title;
  const src = safeURL(link.href);
  if (!src) {
    const node = options.imageNode && options.imageNode(link.href, alt, link.title);
    return node || document.createTextNode(alt);
  }

  const img = h("img", {
    class: "prose__img",
    src,
    alt,
    title: link.title || null,
    loading: "lazy",
    decoding: "async",
  });
  // An image that will not load collapses to the words that describe it, which
  // is more use than a broken glyph and is what somebody wrote the alt text for.
  img.addEventListener("error", () => {
    const fallback = h("span", { class: "prose__img-missing" }, alt || "image");
    if (img.parentNode) img.parentNode.replaceChild(fallback, img);
  });
  return img;
}

/**
 * safeURL returns a target worth turning into a link, or an empty string.
 *
 * The list is short on purpose. A javascript: target in a document body is
 * either a mistake or an attack, and in both cases the right answer is to show
 * the words and drop the target.
 */
export function safeURL(href) {
  const value = String(href || "").trim();
  if (/^(https?:|mailto:)/i.test(value)) return value;
  return "";
}

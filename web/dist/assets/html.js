// An HTML renderer for the preview.
//
// This is the first thing in the codebase that parses markup somebody else
// wrote, and a corpus is untrusted input by definition: it holds whatever
// anybody at the company ever wrote down, including the person who was checking
// whether the wiki escaped script tags.
//
// So it parses into a detached document and then copies out the elements and
// attributes it recognises, one at a time, building nodes as it goes. There is
// never a string of markup in the middle of it. A sanitiser that produces a
// string and assigns it has parsed twice, and mutation XSS is the whole class
// of bug that lives in the gap between the two parses. This has no gap: the
// output is made of elements this file named, carrying attributes this file
// named, and nothing else can reach it.
//
// The browser's own Element.setHTML() does the same job against the real parser
// and is not used, because Safari does not have it, so it could only ever be a
// second sanitiser sitting beside this one, and two sanitisers that disagree is
// a worse position than one that is tested everywhere.

import { h } from "genba/dom.js";
import { codeBlock, safeURL } from "genba/markdown.js";
import { frame } from "genba/table.js";

// The elements that survive, and what each becomes. Presentational tags are
// folded into the one element that means what they meant, so b and strong are
// one thing downstream and the stylesheet has one rule for it.
const TAGS = {
  h1: "h1",
  h2: "h2",
  h3: "h3",
  h4: "h4",
  h5: "h5",
  h6: "h6",
  p: "p",
  br: "br",
  hr: "hr",
  ul: "ul",
  ol: "ol",
  li: "li",
  dl: "dl",
  dt: "dt",
  dd: "dd",
  blockquote: "blockquote",
  table: "table",
  thead: "thead",
  tbody: "tbody",
  tfoot: "tfoot",
  tr: "tr",
  th: "th",
  td: "td",
  caption: "caption",
  figure: "figure",
  figcaption: "figcaption",
  a: "a",
  img: "img",
  code: "code",
  kbd: "code",
  samp: "code",
  strong: "strong",
  b: "strong",
  em: "em",
  i: "em",
  del: "del",
  s: "del",
  ins: "ins",
  mark: "mark",
  sup: "sup",
  sub: "sub",
};

// The classes that make the output look like the rest of the prose renderer,
// which is what stops an HTML document reading as a different product.
const CLASS = {
  h1: "prose__h",
  h2: "prose__h",
  h3: "prose__h",
  h4: "prose__h",
  h5: "prose__h",
  h6: "prose__h",
  p: "prose__p",
  hr: "prose__rule",
  ul: "prose__list",
  ol: "prose__list",
  li: "prose__item",
  blockquote: "prose__quote",
  table: "prose__table",
  a: "prose__link",
  img: "prose__img",
  code: "prose__code",
  strong: "prose__strong",
  em: "prose__em",
  del: "prose__del",
  figure: "prose__figure",
};

// The attributes that survive, named by the element they are allowed on rather
// than in one list, because href means something on an anchor and nothing at
// all on a table cell. Every event handler attribute is absent from this table,
// which is the whole of how onclick is dealt with.
const ATTRS = {
  a: ["title"],
  img: ["width", "height"],
  th: ["colspan", "rowspan", "scope"],
  td: ["colspan", "rowspan"],
  ol: ["start"],
};

// Elements that go, and take everything inside them with them. These are the
// ones whose content is not content: a script's text is a program, a style's
// text is a stylesheet, and an svg is a document in another language that has
// its own way of holding a script. An audio or video element here would be a
// player pointed at a URL nobody vetted, and the media renderer is where a
// player belongs.
const CUT = new Set([
  "script",
  "style",
  "noscript",
  "template",
  "iframe",
  "frame",
  "frameset",
  "object",
  "embed",
  "applet",
  "svg",
  "math",
  "canvas",
  "audio",
  "video",
  "source",
  "track",
  "form",
  "input",
  "button",
  "select",
  "option",
  "textarea",
  "dialog",
  "link",
  "meta",
  "base",
  "head",
  "title",
]);

/**
 * render turns a page of HTML into nodes.
 *
 * options.imageNode is given the source of an image the renderer will not load
 * on its own, which is every image written as a path relative to the document.
 * It returns a node to put there, or nothing to fall back to the alt text. It
 * is the same option markdown.js takes, so a document containing a picture
 * beside it works the same whichever of the two it was written in.
 */
export function render(source, options = {}) {
  // A document parsed this way has no browsing context, so nothing in it runs
  // and nothing in it is fetched. The img tags below never load anything: their
  // src is read as a string and the element that ends up on screen is one this
  // file made.
  const parsed = new DOMParser().parseFromString(String(source || ""), "text/html");
  const out = document.createDocumentFragment();
  walk(parsed.body, out, options);
  return out;
}

/** walk copies the children of a node into the output, deciding one at a time. */
function walk(node, into, options) {
  for (const child of node.childNodes) {
    if (child.nodeType === Node.TEXT_NODE) {
      into.appendChild(document.createTextNode(child.data));
      continue;
    }
    // Comments, processing instructions and the doctype are not content and
    // carry nothing a reader wants.
    if (child.nodeType !== Node.ELEMENT_NODE) continue;

    const tag = child.tagName.toLowerCase();
    if (CUT.has(tag)) continue;

    const made = element(tag, child, options);
    if (made === undefined) {
      // An element this file does not know is not the same as an element it
      // refuses. A section, a main or a div is a box somebody put around their
      // words, and dropping the box with the words in it would empty most
      // documents. So the box goes and what was inside it stays, which is safe
      // for the same reason everything here is safe: whatever comes out of the
      // recursion is made of elements named above.
      walk(child, into, options);
      continue;
    }
    if (made) into.appendChild(made);
  }
}

/**
 * element builds one node, or returns undefined to unwrap it.
 *
 * A null return drops the element and its contents, which happens only where
 * the contents were never text in the first place.
 */
function element(tag, from, options) {
  if (tag === "pre") {
    // A block of code is code whatever markup it arrived wrapped in, and
    // codeBlock is where the copy button and the keyboard reachable scroll
    // already live. The text is taken rather than walked, because the tags
    // inside a highlighted block are somebody else's highlighter and ours is
    // about to run over the same characters.
    return codeBlock(from.textContent.replace(/\n$/, ""), language(from));
  }

  if (tag === "table") {
    const table = h("table", { class: CLASS.table });
    walk(from, table, options);
    return frame(table);
  }

  if (tag === "img") {
    return picture(from, options);
  }

  const name = TAGS[tag];
  if (!name) return undefined;

  if (tag === "a") {
    const href = safeURL(from.getAttribute("href"));
    // A scheme nobody vetted is not a link. javascript: and data: in a document
    // body are either a mistake or an attack, and in both cases the words are
    // what somebody wrote and only the target is suspect, so the words stay.
    if (!href) return undefined;
    const el = h("a", { class: CLASS.a, href, target: "_blank", rel: "noreferrer noopener" });
    copy(el, from, ATTRS.a);
    walk(from, el, options);
    return el;
  }

  const el = h(name, CLASS[name] ? { class: CLASS[name] } : {});
  copy(el, from, ATTRS[tag]);
  if (name !== "br" && name !== "hr") walk(from, el, options);
  return el;
}

/** copy moves the attributes that are allowed on this element and no others. */
function copy(to, from, allowed) {
  for (const attr of allowed || []) {
    const value = from.getAttribute(attr);
    if (value) to.setAttribute(attr, value);
  }
}

/**
 * picture draws an image out of a document.
 *
 * An absolute http source is drawn as itself. Anything else is a path relative
 * to the document, which only the caller can turn into something fetchable,
 * and a source that is neither is nothing at all.
 */
function picture(from, options) {
  const alt = from.getAttribute("alt") || "";
  const raw = from.getAttribute("src") || "";
  const src = safeURL(raw);
  if (!src) {
    const node = options.imageNode && options.imageNode(raw, alt, "");
    return node || (alt ? document.createTextNode(alt) : null);
  }
  const img = h("img", { class: CLASS.img, src, alt, loading: "lazy", decoding: "async" });
  copy(img, from, ATTRS.img);
  return img;
}

/**
 * language reads the language a highlighter was told to use.
 *
 * Every generator writes it the same way, as a class on the pre or on the code
 * inside it, and it is the only piece of a foreign highlighter's markup worth
 * keeping.
 */
function language(pre) {
  const inner = pre.querySelector("code");
  const classes = `${pre.className || ""} ${(inner && inner.className) || ""}`;
  const named = /(?:^|\s)(?:language|lang)-([\w+#]+)/.exec(classes);
  return named ? named[1].toLowerCase() : "";
}

// Content rendering.
//
// One place decides what a document is and how to draw it. The drawer asks for
// a body and gets prose, code, a picture, a notebook, a recording or plain
// text, and a result row asks for a tile and gets a thumbnail or an icon.
// Everything downstream of that decision is presentation, so adding a media
// type is a line in a table here rather than a branch in three views.

import { h, svg } from "genba/dom.js";
import { api } from "genba/api.js";
import { render as markdown, codeBlock } from "genba/markdown.js";
import { render as htmlDocument } from "genba/html.js";
import { parse as parseNotebook, render as notebookCells } from "genba/notebook.js";
import { player } from "genba/media.js";
import { terms, mark, passage } from "genba/marks.js";
import { kindIcon, sourceColor, bytes as formatBytes } from "genba/format.js";

// Media types the interface knows how to draw as code, and the language each
// one is highlighted as. A type that is not here and is not an image is drawn
// as the text it is.
const CODE = {
  "text/x-go": "go",
  "text/javascript": "javascript",
  "text/x-typescript": "typescript",
  "text/x-python": "python",
  "text/x-rust": "rust",
  "text/x-c": "c",
  "text/x-c++": "cpp",
  "text/x-java": "java",
  "text/x-ruby": "ruby",
  "text/x-shellscript": "shell",
  "text/x-sql": "sql",
  "text/x-yaml": "yaml",
  "text/x-toml": "toml",
  "text/x-protobuf": "proto",
  "application/json": "json",
  "text/css": "css",
  "text/html": "html",
};

// The types whose body is prose, and how the words in it are written.
//
// Markdown is prose because somebody wrote it as prose. The rest are prose
// because the extractor turned them into the same markdown subset on the way
// in: headings, blank line separated paragraphs, list items and pipe tables. A
// PDF and a Word document arrive here as text with structure in it, and showing
// that as a preformatted wall was throwing the structure away at the last step.
const PROSE = {
  "text/markdown": "markdown",
  "text/html": "html",
  "application/pdf": "markdown",
  "application/vnd.openxmlformats-officedocument.wordprocessingml.document": "markdown",
  "application/vnd.openxmlformats-officedocument.presentationml.presentation": "markdown",
  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "markdown",
};

const NOTEBOOK = "application/x-ipynb+json";

const BY_EXTENSION = {
  go: "go",
  js: "javascript",
  mjs: "javascript",
  ts: "typescript",
  tsx: "typescript",
  py: "python",
  rb: "ruby",
  rs: "rust",
  sh: "shell",
  sql: "sql",
  yaml: "yaml",
  yml: "yaml",
  json: "json",
  css: "css",
  html: "html",
  md: "markdown",
};

/**
 * shapeOf says which renderer a document belongs to.
 *
 * It returns one of image, media, notebook, prose, code or text. The media type
 * is the answer when there is one, because the connector decided it while it
 * had the file in its hands, and the file name is only a fallback for a
 * document that arrived without one.
 *
 * text is the last line and it is not a failure state. A document nobody here
 * has anything clever to say about is shown plainly, which is better than
 * showing it wrongly.
 */
export function shapeOf(d) {
  const media = (d.media_type || "").toLowerCase();
  if (media.startsWith("image/")) return "image";
  if (media.startsWith("audio/") || media.startsWith("video/")) return "media";
  if (media === NOTEBOOK) return "notebook";
  if (PROSE[media]) return "prose";
  if (CODE[media]) return "code";
  if (!media && languageOf(d)) return "code";
  return "text";
}

/**
 * detailOf is the one extra fact worth putting beside a document's kind.
 *
 * It is what the shape makes a reader ask first. A PDF is how long, a recording
 * is how long in the other sense, and everything else answers nothing, which is
 * why this returns an empty string rather than inventing a line.
 */
export function detailOf(d) {
  const props = d.properties || {};
  const pages = Number(props.pages || props.page_count || 0);
  if (pages > 0) return pages === 1 ? "1 page" : `${pages} pages`;

  const seconds = Number(props.duration_seconds || props.duration || 0);
  if (seconds > 0) {
    const mins = Math.floor(seconds / 60);
    const secs = Math.round(seconds % 60);
    return mins ? `${mins} min ${secs} s` : `${secs} s`;
  }
  return "";
}

/** languageOf is the language a code document is highlighted as. */
export function languageOf(d) {
  const media = (d.media_type || "").toLowerCase();
  if (CODE[media]) return CODE[media];
  const ext = String(d.id || "").split(".").pop().toLowerCase();
  return BY_EXTENSION[ext] || "";
}

/**
 * body renders a document into the preview.
 *
 * Every branch produces the same reading experience at a different density:
 * one measure, one type scale, and no case where the reader has to work out
 * what they are looking at.
 *
 * options.query is what somebody typed to get here. Those words are marked
 * wherever they appear in the text, whatever shape the document turned out to
 * be, which is why it happens once out here rather than six times in there. The
 * caller reveals the first of them once the nodes are on the page, because
 * scrolling to something that is not in a document yet scrolls to nothing.
 *
 * options.at is a passage somebody was sent to, which is what a citation under
 * an answer carries. It is marked before the words are, and the order is not
 * incidental: marking the words first splits the sentence into a dozen nodes and
 * hides the marked ones from anything looking for it, so the sentence a reader
 * was sent to would be the one thing on the page that could not be found.
 */
export function body(d, options = {}) {
  const node = draw(d);
  if (options.at) passage(node, options.at);
  mark(node, terms(options.query));
  return node;
}

function draw(d) {
  const shape = shapeOf(d);

  if (shape === "image") {
    return h("div", { class: "preview preview--image" }, figure(d));
  }
  // A recording is the one shape whose document survives having no text: the
  // transcript may be missing and the recording is still there to play.
  if (shape === "media") {
    return h(
      "div",
      { class: "preview preview--media" },
      d.body
        ? h("div", { class: "prose" }, markdown(d.body))
        : h("p", { class: "preview__empty" }, "Nothing was transcribed from this recording."),
      player(d),
    );
  }
  if (!d.body) {
    return h("p", { class: "preview__empty" }, "This document has no text body.");
  }
  if (shape === "notebook") {
    const parsed = parseNotebook(d.body);
    // A file that claims to be a notebook and is not falls through to the text
    // below, because the honest answer to unreadable JSON is the JSON.
    if (parsed) {
      return h(
        "div",
        { class: "preview preview--notebook" },
        truncated(d),
        notebookCells(parsed, { imageNode: (src, alt) => inlineImage(d, src, alt) }),
      );
    }
  }
  if (shape === "prose") {
    const imaging = { imageNode: (src, alt) => inlineImage(d, src, alt) };
    const media = (d.media_type || "").toLowerCase();
    const nodes =
      PROSE[media] === "html" ? htmlDocument(d.body, imaging) : markdown(d.body, imaging);
    // A document whose first line is its own title would otherwise show that
    // title twice, once in the head of the drawer and once at the top of the
    // body, which reads as a rendering bug rather than as a document.
    const first = nodes.firstElementChild;
    if (first && first.tagName === "H1" && same(first.textContent, d.title)) first.remove();
    return h("div", { class: "prose" }, truncated(d), nodes);
  }
  if (shape === "code") {
    // Numbers here and nowhere else. This is the whole file, so its first line
    // is line one and every line below it has an address somebody can send.
    return h(
      "div",
      { class: "preview preview--code" },
      truncated(d),
      codeBlock(d.body, languageOf(d), { numbers: true }),
    );
  }
  // Reachable by keyboard for the same reason a code block is: the preview
  // scrolls, and a scrolling region a keyboard cannot reach is a document a
  // keyboard cannot read.
  return h("div", { class: "preview" }, h("pre", { class: "plain", tabindex: "0" }, d.body));
}

/**
 * truncated is the line that says the body is a prefix.
 *
 * The connector records this when a file went over the extraction budget, and
 * it is worth a sentence on the page because the failure is otherwise invisible:
 * the document is there, it looks complete, and a phrase in the missing half
 * cannot be found by anybody.
 */
function truncated(d) {
  if (!d.properties || d.properties.truncated !== "true") return null;
  return h(
    "p",
    { class: "preview__note" },
    "This file was too large to read in full, so the end of it is missing here and from the index.",
  );
}

/** same compares two headings the way a reader would, ignoring spacing and case. */
function same(a, b) {
  const flat = (v) => String(v || "").trim().replace(/\s+/g, " ").toLowerCase();
  return Boolean(flat(a)) && flat(a) === flat(b);
}

/**
 * figure is a whole document that is an image.
 *
 * The caption carries what the file is rather than a description of it, because
 * a description is the alt text and the alt text belongs to whoever wrote the
 * document.
 */
function figure(d) {
  const caption = h("figcaption", { class: "figure__caption" });
  const img = image(d.id, d.title || "", {
    // The preview fits the image to the space rather than to its own shape, so
    // the box is not held to the aspect ratio the way an inline image is.
    reserve: false,
    onLoad: ({ width, height, blob }) => {
      const parts = [];
      if (width && height) parts.push(`${width} by ${height} pixels`);
      const type = (d.media_type || "").split("/")[1];
      if (type) parts.push(type.replace("+xml", "").toUpperCase());
      const size = Number((d.properties && d.properties.size_bytes) || (blob && blob.size) || 0);
      if (size) parts.push(formatBytes(size));
      caption.textContent = parts.join(" · ");
    },
  });
  return h("figure", { class: "figure" }, img, caption);
}

/**
 * inlineImage draws an image written inside a markdown document.
 *
 * The renderer already decided whether this sits in a figure or in the middle
 * of a sentence, so all this returns is the image itself.
 */
function inlineImage(d, src, alt) {
  const id = resolve(d.id, src);
  if (!id) return null;
  return image(id, alt, { class: "prose__img" });
}

/**
 * resolve turns a path written inside a document into the id of the document
 * holding it.
 *
 * A document id is a source name and a path, and an image beside a page is a
 * path relative to that page, so this is the same join a reader does in their
 * head when they see the markdown.
 */
export function resolve(from, src) {
  const value = String(src || "").trim();
  if (!value || /^[a-z][\w+.-]*:/i.test(value) || value.startsWith("//") || value.startsWith("#")) return "";

  const cut = String(from || "").indexOf(":");
  if (cut < 0) return "";
  const prefix = from.slice(0, cut + 1);
  const base = from.slice(cut + 1).split("/").slice(0, -1);

  const parts = value.startsWith("/") ? [] : base;
  const out = [...parts];
  for (const part of value.replace(/^\//, "").split("/")) {
    if (part === "." || part === "") continue;
    if (part === "..") out.pop();
    else out.push(part);
  }
  return out.length ? prefix + out.join("/") : "";
}

// Images -------------------------------------------------------------------

// Bytes arrive through fetch, so they arrive as a blob and are shown through an
// object URL. The cache keeps the last handful alive: scrolling a result list
// back and forth should not refetch, and holding every image a session ever
// touched should not be how the tab runs out of memory.
const LIVE = new Map();
const LIVE_MAX = 64;

/**
 * TILE and CELL are the sizes the two views ask for.
 *
 * A tile is 56 css pixels and a grid cell is around 200. The tile asks for the
 * size that matches the display it is on, because a 96 pixel picture in a 56
 * pixel box is four times the bytes of a 48 pixel one and looks identical on a
 * screen whose pixel ratio is one.
 *
 * They are the server's sizes rather than ours. A size it does not publish is a
 * refusal, which is what keeps the number of pictures it has to generate a
 * property of the product rather than of whatever a client felt like asking for.
 */
export const TILE = window.devicePixelRatio > 1 ? 96 : 48;
export const CELL = 256;

/**
 * load fetches an image once and hands the same object URL to every caller.
 *
 * The key is the id and the size together, so a row and a grid cell of the same
 * document are two entries. They are two pictures: the thumbnail endpoint is
 * what makes the small one small, and reusing the large one for a 56 pixel box
 * would be the bug this cache exists beside.
 */
function load(id, size, version) {
  const key = size ? `${id} ${size}` : id;
  const held = LIVE.get(key);
  if (held) {
    LIVE.delete(key);
    LIVE.set(key, held);
    return held;
  }

  const wanted = size ? api.thumbnail(id, size, version) : api.content(id);
  const pending = wanted.then((res) => ({ ...res, url: URL.createObjectURL(res.blob) }));
  pending.catch(() => LIVE.delete(key));
  LIVE.set(key, pending);

  while (LIVE.size > LIVE_MAX) {
    const [oldest] = LIVE.keys();
    const dropped = LIVE.get(oldest);
    LIVE.delete(oldest);
    dropped.then((v) => URL.revokeObjectURL(v.url)).catch(() => {});
  }
  return pending;
}

let watcher = null;
const jobs = new WeakMap();

/** near runs the job when the element is close to the viewport. */
function near(el, job) {
  if (!("IntersectionObserver" in window)) {
    job();
    return;
  }
  if (!watcher) {
    watcher = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!entry.isIntersecting) continue;
          watcher.unobserve(entry.target);
          const run = jobs.get(entry.target);
          jobs.delete(entry.target);
          if (run) run();
        }
      },
      { rootMargin: "400px" },
    );
  }
  jobs.set(el, job);
  watcher.observe(el);
}

/**
 * image is a box that fills itself in when it scrolls into view.
 *
 * The box exists before the bytes do and keeps its place afterwards, so nothing
 * below it moves when the image lands. An image that fails to load leaves its
 * alt text, which is the sentence somebody wrote about it and is more use than
 * a broken glyph.
 *
 * options.size asks the server for a thumbnail of that many pixels instead of
 * the file. A list of twenty screenshots drawn from the files themselves is
 * forty megabytes over the wire for four hundred pixels of picture, and no
 * amount of lazy loading makes that cheap once somebody scrolls.
 */
export function image(id, alt, options = {}) {
  const box = h("span", { class: `image ${options.class || ""}`.trim(), "data-state": "idle" });

  near(box, async () => {
    box.dataset.state = "loading";
    try {
      const res = await load(id, options.size, options.version);
      const img = h("img", {
        class: "image__img",
        src: res.url,
        alt: alt || "",
        decoding: "async",
        width: res.width || null,
        height: res.height || null,
      });
      img.addEventListener("load", () => {
        box.dataset.state = "ready";
      });
      img.addEventListener("error", () => fail(box, alt));
      if (res.width && res.height && options.reserve !== false) {
        box.style.aspectRatio = `${res.width} / ${res.height}`;
      }
      box.appendChild(img);
      if (options.onLoad) options.onLoad(res);
    } catch {
      fail(box, alt);
    }
  });

  return box;
}

function fail(box, alt) {
  box.dataset.state = "failed";
  box.textContent = alt || "Image not available";
}

/**
 * tile is the small square at the head of a result row.
 *
 * For an image it is the image, which is the fastest way to recognise one. For
 * everything else it is the icon for the kind on a tint of the source colour,
 * which turns the left edge of the list into something the eye can group by
 * without reading a word.
 */
export function tile(hit) {
  if (shapeOf(hit) === "image") {
    return h(
      "span",
      { class: "tile tile--image" },
      image(hit.id, hit.title || "", { size: TILE, version: hit.modified_at, reserve: false }),
    );
  }
  return h(
    "span",
    {
      class: "tile",
      // The source colour is a two pixel rule under the glyph rather than a
      // fill. A list of twenty tinted squares is a list nobody can scan, and
      // the rule groups by source just as well at a tenth of the ink.
      style: { borderBottomColor: sourceColor(hit.source) },
      "aria-hidden": "true",
    },
    svg(kindIcon(hit.kind), 24),
  );
}

/**
 * cover is the picture at the top of a grid cell.
 *
 * It is the same decision the tile makes at a different size, and the same
 * fallback: a document in the grid that is not an image, or one whose bytes we
 * cannot render, is the icon for its kind rather than a hole in the layout.
 *
 * The frame holds a fixed shape whether or not the picture has arrived, which
 * is what stops a grid from rearranging itself under somebody's pointer while
 * twenty four thumbnails land in whatever order they land in.
 */
export function cover(hit) {
  if (shapeOf(hit) === "image") {
    return h(
      "span",
      { class: "cover" },
      // The alt is empty because the title is written underneath in the cell,
      // and a screen reader that reads the same file name twice in a row is
      // reading twice as much of a page of twenty four images.
      image(hit.id, "", { size: CELL, version: hit.modified_at, class: "cover__img", reserve: false }),
    );
  }
  return h(
    "span",
    { class: "cover cover--icon", style: { borderBottomColor: sourceColor(hit.source) }, "aria-hidden": "true" },
    svg(kindIcon(hit.kind), 32),
  );
}

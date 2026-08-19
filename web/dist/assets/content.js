// Content rendering.
//
// One place decides what a document is and how to draw it. The drawer asks for
// a body and gets prose, code, an image or plain text, and a result row asks for
// a tile and gets a thumbnail or an icon. Everything downstream of that decision
// is presentation, so adding a media type is a line in a table here rather than
// a branch in three views.

import { h, svg } from "./dom.js";
import { api } from "./api.js";
import { render as markdown, codeBlock } from "./markdown.js";
import { kindIcon, sourceColor, bytes as formatBytes } from "./format.js";

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
 * It returns one of image, prose, code or text. The media type is the answer
 * when there is one, because the connector decided it while it had the file in
 * its hands, and the file name is only a fallback for a document that arrived
 * without one.
 */
export function shapeOf(d) {
  const media = (d.media_type || "").toLowerCase();
  if (media.startsWith("image/")) return "image";
  if (media === "text/markdown") return "prose";
  if (CODE[media]) return "code";
  if (!media && languageOf(d)) return "code";
  return "text";
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
 */
export function body(d) {
  const shape = shapeOf(d);

  if (shape === "image") {
    return h("div", { class: "preview preview--image" }, figure(d));
  }
  if (!d.body) {
    return h("p", { class: "preview__empty" }, "This document has no text body.");
  }
  if (shape === "prose") {
    const nodes = markdown(d.body, { imageNode: (src, alt) => inlineImage(d, src, alt) });
    // A document whose first line is its own title would otherwise show that
    // title twice, once in the head of the drawer and once at the top of the
    // body, which reads as a rendering bug rather than as a document.
    const first = nodes.firstElementChild;
    if (first && first.tagName === "H1" && same(first.textContent, d.title)) first.remove();
    return h("div", { class: "prose" }, nodes);
  }
  if (shape === "code") {
    return h("div", { class: "preview preview--code" }, codeBlock(d.body, languageOf(d)));
  }
  return h("div", { class: "preview" }, h("pre", { class: "plain" }, d.body));
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

function load(id) {
  const held = LIVE.get(id);
  if (held) {
    LIVE.delete(id);
    LIVE.set(id, held);
    return held;
  }

  const pending = api.content(id).then((res) => ({ ...res, url: URL.createObjectURL(res.blob) }));
  pending.catch(() => LIVE.delete(id));
  LIVE.set(id, pending);

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
 */
export function image(id, alt, options = {}) {
  const box = h("span", { class: `image ${options.class || ""}`.trim(), "data-state": "idle" });

  near(box, async () => {
    box.dataset.state = "loading";
    try {
      const res = await load(id);
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
    return h("span", { class: "tile tile--image" }, image(hit.id, hit.title || ""));
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

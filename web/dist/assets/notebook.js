// Notebooks.
//
// A notebook is JSON, and a preview that shows it as JSON is showing somebody
// the file format instead of the document. It is a sequence of prose cells and
// code cells, both of which already have a renderer, so this file is mostly
// about reading the shape of the JSON and handing each cell to the right one.
//
// Outputs are the part worth being careful about. A cell can emit a picture, a
// table of HTML, a stack trace or half a megabyte of logging, and the ones that
// are shown here are the ones that are text or a raster image. Everything else
// is named rather than drawn, because a preview that renders whatever a
// notebook emitted is a preview that renders whatever anybody put in a
// notebook.

import { h } from "genba/dom.js";
import { render as markdown, codeBlock } from "genba/markdown.js";

// Image outputs that are drawn. They are decoded by the browser as pictures and
// cannot be anything else, which is not true of the fifth entry every notebook
// format lists, image/svg+xml, because an SVG is a document that can hold a
// script.
const PICTURES = ["image/png", "image/jpeg", "image/gif", "image/webp"];

// How much of one output is shown. A cell that printed a training log has
// nothing to say after the first screen of it, and a preview that carries all
// of it is a preview nobody can scroll past.
const MAX_OUTPUT = 4000;

/**
 * parse reads a notebook, or returns null if this is not one.
 *
 * A file whose media type says notebook and whose bytes say otherwise is a file
 * the caller should fall back on, rather than an error to put on the screen.
 */
export function parse(source) {
  try {
    const nb = JSON.parse(String(source || ""));
    if (!nb || !Array.isArray(nb.cells)) return null;
    return nb;
  } catch {
    return null;
  }
}

/** language is what the code cells in this notebook are written in. */
export function language(nb) {
  const info = nb.metadata || {};
  const named =
    (info.language_info && info.language_info.name) ||
    (info.kernelspec && (info.kernelspec.language || info.kernelspec.name)) ||
    "";
  return String(named).toLowerCase();
}

/** render draws a parsed notebook. */
export function render(nb, options = {}) {
  const lang = language(nb);
  const out = h("div", { class: "notebook" });
  let counted = 0;

  for (const cell of nb.cells) {
    const source = text(cell.source);
    if (cell.cell_type === "markdown" || cell.cell_type === "raw") {
      if (!source.trim()) continue;
      out.appendChild(h("div", { class: "notebook__prose prose" }, markdown(source, options)));
      continue;
    }
    if (cell.cell_type !== "code") continue;

    counted++;
    // The execution count is how somebody reading a notebook knows what ran and
    // in what order, and a cell that never ran says so with a bracket and
    // nothing in it, the same as every notebook interface does.
    const n = Number.isInteger(cell.execution_count) ? String(cell.execution_count) : " ";
    out.appendChild(
      h(
        "div",
        { class: "notebook__cell" },
        h("span", { class: "notebook__count", "aria-hidden": "true" }, `[${n}]`),
        h("div", { class: "notebook__code" }, codeBlock(source, lang)),
        outputs(cell.outputs || []),
      ),
    );
  }

  if (!counted && !out.firstChild) {
    return h("p", { class: "preview__empty" }, "This notebook has no cells.");
  }
  return out;
}

/** outputs draws what a cell produced, or nothing when it produced nothing. */
function outputs(list) {
  const drawn = [];
  for (const out of list) {
    const node = output(out);
    if (node) drawn.push(node);
  }
  if (!drawn.length) return null;
  return h("div", { class: "notebook__outputs" }, drawn);
}

function output(out) {
  const type = out.output_type;

  if (type === "stream") {
    return pre(text(out.text), out.name === "stderr" ? "notebook__out notebook__out--err" : "notebook__out");
  }

  if (type === "error") {
    // A traceback arrives with terminal colour codes in it, which are noise on
    // a page and are the only thing between the reader and the message.
    const body = [out.ename, out.evalue].filter(Boolean).join(": ");
    const trace = text(out.traceback).replace(/\u001b\[[\d;]*m/g, "");
    return pre(trace || body, "notebook__out notebook__out--err");
  }

  if (type !== "execute_result" && type !== "display_data") return null;

  const data = out.data || {};
  for (const mime of PICTURES) {
    if (!data[mime]) continue;
    const encoded = text(data[mime]).replace(/\s+/g, "");
    return h("img", {
      class: "notebook__image",
      src: `data:${mime};base64,${encoded}`,
      alt: "Output of this cell",
      loading: "lazy",
      decoding: "async",
    });
  }
  if (data["text/plain"]) return pre(text(data["text/plain"]), "notebook__out");

  // Something was produced and it is not anything drawn here. Saying so is more
  // use than silence, because a cell with no output and a cell whose output was
  // an interactive widget look identical otherwise.
  const named = Object.keys(data)[0];
  if (!named) return null;
  return h("p", { class: "notebook__elided" }, `Output not shown: ${named}`);
}

function pre(body, cls) {
  const cut = body.length > MAX_OUTPUT;
  // Reachable by keyboard because it scrolls, which is the same rule every
  // other block of preformatted text in the interface follows.
  return h(
    "pre",
    { class: cls, tabindex: "0" },
    cut ? body.slice(0, MAX_OUTPUT) : body,
    cut && h("span", { class: "notebook__elided" }, "\nOutput continues"),
  );
}

/** text joins the array of lines a notebook writes every string as. */
function text(value) {
  if (Array.isArray(value)) return value.join("");
  return String(value === undefined || value === null ? "" : value);
}

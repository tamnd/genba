// Minimal DOM helpers.
//
// This is the whole rendering library. It is about forty lines because the
// interface does not need a framework to be fast, and because everything a
// framework would give us here is something we would then have to build, tag
// and cache.
//
// Nothing in this file interpolates a string into markup. Text goes in through
// textContent and attributes go in through setAttribute, so a document title
// that happens to contain angle brackets is a document title and not a script.

/**
 * h builds an element.
 *
 * Attributes are set as attributes, except for a handful that only exist as
 * properties, and a key starting with "on" is an event listener. A child that
 * is null or false is dropped, so a caller can write conditions inline.
 */
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key.startsWith("on") && typeof value === "function") {
      el.addEventListener(key.slice(2).toLowerCase(), value);
    } else if (key === "class") {
      el.className = value;
    } else if (key === "dataset") {
      Object.assign(el.dataset, value);
    } else if (key === "style" && typeof value === "object") {
      Object.assign(el.style, value);
    } else if (key === "value" || key === "checked" || key === "disabled") {
      el[key] = value;
    } else {
      el.setAttribute(key, value === true ? "" : value);
    }
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue;
    if (Array.isArray(child)) {
      append(el, child);
    } else if (child instanceof Node) {
      el.appendChild(child);
    } else {
      el.appendChild(document.createTextNode(String(child)));
    }
  }
}

/** clear removes every child of a node. */
export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

/** replace swaps a node's contents for the given children in one go. */
export function replace(node, ...children) {
  clear(node);
  append(node, children);
  return node;
}

/** svg builds an inline icon from a path, sized to the surrounding text. */
export function svg(path, size = 16) {
  const el = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  el.setAttribute("viewBox", "0 0 24 24");
  el.setAttribute("width", size);
  el.setAttribute("height", size);
  el.setAttribute("fill", "none");
  el.setAttribute("stroke", "currentColor");
  el.setAttribute("stroke-width", "1.8");
  el.setAttribute("stroke-linecap", "round");
  el.setAttribute("stroke-linejoin", "round");
  el.setAttribute("aria-hidden", "true");
  const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
  p.setAttribute("d", path);
  el.appendChild(p);
  return el;
}

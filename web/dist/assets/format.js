// Formatting shared by every view.

const ICONS = {
  search: "M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14ZM20 20l-4-4",
  home: "M4 10.5 12 4l8 6.5V20a1 1 0 0 1-1 1h-4v-6H9v6H5a1 1 0 0 1-1-1v-9.5Z",
  clock: "M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18ZM12 7v5l3.5 2",
  star: "m12 4 2.4 5 5.6.8-4 3.9.9 5.5-4.9-2.6-4.9 2.6.9-5.5-4-3.9 5.6-.8Z",
  people: "M8 11a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7ZM2 20a6 6 0 0 1 12 0M17 20a5 5 0 0 0-3-4.6M16.5 11a3 3 0 1 0 0-6",
  doc: "M6 3h7l5 5v13a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1ZM13 3v5h5",
  chat: "M4 5h16v11H9l-5 4V5Z",
  ticket: "M5 5h14v5a2 2 0 0 0 0 4v5H5v-5a2 2 0 0 0 0-4V5Z",
  code: "m9 8-5 4 5 4M15 8l5 4-5 4",
  close: "M6 6l12 12M18 6 6 18",
  external: "M14 5h5v5M19 5l-8 8M18 14v5H5V6h5",
  slider: "M4 8h10M18 8h2M4 16h4M12 16h8M14 5v6M8 13v6",
  sun: "M12 5v-2M12 21v-2M5 12H3M21 12h-2M6.6 6.6 5.2 5.2M18.8 18.8l-1.4-1.4M6.6 17.4l-1.4 1.4M18.8 5.2l-1.4 1.4M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8Z",
  moon: "M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5Z",
  keyboard: "M4 7h16v10H4V7ZM7 11h.01M11 11h.01M15 11h.01M8 14h8",
  menu: "M4 7h16M4 12h16M4 17h16",
  check: "m5 12 5 5 9-10",
  alert: "M12 4 2.5 20h19L12 4ZM12 10v4M12 17.5h.01",
  flag: "M6 21V4M6 4.5h12l-3 4 3 4H6",
  rows: "M4 6h16M4 10h16M4 14h16M4 18h16",
  grid: "M4 4h7v7H4V4ZM13 4h7v7h-7V4ZM4 13h7v7H4v-7ZM13 13h7v7h-7v-7Z",
  preview: "M4 6h16v12H4zM4 10h16",
  copy: "M9 9h10v11H9zM5 15V4h10",
  image: "M4 5h16v14H4V5ZM4 16l4.5-4.5 3 3 3.5-3.5L20 16M9.5 9.5h.01",
  "arrow-left": "M19 12H5M11 6l-6 6 6 6",
  "arrow-right": "M5 12h14M13 6l6 6-6 6",
  "arrow-up": "M12 19V5M6 11l6-6 6 6",
  "arrow-down": "M12 5v14M6 13l6 6 6-6",
  lock: "M8 10V7.5a4 4 0 0 1 8 0V10M5.5 10h13v10h-13V10Z",
  link: "M10.5 13.5a4 4 0 0 0 5.7 0l2.3-2.3a4 4 0 0 0-5.7-5.7l-1.1 1.1M13.5 10.5a4 4 0 0 0-5.7 0l-2.3 2.3a4 4 0 0 0 5.7 5.7l1.1-1.1",
};

export function icon(name) {
  return ICONS[name] || ICONS.doc;
}

const KIND_ICONS = {
  page: "doc",
  file: "doc",
  message: "chat",
  email: "chat",
  ticket: "ticket",
  code: "code",
  person: "people",
  calendar: "clock",
  image: "image",
  video: "image",
};

export function kindIcon(kind) {
  return icon(KIND_ICONS[kind] || "doc");
}

const SOURCE_COLORS = {
  gdrive: "var(--source-drive)",
  drive: "var(--source-drive)",
  slack: "var(--source-slack)",
  github: "var(--source-github)",
  jira: "var(--source-jira)",
  notion: "var(--source-notion)",
};

export function sourceColor(source) {
  return SOURCE_COLORS[source] || "var(--source-files)";
}

// Schemes a browser will not navigate to from a page served over HTTP.
//
// A file:// link is the one that matters, because the file connector writes one
// for every document it reads and clicking it does nothing at all: no
// navigation, no error, no console message. The others are here because a
// document's URL comes from a connector and a connector reads a corpus we did
// not write.
const DEAD = new Set(["file:", "data:", "blob:", "javascript:", "vbscript:", "about:"]);

/**
 * followable reports whether clicking this URL would go anywhere.
 *
 * A custom scheme is followable: slack:// and vscode:// are handed to the
 * operating system and open the application somebody wanted. The test is the
 * short list above rather than a list of the schemes we approve of, because a
 * connector we have not written yet should not have to be added to a table
 * here before its links work.
 */
export function followable(url) {
  if (!url) return false;
  try {
    return !DEAD.has(new URL(url, location.href).protocol);
  } catch {
    return false;
  }
}

/**
 * copyable is what goes on the clipboard in place of a link nobody can follow.
 *
 * For a file URL that is the path, decoded, which is what somebody pastes into
 * a terminal or an editor. For anything else it is the URL as it arrived,
 * because we do not know enough about it to improve on it.
 */
export function copyable(url) {
  const value = String(url || "");
  try {
    const parsed = new URL(value, location.href);
    if (parsed.protocol !== "file:") return value;
    return decodeURIComponent(parsed.pathname) || value;
  } catch {
    return value;
  }
}

/** title cases a source or kind for display without touching the value. */
export function label(value) {
  if (!value) return "";
  return value.charAt(0).toUpperCase() + value.slice(1);
}

/**
 * facetLabels is what a filter row shows for each value in one facet.
 *
 * A facet value is an identity. It goes in the URL, it goes in the request and
 * it is what the count was counted against, so it is never changed. What it is
 * not is something to read: a container on a file corpus is an absolute path,
 * and a column of them truncated on the right is a column of identical rows
 * ending in an ellipsis, distinguished only by the part that was cut off.
 *
 * So a value has three forms. The value itself, a label, which is the last
 * segment, because the end of a path is the part that identifies it, and a
 * context, which is everything in front of it and is elided from the left when
 * it does not fit.
 *
 * Where two values in the same facet would produce the same label, both grow a
 * segment, and they keep growing until they differ. Four folders called en-US
 * under four different books are four rows that all read en-US, which is worse
 * than a truncated path because it reads as a bug rather than as a truncation.
 * Growing is per collision rather than per facet, so one pair of deep duplicates
 * does not lengthen every other row in the list.
 *
 * It is done here rather than on the server because it is a display decision,
 * and the server has no idea how wide the window is.
 */
export function facetLabels(values) {
  const parts = values.map((v) => String(v || "").split("/").filter(Boolean));
  const depth = parts.map(() => 1);
  // Terminates because a round only ever deepens a value that has a segment
  // left to give, and a value that has run out is left where it is: two paths
  // that differ only above their common root end up identical and readable,
  // which is the truthful answer rather than a loop.
  for (;;) {
    const groups = new Map();
    parts.forEach((segments, i) => {
      const key = segments.slice(segments.length - depth[i]).join("/");
      const group = groups.get(key);
      if (group) group.push(i);
      else groups.set(key, [i]);
    });
    let grew = false;
    for (const group of groups.values()) {
      if (group.length < 2) continue;
      for (const i of group) {
        if (depth[i] >= parts[i].length) continue;
        depth[i]++;
        grew = true;
      }
    }
    if (!grew) break;
  }
  return values.map((value, i) => {
    const segments = parts[i];
    if (!segments.length) return { value, label: String(value || ""), context: "" };
    const at = Math.max(segments.length - depth[i], 0);
    return {
      value,
      label: segments.slice(at).join("/"),
      // Spaced, because this is being read rather than pasted, and a spaced
      // separator is where a long one is allowed to wrap or to be cut.
      context: segments.slice(0, at).join(" / "),
    };
  });
}

const RELATIVE = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
const ABSOLUTE = new Intl.DateTimeFormat(undefined, {
  year: "numeric",
  month: "short",
  day: "numeric",
});

const UNITS = [
  ["year", 365 * 24 * 3600],
  ["month", 30 * 24 * 3600],
  ["week", 7 * 24 * 3600],
  ["day", 24 * 3600],
  ["hour", 3600],
  ["minute", 60],
];

/**
 * when formats a timestamp the way a person reads it: relative while that is
 * still meaningful, and an actual date once it is not.
 */
export function when(iso) {
  if (!iso) return "";
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return "";
  const seconds = (t.getTime() - Date.now()) / 1000;
  const magnitude = Math.abs(seconds);
  if (magnitude > 180 * 24 * 3600) return ABSOLUTE.format(t);
  for (const [unit, size] of UNITS) {
    if (magnitude >= size) return RELATIVE.format(Math.round(seconds / size), unit);
  }
  return RELATIVE.format(Math.round(seconds), "second");
}

export function exact(iso) {
  if (!iso) return "";
  const t = new Date(iso);
  return Number.isNaN(t.getTime()) ? "" : t.toLocaleString();
}

const NUMBER = new Intl.NumberFormat();

export function number(n) {
  return NUMBER.format(n || 0);
}

/**
 * roughly renders an estimate as an estimate.
 *
 * A count of a tree that is still being walked is not exact and printing it to
 * the unit says it is. Twenty two thousand two hundred and thirty five is a
 * claim about a number nobody counted; twenty two thousand is the same
 * information without the claim, and it is what somebody reads off the line
 * anyway.
 */
export function roughly(n) {
  const size = Number(n) || 0;
  if (size < 100) return number(size);
  const step = size < 1000 ? 10 : size < 10000 ? 100 : 1000;
  return number(Math.round(size / step) * step);
}

/** duration renders the server's own timing, rounded to something readable. */
export function duration(ms) {
  if (ms === undefined || ms === null) return "";
  if (ms < 1) return "under a millisecond";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

/**
 * bytes renders a file size the way a file manager does.
 *
 * Two significant figures is all anybody reads off a caption, and the unit is
 * the decimal one because that is what the operating system showing the same
 * file will say.
 */
export function bytes(n) {
  const size = Number(n) || 0;
  if (size < 1000) return `${size} B`;
  const units = ["KB", "MB", "GB"];
  let value = size / 1000;
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000;
    unit++;
  }
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`;
}

export function initials(name) {
  const parts = String(name || "?").trim().split(/[\s@._-]+/).filter(Boolean);
  if (!parts.length) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[1][0]).toUpperCase();
}

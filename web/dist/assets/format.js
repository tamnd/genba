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
  rows: "M4 6h16M4 10h16M4 14h16M4 18h16",
  preview: "M4 6h16v12H4zM4 10h16",
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

/** title cases a source or kind for display without touching the value. */
export function label(value) {
  if (!value) return "";
  return value.charAt(0).toUpperCase() + value.slice(1);
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

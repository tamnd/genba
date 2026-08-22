// The two preferences that survive a reload.
//
// Both belong to the person rather than to the corpus, both are local to this
// browser, and neither is ever sent to the server. They live in a module of
// their own because three places have to agree about them: the inline script in
// the document, which applies them before the first paint, the shell, which
// toggles them from the header, and the settings screen, where they are
// actually chosen.
//
// The theme has three choices and only two of them are ever stored. No key at
// all means follow the system, which is what somebody who has never opened the
// settings screen gets and what choosing System puts back. A stored value that
// meant the same thing would be a second way of saying nothing, and the inline
// script would have to know about it too.

export const THEME_KEY = "genba.theme";
export const DENSITY_KEY = "genba.density";

/** THEMES is the choice, in the order it is offered. */
export const THEMES = [
  { value: "", label: "System", hint: "Follow the appearance this device is set to" },
  { value: "light", label: "Light", hint: "" },
  { value: "dark", label: "Dark", hint: "" },
];

/**
 * DENSITIES is the other one.
 *
 * Compact reduces the row padding and drops the body size one step. It does not
 * reintroduce borders, change the colour system or touch the type scale above
 * body, so it is two token overrides rather than a second theme.
 */
export const DENSITIES = [
  { value: "comfortable", label: "Comfortable", hint: "Room to read one document at a time" },
  { value: "compact", label: "Compact", hint: "More rows on screen, for triaging a long list" },
];

const dark = matchMedia("(prefers-color-scheme: dark)");

/** theme is the stored choice, or empty for the system's. */
export function theme() {
  return read(THEME_KEY);
}

/** density is the stored choice, which has a default rather than a system. */
export function density() {
  return read(DENSITY_KEY) || DENSITIES[0].value;
}

/** setTheme stores a choice and applies it. An empty value is the system's. */
export function setTheme(value) {
  write(THEME_KEY, value);
  apply();
}

export function setDensity(value) {
  write(DENSITY_KEY, value === DENSITIES[0].value ? "" : value);
  apply();
}

/**
 * apply writes both preferences onto the root element.
 *
 * The theme is resolved here rather than left absent, because the header button
 * reads what is on screen to decide which way to toggle, and an absent
 * attribute would make it guess.
 */
export function apply() {
  const root = document.documentElement;
  root.dataset.theme = theme() || (dark.matches ? "dark" : "light");
  root.dataset.density = density();
}

// A device that changes its mind while the tab is open, which is what happens
// at sunset on a machine set to switch automatically. Only where nothing was
// chosen: somebody who asked for light stays in light after dark.
dark.addEventListener("change", () => {
  if (!theme()) apply();
});

// Storage throws rather than returning nothing in a browser that has turned it
// off, and a preference that cannot be saved is a preference that lasts until
// the reload rather than a screen that fails to paint.
function read(key) {
  try {
    return localStorage.getItem(key) || "";
  } catch {
    return "";
  }
}

function write(key, value) {
  try {
    if (value) localStorage.setItem(key, value);
    else localStorage.removeItem(key);
  } catch {
    // Nothing to say about it. The choice applies to this tab either way.
  }
}

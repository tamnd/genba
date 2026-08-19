// The application shell.
//
// It owns three things and nothing else: the URL, the keyboard, and which view
// is on screen. Everything that draws is in its own module and takes a callback
// rather than reaching back in here, which is what keeps a change to the result
// card from being a change to routing.

import { h, replace, svg } from "./dom.js";
import { api, identity, setIdentity, ApiError } from "./api.js";
import * as urlState from "./state.js";
import { icon, initials, label, sourceColor } from "./format.js";
import { Omnibox, shortcutLabel } from "./omnibox.js";
import { Results, VERTICALS } from "./results.js";
import { Drawer } from "./drawer.js";
import { Home } from "./home.js";

const THEME_KEY = "genba.theme";

class App {
  constructor(root) {
    this.root = root;
    this.session = null;
    this.query = urlState.read();
    this.pending = null;
    this.lastRequest = null;

    this.omnibox = new Omnibox({
      onSearch: (text) => this.go({ ...this.query, q: text, offset: 0, open: "" }),
      onOpen: (id) => this.open(id),
    });
    this.results = new Results({
      onQuery: (q) => this.go(q),
      onOpen: (id) => this.open(id),
    });
    this.home = new Home({
      onQuery: (q) => this.go({ ...urlState.read(""), ...q }),
      onOpen: (id) => this.open(id),
    });
    this.drawer = new Drawer({
      onClose: () => this.go({ ...this.query, open: "" }, { replace: true }),
    });

    this.main = h("main", { class: "main", id: "main" });
    this.live = h("div", {
      class: "visually-hidden",
      role: "status",
      "aria-live": "polite",
      "aria-atomic": "true",
    });

    this.rail = this.buildRail();
    this.build();
    this.bindKeys();

    window.addEventListener("popstate", () => {
      this.query = urlState.read();
      this.sync();
    });
  }

  build() {
    replace(
      this.root,
      h(
        "div",
        { class: "app" },
        h("a", { class: "visually-hidden", href: "#main" }, "Skip to results"),
        this.rail,
        h(
          "header",
          { class: "header" },
          h(
            "button",
            {
              class: "icon-button rail__toggle",
              type: "button",
              "aria-label": "Menu",
              onClick: () => {
                const open = this.rail.dataset.open === "true";
                this.rail.dataset.open = String(!open);
              },
            },
            svg(icon("menu")),
          ),
          this.omnibox.el,
          h(
            "div",
            { class: "header__actions" },
            h(
              "button",
              {
                class: "icon-button",
                type: "button",
                "aria-label": "Keyboard shortcuts",
                title: "Keyboard shortcuts (?)",
                onClick: () => this.shortcuts(),
              },
              svg(icon("keyboard")),
            ),
            h(
              "button",
              {
                class: "icon-button",
                type: "button",
                "aria-label": "Switch theme",
                title: "Switch theme",
                onClick: () => this.theme(),
              },
              svg(icon(document.documentElement.dataset.theme === "dark" ? "sun" : "moon")),
            ),
          ),
        ),
        this.main,
      ),
      this.drawer.scrim,
      this.drawer.el,
      this.live,
    );
  }

  buildRail() {
    const who = identity();
    return h(
      "nav",
      { class: "rail", "aria-label": "Main" },
      h(
        "a",
        { class: "brand", href: "/", onClick: (e) => this.link(e, {}) },
        h("span", { class: "brand__mark" }, "G"),
        "genba",
      ),
      h(
        "div",
        { class: "rail__section" },
        h(
          "a",
          {
            class: "rail__link",
            href: "/",
            "aria-current": this.query.q || urlState.count(this.query) ? null : "page",
            onClick: (e) => this.link(e, {}),
          },
          svg(icon("home"), 15),
          "Home",
        ),
        h(
          "a",
          {
            class: "rail__link",
            href: "?sort=recent",
            onClick: (e) => this.link(e, { q: "", sort: "recent" }),
          },
          svg(icon("clock"), 15),
          "Recent",
        ),
      ),
      h(
        "div",
        { class: "rail__section" },
        h("h2", { class: "rail__title" }, "Verticals"),
        VERTICALS.filter((v) => v.id !== "all").map((v) =>
          h(
            "a",
            {
              class: "rail__link",
              href: `?tab=${v.id}`,
              onClick: (e) => this.link(e, { ...this.query, tab: v.id, kind: [], offset: 0 }),
            },
            svg(icon(v.id === "people" ? "people" : v.id === "code" ? "code" : v.id === "messages" ? "chat" : v.id === "tickets" ? "ticket" : "doc"), 15),
            v.title,
          ),
        ),
      ),
      h("div", { class: "rail__section", id: "rail-sources" }),
      h(
        "button",
        {
          class: "rail__foot",
          type: "button",
          title: "Change who you are searching as",
          onClick: () => this.switchIdentity(),
        },
        h("span", { class: "avatar" }, initials(who.subject)),
        h("span", {}, who.subject),
        svg(icon("slider"), 14),
      ),
    );
  }

  /** link makes a rail entry a real anchor that still routes on the client. */
  link(e, query) {
    if (e.metaKey || e.ctrlKey || e.shiftKey) return;
    e.preventDefault();
    this.go({ ...urlState.read(""), ...query });
  }

  async start() {
    try {
      this.session = await api.me();
      this.renderSources();
    } catch (err) {
      this.fail(err);
      return;
    }
    this.sync();
  }

  renderSources() {
    const holder = this.rail.querySelector("#rail-sources");
    const sources = (this.session && this.session.sources) || [];
    if (!sources.length) {
      replace(holder);
      return;
    }
    replace(
      holder,
      h("h2", { class: "rail__title" }, "Sources"),
      sources.slice(0, 8).map((s) =>
        h(
          "a",
          {
            class: "rail__link",
            href: `?source=${encodeURIComponent(s.value)}`,
            onClick: (e) => this.link(e, { source: [s.value] }),
          },
          h("span", { class: "source__dot", style: { background: sourceColor(s.value) } }),
          label(s.value),
          h("span", { class: "rail__count" }, s.count),
        ),
      ),
    );
  }

  /** go pushes a new query into the URL and renders it. */
  go(query, opts = {}) {
    const url = urlState.write(query);
    if (opts.replace) history.replaceState(null, "", url);
    else history.pushState(null, "", url);
    this.query = urlState.read();
    this.sync();
  }

  /** sync renders whatever the URL currently says. */
  sync() {
    this.omnibox.value = this.query.q;
    const searching = Boolean(this.query.q) || urlState.count(this.query) > 0 || Boolean(this.query.sort);

    if (searching) this.search();
    else this.showHome();

    if (this.query.open && this.drawer.currentId !== this.query.open) {
      this.drawer.currentId = this.query.open;
      this.drawer.show(this.query.open);
    } else if (!this.query.open) {
      this.drawer.currentId = null;
      if (this.drawer.open) this.drawer.close();
    }
  }

  async showHome() {
    if (this.main.firstChild !== this.home.el) replace(this.main, this.home.el);
    await this.home.render(this.session);
  }

  async search() {
    if (this.main.firstChild !== this.results.el) replace(this.main, this.results.el);

    const request = urlState.params(this.query, VERTICALS);
    this.results.loading(this.query);

    if (this.pending) this.pending.abort();
    const controller = new AbortController();
    this.pending = controller;
    try {
      const res = await api.search(request, controller.signal);
      if (controller.signal.aborted) return;
      this.results.render(this.query, res);
      this.announce(res);
    } catch (err) {
      if (err.name === "AbortError") return;
      this.fail(err);
    }
  }

  /**
   * announce tells a screen reader what happened, because the results appear
   * somewhere else on the page and nothing else would say so.
   */
  announce(res) {
    const n = res.total || 0;
    replace(this.live, `${n} ${n === 1 ? "result" : "results"} for ${this.query.q || "the current filters"}`);
  }

  fail(err) {
    const unauthenticated = err instanceof ApiError && err.status === 401;
    replace(
      this.main,
      h(
        "div",
        { class: "state state--error" },
        h("p", { class: "state__title" }, unauthenticated ? "Not signed in" : "Something went wrong"),
        h(
          "p",
          { class: "state__body" },
          unauthenticated
            ? "This build authenticates from a request header. Pick who to search as and try again."
            : err.message,
        ),
        h(
          "div",
          { class: "state__actions" },
          unauthenticated
            ? h(
                "button",
                { class: "button button--primary", type: "button", onClick: () => this.switchIdentity() },
                "Choose an identity",
              )
            : h(
                "button",
                { class: "button button--primary", type: "button", onClick: () => this.sync() },
                "Try again",
              ),
        ),
      ),
    );
  }

  open(id) {
    this.go({ ...this.query, open: id }, { replace: true });
  }

  theme() {
    const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    localStorage.setItem(THEME_KEY, next);
    this.build();
    this.sync();
  }

  /**
   * switchIdentity is how the permission model gets tested by hand.
   *
   * Every request carries these headers and the storage driver applies them, so
   * changing the subject or the groups here changes what the same query
   * returns. That is the fastest way to see for yourself that the filter is
   * real.
   */
  switchIdentity() {
    const who = identity();
    const subject = prompt("Search as which subject?", who.subject);
    if (subject === null) return;
    const tenant = prompt("Tenant, or empty for the single tenant case", who.tenant);
    if (tenant === null) return;
    const groups = prompt(
      "Groups, comma separated. These are the group keys a connector wrote, for example gdrive:eng@acme.com",
      who.groups.join(","),
    );
    if (groups === null) return;
    const identities = prompt(
      "Source identities, comma separated, as source:value. For example gdrive:mei@acme.com",
      who.identities.join(","),
    );
    if (identities === null) return;

    setIdentity({
      subject: subject.trim() || "dev",
      tenant: tenant.trim(),
      groups: groups.split(",").map((s) => s.trim()).filter(Boolean),
      identities: identities.split(",").map((s) => s.trim()).filter(Boolean),
    });
    location.reload();
  }

  shortcuts() {
    const rows = [
      ["Focus search", [shortcutLabel()]],
      ["Next result", ["j"]],
      ["Previous result", ["k"]],
      ["Open preview", ["Enter"]],
      ["Open in source", ["o"]],
      ["Go home", ["g", "h"]],
      ["Close", ["Esc"]],
      ["This list", ["?"]],
    ];
    const returnTo = document.activeElement;
    const dismiss = () => {
      dialog.remove();
      if (returnTo && returnTo.focus) returnTo.focus();
    };
    const dialog = h(
      "div",
      {
        class: "dialog",
        role: "dialog",
        "aria-modal": "true",
        "aria-label": "Keyboard shortcuts",
        onClick: (e) => {
          if (e.target === dialog) dismiss();
        },
        onKeydown: (e) => {
          if (e.key === "Escape" || e.key === "?") {
            e.preventDefault();
            e.stopPropagation();
            dismiss();
          }
        },
      },
      h(
        "div",
        { class: "dialog__panel", tabindex: "-1" },
        h("h2", { class: "dialog__title" }, "Keyboard shortcuts"),
        rows.map(([what, keys]) =>
          h(
            "div",
            { class: "shortcut" },
            what,
            h("span", { class: "shortcut__keys" }, keys.map((k) => h("kbd", { class: "kbd" }, k))),
          ),
        ),
      ),
    );
    document.body.appendChild(dialog);
    dialog.querySelector(".dialog__panel").focus();
  }

  bindKeys() {
    let chord = null;
    document.addEventListener("keydown", (e) => {
      const typing = e.target.matches("input, textarea, select");

      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        this.omnibox.focus();
        return;
      }
      if (e.key === "Escape") {
        if (this.drawer.open) return; // the drawer handles its own Escape
        if (typing) e.target.blur();
        return;
      }
      if (typing || e.metaKey || e.ctrlKey || e.altKey) return;

      // A two key sequence: g then a destination. The first key arms it and it
      // disarms itself, so a stray g does nothing rather than waiting forever.
      if (chord === "g") {
        chord = null;
        const destinations = { h: {}, s: { q: "", sort: "recent" }, a: { tab: "all" } };
        if (destinations[e.key]) {
          e.preventDefault();
          this.go({ ...urlState.read(""), ...destinations[e.key] });
        }
        return;
      }

      switch (e.key) {
        case "/":
          e.preventDefault();
          this.omnibox.focus();
          break;
        case "g":
          chord = "g";
          setTimeout(() => (chord = null), 1200);
          break;
        case "j":
          e.preventDefault();
          this.results.move(1);
          break;
        case "k":
          e.preventDefault();
          this.results.move(-1);
          break;
        case "Enter":
        case "p": {
          const hit = this.results.current();
          if (!hit) return;
          e.preventDefault();
          this.open(hit.id);
          break;
        }
        case "o": {
          const hit = this.results.current();
          if (!hit || !hit.url) return;
          e.preventDefault();
          window.open(hit.url, "_blank", "noreferrer");
          break;
        }
        case "?":
          e.preventDefault();
          this.shortcuts();
          break;
        default:
          break;
      }
    });
  }
}

// Theme before first paint, so a dark theme does not arrive as a white flash.
const saved = localStorage.getItem(THEME_KEY);
if (saved) {
  document.documentElement.dataset.theme = saved;
} else if (window.matchMedia("(prefers-color-scheme: dark)").matches) {
  document.documentElement.dataset.theme = "dark";
}

const app = new App(document.getElementById("app"));
app.start();

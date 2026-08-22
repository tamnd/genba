// The half of the administration screen that changes something.
//
// Everything else on that screen reports. This adds a connector, switches one
// on and off, asks for a sync now and forgets one, which between them are the
// operations that today mean editing a unit file and restarting the server.
// Doing them here is not a convenience: a restart drops every connector for as
// long as it takes to come back, so the cost of adding a second directory is
// paid by the first one.
//
// Two rules run through the whole file. The server's own sentence is what gets
// printed, because the useful half of a refusal is the part this file cannot
// write: which field is wrong, or that the connector came from the command line
// and there is a unit file to edit. And every write answers with the connector
// list that resulted from it, so the screen paints what is true afterwards
// rather than guessing and then asking.

import { h, replace, svg } from "genba/dom.js";
import { api } from "genba/api.js";
import { icon } from "genba/format.js";

// The directory connector's settings, in the order somebody fills them in.
//
// These are written out here rather than asked for, because the server has no
// endpoint that describes its own connectors, and every key in this list is a
// field the supervisor actually reads. That matters more than it looks: the
// settings are passed through untouched, so a key invented in this file would
// be accepted by the decoder, ignored by the connector, and never mentioned
// again by anybody.
const CORPUS = [
  {
    key: "dir",
    name: "Directory",
    hint: "An absolute path on the server, not on the machine this browser is on",
  },
  {
    key: "acl",
    name: "Who may read it",
    type: "choice",
    choices: [
      ["tenant", "Everybody in the tenant"],
      ["owners", "Whoever the OWNERS files name"],
      ["os", "Whoever the file permissions allow"],
    ],
    hint: "The tenant rule is the right one for documentation everybody may read",
  },
  {
    key: "identity",
    name: "Identity source",
    hint: "Which directory the account names on those files belong to, such as unix. The file permissions rule needs one",
  },
  {
    key: "domain",
    name: "Domain",
    hint: "The domain a world readable file grants to. Empty leaves it granting nothing",
  },
  {
    key: "refresh",
    name: "Sync every",
    hint: "Like 30s or 5m. Empty syncs once, when it starts",
  },
  {
    key: "reconcile",
    name: "Sweep every",
    hint: "How often to count the index against the tree. Empty sweeps after every sync",
  },
  {
    key: "watch",
    name: "Watch for changes",
    type: "check",
    hint: "Ask the operating system what moved instead of walking the tree. Needs a sync interval to be any use",
  },
];

// The object storage connector's settings.
const BUCKET = [
  { key: "bucket", name: "Bucket", hint: "The bucket to read" },
  {
    key: "endpoint",
    name: "Endpoint",
    hint: "Where the request goes, like https://s3.eu-west-1.amazonaws.com",
  },
  { key: "region", name: "Region", hint: "Part of what the signature covers. Empty means us-east-1" },
  { key: "prefix", name: "Prefix", hint: "Narrows the listing to one part of the bucket" },
  {
    key: "acl",
    name: "Who may read it",
    type: "choice",
    choices: [
      ["tenant", "Everybody in the tenant"],
      ["bucket", "Whoever the bucket policy names"],
      ["object", "Whoever each object's own policy names"],
    ],
    hint: "The object rule reads one policy per object, which on a large bucket is the slowest thing this server does",
  },
  {
    key: "identity",
    name: "Identity source",
    hint: "Which directory the names in those policies belong to. The bucket and object rules need one",
  },
  {
    key: "domain",
    name: "Domain",
    hint: "The mail domain that counts as this tenant. Empty leaves every grant written as an address foreign",
  },
  {
    key: "path_style",
    name: "Bucket in the path",
    type: "check",
    hint: "What MinIO and Ceph need, and what S3 itself does not",
  },
  { key: "refresh", name: "Sync every", hint: "Like 5m or 1h. Empty lists once, when it starts" },
  {
    key: "reconcile",
    name: "Sweep every",
    hint: "How often to count the index against the bucket. Empty sweeps after every sync",
  },
  {
    key: "rate",
    name: "Requests a second",
    type: "number",
    hint: "The ceiling the crawl keeps itself under. Empty takes a cautious default",
  },
  {
    key: "burst",
    name: "Burst",
    type: "number",
    hint: "How many requests may go out back to back before the rate binds",
  },
  {
    key: "retries",
    name: "Retries",
    type: "number",
    hint: "How many times a refused request is tried again before the sync gives up on it",
  },
];

const KINDS = [
  { value: "corpus", name: "Directory", fields: CORPUS },
  {
    value: "bucket",
    name: "Object storage",
    fields: BUCKET,
    // Said on the screen because somebody looking for a field to type a secret
    // into needs to be told there is not one, rather than left to conclude the
    // form is unfinished.
    note: "The keys come from the environment this server was started in, so a bucket added here is read with the credentials it already holds. Nothing typed on this screen is stored as a secret.",
  },
];

export class Connectors {
  /**
   * onChanged is called after every write with the list that came back, or
   * with null for a change that was only to this screen, such as asking to
   * confirm a removal. The screen around this one repaints from it.
   */
  constructor({ onChanged }) {
    this.onChanged = onChanged;
    this.kind = KINDS[0].value;

    // Which source is being changed, or "form" for the one being added. A name
    // rather than a flag, because the line that reports it says which connector
    // is changing, and because a second press has to be dropped for that one
    // write rather than for the whole screen.
    this.busy = "";
    this.error = null;
    this.said = "";

    // The source whose removal has been asked for once. Removing forgets how a
    // connector was configured, which nothing here can undo, so it is asked
    // twice. The confirmation replaces the button that opened it, which means a
    // keyboard lands on it without having to go looking.
    this.confirming = "";

    // Every field ever built, by kind and key, so that switching from one kind
    // to the other and back does not empty what was typed into it.
    this.made = new Map();

    // The nodes that outlive a paint. The screen around this repaints itself
    // every five seconds, and a form rebuilt on that timer would be a form that
    // empties itself while somebody is filling it in.
    this.el = h("section", { class: "panel connectors" });
    this.fields = h("div", { class: "connectors__fields" });
    this.aside = h("div", { class: "connectors__note" });
    this.output = h("p", {
      class: "connectors__output",
      role: "status",
      "aria-live": "polite",
      tabindex: "-1",
      "data-focus-fallback": "",
    });

    const form = this.build();
    replace(
      this.el,
      h(
        "div",
        { class: "panel__head" },
        h("h2", { class: "panel__title" }, "Add a connector"),
        h("span", { class: "panel__note" }, "Takes effect without a restart"),
      ),
      h(
        "p",
        { class: "connectors__lead" },
        "It is remembered across restarts, and starts crawling as soon as it is added unless the box says otherwise.",
      ),
      form,
    );
    this.fill();
    this.say();
  }

  /** build is the form, made once and kept. */
  build() {
    const form = h("form", {
      class: "connectors__form",
      onSubmit: (e) => {
        e.preventDefault();
        this.add();
      },
    });

    this.source = field({
      key: "source",
      name: "Name",
      hint: "What the documents are filed under, like handbook. An existing name replaces that connector",
    });
    this.picker = h(
      "select",
      {
        class: "connectors__input",
        id: "connector-kind",
        onChange: (e) => {
          this.kind = e.target.value;
          this.fill();
        },
      },
      KINDS.map((k) => h("option", { value: k.value }, k.name)),
    );
    this.enabled = field({
      key: "enabled",
      name: "Start it now",
      type: "check",
      hint: "Leave it off to add the settings and start it later",
    });
    this.enabled.input.checked = true;
    const submit = h("button", { class: "button button--primary", type: "submit" }, "Add connector");

    replace(
      form,
      h(
        "div",
        { class: "connectors__fields" },
        this.source.el,
        wrap("Kind", "connector-kind", "What sort of source it is", this.picker),
        this.enabled.el,
      ),
      this.fields,
      this.aside,
      h("div", { class: "connectors__actions" }, submit),
    );
    return form;
  }

  /** fill paints the fields the chosen kind needs, keeping what was typed. */
  fill() {
    const kind = this.chosen();
    replace(this.fields, ...kind.fields.map((spec) => this.field(kind.value, spec).el));
    replace(this.aside, kind.note ? h("p", { class: "connectors__aside" }, kind.note) : null);
  }

  chosen() {
    return KINDS.find((k) => k.value === this.kind) || KINDS[0];
  }

  /** field is one setting's input, built the first time it is asked for. */
  field(kind, spec) {
    const id = `connector-${kind}-${spec.key}`;
    let made = this.made.get(id);
    if (!made) {
      made = field({ ...spec, id });
      this.made.set(id, made);
    }
    return made;
  }

  /**
   * config reads the fields into the settings the connector is given.
   *
   * An empty field is left out rather than sent as an empty string, because the
   * server reads absent as take the default and a form that sent every key
   * would be a form that overwrote a region with nothing.
   */
  config() {
    const kind = this.chosen();
    const out = {};
    for (const spec of kind.fields) {
      const { input } = this.field(kind.value, spec);
      if (spec.type === "check") {
        if (input.checked) out[spec.key] = true;
        continue;
      }
      const value = input.value.trim();
      if (!value) continue;
      out[spec.key] = spec.type === "number" ? Number(value) : value;
    }
    return out;
  }

  /** add sends the form. */
  async add() {
    const source = this.source.input.value.trim();
    if (!source) {
      this.error = new Error("Name the connector.");
      this.said = "";
      this.say();
      this.source.input.focus();
      return;
    }
    const body = {
      source,
      kind: this.kind,
      enabled: this.enabled.input.checked,
      config: this.config(),
    };
    const ok = await this.write("form", () => api.addConnector(body), `${source} was added.`);
    // Cleared only on the way through, so that a refusal leaves every field
    // where it was and the one that is wrong can be corrected rather than typed
    // again.
    if (ok) this.clear();
  }

  /**
   * clear empties the form after a connector was added.
   *
   * The kind and the start box are left where they were, because adding a
   * second directory after a first one is the common thing to do and both of
   * those are on the screen where somebody can see what they are set to. Focus
   * goes back to the name, which is where the next one starts.
   */
  clear() {
    this.source.input.value = "";
    for (const { input } of this.made.values()) {
      if (input.type === "checkbox") input.checked = false;
      else if (input.tagName === "SELECT") input.selectedIndex = 0;
      else input.value = "";
    }
    this.source.input.focus();
  }

  sync(source) {
    return this.write(source, () => api.syncConnector(source), `${source} was asked to sync.`);
  }

  start(source) {
    return this.write(source, () => api.startConnector(source), `${source} was started.`);
  }

  stop(source) {
    return this.write(source, () => api.stopConnector(source), `${source} was stopped.`);
  }

  remove(source) {
    return this.write(source, () => api.dropConnector(source), `${source} was removed.`);
  }

  /**
   * write does one change and hands the result to the screen.
   *
   * Nothing is retried and nothing is sent twice. A retried start is a second
   * crawler against the same source, writing the same document ids from the
   * same tree, and the index is then whichever of the two finished last. That
   * is also why a second press while one is in flight is dropped here rather
   * than by disabling the button: a disabled button loses focus, and a keyboard
   * that pressed Sync would be thrown back to the top of the page for it.
   */
  async write(about, run, said) {
    if (this.busy) return false;
    this.busy = about;
    this.error = null;
    this.said = "";
    this.confirming = "";
    this.say();

    let result = null;
    try {
      result = await run();
      this.said = said;
    } catch (err) {
      this.error = err;
    }
    this.busy = "";
    this.say();
    this.onChanged(result);
    return !this.error;
  }

  /**
   * say prints the outcome of the last write, wherever it came from.
   *
   * One place for all of them, rather than a message beside each button. It is
   * the only spot on the screen that does not move when a connector is added or
   * removed, which is what makes it the right place to say that one was, and it
   * is what a screen reader announces without anybody having to go and look.
   */
  say() {
    if (this.error) {
      replace(
        this.output,
        svg(icon("alert"), 16),
        // The server's sentence, which for a connector configured on the
        // command line says where to change it instead, and for settings that
        // cannot be run names the field.
        this.error.message,
      );
      this.output.className = "connectors__output connectors__output--bad";
      return;
    }
    if (this.busy) {
      replace(this.output, this.busy === "form" ? "Adding it." : `Changing ${this.busy}.`);
      this.output.className = "connectors__output connectors__output--busy";
      return;
    }
    if (this.said) {
      replace(this.output, svg(icon("check"), 16), this.said);
      this.output.className = "connectors__output connectors__output--good";
      return;
    }
    replace(this.output);
    this.output.className = "connectors__output";
  }

  /**
   * controls are the buttons on one connector.
   *
   * They carry a key that survives the repaint, because this screen redraws
   * itself every five seconds and these are rebuilt each time. Without one, a
   * keyboard would be thrown back to the top of the page twice a minute, and
   * pressing Remove would lose the confirmation it just opened.
   */
  controls(c) {
    const source = c.source || "";
    if (!c.managed) {
      return h(
        "p",
        { class: "connector__fixed" },
        svg(icon("lock"), 16),
        "Configured on the command line, so it is changed where this server is started.",
      );
    }
    const confirming = this.confirming === source;
    return h(
      "div",
      { class: "connector__controls" },
      c.enabled
        ? [
            action("Sync now", `sync:${source}`, () => this.sync(source)),
            action("Stop", `stop:${source}`, () => this.stop(source)),
          ]
        : action("Start", `start:${source}`, () => this.start(source)),
      confirming
        ? action("Remove it", `remove:${source}`, () => this.remove(source), "button--danger")
        : action("Remove", `remove:${source}`, () => this.ask(source)),
      confirming ? action("Keep it", `keep:${source}`, () => this.ask("")) : null,
      confirming
        ? h(
            "p",
            { class: "connector__warn" },
            "This forgets how it was configured. The documents it indexed stay where they are.",
          )
        : null,
    );
  }

  /** ask opens or closes the confirmation on one connector's removal. */
  ask(source) {
    this.confirming = source;
    this.onChanged(null);
  }
}

/**
 * field is one labelled control, returned with the control so it can be read.
 *
 * Three shapes rather than three functions, because the label, the hint and the
 * identifier that ties them together are the same in all of them, and a hint
 * that is a label's sibling in one and a fieldset's child in another is two
 * things to get right in the accessibility tree instead of one.
 */
function field(spec) {
  const id = spec.id || `connector-${spec.key}`;
  const hint = `${id}-hint`;
  if (spec.type === "check") {
    const input = h("input", { class: "connectors__check", id, type: "checkbox", "aria-describedby": hint });
    return {
      input,
      el: h(
        "div",
        { class: "connectors__field connectors__field--check" },
        h("span", { class: "connectors__box" }, input, h("label", { class: "connectors__label", for: id }, spec.name)),
        h("span", { class: "connectors__hint", id: hint }, spec.hint),
      ),
    };
  }
  if (spec.type === "choice") {
    const input = h(
      "select",
      { class: "connectors__input", id, "aria-describedby": hint },
      spec.choices.map(([value, name]) => h("option", { value }, name)),
    );
    return { input, el: labelled(spec, id, hint, input) };
  }
  const input = h("input", {
    class: "connectors__input",
    id,
    type: spec.type === "number" ? "number" : "text",
    autocomplete: "off",
    spellcheck: "false",
    "aria-describedby": hint,
  });
  return { input, el: labelled(spec, id, hint, input) };
}

function labelled(spec, id, hint, input) {
  return h(
    "div",
    { class: "connectors__field" },
    h("label", { class: "connectors__label", for: id }, spec.name),
    input,
    h("span", { class: "connectors__hint", id: hint }, spec.hint),
  );
}

/** wrap puts a label and a hint around a control that was built elsewhere. */
function wrap(name, id, hint, control) {
  const described = `${id}-hint`;
  control.setAttribute("aria-describedby", described);
  return labelled({ name, hint }, id, described, control);
}

/**
 * action is one button on one connector.
 *
 * The key is what the screen puts focus back on after it repaints, and it names
 * the connector as well as the verb because two connectors have a Stop button
 * each and focus belongs on the one it was on.
 */
function action(name, key, onClick, extra = "") {
  return h(
    "button",
    {
      class: extra ? `button button--small ${extra}` : "button button--small",
      type: "button",
      dataset: { focusKey: key },
      onClick,
    },
    name,
  );
}

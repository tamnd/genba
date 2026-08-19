// Syntax highlighting.
//
// This is a lexer, not a parser. It finds comments, strings, numbers, keywords
// and the name in front of an open paren, and it leaves everything else as
// plain text. That is five colours, which is all the palette has room for and
// more than enough to read code by: the eye is looking for where a string ends
// and where a block starts, not for a semantic model of the file.
//
// A language it does not know renders as an unhighlighted block, which is a
// correct answer rather than a failure. Highlighting the wrong thing is worse
// than highlighting nothing.

import { h } from "./dom.js";

// Blocks under this many characters are highlighted as they are built. Anything
// longer waits until it is near the viewport, so a document holding a thousand
// line file costs nothing until somebody scrolls to it.
const EAGER = 4000;

/**
 * highlight returns an element holding the highlighted source.
 *
 * It is an element rather than a fragment because a long block is highlighted
 * lazily, and lazily means there has to be something left in the tree to watch.
 */
export function highlight(source, lang) {
  const spec = LANGS[alias(lang)];
  const root = h("span", { class: "code__body" });

  if (!spec) {
    root.textContent = source;
    return root;
  }
  if (source.length <= EAGER) {
    fill(root, source, spec);
    return root;
  }

  root.textContent = source;
  watch(root, () => fill(root, source, spec));
  return root;
}

/** languages returns the names highlighting is available for. */
export function languages() {
  return Object.keys(LANGS).sort();
}

function fill(root, source, spec) {
  const frag = document.createDocumentFragment();
  for (const token of scan(source, spec)) {
    frag.appendChild(token.cls ? h("span", { class: `tok tok--${token.cls}` }, token.text) : document.createTextNode(token.text));
  }
  root.textContent = "";
  root.appendChild(frag);
}

let observer = null;
const pending = new WeakMap();

function watch(el, run) {
  if (!("IntersectionObserver" in window)) {
    run();
    return;
  }
  if (!observer) {
    observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!entry.isIntersecting) continue;
          observer.unobserve(entry.target);
          const job = pending.get(entry.target);
          pending.delete(entry.target);
          if (job) job();
        }
      },
      { rootMargin: "300px" },
    );
  }
  pending.set(el, run);
  observer.observe(el);
}

// The lexer ----------------------------------------------------------------

function scan(src, spec) {
  const out = [];
  let i = 0;
  let run = 0;

  const flush = (to) => {
    if (to > run) out.push({ text: src.slice(run, to) });
  };
  const emit = (end, cls) => {
    flush(i);
    out.push({ text: src.slice(i, end), cls });
    i = end;
    run = end;
  };

  while (i < src.length) {
    let end = commentEnd(src, i, spec);
    if (end > i) {
      emit(end, "comment");
      continue;
    }

    end = stringEnd(src, i, spec);
    if (end > i) {
      emit(end, spec.keyStrings && follows(src, end) === ":" ? "keyword" : "string");
      continue;
    }

    if (spec.rule) {
      const hit = spec.rule(src, i);
      if (hit) {
        emit(hit.end, hit.cls);
        continue;
      }
    }

    if (isDigit(src[i]) && !isWord(src[i - 1])) {
      end = i + 1;
      while (end < src.length && /[\w.]/.test(src[end])) end++;
      emit(end, "number");
      continue;
    }

    if (isWordStart(src[i])) {
      end = i + 1;
      while (end < src.length && isWord(src[end])) end++;
      const cls = classify(src.slice(i, end), src, end, spec);
      if (cls) emit(end, cls);
      else i = end;
      continue;
    }

    i++;
  }

  flush(src.length);
  return out;
}

function classify(word, src, end, spec) {
  const key = spec.fold ? word.toLowerCase() : word;
  if (spec.literals && spec.literals.has(key)) return "number";
  if (spec.keywords.has(key)) return "keyword";
  if (spec.types && spec.types.has(key)) return "func";
  if (spec.keyWords && follows(src, end) === ":" && startsLine(src, end - word.length)) return "keyword";
  if (follows(src, end) === "(") return "func";
  return null;
}

function commentEnd(src, i, spec) {
  for (const marker of spec.line || []) {
    if (src.startsWith(marker, i)) {
      const nl = src.indexOf("\n", i);
      return nl < 0 ? src.length : nl;
    }
  }
  for (const [open, close] of spec.block || []) {
    if (src.startsWith(open, i)) {
      const end = src.indexOf(close, i + open.length);
      return end < 0 ? src.length : end + close.length;
    }
  }
  return i;
}

function stringEnd(src, i, spec) {
  for (const quote of spec.quotes || []) {
    if (!src.startsWith(quote, i)) continue;
    let j = i + quote.length;
    while (j < src.length) {
      if (src[j] === "\\" && quote !== "`" && spec.escapes !== false) {
        j += 2;
        continue;
      }
      if (src.startsWith(quote, j)) return j + quote.length;
      // A single quoted string that runs past the end of its line is almost
      // always an apostrophe in a comment or a word, so it stops there.
      if (src[j] === "\n" && quote.length === 1 && !spec.multiline) return j;
      j++;
    }
    return src.length;
  }
  return i;
}

function follows(src, i) {
  while (i < src.length && (src[i] === " " || src[i] === "\t")) i++;
  return src[i] || "";
}

function startsLine(src, i) {
  for (let j = i - 1; j >= 0; j--) {
    if (src[j] === "\n") return true;
    if (src[j] !== " " && src[j] !== "\t" && src[j] !== "-") return false;
  }
  return true;
}

const isDigit = (c) => c >= "0" && c <= "9";
const isWordStart = (c) => Boolean(c) && /[A-Za-z_$@]/.test(c);
const isWord = (c) => Boolean(c) && /[\w$]/.test(c);

const set = (words) => new Set(words.split(/\s+/));

// The languages ------------------------------------------------------------

const ALIASES = {
  js: "javascript",
  jsx: "javascript",
  mjs: "javascript",
  ts: "typescript",
  tsx: "typescript",
  py: "python",
  golang: "go",
  yml: "yaml",
  sh: "shell",
  bash: "shell",
  zsh: "shell",
  console: "shell",
  md: "markdown",
  htm: "html",
  postgres: "sql",
  sqlite: "sql",
};

function alias(lang) {
  const name = String(lang || "").toLowerCase();
  return ALIASES[name] || name;
}

const LANGS = {
  go: {
    line: ["//"],
    block: [["/*", "*/"]],
    quotes: ['"', "`", "'"],
    multiline: true,
    keywords: set(`break case chan const continue default defer else fallthrough for func go goto if
      import interface map package range return select struct switch type var`),
    types: set(`string bool byte rune error any int int8 int16 int32 int64 uint uint8 uint16 uint32
      uint64 uintptr float32 float64 complex64 complex128 append cap close copy delete len make new
      panic print println recover`),
    literals: set("true false nil iota"),
  },

  javascript: {
    line: ["//"],
    block: [["/*", "*/"]],
    quotes: ['"', "'", "`"],
    keywords: set(`async await break case catch class const continue debugger default delete do else
      export extends finally for from function get if import in instanceof let new of return set
      static super switch this throw try typeof var void while with yield`),
    literals: set("true false null undefined NaN Infinity"),
  },

  typescript: {
    line: ["//"],
    block: [["/*", "*/"]],
    quotes: ['"', "'", "`"],
    keywords: set(`abstract any as asserts async await break case catch class const continue declare
      default delete do else enum export extends finally for from function get if implements import
      in infer instanceof interface is keyof let namespace new of private protected public readonly
      return satisfies set static super switch this throw try type typeof var void while yield`),
    types: set("string number boolean object symbol bigint unknown never void Array Promise Record Partial"),
    literals: set("true false null undefined"),
  },

  python: {
    line: ["#"],
    quotes: ['"""', "'''", '"', "'"],
    keywords: set(`and as assert async await break class continue def del elif else except finally
      for from global if import in is lambda match nonlocal not or pass raise return try while with
      yield`),
    types: set(`int float str bool bytes list dict set tuple len range print open enumerate zip type
      isinstance super self`),
    literals: set("True False None"),
  },

  json: {
    quotes: ['"'],
    keyStrings: true,
    keywords: new Set(),
    literals: set("true false null"),
  },

  yaml: {
    line: ["#"],
    quotes: ['"', "'"],
    keyStrings: true,
    keyWords: true,
    keywords: new Set(),
    literals: set("true false null yes no on off ~"),
  },

  sql: {
    line: ["--"],
    block: [["/*", "*/"]],
    quotes: ["'", '"'],
    fold: true,
    keywords: set(`select from where group by order having limit offset join left right full inner
      outer cross on using union all except intersect insert into values update set delete create
      table view index unique drop alter add column primary foreign key references constraint
      cascade default as distinct with returning case when then else end asc desc between in like
      is not and or exists begin commit rollback pragma explain analyze`),
    types: set(`count sum avg min max coalesce cast text integer real blob boolean timestamp date
      varchar numeric json jsonb uuid serial now length substr lower upper`),
    literals: set("null true false"),
  },

  shell: {
    line: ["#"],
    quotes: ['"', "'"],
    multiline: true,
    keywords: set(`if then else elif fi for in do done while until case esac function return local
      export declare readonly source shift trap set unset exit break continue`),
    types: set(`echo cd ls cat grep sed awk find make go git curl docker kubectl mkdir rm cp mv chmod
      test printf read sleep sudo env`),
  },

  css: {
    block: [["/*", "*/"]],
    quotes: ['"', "'"],
    keywords: set(`important inherit initial unset auto none var calc clamp min max url rgb rgba hsl
      hsla`),
    rule(src, i) {
      if (src[i] === "#" && /[\da-f]{3}/i.test(src.slice(i + 1, i + 4))) {
        let end = i + 1;
        while (end < src.length && /[\da-fA-F]/.test(src[end])) end++;
        return { end, cls: "number" };
      }
      if (src[i] === "@" || src[i] === "." || src[i] === "#" || src[i] === ":") {
        const m = /^[.#:@][\w-]+/.exec(src.slice(i));
        if (m) return { end: i + m[0].length, cls: src[i] === "@" ? "keyword" : "func" };
      }
      if (isWordStart(src[i]) && startsLine(src, i)) {
        const m = /^[-\w]+(?=\s*:)/.exec(src.slice(i));
        if (m) return { end: i + m[0].length, cls: "keyword" };
      }
      return null;
    },
  },

  html: {
    block: [["<!--", "-->"]],
    quotes: ['"', "'"],
    multiline: true,
    keywords: new Set(),
    rule(src, i) {
      if (src[i] !== "<") return null;
      const m = /^<\/?[A-Za-z][\w:-]*/.exec(src.slice(i));
      return m ? { end: i + m[0].length, cls: "func" } : null;
    },
  },

  markdown: {
    quotes: ["`"],
    multiline: true,
    keywords: new Set(),
    rule(src, i) {
      if (!startsLine(src, i)) return null;
      const m = /^(#{1,6}\s|>\s|[-*+]\s|\d+[.)]\s)/.exec(src.slice(i));
      return m ? { end: i + m[0].length, cls: "keyword" } : null;
    },
  },
};

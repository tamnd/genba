// The keyboard walk.
//
// It drives a real Chrome through the sequence a person performs and asserts
// where each key and each click actually led. axe checks that the markup
// describes itself; this checks that the interface does what the markup
// promises, which is a different failure and the one that shipped: the primary
// click target on every result row was an anchor to a file:// URL, which a
// browser served over HTTP will not navigate to, so clicking a result title did
// nothing at all. No audit finds that, because the markup is perfectly correct.
//
// It speaks the DevTools protocol directly rather than through a driver. The
// whole interaction is four commands, and the alternative is a dependency with
// a browser download in it for a script that already has a browser.

import { execFileSync, spawn } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const BASE = process.argv[2] || "http://127.0.0.1:8123";
const DEADLINE = 20_000;
const PATIENCE = 5_000;

const CHROMES = [
  process.env.CHROME_PATH,
  process.env.CHROME_BIN,
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  "/Applications/Chromium.app/Contents/MacOS/Chromium",
  "/usr/bin/google-chrome",
  "/usr/bin/google-chrome-stable",
  "/opt/google/chrome/chrome",
  "/usr/local/share/chrome-linux64/chrome",
  "/usr/bin/chromium",
  "/usr/bin/chromium-browser",
  "/snap/bin/chromium",
];

const NAMES = ["google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"];

/** findChrome takes the first of the usual places, then asks the path. */
function findChrome() {
  const listed = CHROMES.find((path) => path && existsSync(path));
  if (listed) return listed;
  for (const name of NAMES) {
    try {
      const found = execFileSync("command", ["-v", name], { shell: true, encoding: "utf8" }).trim();
      if (found && existsSync(found)) return found;
    } catch {
      // Not on the path, which is the answer rather than an error.
    }
  }
  return "";
}

const chrome = findChrome();
if (!chrome) {
  console.log("keyboard-walk: no Chrome on this machine, so the walk cannot run");
  process.exit(0);
}
if (typeof WebSocket === "undefined") {
  console.log(`keyboard-walk: node ${process.version} has no WebSocket, so the walk cannot run`);
  process.exit(0);
}

const profile = mkdtempSync(join(tmpdir(), "genba-walk-"));
const browser = spawn(
  chrome,
  [
    "--headless=new",
    "--no-sandbox",
    "--disable-gpu",
    // A container gives /dev/shm 64 megabytes and Chrome wants more than that
    // for its renderer, which is how it dies on a runner and nowhere else.
    "--disable-dev-shm-usage",
    "--no-first-run",
    "--no-default-browser-check",
    "--disable-extensions",
    // Port zero, so two runs on one machine do not fight over 9222. Chrome
    // writes the port it chose into the profile directory.
    "--remote-debugging-port=0",
    `--user-data-dir=${profile}`,
    "about:blank",
  ],
  // Chrome says why it will not start on stderr, and a walk that reports only
  // that no port appeared sends whoever reads it to the wrong place.
  { stdio: ["ignore", "ignore", "pipe"] },
);

let complaint = "";
let stopped = null;
browser.stderr.on("data", (chunk) => {
  complaint += chunk;
});
browser.on("exit", (code, signal) => {
  stopped = signal || code;
});

let failures = 0;
try {
  await walk();
} catch (err) {
  console.error(`keyboard-walk: ${err.message}`);
  failures++;
} finally {
  browser.kill();
  // Chrome is still writing its profile out while it shuts down, so the first
  // attempt at the directory finds it not empty. A few retries clear it, and a
  // directory left behind under the temporary directory is not worth failing a
  // build over.
  try {
    rmSync(profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
  } catch {
    // Left for the operating system to reap.
  }
}
process.exit(failures ? 1 : 0);

async function walk() {
  const session = await attach(await endpoint());

  // A results page over the corpus the gate already started, and an id from it
  // for the document route.
  await visit(session, `${BASE}/?q=cache`, "document.querySelectorAll('.result').length > 0");

  await check(
    session,
    "the title of a result is a link into the product",
    `(() => {
      const a = document.querySelector('.result__title');
      return a && a.tagName === 'A' && a.getAttribute('href').startsWith('/d/');
    })()`,
  );

  await press(session, "j", "KeyJ", 74);
  await check(
    session,
    "j moves the cursor onto the first row",
    "document.querySelectorAll('.result[data-active=\"true\"]').length === 1",
  );

  await press(session, "Enter", "Enter", 13);
  await check(
    session,
    "Enter on a row opens the preview",
    "!document.querySelector('.drawer').hidden && location.search.includes('open=')",
  );
  await check(
    session,
    "the preview took focus",
    "document.querySelector('.drawer').contains(document.activeElement)",
  );

  await press(session, "Escape", "Escape", 27);
  await check(
    session,
    "Escape closes the preview",
    "document.querySelector('.drawer').hidden && !location.search.includes('open=')",
  );

  // A plain click on the title opens the preview rather than following the
  // link. This is the assertion the shipped defect would have failed, in the
  // other direction: the click did nothing at all.
  await evaluate(session, "document.querySelector('.result__title').click()");
  await check(
    session,
    "a plain click on the title opens the preview and stays on the results",
    "!document.querySelector('.drawer').hidden && location.pathname === '/'",
  );

  const id = await evaluate(session, "document.querySelector('.result__title').getAttribute('href')");
  await visit(session, BASE + id, "document.querySelector('.page__title').textContent.trim().length > 0");
  await check(
    session,
    "the document route renders the document with a way back",
    `(() => {
      const back = document.querySelector('.page__back-link');
      const title = document.querySelector('.page__title');
      return Boolean(back && back.getAttribute('href') && title.tagName === 'H1');
    })()`,
  );
  await check(
    session,
    "the document page is not the preview drawer",
    "document.querySelector('.drawer').hidden",
  );
  // A link written as a bare query string resolves against whatever path is
  // showing, which was harmless while every path was the search and is not any
  // more. This catches the next one written that way.
  await check(
    session,
    "no link on the document page is the same document with a parameter on the end",
    `[...document.querySelectorAll('a[href]')].every((a) => {
      const u = new URL(a.href, location.href);
      return !(u.pathname.startsWith('/d/') && u.search);
    })`,
  );
}

// The protocol ------------------------------------------------------------

/** endpoint waits for Chrome to say which port it picked. */
async function endpoint() {
  const file = join(profile, "DevToolsActivePort");
  const until = Date.now() + DEADLINE;
  while (Date.now() < until) {
    if (existsSync(file)) {
      const [port, path] = readFileSync(file, "utf8").split("\n");
      if (port && path) return `ws://127.0.0.1:${port.trim()}${path.trim()}`;
    }
    // A Chrome that has already stopped is never going to write the file, and
    // waiting the rest of the deadline to say so only delays the reason.
    if (stopped !== null) throw new Error(`Chrome exited (${stopped})${said()}`);
    await sleep(50);
  }
  throw new Error(`Chrome never reported a debugging port${said()}`);
}

/** said renders whatever Chrome put on stderr, for the error to carry. */
function said() {
  const lines = complaint.trim();
  if (!lines) return ", and said nothing about why";
  return `. It said:\n${lines}`;
}

/**
 * attach opens a tab and returns something that speaks to it.
 *
 * Flat mode, so a message for the tab carries its session id rather than being
 * wrapped in an envelope for the browser to forward.
 */
async function attach(url) {
  const socket = new WebSocket(url);
  const pending = new Map();
  let next = 0;

  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("error", () => reject(new Error("could not open a DevTools connection")), {
      once: true,
    });
  });

  socket.addEventListener("message", (event) => {
    const message = JSON.parse(event.data);
    const waiting = pending.get(message.id);
    if (!waiting) return;
    pending.delete(message.id);
    if (message.error) waiting.reject(new Error(message.error.message));
    else waiting.resolve(message.result);
  });

  const send = (method, params, sessionId) =>
    new Promise((resolve, reject) => {
      const id = ++next;
      pending.set(id, { resolve, reject });
      socket.send(JSON.stringify({ id, method, params: params || {}, sessionId }));
    });

  const { targetId } = await send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await send("Target.attachToTarget", { targetId, flatten: true });
  await send("Page.bringToFront", {}, sessionId);
  return (method, params) => send(method, params, sessionId);
}

/** visit navigates and waits for the interface to have painted the answer. */
async function visit(session, url, ready) {
  await session("Page.navigate", { url });
  await settle(session, ready);
}

/**
 * settle polls until an expression is true.
 *
 * Polling rather than waiting on a load event, because everything on these
 * screens arrives after a fetch and the load event fires long before it.
 */
async function settle(session, expression) {
  const until = Date.now() + DEADLINE;
  for (;;) {
    try {
      if (await evaluate(session, expression)) return;
    } catch {
      // A document mid navigation has no such element yet, which is what this
      // is waiting for rather than an error.
    }
    if (Date.now() > until) throw new Error(`timed out waiting for: ${expression}`);
    await sleep(100);
  }
}

async function evaluate(session, expression) {
  const { result, exceptionDetails } = await session("Runtime.evaluate", {
    expression,
    returnByValue: true,
    awaitPromise: true,
  });
  if (exceptionDetails) throw new Error(exceptionDetails.exception?.description || "evaluation failed");
  return result.value;
}

async function press(session, key, code, keyCode) {
  const text = key.length === 1 ? key : key === "Enter" ? "\r" : undefined;
  for (const type of ["keyDown", "keyUp"]) {
    await session("Input.dispatchKeyEvent", {
      type,
      key,
      code,
      text: type === "keyDown" ? text : undefined,
      windowsVirtualKeyCode: keyCode,
      nativeVirtualKeyCode: keyCode,
    });
  }
  // One frame for the handler to run and the paint to land.
  await sleep(150);
}

/**
 * check waits a little for an assertion to come true and then reports it.
 *
 * It waits because every one of these screens paints after a fetch, and a run
 * that fails on a busy runner for a reason that is not the reason it exists is
 * a test somebody eventually deletes. It gives up quickly, because the budget
 * for all of this is ten milliseconds a request and anything near the ceiling
 * is a failure of a different kind.
 */
async function check(session, what, expression) {
  const until = Date.now() + PATIENCE;
  let ok = false;
  for (;;) {
    try {
      ok = Boolean(await evaluate(session, expression));
    } catch {
      ok = false;
    }
    if (ok || Date.now() > until) break;
    await sleep(100);
  }
  console.log(`keyboard-walk: ${ok ? "ok  " : "FAIL"} ${what}`);
  if (!ok) failures++;
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// A Chrome, and the four commands worth sending it.
//
// The gate drives a real browser twice: once to press the keys a person presses
// and once to render documents and look at what came out. Both need the same
// hundred and fifty lines of finding Chrome, starting it, attaching to a tab and
// evaluating an expression in it, and the alternative to sharing them is a
// dependency with a browser download in it for scripts that already have a
// browser.
//
// It speaks the DevTools protocol directly. The whole interaction is navigate,
// evaluate, dispatch a key and set the viewport.

import { execFileSync, spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

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
export function findChrome() {
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

/**
 * available reports whether this machine can run any of this, and says why not.
 *
 * A machine with no Chrome is not a failing build. The parts of the gate that
 * never flake have already run by the time anything here is called, and a
 * developer without a browser installed should still be able to run the suite.
 */
export function available() {
  if (!findChrome()) return "no Chrome on this machine";
  if (typeof WebSocket === "undefined") return `node ${process.version} has no WebSocket`;
  return "";
}

/**
 * launch starts a headless Chrome and attaches to a tab in it.
 *
 * The returned object holds the session to send commands through and the stop
 * that shuts the browser down, and stop is safe to call more than once.
 */
export async function launch() {
  // A port of our own choosing, so two runs on one machine do not fight over
  // 9222. Asking Chrome for port zero and reading back the number it chose is
  // the other way to do that, and it means reading the DevToolsActivePort file,
  // which on the CI runner Chrome starts and then never writes. Picking the
  // port here costs one bind and leaves nothing to go looking for.
  const port = await freePort();
  const profile = mkdtempSync(join(tmpdir(), "genba-chrome-"));
  const browser = spawn(
    findChrome(),
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
      `--remote-debugging-port=${port}`,
      `--user-data-dir=${profile}`,
      "about:blank",
    ],
    // Chrome says why it will not start on stderr, and a run that reports only
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

  const said = () => {
    const lines = complaint.trim();
    return lines ? `. It said:\n${lines}` : ", and said nothing about why";
  };

  const stop = () => {
    browser.kill();
    // Chrome is still writing its profile out while it shuts down, so the first
    // attempt at the directory finds it not empty. A few retries clear it, and
    // a directory left under the temporary directory is not worth failing a
    // build over.
    try {
      rmSync(profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
    } catch {
      // Left for the operating system to reap.
    }
  };

  try {
    const session = await attach(await endpoint(port, () => stopped, said));
    return { session, stop };
  } catch (err) {
    stop();
    throw err;
  }
}

/** endpoint waits for Chrome to start answering, and returns where to talk. */
async function endpoint(port, exited, said) {
  const until = Date.now() + DEADLINE;
  while (Date.now() < until) {
    try {
      const answer = await fetch(`http://127.0.0.1:${port}/json/version`);
      if (answer.ok) {
        const { webSocketDebuggerUrl } = await answer.json();
        if (webSocketDebuggerUrl) return webSocketDebuggerUrl;
      }
    } catch {
      // Nothing listening yet, which is the thing being waited for rather than
      // an error.
    }
    // A Chrome that has already stopped is never going to answer, and waiting
    // out the rest of the deadline to say so only delays the reason.
    if (exited() !== null) throw new Error(`Chrome exited (${exited()})${said()}`);
    await sleep(50);
  }
  throw new Error(`Chrome never answered on port ${port}${said()}`);
}

/**
 * attach opens a tab and returns something that speaks to it.
 *
 * Flat mode, so a message for the tab carries its session id rather than being
 * wrapped in an envelope for the browser to forward.
 *
 * What comes back is a function to send a command with, carrying an on for the
 * events the tab sends unasked. Only one script needs those, and it is the one
 * that holds a request open to see what the interface does while it waits.
 */
async function attach(url) {
  const socket = new WebSocket(url);
  const pending = new Map();
  const listeners = new Map();
  let next = 0;

  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("error", () => reject(new Error("could not open a DevTools connection")), {
      once: true,
    });
  });

  socket.addEventListener("message", (event) => {
    const message = JSON.parse(event.data);
    // An event has a method and no id, and is for whoever asked for it.
    if (message.method) {
      for (const handler of listeners.get(message.method) || []) handler(message.params || {});
      return;
    }
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
  const session = (method, params) => send(method, params, sessionId);
  session.on = (method, handler) => {
    listeners.set(method, (listeners.get(method) || []).concat(handler));
  };
  return session;
}

/** narrow tells the page it is on a small screen, media queries and all. */
export async function narrow(session, width, height) {
  await session("Emulation.setDeviceMetricsOverride", {
    width,
    height,
    deviceScaleFactor: 1,
    mobile: false,
  });
}

/** visit navigates and waits for the interface to have painted the answer. */
export async function visit(session, url, ready) {
  await session("Page.navigate", { url });
  await settle(session, ready);
}

/**
 * settle polls until an expression is true.
 *
 * Polling rather than waiting on a load event, because everything on these
 * screens arrives after a fetch and the load event fires long before it.
 */
export async function settle(session, expression) {
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

export async function evaluate(session, expression) {
  const { result, exceptionDetails } = await session("Runtime.evaluate", {
    expression,
    returnByValue: true,
    awaitPromise: true,
  });
  if (exceptionDetails) throw new Error(exceptionDetails.exception?.description || "evaluation failed");
  return result.value;
}

/** SHIFT is the modifier bit the protocol wants, for a shifted key. */
export const SHIFT = 8;

export async function press(session, key, code, keyCode, modifiers = 0) {
  const text = key.length === 1 ? key : key === "Enter" ? "\r" : undefined;
  for (const type of ["keyDown", "keyUp"]) {
    await session("Input.dispatchKeyEvent", {
      type,
      key,
      code,
      modifiers,
      text: type === "keyDown" ? text : undefined,
      windowsVirtualKeyCode: keyCode,
      nativeVirtualKeyCode: keyCode,
    });
  }
  // One frame for the handler to run and the paint to land.
  await sleep(150);
}

/** type presses one key per character, which is how text reaches an input. */
export async function type(session, text) {
  for (const ch of text) {
    const upper = ch.toUpperCase();
    await press(session, ch, `Key${upper}`, upper.charCodeAt(0));
  }
}

/**
 * reporter counts assertions and prints them under a name.
 *
 * check waits a little for an assertion to come true before reporting it. It
 * waits because every one of these screens paints after a fetch, and a run that
 * fails on a busy runner for a reason that is not the reason it exists is a
 * test somebody eventually deletes. It gives up quickly, because the budget for
 * all of this is ten milliseconds a request and anything near the ceiling is a
 * failure of a different kind.
 */
export function reporter(name) {
  const state = { failures: 0 };
  return {
    state,
    async check(session, what, expression) {
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
      console.log(`${name}: ${ok ? "ok  " : "FAIL"} ${what}`);
      if (!ok) state.failures++;
    },
  };
}

/** freePort asks the operating system for one and hands it straight back. */
function freePort() {
  return new Promise((resolve, reject) => {
    const server = createServer();
    server.on("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address();
      server.close(() => resolve(port));
    });
  });
}

export function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

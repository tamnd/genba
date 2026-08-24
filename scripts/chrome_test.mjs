// The part of the browser driver that has to settle, driven without a browser.
//
// Everything else in scripts/chrome.mjs is a thin wrapper over a DevTools
// command and is tested by the gate using it. wire is different: what it owns is
// what happens when the answer does not come, and staging a wedged Chrome is
// awkward enough that the failure shipped untested and cost thirty five minutes
// of a session sitting at a prompt with no output.
//
// A socket here is an object with the three methods wire uses. That is the whole
// dependency, so the test can close it, break it, or simply never answer.

import assert from "node:assert/strict";
import test from "node:test";

import { wire } from "./chrome.mjs";

// stub is a socket that records what was sent and answers only when told to.
function stub() {
  const listeners = new Map();
  const sent = [];
  return {
    sent,
    addEventListener(name, handler) {
      listeners.set(name, (listeners.get(name) || []).concat(handler));
    },
    send(payload) {
      sent.push(JSON.parse(payload));
    },
    // emit is the socket delivering an event to whoever attached to it.
    emit(name, event) {
      for (const handler of listeners.get(name) || []) handler(event);
    },
    // reply is the browser answering a command that was sent.
    reply(message) {
      this.emit("message", { data: JSON.stringify(message) });
    },
  };
}

test("a command that is answered resolves with the result", async () => {
  const socket = stub();
  const { send } = wire(socket);

  const answer = send("Runtime.evaluate", { expression: "1 + 1" });
  assert.equal(socket.sent[0].method, "Runtime.evaluate");
  socket.reply({ id: socket.sent[0].id, result: { value: 2 } });

  assert.deepEqual(await answer, { value: 2 });
});

test("a command the browser refuses rejects with what it said", async () => {
  const socket = stub();
  const { send } = wire(socket);

  const answer = send("Page.navigate", { url: "about:blank" });
  socket.reply({ id: socket.sent[0].id, error: { message: "Cannot navigate to invalid URL" } });

  await assert.rejects(answer, /Cannot navigate to invalid URL/);
});

// The failure this file exists for. Before there was a deadline the promise
// below never settled, so every wait built on top of it waited for as long as
// the process lived.
test("a command that is never answered rejects, saying which one", async () => {
  const socket = stub();
  const { send } = wire(socket, 20);

  await assert.rejects(send("Runtime.evaluate", {}, "S1"), (err) => {
    assert.match(err.message, /Runtime\.evaluate/);
    assert.match(err.message, /session S1/);
    assert.match(err.message, /wedged/);
    return true;
  });
});

test("a late answer to a command that already gave up is ignored", async () => {
  const socket = stub();
  const { send } = wire(socket, 20);

  await assert.rejects(send("Runtime.evaluate", {}), /wedged/);
  // The browser waking up after the fact must not throw out of an event
  // handler, which is where an unhandled rejection would come from.
  socket.reply({ id: socket.sent[0].id, result: { value: 2 } });
});

test("a connection that closes takes every outstanding command with it", async () => {
  const socket = stub();
  const { send } = wire(socket);

  const first = send("Runtime.evaluate", {});
  const second = send("Page.navigate", {});
  socket.emit("close", {});

  await assert.rejects(first, /Runtime\.evaluate was outstanding when the DevTools connection closed/);
  await assert.rejects(second, /Page\.navigate was outstanding when the DevTools connection closed/);
});

test("a connection that errors takes every outstanding command with it", async () => {
  const socket = stub();
  const { send } = wire(socket);

  const answer = send("Runtime.evaluate", {});
  socket.emit("error", {});

  await assert.rejects(answer, /the DevTools connection failed/);
});

test("a command sent after the connection went is refused rather than queued", async () => {
  const socket = stub();
  const { send } = wire(socket);

  socket.emit("close", {});
  await assert.rejects(send("Runtime.evaluate", {}), /was not sent because the DevTools connection closed/);
  assert.equal(socket.sent.length, 0, "a command reached a socket nobody is reading");
});

test("an event goes to every handler that asked for it and to no command", async () => {
  const socket = stub();
  const { send, on } = wire(socket);

  const seen = [];
  on("Fetch.requestPaused", (params) => seen.push(params.requestId));
  on("Fetch.requestPaused", (params) => seen.push(`again ${params.requestId}`));

  const answer = send("Fetch.enable", {});
  socket.reply({ method: "Fetch.requestPaused", params: { requestId: "R1" } });
  socket.reply({ id: socket.sent[0].id, result: { ok: true } });

  assert.deepEqual(seen, ["R1", "again R1"]);
  assert.deepEqual(await answer, { ok: true });
});

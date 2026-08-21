// When to check whether the page is still right.
//
// A results tab left open on a second monitor should be correct rather than a
// snapshot of nine minutes ago. There are four ways to learn that it is not,
// and they are in descending order of how much they cost the server: the index
// says so, the connection came back, the tab came back, and a timer.
//
// The timer is the fallback rather than the mechanism. It runs only while the
// tab is visible, so a page left open for a week is not a client that made ten
// thousand requests. A hidden tab makes none at all: an index change that
// arrives while nobody is looking marks the cache stale, and the refresh
// happens when somebody looks.

import { api } from "./api.js";

// INTERVAL is the ceiling on how stale a visible page can get with nothing
// else happening. Sixty seconds because that is the point at which somebody
// glancing back at a page would want it to have noticed.
const INTERVAL = 60_000;

// The stream reconnects with a backoff, because the two reasons it drops are a
// proxy timing out, which is instant to recover from, and a server that is
// down, which is not helped by being asked again immediately.
const RETRY = 2_000;
const RETRY_MAX = 30_000;

export class Live {
  /**
   * onRefresh is called with why, one of "timer", "visible", "online" or
   * "index". onConnection is called with whether the browser thinks it is
   * online, and only when that changes.
   */
  constructor({ onRefresh, onConnection, onIndex, interval = INTERVAL }) {
    this.onRefresh = onRefresh;
    this.onConnection = onConnection;
    this.onIndex = onIndex || (() => {});
    this.interval = interval;
    this.timer = null;
    this.stream = null;
    this.retry = RETRY;
    this.missed = false;
    this.online = navigator.onLine !== false;
  }

  start() {
    document.addEventListener("visibilitychange", () => this.visibility());
    window.addEventListener("online", () => this.connection(true));
    window.addEventListener("offline", () => this.connection(false));
    // A page the browser puts in its back forward cache is frozen with its
    // connections still open, and a browser allows six of them to one origin.
    // Six pages back through the history, each holding a stream, is a live page
    // that cannot fetch anything at all: the request queues behind connections
    // belonging to documents nobody is looking at. That is how a grid of
    // thumbnails came to wait forty seconds for an endpoint answering in two
    // milliseconds. So the stream is dropped on the way out and opened again if
    // this page is the one that comes back.
    window.addEventListener("pagehide", () => this.stop());
    window.addEventListener("pageshow", (e) => {
      if (!e.persisted) return;
      this.retry = RETRY;
      this.listen();
      // However long this page spent frozen, it heard nothing during it.
      this.onRefresh("visible");
    });
    this.tick();
    this.listen();
  }

  /** visible reports whether anybody is looking at this tab. */
  get visible() {
    return document.visibilityState !== "hidden";
  }

  tick() {
    clearInterval(this.timer);
    this.timer = null;
    if (!this.visible) return;
    this.timer = setInterval(() => this.onRefresh("timer"), this.interval);
  }

  visibility() {
    this.tick();
    if (!this.visible) return;
    // Something changed while this tab was in the background, and this is the
    // first moment at which acting on it is worth a request.
    if (this.missed) {
      this.missed = false;
      this.onRefresh("visible");
    }
  }

  connection(online) {
    if (online === this.online) return;
    this.online = online;
    this.onConnection(online);
    if (!online) return;
    this.retry = RETRY;
    this.onRefresh("online");
    this.listen();
  }

  /**
   * stop drops the stream without arranging for another one.
   *
   * The stream is cleared before the abort so that the handler below sees a
   * page that no longer wants one, rather than a drop worth reconnecting from.
   */
  stop() {
    const controller = this.stream;
    if (!controller) return;
    this.stream = null;
    controller.abort();
  }

  /**
   * listen keeps one stream open and reconnects when it ends.
   *
   * A deployment whose driver cannot report writes answers 404 here, which is
   * not a failure: the timer is the fallback and the interface is exactly as
   * correct, only slower to notice. So a 404 stops trying rather than
   * reconnecting forever against an endpoint that will never exist.
   */
  listen() {
    if (this.stream) return;
    const controller = new AbortController();
    this.stream = controller;
    api
      .events((event) => {
        this.retry = RETRY;
        this.onIndex(event);
        if (this.visible) this.onRefresh("index");
        else this.missed = true;
      }, controller.signal)
      .catch((err) => {
        if (err && err.status === 404) this.retry = 0;
      })
      .finally(() => {
        if (this.stream !== controller) return;
        this.stream = null;
        if (!this.retry) return;
        const wait = this.retry;
        this.retry = Math.min(this.retry * 2, RETRY_MAX);
        setTimeout(() => this.listen(), wait);
      });
  }
}

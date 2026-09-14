"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const fs = require("node:fs");
const path = require("node:path");

// main.js requires "electron" at module scope, so it cannot be loaded
// directly under plain node:test -- these are structural/source
// assertions instead, guarding the specific regression this file exists
// to prevent: the discovery wheel handler showing (or otherwise exposing)
// terminal-browser's offscreen BrowserWindow, which renders as a native
// "No content under offscreen mode" window. See requestPage/dispatch's
// wheel handling in main.js and terminal-browser's own hidden-window
// focusContent() precedent this mirrors.
const source = fs.readFileSync(path.join(__dirname, "main.js"), "utf8");

test("discovery wheel handling never shows the offscreen BrowserWindow", () => {
  assert.equal(/\.show\(\)/.test(source), false, "main.js must never call .show() on the discovery BrowserWindow");
  assert.equal(/\.restore\(\)/.test(source), false, "main.js must never un-minimize the discovery BrowserWindow");
});

test("discovery wheel handling never steals OS-level focus", () => {
  assert.equal(
    /steal\s*:\s*true/.test(source),
    false,
    "main.js must never request OS focus-stealing (app.focus({ steal: true }))",
  );
});

test("discovery wheel handling still focuses the hidden window/content and enables CDP focus emulation", () => {
  assert.match(source, /ownerWindow\.focus\(\)/);
  assert.match(source, /contents\.focus\(\)/);
  assert.match(source, /Emulation\.setFocusEmulationEnabled/);
});

test("discovery wheel handling refuses to proceed if the window ever becomes visible", () => {
  assert.match(source, /ownerWindow\.isVisible\(\)/);
});

// Regression guard for "chatgpt unavailable: receive browser bridge
// scrollRegion: EOF" persisting for the rest of the session: a
// net.Socket's own "error" event is fatal (an uncaught exception, crashing
// this whole Electron main process) unless something listens for it, and
// one client-side disconnect (e.g. agentsctl abandoning a superseded/
// cancelled reload by forcing its own read deadline) must never take the
// bridge down for every other request. See main.js's connection handler.
test("the bridge socket server never lets one connection's error crash the process", () => {
  assert.match(source, /socket\.on\(\s*["']error["']/, "each client connection must have its own \"error\" listener");
  assert.match(source, /server\.on\(\s*["']error["']/, "the listening server itself must have an \"error\" listener");
});

test("the bridge never writes a response to an already-destroyed socket", () => {
  assert.match(source, /socket\.destroyed/, "send() must check socket.destroyed before writing");
});

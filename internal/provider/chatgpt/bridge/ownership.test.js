"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const { DiscoveryOwner, captureBelongsToGeneration, terminalBrowserSession } = require("./ownership.js");

test("discovery operations resolve only the registered terminal-browser WebContents", () => {
  const owner = new DiscoveryOwner("nonce");
  assert.equal(owner.register({ token: "nonce", contentsID: 41, sessionKey: "session-x" }), true);

  const contents = new Map([[41, { id: 41 }], [42, { id: 42 }]]);
  assert.equal(owner.resolve((id) => contents.get(id)).id, 41);
  assert.equal(owner.owns(41), true);
  assert.equal(owner.owns(42), false);
  assert.throws(
    () => owner.register({ token: "nonce", contentsID: 42, sessionKey: "session-y" }),
    /ownership changed/,
  );
});

test("unrelated preload registrations cannot claim discovery ownership", () => {
  const owner = new DiscoveryOwner("expected");
  assert.equal(owner.register({ token: "other", contentsID: 42, sessionKey: "session-y" }), false);
  assert.equal(owner.resolve(() => ({ id: 42 })), null);
});

test("late captures from a previous enumeration generation are rejected", () => {
  assert.equal(captureBelongsToGeneration(2, 1), false);
  assert.equal(captureBelongsToGeneration(2, 2), true);
});

test("terminal-browser renderer session identity is read from argv", () => {
  assert.equal(terminalBrowserSession(["electron", "--terminal-browser-session=daemon-7"]), "daemon-7");
  assert.equal(terminalBrowserSession(["electron"]), "");
});

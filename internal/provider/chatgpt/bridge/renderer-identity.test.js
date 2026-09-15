"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const { initializeRendererRole } = require("./renderer-identity.js");

function storage() {
  const values = new Map();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
  };
}

function initialize(url, sessionStorage, ownershipToken = "runtime-token") {
  let registrations = 0;
  let navigationInstalls = 0;
  const identity = initializeRendererRole({
    isMainFrame: true,
    hash: new URL(url).hash,
    storage: sessionStorage,
    ownershipToken,
    registerDiscovery: () => { registrations++; },
    installNavigation: () => { navigationInstalls++; },
  });
  return { identity, registrations, navigationInstalls };
}

test("initial discovery registers without installing keyboard navigation", () => {
  const result = initialize("https://chatgpt.com/#agentsctl-discovery=runtime-token", storage());
  assert.equal(result.identity.discovery, true);
  assert.equal(result.registrations, 1);
  assert.equal(result.navigationInstalls, 0);
});

test("discovery identity survives a full navigation in the same browsing context", () => {
  const sessionStorage = storage();
  initialize("https://chatgpt.com/#agentsctl-discovery=runtime-token", sessionStorage);

  const result = initialize("https://chatgpt.com/g/g-p-project/project", sessionStorage);
  assert.equal(result.identity.discovery, true);
  assert.equal(result.registrations, 0);
  assert.equal(result.navigationInstalls, 0);
});

test("a foreground conversation installs navigation without becoming discovery", () => {
  const result = initialize("https://chatgpt.com/c/conversation-id", storage());
  assert.equal(result.identity.discovery, false);
  assert.equal(result.registrations, 0);
  assert.equal(result.navigationInstalls, 1);
});

test("a marker from another runtime does not claim a foreground renderer", () => {
  const sessionStorage = storage();
  initialize("https://chatgpt.com/#agentsctl-discovery=old-token", sessionStorage, "old-token");

  const result = initialize("https://chatgpt.com/c/conversation-id", sessionStorage, "new-token");
  assert.equal(result.identity.discovery, false);
  assert.equal(result.navigationInstalls, 1);
});

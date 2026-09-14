"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const { confirmNumber, enterNumber, nextPrompt, NumberJumpState } = require("./navigation.js");

test("number selection waits while an exact number is also a longer prefix", () => {
  assert.deepEqual(enterNumber(["1", "10"], "", "1"), {
    kind: "pending",
    exact: true,
    matches: ["1", "10"],
  });
  assert.deepEqual(confirmNumber(["1", "10"], "1"), { kind: "select", number: "1" });
  assert.deepEqual(enterNumber(["1", "10"], "1", "0"), { kind: "select", number: "10" });
});

test("number selection commits an unambiguous number", () => {
  assert.deepEqual(enterNumber(["1", "2", "10"], "", "2"), { kind: "select", number: "2" });
});

test("number selection rejects invalid prefixes without carrying state", () => {
  for (const input of [["0"], ["9"], ["1", "9"]]) {
    const state = new NumberJumpState(["1", "10"]);
    let result;
    for (const key of input) result = state.input(key);
    assert.deepEqual(result, { kind: "invalid" });
    assert.equal(state.prefix, "");
  }
  const cancelled = new NumberJumpState(["1", "10"]);
  cancelled.input("1");
  assert.deepEqual(cancelled.input("Escape"), { kind: "cancel" });
  assert.equal(cancelled.prefix, "");
});

test("prompt navigation walks by stable ID and reaches the conversation bottom", () => {
  const prompts = ["prompt-1", "prompt-2", "prompt-3"];
  assert.deepEqual(nextPrompt(prompts, null, "previous"), { kind: "prompt", promptID: "prompt-3" });
  assert.deepEqual(nextPrompt(prompts, "prompt-3", "previous"), { kind: "prompt", promptID: "prompt-2" });
  assert.deepEqual(nextPrompt(prompts, "prompt-2", "previous"), { kind: "prompt", promptID: "prompt-1" });
  assert.deepEqual(nextPrompt(prompts, null, "next"), { kind: "prompt", promptID: "prompt-1" });
  assert.deepEqual(nextPrompt(prompts, "prompt-1", "next"), { kind: "prompt", promptID: "prompt-2" });
  assert.deepEqual(nextPrompt(prompts, "prompt-3", "next"), { kind: "bottom" });
});

test("prompt navigation recovers when a streaming rerender invalidates the current ID", () => {
  assert.deepEqual(nextPrompt(["new-1", "new-2"], "stale", "previous"), {
    kind: "prompt",
    promptID: "new-2",
  });
  assert.deepEqual(nextPrompt(["new-1", "new-2"], "stale", "next"), {
    kind: "prompt",
    promptID: "new-1",
  });
});

"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const {
  confirmNumber,
  enterNumber,
  initialPromptPosition,
  nextPrompt,
  NumberJumpState,
  promptPosition,
} = require("./navigation.js");

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
  assert.deepEqual(nextPrompt(prompts, initialPromptPosition(), "previous"), promptPosition("prompt-3"));
  assert.deepEqual(nextPrompt(prompts, promptPosition("prompt-3"), "previous"), promptPosition("prompt-2"));
  assert.deepEqual(nextPrompt(prompts, promptPosition("prompt-2"), "previous"), promptPosition("prompt-1"));

  let position = initialPromptPosition();
  position = nextPrompt(prompts, position, "next");
  assert.deepEqual(position, promptPosition("prompt-1"));
  position = nextPrompt(prompts, position, "next");
  assert.deepEqual(position, promptPosition("prompt-2"));
  position = nextPrompt(prompts, position, "next");
  assert.deepEqual(position, promptPosition("prompt-3"));
  position = nextPrompt(prompts, position, "next");
  assert.deepEqual(position, { kind: "bottom" });
  position = nextPrompt(prompts, position, "next");
  assert.deepEqual(position, { kind: "bottom" });
  position = nextPrompt(prompts, position, "previous");
  assert.deepEqual(position, promptPosition("prompt-3"));
});

test("prompt navigation recovers when a streaming rerender invalidates the current ID", () => {
  assert.deepEqual(nextPrompt(["new-1", "new-2"], promptPosition("stale"), "previous"), {
    kind: "prompt",
    promptID: "new-2",
  });
  assert.deepEqual(nextPrompt(["new-1", "new-2"], promptPosition("stale"), "next"), {
    kind: "prompt",
    promptID: "new-1",
  });
});

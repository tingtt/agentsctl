"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const { ChatGPTNavigation, isCloseShortcut, isEditableTarget, isPromptShortcut } = require("./renderer-navigation.js");

function targetInside(selector) {
  return { closest: (query) => query.includes(selector) ? {} : null };
}

test("period activation recognizes every ChatGPT text-entry context", () => {
  const selectors = [
    "input", "textarea", "[contenteditable]", "[role='textbox']",
    "#prompt-textarea", "[data-virtualkeyboard='true']",
  ];
  for (const selector of selectors) {
    assert.equal(isEditableTarget(targetInside(selector)), true, selector);
  }
  assert.equal(isEditableTarget(targetInside("button")), false);
});

test("period keydown remains unowned in text-entry contexts and during IME composition", () => {
  for (const target of [targetInside("input"), targetInside("textarea"), targetInside("[contenteditable]")]) {
    const navigation = new ChatGPTNavigation({}, {});
    let prevented = false;
    navigation.handleKeydown({
      key: ".", code: "Period", target,
      ctrlKey: false, shiftKey: false, altKey: false, metaKey: false, isComposing: false,
      preventDefault: () => { prevented = true; },
      stopImmediatePropagation: () => {},
    });
    assert.equal(prevented, false);
    assert.equal(navigation.jump, null);
  }

  const composing = new ChatGPTNavigation({}, {});
  composing.composing = true;
  let prevented = false;
  composing.handleKeydown({
    key: ".", code: "Period", target: targetInside("button"),
    ctrlKey: false, shiftKey: false, altKey: false, metaKey: false, isComposing: false,
    preventDefault: () => { prevented = true; },
    stopImmediatePropagation: () => {},
  });
  assert.equal(prevented, false);
  assert.equal(composing.jump, null);
});

test("prompt shortcuts match the shifted bracket events delivered by terminal-browser", () => {
  const base = { ctrlKey: true, shiftKey: true, altKey: false, metaKey: false };
  assert.equal(isPromptShortcut({ ...base, key: "{", code: "BracketLeft" }, "previous"), true);
  assert.equal(isPromptShortcut({ ...base, key: "}", code: "BracketRight" }, "next"), true);
  assert.equal(isPromptShortcut({ ...base, key: "[", code: "BracketLeft" }, "previous"), true);
  assert.equal(isPromptShortcut({ ...base, key: "]", code: "BracketRight" }, "next"), true);
});

test("close shortcut owns only unshifted Ctrl+]", () => {
  const base = { ctrlKey: true, altKey: false, metaKey: false, code: "BracketRight" };
  assert.equal(isCloseShortcut({ ...base, key: "]", shiftKey: false }), true);
  assert.equal(isCloseShortcut({ ...base, key: "}", shiftKey: true }), false);
  assert.equal(isPromptShortcut({ ...base, key: "}", shiftKey: true }, "next"), true);
});

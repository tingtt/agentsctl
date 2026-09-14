"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const { responseCursor, targetFrom } = require("./capture.js");

test("capture target preserves opaque cursor and ignores it for series identity", () => {
  const first = targetFrom("https://chatgpt.com/backend-api/gizmos/g-p-internal/conversations?cursor=opaque-a&limit=10");
  const next = targetFrom("https://chatgpt.com/backend-api/gizmos/g-p-internal/conversations?limit=10&cursor=opaque-b");
  assert.equal(first.cursorIn, "opaque-a");
  assert.equal(next.cursorIn, "opaque-b");
  assert.equal(first.seriesKey, next.seriesKey);
});

test("endpoint identity separates otherwise identical request series", () => {
  const first = targetFrom("https://chatgpt.com/backend-api/gizmos/g-p-first/conversations?cursor=0&limit=10");
  const second = targetFrom("https://chatgpt.com/backend-api/gizmos/g-p-second/conversations?cursor=0&limit=10");
  assert.notEqual(first.seriesKey, second.seriesKey);
});

test("non-project and cursor-less requests are ignored", () => {
  assert.equal(targetFrom("https://chatgpt.com/backend-api/conversations?cursor=0"), null);
  assert.equal(targetFrom("https://chatgpt.com/backend-api/gizmos/g-p-internal/conversations"), null);
});

test("terminal cursor accepts absent, null, and empty response shapes", () => {
  for (const payload of [{}, { cursor: null }, { cursor: "" }]) {
    assert.deepEqual(responseCursor(payload), { cursorObserved: true, hasNextCursor: false, nextCursor: "" });
  }
  assert.deepEqual(responseCursor({ cursor: "opaque-next" }), {
    cursorObserved: true,
    hasNextCursor: true,
    nextCursor: "opaque-next",
  });
  assert.throws(() => responseCursor({ cursor: 7 }), /unrecognized type/);
});

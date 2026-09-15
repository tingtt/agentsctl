"use strict";

function matchNumberPrefix(numbers, prefix) {
  const matches = numbers.filter((number) => number.startsWith(prefix));
  if (matches.length === 0) return { kind: "invalid" };
  const exact = matches.includes(prefix);
  if (exact && matches.length === 1) return { kind: "select", number: prefix };
  return { kind: "pending", exact, matches };
}

function enterNumber(numbers, prefix, digit) {
  if (!/^\d$/.test(digit) || (prefix === "" && digit === "0")) return { kind: "invalid" };
  return matchNumberPrefix(numbers, prefix + digit);
}

function confirmNumber(numbers, prefix) {
  return numbers.includes(prefix) ? { kind: "select", number: prefix } : { kind: "invalid" };
}

class NumberJumpState {
  constructor(numbers) {
    this.numbers = [...numbers];
    this.prefix = "";
  }

  input(key) {
    if (key === "Escape") {
      this.cancel();
      return { kind: "cancel" };
    }
    const result = key === "Enter"
      ? confirmNumber(this.numbers, this.prefix)
      : enterNumber(this.numbers, this.prefix, key);
    if (result.kind === "pending") {
      this.prefix += key;
      return { ...result, prefix: this.prefix };
    }
    this.cancel();
    return result;
  }

  cancel() {
    this.prefix = "";
  }
}

function initialPromptPosition() {
  return { kind: "initial" };
}

function promptPosition(promptID) {
  return { kind: "prompt", promptID };
}

function bottomPromptPosition() {
  return { kind: "bottom" };
}

function nextPrompt(promptIDs, position, direction) {
  if (direction !== "previous" && direction !== "next") throw new Error(`unknown prompt direction: ${direction}`);
  if (promptIDs.length === 0) return { kind: "none" };

  if (position.kind === "bottom") {
    return direction === "previous"
      ? promptPosition(promptIDs[promptIDs.length - 1])
      : bottomPromptPosition();
  }
  if (position.kind === "initial") {
    return promptPosition(direction === "previous" ? promptIDs[promptIDs.length - 1] : promptIDs[0]);
  }
  if (position.kind !== "prompt") throw new Error(`unknown prompt position: ${position.kind}`);

  const currentIndex = promptIDs.indexOf(position.promptID);
  if (currentIndex < 0) {
    return promptPosition(direction === "previous" ? promptIDs[promptIDs.length - 1] : promptIDs[0]);
  }
  if (direction === "previous") {
    return currentIndex === 0 ? { kind: "none" } : promptPosition(promptIDs[currentIndex - 1]);
  }
  return currentIndex === promptIDs.length - 1
    ? bottomPromptPosition()
    : promptPosition(promptIDs[currentIndex + 1]);
}

module.exports = {
  bottomPromptPosition,
  confirmNumber,
  enterNumber,
  initialPromptPosition,
  matchNumberPrefix,
  nextPrompt,
  NumberJumpState,
  promptPosition,
};

"use strict";

const { initialPromptPosition, nextPrompt, NumberJumpState } = require("./navigation.js");

const editableSelector = [
  "input",
  "textarea",
  "[contenteditable]:not([contenteditable='false'])",
  "[role='textbox']",
  "[data-virtualkeyboard='true']",
  "#prompt-textarea",
  "[name='prompt-textarea']",
].join(",");

const jumpTargetSelector = [
  "a[href]",
  "button",
  "input:not([type='hidden'])",
  "textarea",
  "select",
  "[contenteditable]:not([contenteditable='false'])",
  "[role='button']",
  "[role='link']",
  "[role='menuitem']",
  "[role='checkbox']",
  "[role='radio']",
  "[role='switch']",
  "[role='tab']",
  "[role='option']",
  "[tabindex]:not([tabindex='-1'])",
].join(",");

const promptSelector = '[data-message-author-role="user"][data-message-id]';

function isEditableTarget(target) {
  if (!target || typeof target.closest !== "function") return false;
  return target.closest(editableSelector) !== null;
}

function isPromptShortcut(event, direction) {
  const code = direction === "previous" ? "BracketLeft" : "BracketRight";
  const key = direction === "previous" ? "{" : "}";
  return event.ctrlKey && event.shiftKey && !event.altKey && !event.metaKey &&
    (event.code === code || event.key === key);
}

function isCloseShortcut(event) {
  return event.ctrlKey && !event.shiftKey && !event.altKey && !event.metaKey &&
    (event.code === "BracketRight" || event.key === "]");
}

function visibleRect(element, view) {
  if (!element.isConnected || element.hidden || element.disabled || element.getAttribute("aria-disabled") === "true") {
    return null;
  }
  if (element.closest("[hidden], [inert], [aria-hidden='true']")) return null;
  if (typeof element.checkVisibility === "function" && !element.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true })) {
    return null;
  }
  const style = view.getComputedStyle(element);
  if (style.display === "none" || style.visibility === "hidden" || style.opacity === "0" || style.pointerEvents === "none") {
    return null;
  }
  return Array.from(element.getClientRects()).find((rect) =>
    rect.width > 0 && rect.height > 0 && rect.bottom > 0 && rect.right > 0 &&
    rect.top < view.innerHeight && rect.left < view.innerWidth,
  ) || null;
}

function discoverJumpTargets(document, view) {
  const targets = [];
  for (const element of document.querySelectorAll(jumpTargetSelector)) {
    const rect = visibleRect(element, view);
    if (rect) targets.push({ number: String(targets.length + 1), element, rect });
  }
  return targets;
}

function createOverlay(document, targets) {
  const host = document.createElement("div");
  host.setAttribute("data-agentsctl-jump-overlay", "");
  host.setAttribute("aria-hidden", "true");
  host.style.cssText = [
    "all:initial",
    "position:fixed",
    "inset:0",
    "width:100vw",
    "height:100vh",
    "pointer-events:none",
    "z-index:2147483647",
    "contain:layout style",
  ].join(";");
  const root = host.attachShadow({ mode: "closed" });
  const style = document.createElement("style");
  style.textContent = `
    .hint {
      position: fixed;
      padding: 2px 4px;
      border: 1px solid #7f1d1d;
      border-radius: 3px;
      background: #dc2626;
      color: #fff;
      box-shadow: 0 1px 3px rgb(0 0 0 / 45%);
      font: 700 12px/1.2 ui-monospace, SFMono-Regular, Menlo, monospace;
      white-space: nowrap;
      pointer-events: none;
    }
    .hint[hidden] { display: none; }
  `;
  root.appendChild(style);
  for (const target of targets) {
    const label = document.createElement("span");
    label.className = "hint";
    label.textContent = target.number;
    label.style.left = `${Math.max(0, Math.round(target.rect.left))}px`;
    label.style.top = `${Math.max(0, Math.round(target.rect.top))}px`;
    root.appendChild(label);
    target.label = label;
  }
  document.documentElement.appendChild(host);
  return host;
}

function filterOverlay(targets, prefix) {
  for (const target of targets) target.label.hidden = prefix !== "" && !target.number.startsWith(prefix);
}

function shouldFocusOnly(element) {
  const tag = element.tagName.toLowerCase();
  if (tag === "textarea" || tag === "select" || element.isContentEditable || element.getAttribute("role") === "textbox") {
    return true;
  }
  if (tag !== "input") {
    return !element.matches([
      "a[href]", "button", "[role='button']", "[role='link']", "[role='menuitem']",
      "[role='checkbox']", "[role='radio']", "[role='switch']", "[role='tab']", "[role='option']",
    ].join(","));
  }
  return !["button", "checkbox", "color", "file", "image", "radio", "range", "reset", "submit"].includes(
    (element.getAttribute("type") || "text").toLowerCase(),
  );
}

function activateTarget(element) {
  if (!element.isConnected) return;
  element.focus({ preventScroll: true });
  if (!shouldFocusOnly(element)) element.click();
}

function scrollableAncestor(element, view) {
  for (let node = element.parentElement; node; node = node.parentElement) {
    const style = view.getComputedStyle(node);
    if (/(auto|scroll)/.test(style.overflowY) && node.scrollHeight > node.clientHeight) return node;
  }
  return null;
}

class ChatGPTNavigation {
  constructor(view, document) {
    this.view = view;
    this.document = document;
    this.jump = null;
    this.promptPosition = initialPromptPosition();
    this.composing = false;
    this.handleKeydown = this.handleKeydown.bind(this);
    this.cancelJump = this.cancelJump.bind(this);
  }

  install() {
    this.view.addEventListener("keydown", this.handleKeydown, true);
    this.view.addEventListener("compositionstart", () => { this.composing = true; }, true);
    this.view.addEventListener("compositionend", () => { this.composing = false; }, true);
    this.view.addEventListener("pagehide", this.cancelJump, true);
  }

  own(event) {
    event.preventDefault();
    event.stopImmediatePropagation();
  }

  handleKeydown(event) {
    if (event.isComposing || this.composing) return;
    if (this.jump && this.handleJumpKey(event)) return;

    if (isPromptShortcut(event, "previous")) {
      this.own(event);
      this.navigatePrompt("previous");
      return;
    }
    if (isPromptShortcut(event, "next")) {
      this.own(event);
      this.navigatePrompt("next");
      return;
    }
    if (event.key === "." && !event.ctrlKey && !event.altKey && !event.metaKey && !event.shiftKey &&
        !isEditableTarget(event.target)) {
      const targets = discoverJumpTargets(this.document, this.view);
      if (targets.length === 0) return;
      this.own(event);
      this.jump = {
        targets,
        numbers: new NumberJumpState(targets.map((target) => target.number)),
        overlay: createOverlay(this.document, targets),
      };
      this.view.addEventListener("scroll", this.cancelJump, true);
      this.view.addEventListener("resize", this.cancelJump, true);
    }
  }

  handleJumpKey(event) {
    if (event.ctrlKey || event.altKey || event.metaKey) return false;
    if (event.key === "Escape") {
      this.own(event);
      this.cancelJump();
      return true;
    }
    if (!/^\d$/.test(event.key) && event.key !== "Enter" && event.key !== "Escape") {
      if (event.key.length === 1) this.cancelJump();
      return false;
    }
    const result = this.jump.numbers.input(event.key);
    this.own(event);
    if (result.kind === "pending") {
      filterOverlay(this.jump.targets, result.prefix);
      return true;
    }
    if (result.kind === "select") {
      const target = this.jump.targets.find((candidate) => candidate.number === result.number)?.element;
      this.cancelJump();
      if (target) activateTarget(target);
      return true;
    }
    this.cancelJump();
    return true;
  }

  cancelJump() {
    if (!this.jump) return;
    this.jump.overlay.remove();
    this.jump = null;
    this.view.removeEventListener("scroll", this.cancelJump, true);
    this.view.removeEventListener("resize", this.cancelJump, true);
  }

  prompts() {
    return Array.from(this.document.querySelectorAll(promptSelector));
  }

  clearPromptFocus() {
    for (const prompt of this.document.querySelectorAll("[data-agentsctl-prompt-current]")) {
      if (prompt.hasAttribute("data-agentsctl-temporary-tabindex")) prompt.removeAttribute("tabindex");
      prompt.removeAttribute("data-agentsctl-temporary-tabindex");
      prompt.removeAttribute("data-agentsctl-prompt-current");
      if (this.document.activeElement === prompt) prompt.blur();
    }
  }

  navigatePrompt(direction) {
    const prompts = this.prompts();
    const byID = new Map(prompts.map((prompt) => [prompt.getAttribute("data-message-id"), prompt]));
    const decision = nextPrompt([...byID.keys()], this.promptPosition, direction);
    if (decision.kind === "none") return;
    this.clearPromptFocus();
    if (decision.kind === "bottom") {
      const container = prompts.length > 0 ? scrollableAncestor(prompts[prompts.length - 1], this.view) : null;
      if (container) container.scrollTo({ top: container.scrollHeight, behavior: "auto" });
      else this.view.scrollTo({ top: this.document.documentElement.scrollHeight, behavior: "auto" });
      this.promptPosition = decision;
      return;
    }

    const prompt = byID.get(decision.promptID);
    if (!prompt) {
      this.promptPosition = initialPromptPosition();
      return;
    }
    prompt.setAttribute("data-agentsctl-prompt-current", "");
    if (!prompt.hasAttribute("tabindex")) {
      prompt.setAttribute("tabindex", "-1");
      prompt.setAttribute("data-agentsctl-temporary-tabindex", "");
    }
    prompt.focus({ preventScroll: true });
    prompt.scrollIntoView({ block: "start", behavior: "auto" });
    this.promptPosition = decision;
  }
}

function installChatGPTNavigation(view, document) {
  const navigation = new ChatGPTNavigation(view, document);
  navigation.install();
  return navigation;
}

module.exports = {
  ChatGPTNavigation,
  activateTarget,
  discoverJumpTargets,
  installChatGPTNavigation,
  isCloseShortcut,
  isEditableTarget,
  isPromptShortcut,
};

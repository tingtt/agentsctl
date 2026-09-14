"use strict";

const { ipcRenderer } = require("electron");

const navigationLogic = (() => {
  const module = { exports: {} };
  ((module, exports) => {
    /*__AGENTSCTL_CHATGPT_NAVIGATION_MODULE__*/
  })(module, module.exports);
  return module.exports;
})();

const rendererNavigation = (() => {
  const module = { exports: {} };
  const requireNavigation = (path) => {
    if (path === "./navigation.js") return navigationLogic;
    throw new Error(`unsupported renderer navigation module: ${path}`);
  };
  ((module, exports, require) => {
    /*__AGENTSCTL_CHATGPT_RENDERER_NAVIGATION_MODULE__*/
  })(module, module.exports, requireNavigation);
  return module.exports;
})();

const { installChatGPTNavigation, isCloseShortcut } = rendererNavigation;

const ownershipToken = "__AGENTSCTL_CHATGPT_OWNER_TOKEN__";
const requestChannel = `agentsctl-chatgpt:request:${ownershipToken}`;
const responseChannel = `agentsctl-chatgpt:response:${ownershipToken}`;
const registerChannel = `agentsctl-chatgpt:register:${ownershipToken}`;

const projectIDPattern = /^g-p-[A-Za-z0-9_-]+$/;

function classifyLink(href) {
  if (typeof href !== "string") return { any: false, project: false };
  const project = /^\/g\/g-p-[A-Za-z0-9_-]+\/c\//.test(href);
  return { any: project || href.startsWith("/c/"), project };
}

function scrollableAncestor(element) {
  let node = element.parentElement;
  while (node && node !== document.body) {
    const style = getComputedStyle(node);
    if (/(auto|scroll)/.test(style.overflowY) && node.scrollHeight > node.clientHeight) return node;
    node = node.parentElement;
  }
  return null;
}

function scrollRegion() {
  const counts = new Map();
  for (const anchor of document.querySelectorAll('a[href^="/c/"], a[href^="/g/g-p-"]')) {
    const classification = classifyLink(anchor.getAttribute("href"));
    if (!classification.any) continue;
    const container = scrollableAncestor(anchor);
    if (!container) continue;
    const count = counts.get(container) || { any: 0, project: 0 };
    count.any++;
    if (classification.project) count.project++;
    counts.set(container, count);
  }
  let best = null;
  let bestCount = null;
  for (const [container, count] of counts) {
    if (!best || count.project > bestCount.project || (count.project === bestCount.project && count.any > bestCount.any)) {
      best = container;
      bestCount = count;
    }
  }
  if (!best) return { found: false, candidateCount: counts.size };
  const rect = best.getBoundingClientRect();
  return {
    found: true,
    candidateCount: counts.size,
    projectLinkCount: bestCount.project,
    conversationLinkCount: bestCount.any,
    scrollTop: Math.round(best.scrollTop),
    scrollHeight: Math.round(best.scrollHeight),
    clientHeight: Math.round(best.clientHeight),
    rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
  };
}

async function dispatch(request) {
  if (request.method === "navigateProject") {
    if (!projectIDPattern.test(request.projectID || "")) throw new Error("invalid Project ID");
    location.assign(`https://chatgpt.com/g/${request.projectID}/project`);
    return { accepted: true };
  }
  if (request.method === "scrollRegion") return scrollRegion();
  throw new Error(`unsupported browser method: ${request.method}`);
}

ipcRenderer.on(requestChannel, async (_event, message) => {
  try {
    const result = await dispatch(message.request);
    ipcRenderer.send(responseChannel, { ipcRequestID: message.ipcRequestID, ok: true, result });
  } catch (error) {
    ipcRenderer.send(responseChannel, {
      ipcRequestID: message.ipcRequestID,
      ok: false,
      error: error instanceof Error ? error.message : String(error),
    });
  }
});

const discoveryRenderer = process.isMainFrame && location.hash === `#agentsctl-discovery=${ownershipToken}`;

if (discoveryRenderer) {
  const prefix = "--terminal-browser-session=";
  const sessionArgument = process.argv.find((argument) => argument.startsWith(prefix));
  const sessionKey = sessionArgument ? sessionArgument.slice(prefix.length) : "";
  void ipcRenderer.invoke(registerChannel, { ownershipToken, sessionKey });
}

if (process.isMainFrame && !discoveryRenderer) installChatGPTNavigation(globalThis, document);

addEventListener("keydown", (event) => {
  if (isCloseShortcut(event)) {
    event.preventDefault();
    event.stopImmediatePropagation();
    globalThis.terminalBrowser.quit();
  }
}, true);

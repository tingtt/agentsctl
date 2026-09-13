"use strict";

const { ipcRenderer } = require("electron");

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

ipcRenderer.on("agentsctl-chatgpt:request", async (_event, message) => {
  try {
    const result = await dispatch(message.request);
    ipcRenderer.send("agentsctl-chatgpt:response", { ipcRequestID: message.ipcRequestID, ok: true, result });
  } catch (error) {
    ipcRenderer.send("agentsctl-chatgpt:response", {
      ipcRequestID: message.ipcRequestID,
      ok: false,
      error: error instanceof Error ? error.message : String(error),
    });
  }
});

addEventListener("keydown", (event) => {
  if (event.ctrlKey && event.key === "]") {
    event.preventDefault();
    event.stopImmediatePropagation();
    globalThis.terminalBrowser.quit();
  }
}, true);

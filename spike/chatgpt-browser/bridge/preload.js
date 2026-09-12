"use strict";

const { ipcRenderer } = require("electron");

const projectIDPattern = /^g-p-[A-Za-z0-9_-]+$/;
const conversationIDPattern = /^[0-9a-f]{8}-[0-9a-f-]{27,}$/i;

function redactIdentifiers(value) {
  return value
    .replace(/g-p-[A-Za-z0-9_-]+/g, "g-p-<redacted>")
    .replace(/[0-9a-f]{8}-[0-9a-f-]{27,}/gi, "<conversation-id>");
}

function allObjects(value, found = []) {
  if (!value || typeof value !== "object") return found;
  if (Array.isArray(value)) {
    for (const item of value) allObjects(item, found);
    return found;
  }
  found.push(value);
  for (const child of Object.values(value)) allObjects(child, found);
  return found;
}

function firstString(object, keys) {
  for (const key of keys) {
    if (typeof object[key] === "string" && object[key]) return object[key];
  }
  return null;
}

function projectsFrom(payload) {
  if (!payload || typeof payload !== "object") throw new Error("project response is not an object");
  const projects = new Map();
  for (const object of allObjects(payload)) {
    const id = firstString(object, ["id", "gizmo_id"]);
    if (!id || !projectIDPattern.test(id)) continue;
    const display = object.display && typeof object.display === "object" ? object.display : {};
    const name = firstString(object, ["name", "title", "display_name"]) ||
      firstString(display, ["name", "title", "display_name"]);
    if (!name) continue;
    // No URL field: Phase 7 showed a bare Project ID URL is not canonical (the real one carries a
    // human-readable name slug this harness never resolves), so no candidate is offered here.
    projects.set(id, { id, name });
  }
  if (projects.size === 0) throw new Error("project response has no recognized projects");
  return [...projects.values()];
}

function conversationsFrom(payload, projectID) {
  if (!payload || typeof payload !== "object") {
    throw new Error("conversation response is not an object");
  }
  const hasRecognizedCollection = Array.isArray(payload.items) || Array.isArray(payload.conversations);
  const candidates = new Map();
  for (const object of allObjects(payload)) {
    const id = firstString(object, ["id", "conversation_id"]);
    if (!id || !conversationIDPattern.test(id)) continue;
    const title = firstString(object, ["title", "name"]);
    const createdAt = object.create_time ?? object.created_at ?? null;
    const updatedAt = object.update_time ?? object.updated_at ?? null;
    if (!title && createdAt === null && updatedAt === null) continue;
    const association = firstString(object, ["gizmo_id", "project_id"]);
    const discriminator = {};
    for (const key of [
      "conversation_type", "workspace_type", "is_work", "async_status", "status", "kind", "type",
    ]) {
      if (["string", "boolean", "number"].includes(typeof object[key])) {
        discriminator[key] = object[key];
      }
    }
    candidates.set(id, {
      id,
      title: title || "",
      createdAt,
      updatedAt,
      archived: object.is_archived ?? object.archived ?? false,
      projectID: association || projectID,
      discriminator,
      availableKeys: Object.keys(object).sort(),
      openURLCandidates: [
        `https://chatgpt.com/c/${id}`,
        `https://chatgpt.com/g/${projectID}/c/${id}`,
      ],
    });
  }
  if (candidates.size === 0 && !hasRecognizedCollection) {
    throw new Error("conversation response schema is not recognized");
  }
  return [...candidates.values()];
}

function rawTopLevelItems(payload) {
  if (Array.isArray(payload)) return payload;
  if (payload && Array.isArray(payload.items)) return payload.items;
  if (payload && Array.isArray(payload.conversations)) return payload.conversations;
  return [];
}

function globalConversationsFrom(payload, projectID) {
  if (!payload || typeof payload !== "object") throw new Error("global conversation response is not an object");
  if (!projectIDPattern.test(projectID || "")) throw new Error("invalid Project ID");
  const candidates = new Map();
  for (const object of allObjects(payload)) {
    const id = firstString(object, ["id", "conversation_id"]);
    if (!id || !conversationIDPattern.test(id)) continue;
    const association = firstString(object, ["gizmo_id", "project_id"]);
    if (association !== projectID) continue;
    const title = firstString(object, ["title", "name"]);
    const createdAt = object.create_time ?? object.created_at ?? null;
    const updatedAt = object.update_time ?? object.updated_at ?? null;
    const discriminator = {};
    for (const key of [
      "conversation_type", "workspace_type", "is_work", "async_status", "status", "kind", "type", "is_visible",
    ]) {
      if (["string", "boolean", "number"].includes(typeof object[key])) {
        discriminator[key] = object[key];
      }
    }
    candidates.set(id, {
      id,
      title: title || "",
      createdAt,
      updatedAt,
      archived: object.is_archived ?? object.archived ?? false,
      projectID: association,
      discriminator,
      availableKeys: Object.keys(object).sort(),
      openURLCandidates: [
        `https://chatgpt.com/c/${id}`,
        `https://chatgpt.com/g/${projectID}/c/${id}`,
      ],
    });
  }
  return [...candidates.values()];
}

// pinsFrom sanitizes the observed, undocumented /backend-api/pins response for the Phase 5
// is_starred coverage experiment (README "ChatGPT Phase 5"). It recognizes only conversation-ID-
// shaped values (the same allowlisted id/conversation_id fields and pattern used everywhere else
// in this bridge, found via a deep object walk since the pins payload's exact shape is unconfirmed),
// never a pin/star item's title or other content — so a pinned item's title never crosses the
// bridge, even though its conversation ID (an already-established, non-title identity crossing
// pattern elsewhere in this file) does.
function pinsFrom(payload) {
  if (!payload || typeof payload !== "object") throw new Error("pins response is not an object");
  const ids = new Set();
  for (const object of allObjects(payload)) {
    const id = firstString(object, ["id", "conversation_id"]);
    if (id && conversationIDPattern.test(id)) ids.add(id);
  }
  return { rawItemCount: rawTopLevelItems(payload).length, ids: [...ids] };
}

function tasksFrom(payload, knownConversationIDs) {
  if (!payload || typeof payload !== "object") throw new Error("tasks response is not an object");
  const known = new Set(knownConversationIDs.filter((id) => conversationIDPattern.test(id)));
  let conversationIDMatches = 0;
  let overlapWithKnownConversations = 0;
  const statusValues = new Set();
  const statusFieldCandidates = new Set();
  for (const object of allObjects(payload)) {
    const id = firstString(object, ["conversation_id"]);
    if (id && conversationIDPattern.test(id)) {
      conversationIDMatches++;
      if (known.has(id)) overlapWithKnownConversations++;
    }
    for (const key of ["status", "task_status", "state", "kind", "type"]) {
      if (typeof object[key] === "string" && object[key]) {
        statusValues.add(`${key}=${object[key]}`);
        statusFieldCandidates.add(key);
      }
    }
  }
  return {
    conversationIDMatches,
    overlapWithKnownConversations,
    statusFieldCandidates: [...statusFieldCandidates].sort(),
    distinctStatusValueCount: statusValues.size,
  };
}

function compareConversationEvidence(payloads) {
  const markers = ["work", "agent", "async", "type", "kind", "status", "mode", "task", "template", "origin"];
  const excluded = ["content", "text", "title", "prompt", "author", "user", "email", "name"];
  const samples = payloads.map((payload) => {
    const fields = new Map();
    const visit = (value, path, depth) => {
      if (!value || typeof value !== "object" || depth > 12) return;
      const entries = Array.isArray(value) ? value.slice(0, 200).map((item) => ["[]", item]) : Object.entries(value);
      for (const [key, child] of entries) {
        const lower = key.toLowerCase();
        if (excluded.some((word) => lower.includes(word))) continue;
        const childPath = path ? `${path}.${key}` : key;
        if (child === null || ["string", "boolean", "number"].includes(typeof child)) {
          if (markers.some((word) => lower.includes(word))) fields.set(childPath, `${typeof child}:${String(child)}`);
        } else {
          visit(child, childPath, depth + 1);
        }
      }
    };
    visit(payload, "", 0);
    return fields;
  });
  const paths = new Set(samples.flatMap((sample) => [...sample.keys()]));
  return [...paths].sort().map((path) => {
    const present = samples.filter((sample) => sample.has(path));
    const distinct = [...new Set(present.map((sample) => sample.get(path)))];
    return {
      path,
      presentCount: present.length,
      distinctValueCount: distinct.length,
      // Values are structural enum/status-style labels on allowlisted marker fields (never content/title/author/etc,
      // excluded above), so exposing them crosses no credential or PII boundary. Project/conversation IDs that show
      // up as a *value* (e.g. conversation_template_id, working_turn_id) are still redacted before display; distinct
      // counts above are computed from the raw values, so redaction never hides genuine per-item uniqueness.
      values: [...new Set(distinct.map((value) => {
        const redacted = redactIdentifiers(value);
        return redacted.length > 64 ? `${redacted.slice(0, 61)}...<redacted>` : redacted;
      }))]
        .slice(0, 10)
        .sort(),
      types: [...new Set(present.map((sample) => sample.get(path).split(":", 1)[0]))].sort(),
    };
  });
}

function findScrollableAncestor(element) {
  let node = element.parentElement;
  while (node && node !== document.body) {
    const style = getComputedStyle(node);
    if (/(auto|scroll)/.test(style.overflowY) && node.scrollHeight > node.clientHeight) return node;
    node = node.parentElement;
  }
  return null;
}

async function simulateSidebarScroll() {
  const anchors = [...document.querySelectorAll('a[href^="/c/"]')];
  const before = anchors.length;
  const containers = new Set();
  for (const anchor of anchors) {
    const container = findScrollableAncestor(anchor);
    if (container) containers.add(container);
  }
  if (containers.size === 0) {
    return { triggered: false, reason: "no scrollable sidebar container found", conversationLinkCountBefore: before, conversationLinkCountAfter: before };
  }
  const candidateDiagnostics = [...containers].map((container, index) => ({
    index,
    clientHeight: container.clientHeight,
    scrollHeight: container.scrollHeight,
    scrollTopBefore: container.scrollTop,
    conversationLinksBefore: container.querySelectorAll('a[href^="/c/"]').length,
    projectLinksBefore: container.querySelectorAll('a[href^="/g/g-p-"]').length,
  }));
  for (let i = 0; i < 6; i++) {
    for (const container of containers) {
      container.scrollTop = container.scrollHeight;
      container.dispatchEvent(new Event("scroll", { bubbles: true }));
    }
    await new Promise((resolve) => setTimeout(resolve, 400));
  }
  for (const diagnostic of candidateDiagnostics) {
    const container = [...containers][diagnostic.index];
    diagnostic.scrollTopAfter = container.scrollTop;
    diagnostic.conversationLinksAfter = container.querySelectorAll('a[href^="/c/"]').length;
    diagnostic.projectLinksAfter = container.querySelectorAll('a[href^="/g/g-p-"]').length;
  }
  return {
    triggered: true,
    containerCount: containers.size,
    conversationLinkCountBefore: before,
    conversationLinkCountAfter: document.querySelectorAll('a[href^="/c/"]').length,
    candidates: candidateDiagnostics,
  };
}

async function fetchJSON(path) {
  if (location.hostname !== "chatgpt.com") throw new Error("refusing non-ChatGPT origin");
  const response = await fetch(path, {
    credentials: "include",
    headers: { accept: "application/json" },
  });
  if (!response.ok) throw new Error(`undocumented endpoint returned HTTP ${response.status}`);
  const contentType = response.headers.get("content-type") || "";
  if (!contentType.includes("application/json")) {
    throw new Error("undocumented endpoint returned non-JSON content");
  }
  return response.json();
}

async function dispatch(request) {
  if (request.method === "pageInfo") {
    const textEquals = (element, expected) => element.textContent?.trim().toLowerCase() === expected;
    const controls = [...document.querySelectorAll("a, button")];
    const backendPaths = performance.getEntriesByType("resource")
      .map((entry) => {
        try {
          const url = new URL(entry.name);
          if (url.hostname !== "chatgpt.com" || !url.pathname.startsWith("/backend-api/")) return null;
          return redactIdentifiers(url.pathname);
        } catch {
          return null;
        }
      })
      .filter(Boolean);
    return {
      href: location.href,
      title: document.title,
      readyState: document.readyState,
      loginPromptVisible: controls.some((element) =>
        textEquals(element, "log in") || textEquals(element, "sign up")
      ),
      accountControlPresent: Boolean(document.querySelector(
        '[data-testid="accounts-profile-button"], [data-testid="profile-button"], button[aria-label*="profile" i], button[aria-label*="account" i]'
      )),
      conversationLinkCount: document.querySelectorAll('a[href^="/c/"]').length,
      projectLinkCount: document.querySelectorAll('a[href^="/g/g-p-"]').length,
      observedBackendPaths: [...new Set(backendPaths)].sort().slice(0, 100),
    };
  }
  if (request.method === "sanitizeProjects") {
    return projectsFrom(request.payload);
  }
  if (request.method === "sanitizeTasks") {
    return tasksFrom(request.payload, Array.isArray(request.conversationIDs) ? request.conversationIDs : []);
  }
  if (request.method === "sanitizePins") {
    const { rawItemCount, ids } = pinsFrom(request.payload);
    return { rawItemCount, recognizedIDCount: ids.length, ids };
  }
  if (request.method === "sanitizeGlobalConversations") {
    return globalConversationsFrom(request.payload, request.projectID);
  }
  if (request.method === "sanitizeGlobalConversationsCaptureItems") {
    if (!projectIDPattern.test(request.projectID || "")) throw new Error("invalid Project ID");
    const items = globalConversationsFrom(request.payload, request.projectID);
    // Cross-checks a caller-supplied allowlist of already-known Project conversation IDs (from the
    // project-scoped endpoint) against this RAW page's own association field. A known Project
    // conversation ID appearing in this page whose association no longer resolves to the configured
    // Project is unambiguous schema-drift evidence (a renamed gizmo_id/project_id field, say) — not
    // guessed from field-presence heuristics that could misfire on ordinary non-Project chats.
    const knownIDs = new Set(
      Array.isArray(request.knownConversationIDs)
        ? request.knownConversationIDs.filter((id) => typeof id === "string" && conversationIDPattern.test(id))
        : []
    );
    let knownIDsSeen = 0;
    let knownIDsMismatched = 0;
    // Live evidence (Phase 5 second follow-up, multiple captures, 28 raw items each) showed every
    // raw conversation item — Project-associated or not — carries `gizmo_id` or `project_id` as an
    // own property; ordinary non-Project chats have it present but null, never absent. A raw item
    // exposing NEITHER key at all is therefore genuine schema drift (e.g. a rename), not a normal
    // non-Project conversation, and is counted here so mergeProjectPages can fail closed on it. This
    // is a universal per-item check; the known-ID cross-check above additionally catches the
    // narrower case of a *value* that resolves to the wrong Project, kept as defense in depth.
    let unrecognizedAssociationCount = 0;
    for (const raw of rawTopLevelItems(request.payload)) {
      const id = firstString(raw, ["id", "conversation_id"]);
      if (id && knownIDs.has(id)) {
        knownIDsSeen++;
        const association = firstString(raw, ["gizmo_id", "project_id"]);
        if (association !== request.projectID) knownIDsMismatched++;
      }
      if (!raw || typeof raw !== "object") continue;
      const hasGizmoID = Object.prototype.hasOwnProperty.call(raw, "gizmo_id");
      const hasProjectID = Object.prototype.hasOwnProperty.call(raw, "project_id");
      if (!hasGizmoID && !hasProjectID) unrecognizedAssociationCount++;
    }
    return { items, knownIDsSeen, knownIDsMismatched, unrecognizedAssociationCount };
  }
  if (request.method === "simulateSidebarScroll") {
    return simulateSidebarScroll();
  }
  if (request.method === "sanitizeGlobalConversationsPage") {
    if (!projectIDPattern.test(request.projectID || "")) throw new Error("invalid Project ID");
    const search = new URLSearchParams();
    for (const [key, value] of Object.entries(request.params || {})) {
      if (typeof value !== "string" && typeof value !== "number") continue;
      search.set(key, String(value));
    }
    const query = search.toString();
    const payload = await fetchJSON(`/backend-api/conversations${query ? `?${query}` : ""}`);
    const itemsArray = Array.isArray(payload) ? payload
      : Array.isArray(payload?.items) ? payload.items
      : Array.isArray(payload?.conversations) ? payload.conversations
      : null;
    if (!itemsArray) throw new Error("global conversation page response has no recognized item collection");
    const meta = {};
    if (payload && typeof payload === "object" && !Array.isArray(payload)) {
      for (const [key, value] of Object.entries(payload)) {
        if (value === null || ["string", "number", "boolean"].includes(typeof value)) meta[key] = value;
      }
    }
    return {
      items: globalConversationsFrom(payload, request.projectID),
      rawItemCount: itemsArray.length,
      meta,
    };
  }
  if (request.method === "navigateRoot") {
    location.assign("https://chatgpt.com/");
    return { accepted: true };
  }
  if (request.method === "navigateToPath") {
    const canonicalPath = /^\/c\/[0-9a-f-]{36}$/i;
    const projectScopedPath = /^\/g\/g-p-[A-Za-z0-9_-]+\/c\/[0-9a-f-]{36}$/i;
    if (typeof request.path !== "string" || !(canonicalPath.test(request.path) || projectScopedPath.test(request.path))) {
      throw new Error("invalid navigation path");
    }
    location.assign(`https://chatgpt.com${request.path}`);
    return { accepted: true };
  }
  if (request.method === "sanitizeConversations") {
    if (!projectIDPattern.test(request.projectID || "")) throw new Error("invalid Project ID");
    return conversationsFrom(request.payload, request.projectID);
  }
  if (request.method === "navigateProject") {
    if (!projectIDPattern.test(request.projectID || "")) throw new Error("invalid Project ID");
    location.assign(`https://chatgpt.com/g/${request.projectID}/project`);
    return { accepted: true };
  }
  if (request.method === "navigateConversation") {
    if (!conversationIDPattern.test(request.conversationID || "")) throw new Error("invalid conversation ID");
    location.assign(`https://chatgpt.com/c/${request.conversationID}`);
    return { accepted: true };
  }
  if (request.method === "compareConversationEvidence") {
    if (!Array.isArray(request.payloads) || request.payloads.length === 0) {
      throw new Error("conversation evidence payloads are missing");
    }
    return compareConversationEvidence(request.payloads);
  }
  if (request.method === "projects") {
    return projectsFrom(await fetchJSON("/backend-api/gizmos/snorlax/sidebar"));
  }
  if (request.method === "tasks") {
    return tasksFrom(await fetchJSON("/backend-api/tasks"), Array.isArray(request.conversationIDs) ? request.conversationIDs : []);
  }
  if (request.method === "conversations") {
    if (!projectIDPattern.test(request.projectID || "")) throw new Error("invalid Project ID");
    const path = `/backend-api/gizmos/${encodeURIComponent(request.projectID)}/conversations`;
    return conversationsFrom(await fetchJSON(path), request.projectID);
  }
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

if (process.env.AGENTSCTL_CHATGPT_CLOSE_KEY === "1") {
  addEventListener("keydown", (event) => {
    if (event.ctrlKey && event.key === "]") {
      event.preventDefault();
      event.stopImmediatePropagation();
      globalThis.terminalBrowser.quit();
    }
  }, true);
}

"use strict";

const fs = require("node:fs");
const net = require("node:net");
const crypto = require("node:crypto");
const { app, ipcMain, webContents } = require("electron");

const socketPath = process.env.AGENTSCTL_CHATGPT_BRIDGE_SOCKET;
const pending = new Map();
let nextIPCRequestID = 1;
let capturedProjects = null;
let capturedTasks = null;
let capturedGlobalConversations = null;
let capturedPins = null;
// Incremented on every fresh /backend-api/pins capture, so a caller (see the "pins" dispatch
// handler) can tell a genuinely new observation apart from the same stale one returned twice —
// needed for the Phase 5 is_starred experiment's before/after pins comparison (README Phase E).
let pinsCaptureID = 0;
const conversationIDPattern = /^[0-9a-f]{8}-[0-9a-f-]{27,}$/i;
// Capture identity (CaptureID) is assigned once, at capture time, and never reused or reinterpreted
// — it is a bridge-internal reference to one observed response, nothing else. Pagination identity
// (which series a page belongs to, and its offset within that series) is a separate, explicit
// concept computed from the response's own query/metadata; see seriesKeyFrom/recordGlobalConversationsPage.
let nextGlobalConversationCaptureID = 1;
const globalConversationsCaptures = new Map();
const globalConversationsCaptureOrder = [];
const evictedGlobalConversationCaptureIDs = new Set();
const MAX_GLOBAL_CONVERSATIONS_CAPTURES = 50;
const capturedConversations = new Map();
const capturedConversationDetails = new Map();
const attachedDebuggers = new WeakSet();
let attachedDebuggerCount = 0;
let matchedResponseCount = 0;
let captureErrorCount = 0;

if (!socketPath) {
  throw new Error("AGENTSCTL_CHATGPT_BRIDGE_SOCKET is required");
}

function chatGPTContents() {
  return webContents
    .getAllWebContents()
    .find((contents) => {
      try {
        return new URL(contents.getURL()).hostname === "chatgpt.com";
      } catch {
        return false;
      }
    });
}

function captureTarget(rawURL) {
  try {
    const url = new URL(rawURL);
    if (url.hostname !== "chatgpt.com") return null;
    if (url.pathname === "/backend-api/gizmos/snorlax/sidebar") return { kind: "projects" };
    if (url.pathname === "/backend-api/tasks" || url.pathname.startsWith("/backend-api/tasks/")) return { kind: "tasks" };
    if (url.pathname === "/backend-api/pins") return { kind: "pins" };
    const match = /^\/backend-api\/gizmos\/(g-p-[A-Za-z0-9_-]+)\/conversations$/.exec(url.pathname);
    if (match) return { kind: "conversations", projectID: match[1] };
    if (url.pathname === "/backend-api/conversations") return { kind: "globalConversations" };
    const conversationMatch = /^\/backend-api\/conversations\/([0-9a-f]{8}-[0-9a-f-]{27,})$/i.exec(url.pathname);
    return conversationMatch ? { kind: "conversation", conversationID: conversationMatch[1] } : null;
  } catch {
    return null;
  }
}

function attachCapture(contents) {
  if (attachedDebuggers.has(contents)) return;
  try {
    if (!contents.debugger.isAttached()) contents.debugger.attach("1.3");
  } catch {
    return;
  }
  attachedDebuggers.add(contents);
  attachedDebuggerCount++;
  const responses = new Map();
  contents.debugger.on("message", async (_event, method, params) => {
    if (method === "Network.responseReceived") {
      const target = captureTarget(params.response?.url || "");
      if (target && params.response.status === 200) {
        matchedResponseCount++;
        responses.set(params.requestId, { ...target, url: params.response.url });
      }
      return;
    }
    if (method !== "Network.loadingFinished") return;
    const target = responses.get(params.requestId);
    if (!target) return;
    responses.delete(params.requestId);
    try {
      const result = await contents.debugger.sendCommand("Network.getResponseBody", {
        requestId: params.requestId,
      });
      const body = result.base64Encoded
        ? Buffer.from(result.body, "base64").toString("utf8")
        : result.body;
      if (Buffer.byteLength(body) > 5 * 1024 * 1024) return;
      const payload = JSON.parse(body);
      if (target.kind === "projects") capturedProjects = payload;
      else if (target.kind === "tasks") capturedTasks = payload;
      else if (target.kind === "globalConversations") {
        capturedGlobalConversations = payload;
        recordGlobalConversationsPage(target.url, payload);
      } else if (target.kind === "pins") { capturedPins = payload; pinsCaptureID++; }
      else if (target.kind === "conversations") capturedConversations.set(target.projectID, payload);
      else capturedConversationDetails.set(target.conversationID, payload);
    } catch {
      captureErrorCount++;
      // A missing or malformed capture remains unavailable and fails closed at dispatch.
    }
  });
  contents.debugger.sendCommand("Network.enable").catch(() => {});
}

for (const contents of webContents.getAllWebContents()) attachCapture(contents);
app.on("web-contents-created", (_event, contents) => attachCapture(contents));

async function waitForCapture(read) {
  const deadline = Date.now() + 15_000;
  let value = read();
  while (!value && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    value = read();
  }
  if (!value) throw new Error("official ChatGPT response was not captured");
  return value;
}

async function requestPage(request) {
  const deadline = Date.now() + 15_000;
  let contents = chatGPTContents();
  while (!contents && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    contents = chatGPTContents();
  }
  if (!contents) {
    return Promise.reject(new Error("ChatGPT page is unavailable"));
  }

  const ipcRequestID = nextIPCRequestID++;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(ipcRequestID);
      reject(new Error("browser response timed out"));
    }, 10_000);
    pending.set(ipcRequestID, { resolve, reject, timer, senderID: contents.id });
    contents.send("agentsctl-chatgpt:request", { ipcRequestID, request });
  });
}

ipcMain.on("agentsctl-chatgpt:response", (event, message) => {
  const entry = pending.get(message?.ipcRequestID);
  if (!entry || event.sender.id !== entry.senderID) return;

  clearTimeout(entry.timer);
  pending.delete(message.ipcRequestID);
  if (message.ok) entry.resolve(message.result);
  else entry.reject(new Error(message.error || "browser request failed"));
});

function send(socket, response) {
  socket.write(`${JSON.stringify(response)}\n`);
}

async function dispatch(request) {
  if (!request || !Number.isSafeInteger(request.id) || typeof request.method !== "string") {
    throw new Error("request must contain an integer id and string method");
  }
  if (request.method === "ping") {
    return {
      bridge: "agentsctl-chatgpt-browser",
      protocol: 1,
      attachedDebuggerCount,
      matchedResponseCount,
      captureErrorCount,
      projectsCaptured: capturedProjects !== null,
      conversationCollectionsCaptured: capturedConversations.size,
    };
  }
  if (![
    "pageInfo", "projects", "tasks", "conversations", "globalConversations", "conversationEvidence", "openURLProbe",
    "globalConversationsPages", "globalConversationsPage", "globalConversationsCaptureItems", "simulateSidebarScroll",
    "pins",
  ].includes(request.method)) {
    throw new Error(`unsupported method: ${request.method}`);
  }
  if (request.method === "globalConversationsPages") {
    return [...globalConversationsCaptures.values()].map((capture) => ({
      captureID: capture.captureID,
      seriesKey: capture.seriesKey,
      query: capture.query, // redacted, for human-readable diagnostics only — never pagination identity
      hideSnorlax: capture.hideSnorlax,
      isArchived: capture.isArchived,
      isStarred: capture.isStarred,
      order: capture.order,
      hasUnknownParameters: capture.hasUnknownParameters,
      offset: capture.offset,
      limit: capture.limit,
      recognizedCollection: capture.recognizedCollection,
      rawItemCount: capture.rawItemCount,
      recognizedIDCount: capture.recognizedIDCount,
      rawIdentityDigest: capture.rawIdentityDigest,
      topLevelKeys: capture.topLevelKeys,
      meta: capture.meta,
    }));
  }
  if (request.method === "globalConversationsCaptureItems") {
    if (!/^g-p-[A-Za-z0-9_-]+$/.test(request.projectID || "")) {
      throw new Error("invalid Project ID");
    }
    const capture = globalConversationsCaptures.get(request.captureID);
    if (!capture) {
      if (evictedGlobalConversationCaptureIDs.has(request.captureID)) {
        throw new Error(`capture ${request.captureID} was evicted from bridge history (history bound exceeded) and can no longer be referenced`);
      }
      throw new Error(`no captured global conversations page with capture ID ${request.captureID}`);
    }
    return requestPage({
      method: "sanitizeGlobalConversationsCaptureItems",
      projectID: request.projectID,
      payload: capture.payload,
      knownConversationIDs: Array.isArray(request.knownConversationIDs) ? request.knownConversationIDs : [],
    });
  }
  if (request.method === "globalConversationsPage") {
    if (!/^g-p-[A-Za-z0-9_-]+$/.test(request.projectID || "")) {
      throw new Error("invalid Project ID");
    }
    return requestPage({
      method: "sanitizeGlobalConversationsPage",
      projectID: request.projectID,
      params: request.params && typeof request.params === "object" ? request.params : {},
    });
  }
  if (request.method === "projects") {
    const payload = await waitForCapture(() => capturedProjects);
    return requestPage({ method: "sanitizeProjects", payload });
  }
  if (request.method === "tasks") {
    const payload = await waitForCapture(() => capturedTasks);
    return requestPage({ method: "sanitizeTasks", payload, conversationIDs: request.conversationIDs || [] });
  }
  if (request.method === "pins") {
    // Passive-only, like every other capture in this bridge: no fetch is issued here, and if
    // /backend-api/pins was never observed yet this returns an honest empty snapshot (captureID 0)
    // rather than blocking — the Phase 5 experiment treats a captureID that never advances as
    // "not verified", not as evidence of anything about pin state itself.
    if (!capturedPins) {
      return { captureID: 0, rawItemCount: 0, recognizedIDCount: 0, ids: [] };
    }
    const sanitized = await requestPage({ method: "sanitizePins", payload: capturedPins });
    return { captureID: pinsCaptureID, ...sanitized };
  }
  if (request.method === "globalConversations") {
    if (!/^g-p-[A-Za-z0-9_-]+$/.test(request.projectID || "")) {
      throw new Error("invalid Project ID");
    }
    let payload = capturedGlobalConversations;
    if (!payload) {
      await requestPage({ method: "navigateRoot" });
      payload = await waitForCapture(() => capturedGlobalConversations);
    }
    return requestPage({ method: "sanitizeGlobalConversations", projectID: request.projectID, payload });
  }
  if (request.method === "conversations") {
    if (!/^g-p-[A-Za-z0-9_-]+$/.test(request.projectID || "")) {
      throw new Error("invalid Project ID");
    }
    let payload = capturedConversations.get(request.projectID);
    if (!payload) {
      await requestPage({ method: "navigateProject", projectID: request.projectID });
      payload = await waitForCapture(() => capturedConversations.get(request.projectID));
    }
    return requestPage({ method: "sanitizeConversations", projectID: request.projectID, payload });
  }
  if (request.method === "conversationEvidence") {
    const ids = request.conversationIDs;
    if (!Array.isArray(ids) || ids.length === 0 || ids.length > 20 ||
        ids.some((id) => !/^[0-9a-f]{8}-[0-9a-f-]{27,}$/i.test(id))) {
      throw new Error("conversationEvidence requires 1-20 valid conversation IDs");
    }
    const payloads = [];
    for (const conversationID of ids) {
      let payload = capturedConversationDetails.get(conversationID);
      if (!payload) {
        await requestPage({ method: "navigateConversation", conversationID });
        try {
          payload = await waitForCapture(() => capturedConversationDetails.get(conversationID));
        } catch {
          const info = await requestPage({ method: "pageInfo" });
          const paths = Array.isArray(info.observedBackendPaths) ? info.observedBackendPaths : [];
          throw new Error(`conversation detail was not captured; observed paths: ${paths.join(", ") || "none"}`);
        }
      }
      payloads.push(payload);
    }
    return requestPage({ method: "compareConversationEvidence", payloads });
  }
  if (request.method === "openURLProbe") {
    if (!/^g-p-[A-Za-z0-9_-]+$/.test(request.projectID || "")) {
      throw new Error("invalid Project ID");
    }
    // Selection stays inside this process: only style/readiness/login-redirect results cross the socket,
    // never which conversation ID was chosen.
    const detailIDs = [...capturedConversationDetails.keys()];
    const workLikeID = detailIDs.find((id) => payloadHasAsyncSource(capturedConversationDetails.get(id)));
    const chatLikeID = detailIDs.find((id) => id !== workLikeID && !payloadHasAsyncSource(capturedConversationDetails.get(id)));
    if (!workLikeID || !chatLikeID) {
      throw new Error("openURLProbe requires at least one async_source-bearing and one plain conversation already captured via conversationEvidence");
    }
    const waitForReady = async (timeoutMs = 8000) => {
      const deadline = Date.now() + timeoutMs;
      let info = await requestPage({ method: "pageInfo" });
      while (info.readyState !== "complete" && Date.now() < deadline) {
        await new Promise((resolve) => setTimeout(resolve, 300));
        info = await requestPage({ method: "pageInfo" });
      }
      return info;
    };
    const redact = (value) => value
      .replace(/g-p-[A-Za-z0-9_-]+/g, "g-p-<redacted>")
      .replace(/[0-9a-f]{8}-[0-9a-f-]{27,}/gi, "<conversation-id>");
    const probe = async (conversationID, style) => {
      const path = style === "canonical"
        ? `/c/${conversationID}`
        : `/g/${request.projectID}/c/${conversationID}`;
      await requestPage({ method: "navigateToPath", path });
      const info = await waitForReady();
      return {
        style,
        readyState: info.readyState,
        loginPromptVisible: info.loginPromptVisible,
        hrefMatchesExpectedPath: info.href === `https://chatgpt.com${path}`,
        redactedHref: redact(info.href),
        redactedExpectedPath: redact(`https://chatgpt.com${path}`),
      };
    };
    return {
      workLikeCanonical: await probe(workLikeID, "canonical"),
      workLikeProjectScoped: await probe(workLikeID, "project-scoped"),
      chatLikeCanonical: await probe(chatLikeID, "canonical"),
      chatLikeProjectScoped: await probe(chatLikeID, "project-scoped"),
    };
  }
  return requestPage(request);
}

// Reports pagination wire evidence without exposing opaque tokens: plain integers/booleans/short
// enum-like strings (e.g. "updated") are shown verbatim, anything else is length-only redacted.
function describeQueryValue(value) {
  if (/^-?\d+$/.test(value)) return value;
  if (/^(true|false)$/i.test(value)) return value;
  if (value.length <= 20 && /^[a-z0-9_.-]+$/i.test(value)) return value;
  return `<redacted:${value.length}ch>`;
}

function describeQuery(rawURL) {
  try {
    const url = new URL(rawURL);
    return [...url.searchParams.entries()]
      .map(([key, value]) => ({ key, value: describeQueryValue(value) }))
      .sort((a, b) => a.key.localeCompare(b.key));
  } catch {
    return [];
  }
}

function topLevelKeysOf(payload) {
  return payload && typeof payload === "object" && !Array.isArray(payload) ? Object.keys(payload).sort() : [];
}

function itemsArrayOf(payload) {
  if (Array.isArray(payload)) return payload;
  if (payload && Array.isArray(payload.items)) return payload.items;
  if (payload && Array.isArray(payload.conversations)) return payload.conversations;
  return null;
}

// Only non-array, non-object top-level fields (offset/limit/total-style pagination metadata) are
// surfaced; none of the observed shapes place an ID at this level, but scalars are reported as-is
// only because they are plain numbers/booleans here, not because IDs would be considered safe.
function scalarMetaOf(payload) {
  const meta = {};
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) return meta;
  for (const [key, value] of Object.entries(payload)) {
    if (value === null || ["string", "number", "boolean"].includes(typeof value)) meta[key] = value;
  }
  return meta;
}

// A raw item's stable ID, recognized only by the same key/shape rule used everywhere else in this
// bridge (id or conversation_id, matching conversationIDPattern). Returns null — never throws — so
// callers can count how many raw items were NOT recognized, which is the schema-drift signal.
function rawItemID(item) {
  if (!item || typeof item !== "object" || Array.isArray(item)) return null;
  const id = typeof item.id === "string" ? item.id : (typeof item.conversation_id === "string" ? item.conversation_id : null);
  return id && conversationIDPattern.test(id) ? id : null;
}

// Query parameters this bridge understands well enough to reason about which session universe a
// series represents (see README Phase A/B/C). `limit`/`offset` are pagination shape, not filters,
// but are included here because their presence is expected and harmless; `order` is tracked as
// informational metadata only (it can reorder results but, as far as this bridge has observed,
// never removes items from the set). Any OTHER parameter is unknown, and — since it could be
// filtering the result set in a way this bridge doesn't understand — a series carrying one must
// never be trusted as representing a known target universe. This is deliberately separate from
// SeriesKey (an opaque identity digest): SeriesKey already safely includes unknown parameters (so
// it never collides two different real series), but callers need this list to decide whether a
// series' *meaning* can be trusted at all.
const KNOWN_SAFE_QUERY_PARAMETERS = new Set(["limit", "offset", "order", "is_archived", "is_starred", "hide_snorlax"]);

// Recognizes one query parameter's value against an explicit allowlist of values this bridge has
// confirmed the meaning of, distinguishing THREE states a caller must never conflate:
//   - the key absent entirely                              -> "absent"
//   - the key present with a value NOT in `recognizedValues` -> "unknown"
//   - the key present with a recognized value               -> that value itself (e.g. "true")
// This replaces an earlier `value === "true" ? true : false` fallback that silently treated any
// unrecognized value (a typo, a future new mode, a malformed response) the same as the confirmed
// "false" case. Never coerce "absent" or "unknown" into a recognized value's meaning downstream.
function recognizeQueryValue(query, key, recognizedValues) {
  const entry = query.find((item) => item.key === key);
  if (!entry) return "absent";
  return recognizedValues.includes(entry.value) ? entry.value : "unknown";
}

// Safe, allowlisted semantic metadata about a query — never used as pagination identity (that's
// SeriesKey's job) and never containing an opaque/raw value. `hideSnorlax`/`isArchived`/`isStarred`/
// `order` are each one of a small set of values this bridge has confirmed the meaning of, or
// "absent"/"unknown" (see recognizeQueryValue) — never silently defaulted. `hasUnknownParameters`
// is the fail-closed signal for "this bridge doesn't fully understand what this query selects" at
// the query-shape level (an entirely unrecognized parameter name, as opposed to an unrecognized
// value on a known one).
function seriesDescriptorFrom(query) {
  return {
    hideSnorlax: recognizeQueryValue(query, "hide_snorlax", ["true", "false"]),
    isArchived: recognizeQueryValue(query, "is_archived", ["true", "false"]),
    isStarred: recognizeQueryValue(query, "is_starred", ["true", "false"]),
    order: recognizeQueryValue(query, "order", ["updated"]),
    hasUnknownParameters: query.some((entry) => !KNOWN_SAFE_QUERY_PARAMETERS.has(entry.key)),
  };
}

// The pagination series identity: a digest of the RAW (unredacted) query with `offset` removed,
// keys/values sorted for order-independence, JSON-encoded for unambiguous delimiting (so no
// combination of key/value strings can be crafted to collide with a different combination), then
// SHA-256'd so no raw query value — including opaque ones — ever needs to leave the bridge.
//
// This is deliberately NOT built from the redacted `query` diagnostic array: that redaction maps
// any two same-length opaque values to the same "<redacted:Nch>" placeholder, which would let two
// genuinely different query series collide into one SeriesKey and be wrongly merged into the same
// offset chain. Two responses belong to the same series only if every parameter other than offset
// (limit, order, is_archived, is_starred, hide_snorlax, ...) has the same raw value.
//
// Mirrored (for testing only — this is the real implementation) in main_test.go's
// testCanonicalSeriesKey; keep the two in sync if this changes.
function canonicalSeriesKeyFrom(rawURL) {
  try {
    const url = new URL(rawURL);
    const pairs = [...url.searchParams.entries()].filter(([key]) => key !== "offset");
    pairs.sort(([keyA, valueA], [keyB, valueB]) => {
      if (keyA !== keyB) return keyA < keyB ? -1 : 1;
      return valueA < valueB ? -1 : valueA > valueB ? 1 : 0;
    });
    return crypto.createHash("sha256").update(JSON.stringify(pairs)).digest("hex");
  } catch {
    return null;
  }
}

// Response-reported offset/limit (from scalarMetaOf), not the request's own query string — this is
// what the server actually says it applied. Missing or non-integer is reported as null, not 0,
// so a page whose pagination metadata cannot be trusted is never silently treated as offset 0.
function integerMetaField(meta, key) {
  const value = meta[key];
  return typeof value === "number" && Number.isInteger(value) && value >= 0 ? value : null;
}

function recordGlobalConversationsPage(url, payload) {
  const items = itemsArrayOf(payload);
  // A response whose top-level collection field can't be recognized (renamed from items/
  // conversations, or not an array at all) must never be treated as "0 items" — that would look
  // identical to a legitimate, recognized empty final page and could produce a false COMPLETE.
  const recognizedCollection = items !== null;
  const query = describeQuery(url);
  // hide_snorlax=true excludes Project/gizmo-associated conversations from `items` (empirically
  // confirmed: identical account state, item-level Project association present only when this is
  // false or absent) even though it does not appear to affect the unreliable top-level `total`.
  // Only pages where this is false/absent are valid input for Project enumeration. The recognized
  // three-state value (not a bare boolean) is computed below by seriesDescriptorFrom.
  const meta = scalarMetaOf(payload);
  const rawItemCount = recognizedCollection ? items.length : 0;
  const recognizedIDs = recognizedCollection ? items.map(rawItemID).filter(Boolean) : [];
  const captureID = nextGlobalConversationCaptureID++;
  globalConversationsCaptures.set(captureID, {
    captureID,
    // Pagination identity (SeriesKey) is a digest of the RAW, unredacted query — see
    // canonicalSeriesKeyFrom — so it can never collide the way the redacted `query` diagnostic
    // below could (two different opaque values of the same length both redact to the same
    // placeholder). `query` here is for human-readable diagnostics only and must never be used to
    // decide whether two captures belong to the same pagination series.
    seriesKey: canonicalSeriesKeyFrom(url),
    query,
    ...seriesDescriptorFrom(query),
    offset: integerMetaField(meta, "offset"),
    limit: integerMetaField(meta, "limit"),
    recognizedCollection,
    topLevelKeys: topLevelKeysOf(payload),
    rawItemCount,
    recognizedIDCount: recognizedIDs.length,
    // A fingerprint of the RAW page's own conversation identity (every item, not just this
    // Project's), so re-observing the same series+offset can be checked for consistency even when
    // the Project-filtered subset happens to look the same across two different raw pages. Only
    // meaningful when recognizedCollection is true — an unrecognized-shape page is rejected before
    // this digest would ever be consulted (see the required-field checks in mergeProjectPages).
    rawIdentityDigest: crypto.createHash("sha256").update([...recognizedIDs].sort().join(",")).digest("hex"),
    meta,
    payload,
  });
  globalConversationsCaptureOrder.push(captureID);
  if (globalConversationsCaptureOrder.length > MAX_GLOBAL_CONVERSATIONS_CAPTURES) {
    const evicted = globalConversationsCaptureOrder.shift();
    globalConversationsCaptures.delete(evicted);
    evictedGlobalConversationCaptureIDs.add(evicted);
  }
}

function payloadHasAsyncSource(payload) {
  try {
    return JSON.stringify(payload).includes('"async_source"');
  } catch {
    return false;
  }
}

try {
  const stat = fs.lstatSync(socketPath);
  if (!stat.isSocket()) throw new Error(`${socketPath} exists and is not a socket`);
  fs.unlinkSync(socketPath);
} catch (error) {
  if (error.code !== "ENOENT") throw error;
}

const server = net.createServer((socket) => {
  socket.setEncoding("utf8");
  let buffered = "";
  socket.on("data", (chunk) => {
    buffered += chunk;
    if (buffered.length > 64 * 1024) {
      socket.destroy(new Error("request exceeds 64 KiB"));
      return;
    }
    for (;;) {
      const newline = buffered.indexOf("\n");
      if (newline < 0) break;
      const line = buffered.slice(0, newline);
      buffered = buffered.slice(newline + 1);
      if (!line.trim()) continue;

      let request;
      try {
        request = JSON.parse(line);
      } catch {
        send(socket, { id: null, ok: false, error: "invalid JSON request" });
        continue;
      }
      dispatch(request).then(
        (result) => send(socket, { id: request.id, ok: true, result }),
        (error) => send(socket, { id: request.id, ok: false, error: error.message }),
      );
    }
  });
});

server.listen(socketPath, () => fs.chmodSync(socketPath, 0o600));
server.on("close", () => {
  try {
    fs.unlinkSync(socketPath);
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
});

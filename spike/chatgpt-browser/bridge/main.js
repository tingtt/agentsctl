"use strict";

const fs = require("node:fs");
const net = require("node:net");
const { app, ipcMain, webContents } = require("electron");

const socketPath = process.env.AGENTSCTL_CHATGPT_BRIDGE_SOCKET;
const pending = new Map();
let nextIPCRequestID = 1;
let capturedProjects = null;
let capturedTasks = null;
let capturedGlobalConversations = null;
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
        responses.set(params.requestId, target);
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
      else if (target.kind === "globalConversations") capturedGlobalConversations = payload;
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
  if (!["pageInfo", "projects", "tasks", "conversations", "globalConversations", "conversationEvidence", "openURLProbe"].includes(request.method)) {
    throw new Error(`unsupported method: ${request.method}`);
  }
  if (request.method === "projects") {
    const payload = await waitForCapture(() => capturedProjects);
    return requestPage({ method: "sanitizeProjects", payload });
  }
  if (request.method === "tasks") {
    const payload = await waitForCapture(() => capturedTasks);
    return requestPage({ method: "sanitizeTasks", payload, conversationIDs: request.conversationIDs || [] });
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

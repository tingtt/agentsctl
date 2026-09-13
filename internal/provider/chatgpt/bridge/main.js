"use strict";

const fs = require("node:fs");
const net = require("node:net");
const crypto = require("node:crypto");
const { app, ipcMain, webContents, BrowserWindow } = require("electron");

const socketPath = "__AGENTSCTL_CHATGPT_SOCKET_PATH__";
const pending = new Map();
const enumerations = new Map();
const attached = new WeakSet();
let nextIPCRequestID = 1;
let nextCaptureID = 1;
let nextGeneration = 1;
const maxCaptures = 128;
const conversationIDPattern = /^[0-9a-f]{8}-[0-9a-f-]{27,}$/i;

function validProjectID(value) {
  return typeof value === "string" && /^g-p-[A-Za-z0-9_-]+$/.test(value);
}

function chatGPTContents() {
  return webContents.getAllWebContents().find((contents) => {
    try {
      return new URL(contents.getURL()).hostname === "chatgpt.com";
    } catch {
      return false;
    }
  });
}

function targetFrom(rawURL) {
  try {
    const url = new URL(rawURL);
    if (url.hostname !== "chatgpt.com") return null;
    const match = /^\/backend-api\/gizmos\/(g-p-[A-Za-z0-9_-]+)\/conversations$/.exec(url.pathname);
    if (!match || !url.searchParams.has("cursor")) return null;
    return {
      projectID: match[1],
      cursorIn: url.searchParams.get("cursor"),
      seriesKey: cursorSeriesKey(rawURL),
    };
  } catch {
    return null;
  }
}

function cursorSeriesKey(rawURL) {
  const url = new URL(rawURL);
  const pairs = [...url.searchParams.entries()].filter(([key]) => key !== "cursor");
  pairs.sort(([keyA, valueA], [keyB, valueB]) => {
    if (keyA !== keyB) return keyA < keyB ? -1 : 1;
    return valueA < valueB ? -1 : valueA > valueB ? 1 : 0;
  });
  return crypto.createHash("sha256").update(JSON.stringify(pairs)).digest("hex");
}

function firstString(object, keys) {
  for (const key of keys) {
    if (typeof object[key] === "string" && object[key]) return object[key];
  }
  return "";
}

// sanitizePage runs in Electron's capture process before the captured body is
// retained or crosses IPC/socket boundaries. Only catalog metadata survives.
function sanitizePage(payload) {
  if (!payload || typeof payload !== "object" || Array.isArray(payload) || !Array.isArray(payload.items)) {
    throw new Error("Project conversation response schema is not recognized");
  }
  if (!Object.prototype.hasOwnProperty.call(payload, "cursor")) {
    throw new Error("Project conversation response has no explicit cursor state");
  }
  const items = payload.items.map((raw) => {
    if (!raw || typeof raw !== "object" || Array.isArray(raw)) throw new Error("conversation item is not an object");
    const id = firstString(raw, ["id", "conversation_id"]);
    const title = firstString(raw, ["title", "name"]);
    if (!conversationIDPattern.test(id)) throw new Error("conversation item has no recognizable ID");
    if (!title) throw new Error("conversation item has no title");
    if (typeof raw.create_time !== "string") throw new Error("conversation item has no create_time");
    if (typeof raw.update_time !== "string") throw new Error("conversation item has no update_time");
    return { id, title, createdAt: raw.create_time, updatedAt: raw.update_time };
  });
  if (payload.cursor === null || payload.cursor === "") {
    return { items, cursorObserved: true, hasNextCursor: false, nextCursor: "" };
  }
  if (typeof payload.cursor !== "string") {
    throw new Error("Project conversation cursor has an unrecognized type");
  }
  return { items, cursorObserved: true, hasNextCursor: true, nextCursor: payload.cursor };
}

function attachCapture(contents) {
  if (attached.has(contents)) return;
  try {
    if (!contents.debugger.isAttached()) contents.debugger.attach("1.3");
  } catch {
    return;
  }
  attached.add(contents);
  const responses = new Map();
  contents.debugger.on("message", async (_event, method, params) => {
    if (method === "Network.responseReceived") {
      const target = targetFrom(params.response?.url || "");
      const active = target ? enumerations.get(target.projectID) : null;
      if (target && active && params.response.status === 200) {
        responses.set(params.requestId, { ...target, generation: active.generation });
      }
      return;
    }
    if (method !== "Network.loadingFinished") return;
    const target = responses.get(params.requestId);
    if (!target) return;
    responses.delete(params.requestId);
    try {
      const result = await contents.debugger.sendCommand("Network.getResponseBody", { requestId: params.requestId });
      const body = result.base64Encoded ? Buffer.from(result.body, "base64").toString("utf8") : result.body;
      if (Buffer.byteLength(body) > 5 * 1024 * 1024) throw new Error("captured response exceeds 5 MiB");
      const active = enumerations.get(target.projectID);
      if (!active || active.generation !== target.generation) return;
      if (active.captures.length >= maxCaptures) throw new Error("capture history limit reached");
      const page = sanitizePage(JSON.parse(body));
      active.captures.push({
        captureID: nextCaptureID++,
        cursorIn: target.cursorIn,
        seriesKey: target.seriesKey,
        ...page,
      });
    } catch (error) {
      const active = enumerations.get(target.projectID);
      if (active && active.generation === target.generation) {
        active.error = error instanceof Error ? error.message : String(error);
      }
    }
  });
  contents.debugger.sendCommand("Network.enable").catch(() => {});
}

for (const contents of webContents.getAllWebContents()) attachCapture(contents);
app.on("web-contents-created", (_event, contents) => attachCapture(contents));

async function requestPage(request) {
  const deadline = Date.now() + 15_000;
  let contents = chatGPTContents();
  while (!contents && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    contents = chatGPTContents();
  }
  if (!contents) throw new Error("ChatGPT page is unavailable");
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

async function dispatch(request) {
  if (!request || !Number.isSafeInteger(request.id) || typeof request.method !== "string") {
    throw new Error("request must contain an integer id and string method");
  }
  if (request.method === "ping") return { protocol: 1 };
  if (!validProjectID(request.projectID)) throw new Error("invalid Project ID");
  if (request.method === "beginList") {
    enumerations.set(request.projectID, { generation: nextGeneration++, captures: [], error: "" });
    return requestPage({ method: "navigateProject", projectID: request.projectID });
  }
  if (request.method === "captures") {
    const active = enumerations.get(request.projectID);
    if (!active) throw new Error("Project enumeration has not started");
    if (active.error) throw new Error(active.error);
    return active.captures;
  }
  if (request.method === "scrollRegion") return requestPage({ method: "scrollRegion" });
  if (request.method !== "wheel") throw new Error(`unsupported method: ${request.method}`);

  const contents = chatGPTContents();
  if (!contents) throw new Error("ChatGPT page is unavailable");
  const initial = await requestPage({ method: "scrollRegion" });
  if (!initial.found) return { found: false, initial, final: initial, ticks: 0 };
  const ownerWindow = BrowserWindow.fromWebContents(contents);
  if (ownerWindow) {
    if (ownerWindow.isMinimized()) ownerWindow.restore();
    ownerWindow.show();
    ownerWindow.focus();
  }
  if (typeof app.focus === "function") app.focus({ steal: true });
  contents.focus();
  if (!contents.debugger.isAttached()) contents.debugger.attach("1.3");
  await contents.debugger.sendCommand("Emulation.setFocusEmulationEnabled", { enabled: true });
  // Let Chromium observe the focus-emulation transition before delivering
  // the first real wheel detent. The validated terminal-browser path needs
  // this focus state for wheel input to reach the offscreen-rendered page.
  await new Promise((resolve) => setTimeout(resolve, 150));
  const x = Math.round(initial.rect.x + initial.rect.width / 2);
  const y = Math.round(initial.rect.y + Math.min(initial.rect.height / 2, Math.max(initial.rect.height - 4, 0)));
  const detent = process.platform === "darwin" ? 40 : 120;
  const tickCount = Number.isInteger(request.ticks) ? Math.max(1, Math.min(request.ticks, 8)) : 3;
  contents.sendInputEvent({ type: "mouseMove", x, y });
  let final = initial;
  for (let i = 0; i < tickCount; i++) {
    contents.sendInputEvent({
      type: "mouseWheel", x, y, deltaX: 0, deltaY: -detent,
      wheelTicksX: 0, wheelTicksY: -1, hasPreciseScrollingDeltas: false,
      canScroll: true, modifiers: [],
    });
    await new Promise((resolve) => setTimeout(resolve, 250));
    final = await requestPage({ method: "scrollRegion" });
  }
  return { found: true, initial, final, ticks: tickCount };
}

function send(socket, response) {
  socket.write(`${JSON.stringify(response)}\n`);
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
    if (Buffer.byteLength(buffered) > 64 * 1024) {
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

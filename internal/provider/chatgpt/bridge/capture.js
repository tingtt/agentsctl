"use strict";

const crypto = require("node:crypto");

function targetFrom(rawURL) {
  try {
    const url = new URL(rawURL);
    if (url.hostname !== "chatgpt.com") return null;
    const match = /^\/backend-api\/gizmos\/(g-p-[A-Za-z0-9_-]+)\/conversations$/.exec(url.pathname);
    if (!match || !url.searchParams.has("cursor")) return null;
    const parameters = [...url.searchParams.entries()].filter(([key]) => key !== "cursor");
    parameters.sort(([keyA, valueA], [keyB, valueB]) => {
      if (keyA !== keyB) return keyA < keyB ? -1 : 1;
      return valueA < valueB ? -1 : valueA > valueB ? 1 : 0;
    });
    const seriesKey = crypto.createHash("sha256").update(JSON.stringify({
      endpointIdentity: match[1],
      parameters,
    })).digest("hex");
    return { cursorIn: url.searchParams.get("cursor"), seriesKey };
  } catch {
    return null;
  }
}

module.exports = { targetFrom };

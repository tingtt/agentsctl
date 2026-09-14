"use strict";

function terminalBrowserSession(argv) {
  const prefix = "--terminal-browser-session=";
  const argument = argv.find((value) => typeof value === "string" && value.startsWith(prefix));
  return argument ? argument.slice(prefix.length) : "";
}

class DiscoveryOwner {
  constructor(token) {
    if (typeof token !== "string" || !token) throw new Error("discovery ownership token is required");
    this.token = token;
    this.contentsID = null;
    this.sessionKey = "";
  }

  acceptsToken(token) {
    return token === this.token;
  }

  register({ token, contentsID, sessionKey }) {
    if (!this.acceptsToken(token)) return false;
    if (!Number.isSafeInteger(contentsID) || contentsID <= 0 || typeof sessionKey !== "string" || !sessionKey) {
      throw new Error("discovery renderer supplied invalid ownership metadata");
    }
    if (this.contentsID !== null && (this.contentsID !== contentsID || this.sessionKey !== sessionKey)) {
      throw new Error("discovery renderer ownership changed unexpectedly");
    }
    this.contentsID = contentsID;
    this.sessionKey = sessionKey;
    return true;
  }

  owns(contentsID) {
    return this.contentsID !== null && contentsID === this.contentsID;
  }

  resolve(findByID) {
    if (this.contentsID === null) return null;
    return findByID(this.contentsID) || null;
  }
}

function captureBelongsToGeneration(activeGeneration, capturedGeneration) {
  return Number.isSafeInteger(activeGeneration) && activeGeneration === capturedGeneration;
}

module.exports = { DiscoveryOwner, captureBelongsToGeneration, terminalBrowserSession };

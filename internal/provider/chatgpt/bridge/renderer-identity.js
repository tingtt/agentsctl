"use strict";

const discoveryMarkerPrefix = "agentsctl-chatgpt:discovery:";

function resolveRendererIdentity({ isMainFrame, hash, storage, ownershipToken }) {
  if (!isMainFrame) return { discovery: false, registerDiscovery: false, installNavigation: false };

  const initialDiscovery = hash === `#agentsctl-discovery=${ownershipToken}`;
  const markerKey = discoveryMarkerPrefix + ownershipToken;
  let retainedDiscovery = false;
  try {
    if (initialDiscovery) storage.setItem(markerKey, "owned");
    retainedDiscovery = storage.getItem(markerKey) === "owned";
  } catch {
    // The initial URL still belongs to discovery even if browser storage is unavailable.
  }

  const discovery = initialDiscovery || retainedDiscovery;
  return {
    discovery,
    registerDiscovery: initialDiscovery,
    installNavigation: !discovery,
  };
}

function initializeRendererRole(options) {
  const identity = resolveRendererIdentity(options);
  if (identity.registerDiscovery) options.registerDiscovery();
  if (identity.installNavigation) options.installNavigation();
  return identity;
}

module.exports = { initializeRendererRole, resolveRendererIdentity };

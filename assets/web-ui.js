"use strict";
/**
 * Browser-only contract: reference/html-ui.md and Airlock's utils/appWebUI.ts.
 * @typedef {{type: "airlock:web-ui:v1", action: "probe" | "back", nonce: string}} HostMessage
 * @typedef {{type: "airlock:web-ui:v1", action: "ready"} |
 *   {type: "airlock:web-ui:v1", action: "capabilities", nonce: string, features: string[]} |
 *   {type: "airlock:web-ui:v1", action: "state", nonce: string, url: string, canGoBack?: boolean}} AppMessage
 * @typedef {{topic: string, revision: string}} UIChange
 */
// The enclosing function receives only the authenticated sync-owned origin.
const disableNavigation = document.currentScript.dataset.airlockDisableNavigation === "true";
document.addEventListener("htmx:response:error", function(event) {
  if (event.detail?.ctx?.response?.status === 401) window.location.reload();
});
installUIChanges();
if (window.parent === window) return;
trustedParentOrigin = new URL(trustedParentOrigin).origin;
const type = "airlock:web-ui:v1";
let nonce = "";
const supportsNavigation = !disableNavigation && !!window.navigation &&
  typeof navigation.entries === "function" && typeof navigation.traverseTo === "function";
const features = ["location", "auth-recovery"];
if (supportsNavigation) features.push("navigation");

// Navigation entries expose actual same-origin history, including full-page,
// hash, htmx and pushState navigation. Never traverse into an auth/bootstrap page.
function previousEntry() {
  if (!supportsNavigation || !navigation.currentEntry) return null;
  const current = navigation.currentEntry;
  const previous = navigation.entries().find(entry => entry.index === current.index - 1);
  if (!previous || !previous.url) return null;
  const url = new URL(previous.url);
  return url.origin === location.origin && !url.pathname.startsWith("/__air/") ? previous : null;
}

function publish() {
  if (!nonce) return;
  /** @type {AppMessage} */
  const state = { type, action: "state", nonce, url: location.href };
  if (supportsNavigation) state.canGoBack = previousEntry() !== null;
  window.parent.postMessage(state, trustedParentOrigin);
}

window.addEventListener("message", function(event) {
  if (event.source !== window.parent || event.origin !== trustedParentOrigin) return;
  const data = event.data;
  if (!data || data.type !== type || typeof data.nonce !== "string" || !data.nonce || data.nonce.length > 128) return;
  if (data.action === "probe") {
    nonce = data.nonce;
    document.documentElement.setAttribute("data-airlock-embedded", "true");
    window.parent.postMessage({ type, action: "capabilities", nonce, features }, trustedParentOrigin);
    publish();
  } else if (data.action === "back" && data.nonce === nonce && supportsNavigation) {
    const previous = previousEntry();
    if (previous) navigation.traverseTo(previous.key).finished.catch(publish);
    else publish();
  }
});
if (window.navigation) navigation.addEventListener("currententrychange", publish);
else {
  // Observe successful same-document URL changes without maintaining a parallel history.
  for (const method of ["pushState", "replaceState"]) {
    const original = window.history[method];
    window.history[method] = function(...args) {
      const result = original.apply(this, args);
      publish();
      return result;
    };
  }
}
window.addEventListener("popstate", publish);
window.addEventListener("hashchange", publish);
window.addEventListener("pageshow", publish);
window.parent.postMessage({ type, action: "ready" }, trustedParentOrigin);

// One observer and one same-origin stream per document, independent of iframe
// integration and htmx. The topic list is declarative; browser input never selects
// a user. Timers coalesce bursts/reconnect failures, never poll fragment routes.
function installUIChanges() {
  const pattern = /^[a-z][a-z0-9_-]{0,63}$/;
  let controller, observer, reconnect, flush, signature = "", stopped = false;
  let roots = new Map(), pending = new Map(), retry = 1000;
  function topics(element) {
    const value = element.getAttribute("data-airlock-topic");
    if (!value || value.length > 2079) return [];
    const names = value.trim().split(/[\t\n\f\r ]+/);
    return names.length <= 32 && names.every(name => pattern.test(name)) && new Set(names).size === names.length ? names : [];
  }
  function enqueue(change) {
    pending.set(change.topic, change);
    if (flush) return;
    flush = setTimeout(() => {
      flush = undefined;
      const changes = pending;
      pending = new Map();
      for (const [element, names] of roots) {
        if (!element.isConnected) continue;
        for (const name of names) {
          const change = changes.get(name);
          // Non-bubbling events cannot trigger an unrelated ancestor fragment.
          if (change) element.dispatchEvent(new CustomEvent("airlock:change", { detail: change }));
        }
      }
    }, 50);
  }
  function disconnect() {
    controller?.abort();
    controller = undefined;
    clearTimeout(reconnect);
    reconnect = undefined;
  }
  async function connect(key) {
    if (stopped || key !== signature || !key) return;
    const active = new AbortController();
    controller = active;
    const revisions = new Map();
    const query = new URLSearchParams();
    key.split(" ").forEach(topic => query.append("topic", topic));
    try {
      const response = await fetch("/__air/changes?" + query, { credentials: "same-origin", cache: "no-store",
        headers: { Accept: "text/event-stream" }, signal: active.signal });
      if (response.status === 401) { window.location.reload(); return; }
      // Revoked/removed targets and undeclared topics fail closed, without a
      // reconnect storm. A subsequent navigation/subscription change may retry.
      if ([400, 403, 404, 409].includes(response.status)) {
        console.warn("Airlock UI change subscription unavailable (" + response.status + ")");
        return;
      }
      if (!response.ok || !response.headers.get("content-type")?.startsWith("text/event-stream") || !response.body) throw new Error("UI change stream unavailable");
      retry = 1000;
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let boundary;
          while ((boundary = buffer.indexOf("\n\n")) !== -1) {
            // Bound each frame in decoded UTF-16 code units, excluding its LF delimiter.
            if (boundary > 16384) throw new Error("UI change frame exceeds limit");
            const frame = buffer.slice(0, boundary);
            buffer = buffer.slice(boundary + 2);
            if (!frame.startsWith("event: airlock:change\ndata: ")) continue;
            const change = JSON.parse(frame.slice("event: airlock:change\ndata: ".length));
            if (!change || !key.split(" ").includes(change.topic) || typeof change.revision !== "string" ||
                !/^(?:[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})?$/.test(change.revision)) throw new Error("Invalid UI change frame");
            if (!revisions.has(change.topic) || revisions.get(change.topic) !== change.revision) {
              revisions.set(change.topic, change.revision);
              enqueue({ topic: change.topic, revision: change.revision });
            }
          }
          // A trailing LF may be the first half of the delimiter across reads.
          if (buffer.length - (buffer.endsWith("\n") ? 1 : 0) > 16384) throw new Error("UI change frame exceeds limit");
        }
      } finally { await reader.cancel().catch(() => {}); }
    } catch (error) {
      if (active.signal.aborted) return;
    }
    if (!active.signal.aborted && !stopped && key === signature) {
      reconnect = setTimeout(() => connect(key), retry);
      retry = Math.min(retry * 2, 30000);
    }
  }
  function reconcile() {
    if (stopped) return;
    const next = new Map(), names = new Set();
    for (const element of document.querySelectorAll("[data-airlock-topic]")) {
      const list = topics(element);
      if (!list.length) continue;
      next.set(element, list);
      list.forEach(name => names.add(name));
    }
    roots = next;
    const key = names.size <= 32 ? [...names].sort().join(" ") : "";
    if (key === signature) return;
    disconnect();
    pending.clear();
    signature = key;
    retry = 1000;
    connect(key);
  }
  function start() {
    stopped = false;
    if (!observer) {
      observer = new MutationObserver(reconcile);
      observer.observe(document.documentElement, { subtree: true, childList: true, attributes: true, attributeFilter: ["data-airlock-topic"] });
    }
    reconcile();
  }
  document.addEventListener("DOMContentLoaded", start, { once: true });
  if (document.readyState === "interactive" || document.readyState === "complete") start();
  window.addEventListener("pagehide", () => {
    stopped = true;
    disconnect();
    observer?.disconnect();
    observer = undefined;
    clearTimeout(flush);
    flush = undefined;
    pending.clear();
    roots.clear();
    signature = "";
  });
  window.addEventListener("pageshow", event => { if (event.persisted) start(); });
}

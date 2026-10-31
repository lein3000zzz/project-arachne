// Helpers every signed-in page uses: DOM building, API calls that send a
// signed-out user to the login page, the account header, segmented controls.
const $ = (id) => document.getElementById(id);

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === "class") node.className = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const child of children.flat()) {
    if (child != null && child !== false) node.append(child);
  }
  return node;
}

function toLogin() {
  location.assign("/login?next=" + encodeURIComponent(location.pathname + location.search + location.hash));
}

async function call(path, options) {
  const res = await fetch(path, options);
  if (res.status === 401) {
    toLogin();
    throw new Error("signed out");
  }
  return res;
}

// api returns the parsed body, or throws the server's error message.
async function api(path, options) {
  const res = await call(path, options);
  const body = res.status === 204 ? null : await res.json().catch(() => ({}));
  if (!res.ok) throw new Error((body && body.error) || res.statusText);
  return body;
}

const send = (method, path, data) => api(path, {
  method,
  headers: data === undefined ? {} : { "Content-Type": "application/json" },
  body: data === undefined ? undefined : JSON.stringify(data),
});

function ago(iso) {
  const s = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
  if (s < 60) return Math.round(s) + " s ago";
  if (s < 3600) return Math.round(s / 60) + " min ago";
  if (s < 86400) return Math.round(s / 3600) + " h ago";
  return Math.round(s / 86400) + " d ago";
}

const glyphs = { queued: "○", running: "●", indexing: "◐", finished: "✓", dropped: "✕", stalled: "!" };
const active = (status) => status === "queued" || status === "running" || status === "indexing";

function stateOf(status) {
  return el("span", { class: "state " + status },
    el("span", { class: "state-glyph", "aria-hidden": "true" }, glyphs[status] || "·"), status);
}

function segValue(name) {
  const on = document.querySelector(`.segmented[data-name="${name}"] .seg[aria-pressed="true"]`);
  return on ? on.dataset.value : "";
}

function setSeg(name, value) {
  const buttons = document.querySelectorAll(`.segmented[data-name="${name}"] .seg`);
  if (![...buttons].some((b) => b.dataset.value === value)) return;
  buttons.forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.value === value)));
}

document.querySelectorAll(".segmented").forEach((group) => {
  group.addEventListener("click", (e) => {
    const button = e.target.closest(".seg");
    if (button) setSeg(group.dataset.name, button.dataset.value);
  });
});

api("/api/me").then((me) => { $("who").textContent = me.username; }).catch(() => {});

$("logout").addEventListener("click", async () => {
  try { await fetch("/api/logout", { method: "POST" }); } finally { location.assign("/login"); }
});

// An uploaded file's address is on a reserved host no site can have; only its
// owner opens it, through /uploads/.
const uploadsPrefix = "https://uploads.arachne.invalid/";
const isUpload = (url) => typeof url === "string" && url.startsWith(uploadsPrefix);
const pageHref = (url) => (isUpload(url) ? "/uploads/" + url.slice(uploadsPrefix.length) : url);

const maxUploadBytes = 32 << 20;

function fileSize(bytes) {
  return bytes < 1 << 20 ? Math.max(1, Math.round(bytes / 1024)) + " KiB" : (bytes / (1 << 20)).toFixed(1) + " MiB";
}

// tooBig names the files the server would refuse, before sending megabytes.
function tooBig(files) {
  return [...files].filter((f) => f.size > maxUploadBytes).map((f) => f.name);
}

async function uploadFiles(labID, files) {
  const form = new FormData();
  for (const file of files) form.append("files", file, file.name);
  return api(`/api/labs/${labID}/uploads`, { method: "POST", body: form });
}

// A checked claim's mark and what it means: answers and artifacts alike.
const verdictMarks = { supported: "✓", partly: "~", unsupported: "✗", unchecked: "?" };
const verdictNames = {
  supported: "supported by the cited excerpts", partly: "partly supported: some of it is not in the cited excerpts",
  unsupported: "not supported by the cited excerpts", unchecked: "not checked",
};

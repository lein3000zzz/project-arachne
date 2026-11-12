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
  for (const child of children.flat(Infinity)) {
    if (child != null && child !== false) node.append(child);
  }
  return node;
}

// confirmInline asks in place of the button pressed - "question · yes · no" -
// and resolves true on yes. A browser dialog would stop the page and look
// like nothing else on it.
function confirmInline(button, question, yesLabel = "yes") {
  return new Promise((resolve) => {
    const done = (ok) => {
      box.remove();
      button.hidden = false;
      if (!ok) button.focus();
      resolve(ok);
    };
    const yes = el("button", { type: "button", class: "link danger", onclick: () => done(true) }, yesLabel);
    const box = el("span", { class: "confirm", role: "group", "aria-label": question,
      onkeydown: (e) => { if (e.key === "Escape") done(false); } },
      el("span", { class: "confirm-q" }, question), yes, el("button", { type: "button", class: "link", onclick: () => done(false) }, "no"));
    button.hidden = true;
    button.after(box);
    yes.focus();
  });
}

// showError puts a failure next to what failed, for a while.
function showError(next, message) {
  const e = el("span", { class: "error inline-error", role: "alert" }, message);
  next.after(e);
  setTimeout(() => e.remove(), 8000);
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

const plural = (n, word) => `${Number(n).toLocaleString()} ${word}${n === 1 ? "" : "s"}`;

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
// agreeMark adds to a claim's mark how many pages state what bears it out,
// where the ledger knows: "✓3".
function agreeMark(mark, claim) {
  if (!mark || !(claim.agree > 1)) return;
  mark.textContent = (verdictMarks[claim.verdict] || "?") + claim.agree;
  mark.title = `${verdictNames[claim.verdict] || claim.verdict} · stated by ${claim.agree} pages`;
}

// agreeNote is the same, said in the verdict box.
const agreeNote = (claim) => (claim.agree > 1 ? el("span", { class: "agree-note" }, `stated by ${claim.agree} pages of your labs`) : null);

const verdictMarks = { supported: "✓", partly: "~", unsupported: "✗", unchecked: "?" };
const verdictNames = {
  supported: "supported by the cited excerpts", partly: "partly supported: some of it is not in the cited excerpts",
  unsupported: "not supported by the cited excerpts", unchecked: "not checked",
};

// Ids are UUIDs, as the server writes them.
const isID = (s) => /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(s);

// How many rows a list shows at a time: the deployment sets it (ui.list_size).
// Lists drawn before it arrives are drawn again.
const ui = { listSize: 15 };
const listsDrawn = new Set();
fetch("/api/ui").then((res) => (res.ok ? res.json() : {})).then((settings) => {
  if (settings.list_size > 0) ui.listSize = settings.list_size;
  listsDrawn.forEach((draw) => draw());
}).catch(() => {});

// listView shows a long list a page at a time, with filter buttons and a
// search. Polling hands it new items; the page, the filter and what was typed
// stay. filters: [[value, label, keep(item)]]; text(item) is what the search
// looks in; wrap builds the list around the rows.
function listView({ filters, text, row, empty = "None yet.", wrap = (rows) => el("ol", { class: "rows" }, rows) }) {
  const state = { page: 0, filter: filters ? filters[0][0] : "", query: "", items: [] };
  const counts = new Map();

  const search = el("input", { type: "search", class: "list-search", placeholder: "search…", "aria-label": "Search the list" });
  search.addEventListener("input", () => { state.query = search.value.trim().toLowerCase(); state.page = 0; draw(); });

  const pick = (value) => {
    state.filter = value;
    state.page = 0;
    buttons.forEach((x, i) => x.setAttribute("aria-pressed", String(filters[i][0] === value)));
    draw();
  };

  const buttons = (filters || []).map(([value, label]) => {
    const b = el("button", { type: "button", class: "seg", "aria-pressed": String(value === state.filter) }, label);
    b.addEventListener("click", () => pick(value));
    counts.set(value, b);
    return b;
  });

  const body = el("div", { class: "list-body" });
  const box = el("div", { class: "listview" },
    el("div", { class: "list-tools" }, filters ? el("div", { class: "segmented", role: "group", "aria-label": "Show" }, buttons) : null, search),
    body);

  function draw() {
    const keep = (filters || []).find(([value]) => value === state.filter);
    for (const [value, label, pred] of filters || []) {
      counts.get(value).textContent = `${label} ${state.items.filter(pred).length}`;
    }
    const shown = state.items.filter((it) => (!keep || keep[2](it)) && (!state.query || text(it).toLowerCase().includes(state.query)));
    const size = ui.listSize;
    const pages = Math.max(1, Math.ceil(shown.length / size));
    state.page = Math.min(state.page, pages - 1);
    const slice = shown.slice(state.page * size, (state.page + 1) * size);

    const go = (to) => { state.page = to; draw(); box.scrollIntoView({ block: "nearest" }); };
    const pager = pages > 1 ? el("div", { class: "pager" },
      el("button", { type: "button", class: "action small", disabled: state.page === 0, onclick: () => go(state.page - 1) }, "‹ prev"),
      el("span", { class: "note" }, `${state.page * size + 1}–${state.page * size + slice.length} of ${shown.length}`),
      el("button", { type: "button", class: "action small", disabled: state.page === pages - 1, onclick: () => go(state.page + 1) }, "next ›")) : null;

    // replaceChildren would print a null as "null".
    body.replaceChildren(...[slice.length ? wrap(slice.map(row)) : el("p", { class: "note empty" }, state.items.length ? "Nothing matches." : empty), pager]
      .filter(Boolean));
  }

  listsDrawn.add(draw);

  return {
    el: box,
    set(items) { state.items = items; draw(); },
    redraw: draw,
    // Opens the list on one filter, the search cleared: a way in from elsewhere.
    show(value) {
      search.value = "";
      state.query = "";
      pick(value);
    },
  };
}

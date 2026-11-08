"use strict";

// Hardcap dashboard. Plain JavaScript, no build step: the gateway serves
// these files straight from its binary.

const CAP_AT = 80; // percent of the track where the cap wall sits (matches --cap-at)
const DEMO = { naive: "Naive demo", hardcap: "Hardcap demo", workers: 200, seconds: 15 };

const $ = (sel, root = document) => root.querySelector(sel);

// Browser storage can be blocked (private windows, strict settings), so
// every access is wrapped and the page works without it.
const storage = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { v == null ? localStorage.removeItem(k) : localStorage.setItem(k, v); } catch { /* fine */ } },
};

const state = {
  token: storage.get("hardcap.adminToken"),
  projects: [],
  models: ["mock-1"],
  demo: false,
  selected: Number(storage.get("hardcap.selected")) || null,
  lanes: new Map(), // project id -> lane elements
  usage: [],
  keys: [],
};

// h builds an element. Text is always set as text, never as HTML, so a
// project named "<script>" is just a funny name.
function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "text") el.textContent = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c != null && c !== false) el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

// Money arrives in micro-dollars ($1 = 1,000,000).
function usd(micros, digits) {
  const v = micros / 1e6;
  const d = digits ?? (Math.abs(v) < 100 ? 4 : 2);
  return "$" + v.toLocaleString("en-US", { minimumFractionDigits: d, maximumFractionDigits: d });
}
const capUSD = (micros) => "$" + (micros / 1e6).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 6 });
const count = (n) => Number(n || 0).toLocaleString("en-US");

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: { Authorization: "Bearer " + state.token, ...(body ? { "Content-Type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    disconnect("That admin token wasn't accepted.");
    throw new Error("unauthorized");
  }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* not JSON */ }
  if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
  return data;
}

let toastTimer;
function toast(msg, isError) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.toggle("error", !!isError);
  t.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove("show"), 3200);
}

// ---------- Connecting ----------

function showConnected(connected) {
  $("#connect").hidden = connected;
  $("#board").hidden = !connected;
  $("#top-actions").hidden = !connected;
  if (!connected) {
    $("#detail").hidden = true;
    $("#token").focus();
  }
}

function disconnect(message) {
  state.token = null;
  storage.set("hardcap.adminToken", null);
  for (const lane of state.lanes.values()) lane.el.remove();
  state.lanes.clear();
  showConnected(false);
  $("#connect-error").textContent = message || "";
}

$("#connect-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  state.token = $("#token").value.trim();
  $("#connect-error").textContent = "";
  try {
    await refresh();
    storage.set("hardcap.adminToken", state.token);
    $("#token").value = "";
    showConnected(true);
    poll();
  } catch (err) {
    if (err.message !== "unauthorized") $("#connect-error").textContent = "Can't reach the gateway: " + err.message;
  }
});

$("#disconnect").addEventListener("click", () => disconnect());

// ---------- Polling ----------

async function refresh() {
  const data = await api("GET", "/admin/api/overview");
  state.projects = data.projects || [];
  state.models = data.models && data.models.length ? data.models : ["mock-1"];
  state.demo = !!data.demo;
  renderBoard();
}

let pollTimer;
async function poll() {
  clearTimeout(pollTimer);
  if (!state.token) return;
  try {
    await refresh();
    setStatus("");
  } catch (err) {
    if (err.message === "unauthorized") return;
    setStatus("Can't reach the gateway. Retrying…", true);
  }
  pollTimer = setTimeout(poll, document.hidden ? 3000 : 500);
}

let detailTimer;
async function pollDetail() {
  clearTimeout(detailTimer);
  if (!state.token || !state.selected) return;
  try {
    const [usage, keys] = await Promise.all([
      api("GET", `/admin/api/projects/${state.selected}/usage?limit=25`),
      api("GET", `/admin/api/projects/${state.selected}/keys`),
    ]);
    state.usage = usage || [];
    state.keys = keys || [];
    renderDetail();
  } catch { /* the main poll reports connection problems */ }
  detailTimer = setTimeout(pollDetail, document.hidden ? 4000 : 1000);
}

document.addEventListener("visibilitychange", () => { if (!document.hidden) { poll(); pollDetail(); } });

function setStatus(msg, offline) {
  const s = $("#status");
  s.textContent = msg;
  s.classList.toggle("offline", !!offline);
}

// ---------- The board ----------

function renderBoard() {
  const list = $("#lanes");
  const seen = new Set();
  for (const p of state.projects) {
    seen.add(p.id);
    let lane = state.lanes.get(p.id);
    if (!lane) {
      lane = makeLane(p.id);
      state.lanes.set(p.id, lane);
      list.append(lane.el); // projects arrive oldest first, so new ones go last
    }
    updateLane(lane, p);
  }
  for (const [id, lane] of state.lanes) {
    if (!seen.has(id)) { lane.el.remove(); state.lanes.delete(id); }
  }
  if (state.selected && !seen.has(state.selected)) select(null);

  const empty = state.projects.length === 0;
  $("#empty").hidden = !empty;
  $("#lanes-head").hidden = empty;
  $("#empty-demo").hidden = !state.demo;
  $("#empty-demo-hint").hidden = !state.demo;
  $("#run-demo").hidden = !state.demo;
  renderDetailHead();
}

function makeLane(id) {
  const title = h("button", { class: "lane-title", type: "button", onclick: () => select(id) });
  const mode = h("span", { class: "mode" });
  const spent = h("div", { class: "fill spent" });
  const held = h("div", { class: "fill held" });
  const over = h("div", { class: "fill over" });
  const track = h("div", { class: "track" }, spent, held, over, h("div", { class: "cap-wall" }));
  const meter = h("div", { class: "meter", role: "meter", "aria-valuemin": "0" }, track);
  const stats = h("div", { class: "lane-stats" });
  meter.append(stats);
  const spentNum = h("span", { class: "spent-num" });
  const capNum = h("span", { class: "cap-num" });
  const overNum = h("span", { class: "over-num" });
  const stampede = h("button", { type: "button", class: "secondary small", text: "Stampede", onclick: () => stampedeDialog(id) });
  const reset = h("button", { type: "button", class: "ghost small", text: "Reset", onclick: () => resetProject(id) });
  const readout = h("div", { class: "readout" }, spentNum, capNum, overNum, h("div", { class: "lane-actions" }, stampede, reset));
  const el = h("div", { class: "lane", role: "listitem" }, h("div", { class: "lane-name" }, title, mode), meter, readout);
  return { el, title, mode, spent, held, over, meter, stats, spentNum, capNum, overNum, stampede };
}

function updateLane(lane, p) {
  const cap = Math.max(p.cap_micros, 1);
  const pct = (v) => (v / cap) * CAP_AT;
  const total = p.spent_micros + p.reserved_micros;
  const spentIn = Math.min(p.spent_micros, cap);
  const heldIn = Math.max(Math.min(total, cap) - spentIn, 0);
  const over = Math.max(total - p.cap_micros, 0);

  lane.spent.style.width = pct(spentIn) + "%";
  lane.held.style.left = pct(spentIn) + "%";
  lane.held.style.width = pct(heldIn) + "%";
  lane.over.style.width = Math.min(pct(over), 100 - CAP_AT) + "%";

  const breached = p.spent_micros > p.cap_micros;
  lane.el.classList.toggle("breached", breached);
  lane.el.classList.toggle("busy", p.reserved_micros > 0);
  lane.el.classList.toggle("selected", p.id === state.selected);

  lane.title.textContent = p.name;
  lane.mode.textContent = p.mode === "naive" ? "Naive: checks the balance, charges afterwards" : "Hardcap: holds the worst case first";
  lane.mode.className = "mode " + p.mode;
  lane.spentNum.textContent = usd(p.spent_micros);
  lane.capNum.textContent = "of " + capUSD(p.cap_micros) + " cap";
  lane.overNum.textContent = breached ? usd(p.spent_micros - p.cap_micros) + " over" : "";
  lane.meter.setAttribute("aria-valuemax", String(p.cap_micros / 1e6));
  lane.meter.setAttribute("aria-valuenow", String(p.spent_micros / 1e6));
  lane.meter.setAttribute("aria-label", `${p.name}: ${usd(p.spent_micros)} spent of ${capUSD(p.cap_micros)}`);

  const s = p.stampede;
  lane.stampede.hidden = !state.demo;
  lane.stampede.disabled = !!(s && s.running);
  const parts = [
    h("span", {}, "Requests ", h("b", { text: count(p.requests) })),
    h("span", {}, "Held now ", h("b", { text: usd(p.reserved_micros) })),
  ];
  if (s) {
    const tally = [h("b", { text: count(s.ok) }), " served, ", h("b", { text: count(s.refused) }), " refused"];
    if (s.failed) tally.push(", ", h("b", { text: count(s.failed) }), " failed");
    parts.push(h("span", { class: s.running ? "live" : "" }, s.running ? "Stampede running: " : "Last stampede: ", ...tally));
  }
  lane.stats.replaceChildren(...parts);
}

function select(id) {
  state.selected = id;
  storage.set("hardcap.selected", id ? String(id) : null);
  state.usage = [];
  state.keys = [];
  for (const [pid, lane] of state.lanes) lane.el.classList.toggle("selected", pid === id);
  $("#detail").hidden = !id;
  renderDetail();
  pollDetail();
  if (id) $("#detail").scrollIntoView({ behavior: "smooth", block: "nearest" });
}

// ---------- Project detail ----------

function selectedProject() {
  return state.projects.find((p) => p.id === state.selected);
}

let detailBuilt = null;
function renderDetailHead() {
  const p = selectedProject();
  if (!p || detailBuilt !== p.id) return;
  $("#detail-sub").textContent =
    `${p.mode === "naive" ? "Naive mode" : "Hardcap mode"}. ${usd(p.spent_micros)} spent of a ${capUSD(p.cap_micros)} cap.`;
  $("#mode-switch").textContent = p.mode === "naive" ? "Switch to Hardcap" : "Switch to naive";
}

function renderDetail() {
  const p = selectedProject();
  const box = $("#detail");
  if (!p) { box.hidden = true; detailBuilt = null; return; }
  box.hidden = false;
  if (detailBuilt !== p.id) {
    detailBuilt = p.id;
    box.replaceChildren(
      h("div", { class: "detail-head" },
        h("div", {}, h("h2", { text: p.name }), h("p", { class: "sub", id: "detail-sub" })),
        h("div", { class: "row" },
          h("button", { type: "button", class: "secondary", text: "Change cap", onclick: () => capDialog(p.id) }),
          h("button", { type: "button", class: "secondary", id: "mode-switch", onclick: () => switchMode(p.id) }),
          h("button", { type: "button", class: "ghost", text: "Close", onclick: () => select(null) }),
        ),
      ),
      h("div", { class: "detail-grid" },
        h("div", { class: "panel" },
          h("div", { class: "panel-head" }, h("h3", { text: "Recent requests" })),
          h("div", { class: "table-wrap", id: "usage" }),
        ),
        h("div", { class: "panel" },
          h("div", { class: "panel-head" },
            h("h3", { text: "API keys" }),
            h("button", { type: "button", class: "secondary small", text: "Create key", onclick: () => keyDialog(p.id) }),
          ),
          h("ul", { class: "keys", id: "keys" }),
        ),
      ),
    );
  }
  renderDetailHead();
  renderUsage();
  renderKeys();
}

const OUTCOMES = {
  ok: ["Served", "outcome-ok"],
  usage_missing: ["No usage report", "outcome-warn"],
  client_gone: ["Client hung up", "outcome-warn"],
  upstream_failed: ["Provider failed", "outcome-bad"],
  provider_error: ["Provider refused", "outcome-bad"],
  unavailable: ["No provider", "outcome-bad"],
  expired: ["Gateway died: hold charged", "outcome-bad"],
};

function renderUsage() {
  const box = $("#usage");
  if (!box) return;
  if (state.usage.length === 0) {
    box.replaceChildren(h("p", { class: "muted", text: "No requests yet. Requests show up here as soon as they finish." }));
    return;
  }
  const rows = state.usage.map((e) => {
    const [label, cls] = OUTCOMES[e.outcome] || [e.outcome, "outcome-ok"];
    return h("tr", {},
      h("td", { class: "muted", text: new Date(e.created_at).toLocaleTimeString() }),
      h("td", { text: e.model }),
      h("td", { class: "muted", text: e.provider || "none" }),
      h("td", { class: "num", text: e.usage_known ? `${count(e.prompt_tokens)} / ${count(e.completion_tokens)}` : "unknown" }),
      h("td", { class: "num muted", text: usd(e.held_micros, 6) }),
      h("td", { class: "num", text: usd(e.cost_micros, 6) }),
      h("td", { class: cls, text: label }),
    );
  });
  box.replaceChildren(h("table", {},
    h("thead", {}, h("tr", {},
      h("th", { text: "Time" }), h("th", { text: "Model" }), h("th", { text: "Provider" }),
      h("th", { class: "num", text: "Tokens in / out" }), h("th", { class: "num", text: "Held" }),
      h("th", { class: "num", text: "Charged" }), h("th", { text: "Result" }))),
    h("tbody", {}, rows)));
}

function renderKeys() {
  const list = $("#keys");
  if (!list) return;
  if (state.keys.length === 0) {
    list.replaceChildren(h("li", { class: "muted", text: "No keys yet. Create one to send requests to this project." }));
    return;
  }
  list.replaceChildren(...state.keys.slice().reverse().map((k) => {
    const limits = [k.cap_micros != null ? `cap ${capUSD(k.cap_micros)}` : null, `spent ${usd(k.spent_micros)}`]
      .filter(Boolean).join(", ");
    return h("li", { class: k.revoked ? "revoked" : "" },
      h("div", { class: "key-main" },
        h("div", { class: "key-name", text: k.name }),
        h("div", { class: "key-meta", text: `${k.prefix}…  ${limits}` }),
      ),
      k.revoked ? h("span", { class: "muted", text: "Revoked" })
        : h("button", { type: "button", class: "danger small", text: "Revoke", onclick: () => revokeKey(k) }),
    );
  }));
}

// ---------- Actions ----------

async function resetProject(id) {
  const p = state.projects.find((x) => x.id === id);
  if (!p || !confirm(`Reset spending for ${p.name}? Its receipts are deleted and spent goes back to $0.`)) return;
  try {
    await api("POST", `/admin/api/projects/${id}/reset`);
    toast(`${p.name} reset`);
    refresh();
    if (id === state.selected) pollDetail();
  } catch (err) { toast(err.message, true); }
}

async function switchMode(id) {
  const p = state.projects.find((x) => x.id === id);
  if (!p) return;
  const mode = p.mode === "naive" ? "hardcap" : "naive";
  try {
    await api("PATCH", `/admin/api/projects/${id}`, { mode });
    toast(`${p.name} now uses ${mode === "naive" ? "naive" : "Hardcap"} mode`);
    refresh();
  } catch (err) { toast(err.message, true); }
}

async function revokeKey(k) {
  if (!confirm(`Revoke ${k.name}? Apps using it stop working right away.`)) return;
  try {
    await api("POST", `/admin/api/keys/${k.id}/revoke`);
    toast(`${k.name} revoked`);
    pollDetail();
  } catch (err) { toast(err.message, true); }
}

// A single <dialog> serves every form.
let dialogSubmit = null;
function openDialog({ title, body, submit, onSubmit }) {
  $("#dialog-title").textContent = title;
  $("#dialog-body").replaceChildren(...body);
  $("#dialog-error").textContent = "";
  $("#dialog-submit").textContent = submit;
  $("#dialog-submit").hidden = !submit;
  dialogSubmit = onSubmit;
  $("#dialog").showModal();
  const first = $("#dialog-body input, #dialog-body select");
  if (first) first.focus();
}

$("#dialog-cancel").addEventListener("click", () => $("#dialog").close());
$("#dialog-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (!dialogSubmit) { $("#dialog").close(); return; }
  const btn = $("#dialog-submit");
  btn.disabled = true;
  try {
    const keepOpen = await dialogSubmit(new FormData(e.target));
    if (!keepOpen) $("#dialog").close();
  } catch (err) {
    $("#dialog-error").textContent = err.message;
  } finally {
    btn.disabled = false;
  }
});

function field(label, input) {
  const id = "f-" + input.name;
  input.id = id;
  return h("div", {}, h("label", { for: id, text: label }), input);
}

function modeChoices(selected) {
  const choice = (value, title, desc) => h("label", { class: "choice" },
    h("input", { type: "radio", name: "mode", value, checked: value === selected }),
    h("span", {}, title, h("small", { text: desc })));
  return h("div", { class: "choices", role: "radiogroup", "aria-label": "How the cap is enforced" },
    choice("hardcap", "Hardcap", "Holds the worst-case cost before each request. Never goes over the cap."),
    choice("naive", "Naive", "Checks the balance, charges afterwards. Goes over under load: here for comparison."));
}

function projectDialog() {
  openDialog({
    title: "New project",
    submit: "Create project",
    body: [
      field("Name", h("input", { name: "name", required: true, maxlength: "100", placeholder: "My chatbot" })),
      field("Spending cap in US dollars", h("input", { name: "cap", required: true, inputmode: "decimal", value: "1.00", pattern: "\\$?[0-9]*\\.?[0-9]{0,6}" })),
      modeChoices("hardcap"),
    ],
    onSubmit: async (f) => {
      const p = await api("POST", "/admin/api/projects", {
        name: f.get("name"), cap_usd: Number(String(f.get("cap")).replace("$", "")), mode: f.get("mode"),
      });
      toast(`${p.name} created`);
      await refresh();
      select(p.id);
    },
  });
}

function capDialog(id) {
  const p = state.projects.find((x) => x.id === id);
  openDialog({
    title: `Change the cap for ${p.name}`,
    submit: "Save cap",
    body: [field("Spending cap in US dollars", h("input", { name: "cap", required: true, inputmode: "decimal", value: (p.cap_micros / 1e6).toFixed(2) }))],
    onSubmit: async (f) => {
      await api("PATCH", `/admin/api/projects/${id}`, { cap_usd: Number(String(f.get("cap")).replace("$", "")) });
      toast("Cap saved");
      refresh();
    },
  });
}

function keyDialog(id) {
  openDialog({
    title: "Create an API key",
    submit: "Create key",
    body: [
      field("Name", h("input", { name: "name", required: true, maxlength: "100", placeholder: "Production" })),
      field("Its own cap in US dollars (optional)", h("input", { name: "cap", inputmode: "decimal", placeholder: "No cap of its own" })),
      field("Requests per minute (optional)", h("input", { name: "rpm", inputmode: "numeric", placeholder: "Gateway default" })),
    ],
    onSubmit: async (f) => {
      const body = { name: f.get("name") };
      if (f.get("cap")) body.cap_usd = Number(String(f.get("cap")).replace("$", ""));
      if (f.get("rpm")) body.rpm_limit = Number(f.get("rpm"));
      const { secret } = await api("POST", `/admin/api/projects/${id}/keys`, body);
      pollDetail();
      showSecret(secret);
      return true; // keep the dialog: it now shows the secret
    },
  });
}

function showSecret(secret) {
  const input = h("input", { readonly: true, value: secret, "aria-label": "API key" });
  const copy = h("button", { type: "button", class: "secondary", text: "Copy", onclick: async () => {
    try { await navigator.clipboard.writeText(secret); copy.textContent = "Copied"; }
    catch { input.select(); }
  } });
  $("#dialog-title").textContent = "Your new API key";
  $("#dialog-body").replaceChildren(
    h("p", { class: "hint", text: "Copy it now. Hardcap only stores a hash of it, so it can't be shown again." }),
    h("div", { class: "secret" }, input, copy),
  );
  $("#dialog-submit").textContent = "Done";
  $("#dialog-cancel").hidden = true;
  dialogSubmit = null;
  input.select();
  $("#dialog").addEventListener("close", () => { $("#dialog-cancel").hidden = false; }, { once: true });
}

function stampedeDialog(id) {
  const p = state.projects.find((x) => x.id === id);
  const models = h("select", { name: "model" }, state.models.map((m) => h("option", { value: m, selected: m === "mock-1", text: m })));
  openDialog({
    title: `Stampede ${p.name}`,
    submit: "Start stampede",
    body: [
      h("p", { class: "hint", text: "Sends a crowd of clients at this project through the real gateway, as fast as they can go. Use the free mock-1 model unless you mean to spend real quota." }),
      field("Clients at once", h("input", { name: "workers", inputmode: "numeric", value: String(DEMO.workers) })),
      field("Seconds", h("input", { name: "seconds", inputmode: "numeric", value: String(DEMO.seconds) })),
      field("Model", models),
    ],
    onSubmit: async (f) => {
      await api("POST", `/admin/api/projects/${id}/stampede`, {
        workers: Number(f.get("workers")), seconds: Number(f.get("seconds")), model: f.get("model"),
      });
      toast(`Stampede started on ${p.name}`);
    },
  });
}

// The one-click demo: two $1.00 projects, one naive and one Hardcap,
// reset, then stampeded at the same moment.
async function runDemo() {
  const btn = $("#run-demo");
  btn.disabled = true;
  try {
    await refresh();
    const ids = [];
    for (const mode of ["naive", "hardcap"]) {
      let p = state.projects.find((x) => x.name === DEMO[mode]);
      if (!p) p = await api("POST", "/admin/api/projects", { name: DEMO[mode], cap_usd: 1, mode });
      else {
        if (p.stampede && p.stampede.running) throw new Error("The demo is already running");
        await api("POST", `/admin/api/projects/${p.id}/reset`);
      }
      ids.push(p.id);
    }
    await Promise.all(ids.map((id) => api("POST", `/admin/api/projects/${id}/stampede`,
      { workers: DEMO.workers, seconds: DEMO.seconds, model: "mock-1" })));
    toast(`${DEMO.workers} clients are hitting both projects for ${DEMO.seconds} seconds`);
    await refresh();
  } catch (err) {
    toast(err.message, true);
  } finally {
    btn.disabled = false;
  }
}

$("#new-project").addEventListener("click", projectDialog);
$("#empty-new").addEventListener("click", projectDialog);
$("#run-demo").addEventListener("click", runDemo);
$("#empty-demo").addEventListener("click", runDemo);

// ---------- Start ----------

if (state.token) {
  showConnected(true);
  poll();
  if (state.selected) select(state.selected);
} else {
  showConnected(false);
}

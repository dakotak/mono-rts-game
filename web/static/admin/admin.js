// Administrative client. Talks only to the admin gateway (/api/* on the
// admin listener), which mirrors the gRPC AdminService. Every mutation is a
// scheduled command; results arrive on the observation stream.
"use strict";

const { Board, stream, applyMessage, attrPos, isUnit, isWell, tag, fmtPos, fmtValue, log } = RTS;
const CREATE_WELL = "type.googleapis.com/rts.wells.v1.CreateWellCommand";
const CONFIGURE_WELL = "type.googleapis.com/rts.wells.v1.ConfigureWellCommand";
const SET_ENERGY = "type.googleapis.com/rts.energy.v1.SetUnitEnergyCommand";
const SET_HEALTH = "type.googleapis.com/rts.combat.v1.SetHealthCommand";
const CREATE_WRECK = "type.googleapis.com/rts.salvage.v1.CreateWreckCommand";
const SET_WRECK = "type.googleapis.com/rts.salvage.v1.SetWreckEnergyCommand";
// Players are coloured in configuration order.
const PALETTE = ["#e05252", "#4a7fe0", "#43b36b", "#e0a43a", "#a46be0", "#3abfc4", "#d96fb0"];

const $ = (id) => document.getElementById(id);

let token = null;
let world = null; // WorldInfo
let rules = {}; // plugin name -> published rules
let players = []; // [{id, name}]
let tick = 0;
let paused = false;
const entities = new Map();
const selected = new Set();
const pending = new Map(); // command id -> description
// Results can arrive on the stream before the HTTP response that tells us
// which command id is ours (e.g. when stepping right after submitting).
const early = new Map(); // command id -> CommandResult
let stopStream = null;
let statusTimer = null;

const board = new Board($("canvas"), $("board"), {
  onClick: clickTile,
  onBox: (inside, { shift }) => {
    if (!shift) selected.clear();
    for (const e of inside) selected.add(e.id);
    render();
  },
  onContext: (t) => { if (selected.size === 1) reposition(t); },
});

const colorFor = (e) => {
  const i = players.findIndex((p) => p.id === e.ownerId);
  return i < 0 ? "#9aa0a6" : PALETTE[i % PALETTE.length];
};
const playerName = (id) => (players.find((p) => p.id === id) || { name: id ? "player " + id : "none" }).name;
const say = (text, cls) => log($("log"), tick, text, cls);

// ---- API ------------------------------------------------------------------

async function api(path, body) {
  const res = await fetch(path, {
    method: body === undefined ? "GET" : "POST",
    headers: { "Content-Type": "application/json", Authorization: "Bearer " + token },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.error || res.statusText);
    err.status = res.status;
    throw err;
  }
  return data;
}

// mutate sends a scheduled mutation and remembers its description so the
// eventual CommandResult can be reported.
async function mutate(path, body, description) {
  try {
    const resp = await api(path, body);
    if (resp.accepted === false) {
      say(`${description}: rejected — ${resp.rejectionReason}`, "error");
      return;
    }
    say(`${description}: scheduled for tick ${resp.scheduledTick}`, "info");
    if (early.has(resp.commandId)) {
      reportResult(early.get(resp.commandId), description);
      early.delete(resp.commandId);
    } else {
      pending.set(resp.commandId, description);
    }
  } catch (e) {
    say(`${description}: ${e.message}`, "error");
    if (e.status === 401) logout("Admin token rejected.");
  }
}

// ---- session ----------------------------------------------------------------

async function login(t) {
  token = t;
  $("login-error").textContent = "";
  try {
    const snap = await api("/api/snapshot");
    try { sessionStorage.setItem("rts.admin", token); } catch (_) {}
    start(snap);
  } catch (e) {
    token = null;
    $("login-error").textContent = e.status === 401 ? "Invalid admin token." : e.message;
  }
}

function logout(reason) {
  if (stopStream) stopStream();
  clearInterval(statusTimer);
  token = null;
  try { sessionStorage.removeItem("rts.admin"); } catch (_) {}
  $("game").hidden = true;
  $("login").hidden = false;
  $("login-error").textContent = reason || "";
}

function start(snap) {
  $("login").hidden = true;
  $("game").hidden = false;
  players = snap.players;
  renderPlayers();
  world = snap.world;
  try { rules = JSON.parse(world.rulesJson || "{}"); } catch (_) { rules = {}; }
  board.setWorld(world.width, world.height);
  setStatus(snap.status);
  stopStream = stream({
    url: "/api/observe",
    token,
    onMessage: handleMessage,
    onStatus: (s) => setConn(s === "live" ? "live" : "reconnecting…", s === "live" ? "live" : "down"),
    onUnauthorized: () => logout("Admin token rejected."),
  });
  statusTimer = setInterval(refreshStatus, 1000);
}

async function refreshStatus() {
  try { setStatus((await api("/api/status")).status); } catch (_) {}
}

function setStatus(s) {
  paused = s.paused;
  const el = $("sim-status");
  el.textContent = paused ? "paused" : `running @ ${s.tickRateHz} Hz`;
  el.className = "status " + (paused ? "paused" : "running");
  $("pause-btn").textContent = paused ? "Resume" : "Pause";
  $("step-btn").disabled = !paused;
}

function setConn(text, cls) {
  const el = $("conn");
  el.textContent = text;
  el.className = "conn " + (cls || "");
}

function renderPlayers() {
  const legend = $("legend"), sel = $("spawn-owner");
  legend.replaceChildren();
  sel.replaceChildren();
  players.forEach((p, i) => {
    const li = document.createElement("li");
    const sw = document.createElement("span");
    sw.className = "swatch";
    sw.style.background = PALETTE[i % PALETTE.length];
    li.append(sw, `${p.name} (id ${p.id})`);
    legend.append(li);
    sel.append(new Option(p.name, p.id));
  });
  sel.append(new Option("unowned", 0));
}

// ---- observation ------------------------------------------------------------

function handleMessage(msg) {
  tick = applyMessage(entities, msg);
  for (const id of [...selected]) if (!entities.has(id)) selected.delete(id);
  if (msg.update) {
    for (const r of msg.update.commandResults) {
      if (pending.has(r.commandId)) {
        reportResult(r, pending.get(r.commandId));
        pending.delete(r.commandId);
      } else {
        early.set(r.commandId, r);
        // Commands from other admin sessions never get claimed; report them
        // after a short grace period.
        setTimeout(() => { if (early.delete(r.commandId)) reportResult(r, r.commandId); }, 2000);
      }
    }
    for (const ev of msg.update.events) {
      const fields = Object.entries(ev.fields || {}).filter(([k]) => k !== "command_id")
        .map(([k, v]) => `${k}=${fmtValue(v)}`).join(" ");
      say(`${ev.type} ${ev.entityId !== "0" ? tag(ev.entityId) : ""} ${fields}`, "info");
    }
  }
  render();
}

function reportResult(r, desc) {
  if (r.status === "COMMAND_STATUS_FAILED") say(`${desc} failed (${r.errorCode}): ${r.errorMessage}`, "error");
  else say(`${desc}: done (tick ${r.tick})`, "ok");
}

// ---- tools ------------------------------------------------------------------

const tool = () => document.querySelector('input[name="tool"]:checked').value;
const numOrUndef = (id) => ($(id).value === "" ? undefined : Number($(id).value));

function clickTile(t, { shift }) {
  const e = board.entityAt(t.x, t.y);
  switch (tool()) {
    case "spawn":
      if (e) return say("Tile occupied", "error");
      return mutate("/api/spawn", { ownerId: Number($("spawn-owner").value), position: t },
        `Spawn unit for ${playerName(Number($("spawn-owner").value))} at ${fmtPos(t)}`);
    case "well": {
      if (e) return say("Tile occupied", "error");
      const payload = { "@type": CREATE_WELL, position: t };
      const energy = numOrUndef("nw-energy"), capacity = numOrUndef("nw-capacity"), rate = numOrUndef("nw-rate");
      if (energy !== undefined) payload.energy = String(energy);
      if (capacity !== undefined) payload.capacity = String(capacity);
      if (rate !== undefined) payload.extractionRate = String(rate);
      if ($("nw-unlimited").checked) payload.unlimited = true;
      return mutate("/api/command", { payload }, `Create well at ${fmtPos(t)}`);
    }
    case "move":
      return reposition(t);
    case "wreck": {
      // Wreckage is passable: it may be placed under a unit.
      const energy = Math.max(1, Math.floor(Number($("nwreck-energy").value) || 0));
      return mutate("/api/command", { payload: { "@type": CREATE_WRECK, position: t, energy: String(energy) } }, `Create wreck (${energy} energy) at ${fmtPos(t)}`);
    }
    default:
      if (e) {
        if (shift) selected.has(e.id) ? selected.delete(e.id) : selected.add(e.id);
        else { selected.clear(); selected.add(e.id); }
      } else if (!shift) selected.clear();
      render();
  }
}

function reposition(t) {
  if (selected.size !== 1) return say("Select exactly one entity to reposition", "error");
  const id = [...selected][0];
  mutate("/api/move", { entityId: id, position: t }, `Move ${tag(id)} to ${fmtPos(t)}`);
}

function deleteSelected() {
  for (const id of selected) mutate("/api/delete", { entityId: id }, `Delete ${tag(id)}`);
}

function configureWell(id) {
  const v = (name) => $("cw-" + name).value;
  const payload = {
    "@type": CONFIGURE_WELL,
    wellId: id,
    energy: String(Number(v("energy"))),
    capacity: String(Number(v("capacity"))),
    extractionRate: String(Number(v("rate"))),
    unlimited: $("cw-unlimited").checked,
  };
  mutate("/api/command", { payload }, `Configure well ${tag(id)}`);
}

// ---- inspector --------------------------------------------------------------

function renderInspector() {
  const box = $("inspector");
  box.replaceChildren();
  const ids = [...selected];
  if (ids.length === 0) {
    box.innerHTML = '<p class="hint">Click an entity to inspect it. Drag to select several.</p>';
    return;
  }
  if (ids.length > 1) {
    const p = document.createElement("p");
    p.textContent = `${ids.length} entities selected.`;
    box.append(p, button("Delete all", deleteSelected, "danger"));
    return;
  }
  const e = entities.get(ids[0]);
  const table = document.createElement("table");
  const row = (k, v, title) => {
    const tr = table.insertRow();
    tr.insertCell().textContent = k;
    tr.insertCell().textContent = v;
    if (title) tr.title = title;
  };
  const group = (name) => {
    const tr = table.insertRow();
    tr.className = "group";
    const td = tr.insertCell();
    td.colSpan = 2;
    td.textContent = name;
  };
  group("entity");
  row("id", e.id);
  row("kind", e.kind.replace("ENTITY_KIND_", "").toLowerCase());
  row("owner", e.ownerId ? `${playerName(e.ownerId)} (${e.ownerId})` : "none");
  row("position", fmtPos(e.position));
  // Group attributes by owning plugin ("attributes.armor" -> attributes).
  const groups = {};
  for (const k of Object.keys(e.attributes || {}).sort()) {
    const g = k.split(".")[0];
    (groups[g] = groups[g] || []).push(k);
  }
  const defs = (rules.attributes && rules.attributes.attributes) || [];
  const derived = (rules.attributes && rules.attributes.derived) || {};
  for (const g of Object.keys(groups).sort()) {
    group(g);
    for (const k of groups[g]) {
      const name = k.slice(g.length + 1);
      let v = fmtValue(e.attributes[k]);
      let title = "";
      const d = g === "attributes" && defs.find((x) => x.name === name);
      if (d) {
        const cur = Number(e.attributes[k].intValue);
        const next = cur < d.max ? d.cost_table[cur + 1 - d.min] - d.cost_table[cur - d.min] : null;
        v += `  (${d.min}–${d.max}${next !== null ? `, +1 costs ${next}` : ", max"})`;
        title = `mass: ${d.mass}\ncost(v): ${d.cost}\nduration(v): ${d.duration}`;
      } else if (g === "attributes" && derived[name]) {
        title = `derived: ${derived[name]}`;
      }
      row(name, v, title);
    }
  }
  box.append(table);

  if (isWell(e)) {
    const a = (k) => fmtValue(e.attributes["wells." + k]);
    const fs = document.createElement("fieldset");
    fs.className = "form";
    fs.innerHTML = `<legend>Configure well</legend>
      <label>Energy <input id="cw-energy" type="number" min="0" value="${a("energy")}"></label>
      <label>Capacity <input id="cw-capacity" type="number" min="0" value="${a("capacity")}"></label>
      <label>Rate/tick <input id="cw-rate" type="number" min="0" value="${a("extraction_rate")}"></label>
      <label><input id="cw-unlimited" type="checkbox" ${a("unlimited") === "true" ? "checked" : ""}> Unlimited</label>`;
    fs.append(button("Apply", () => configureWell(e.id)));
    box.append(fs);
  }
  if (isUnit(e) && e.attributes && e.attributes["combat.health"]) {
    const a = (k) => fmtValue(e.attributes["combat." + k]);
    const fs = document.createElement("fieldset");
    fs.className = "form";
    fs.innerHTML = `<legend>Integrity (0 destroys)</legend>
      <label>Health (max ${a("max_health")}) <input id="cw-unit-health" type="number" min="0" value="${a("health")}"></label>`;
    fs.append(button("Apply", () => mutate("/api/command", { payload: {
      "@type": SET_HEALTH, unitId: e.id, health: String(Number($("cw-unit-health").value)),
    } }, `Set integrity of ${tag(e.id)}`)));
    box.append(fs);
  }
  if (RTS.isWreck(e)) {
    const fs = document.createElement("fieldset");
    fs.className = "form";
    fs.innerHTML = `<legend>Wreckage energy (0 removes)</legend>
      <label>Energy <input id="cw-wreck-energy" type="number" min="0" value="${fmtValue(e.attributes["salvage.energy"])}"></label>`;
    fs.append(button("Apply", () => mutate("/api/command", { payload: {
      "@type": SET_WRECK, wreckId: e.id, energy: String(Number($("cw-wreck-energy").value)),
    } }, `Set wreck ${tag(e.id)} energy`)));
    box.append(fs);
  }
  if (isUnit(e) && e.attributes && e.attributes["energy.stored"]) {
    const a = (k) => fmtValue(e.attributes["energy." + k]);
    const fs = document.createElement("fieldset");
    fs.className = "form";
    fs.innerHTML = `<legend>Unit energy</legend>
      <label>Energy <input id="cw-unit-energy" type="number" min="0" value="${a("stored")}"></label>
      <label>Capacity <input id="cw-unit-capacity" type="number" min="0" value="${a("capacity")}"></label>`;
    fs.append(button("Apply", () => mutate("/api/command", { payload: {
      "@type": SET_ENERGY,
      unitId: e.id,
      energy: String(Number($("cw-unit-energy").value)),
      capacity: String(Number($("cw-unit-capacity").value)),
    } }, `Set energy of ${tag(e.id)}`)));
    box.append(fs);
  }
  const actions = document.createElement("div");
  actions.className = "actions";
  actions.append(
    button("Reposition…", () => { document.querySelector('input[value="move"]').checked = true; say("Click a tile (or right-click) to reposition", "info"); }),
    button("Delete", deleteSelected, "danger"),
  );
  box.append(actions);
}

function button(text, fn, cls) {
  const b = document.createElement("button");
  b.type = "button";
  b.textContent = text;
  if (cls) b.className = cls;
  b.addEventListener("click", fn);
  return b;
}

// ---- rendering ----------------------------------------------------------------

let inspected = "";

function render() {
  $("tick").textContent = tick;
  $("entity-count").textContent = entities.size;
  const lines = [];
  for (const e of entities.values()) {
    const d = attrPos(e, "movement.destination");
    if (d) lines.push({ from: e.position, to: d, cross: true, color: "rgba(255,255,255,0.25)" });
  }
  board.draw({ entities, selected, lines, colorFor, mine: isUnit });
  // Re-render the inspector only when the inspected data changed, so form
  // inputs being edited are not clobbered every tick.
  const key = JSON.stringify([...selected].map((id) => entities.get(id)));
  const editing = document.activeElement && document.activeElement.id && document.activeElement.id.startsWith("cw-");
  if (key !== inspected && !editing) {
    inspected = key;
    renderInspector();
  }
}

// ---- controls -----------------------------------------------------------------

$("pause-btn").addEventListener("click", async () => {
  try { setStatus({ ...(await api("/api/status")).status, ...(await api("/api/pause", { paused: !paused })) }); }
  catch (e) { say(e.message, "error"); }
});
$("step-btn").addEventListener("click", async () => {
  try {
    const r = await api("/api/step", { count: Number($("step-count").value) || 1 });
    say(`Stepping ${r.count} tick(s) from tick ${r.firstTick}`, "info");
  } catch (e) { say(`Step: ${e.message}`, "error"); }
});
$("logout-btn").addEventListener("click", () => logout(""));
$("rules-btn").addEventListener("click", showRules);
$("rules-close").addEventListener("click", () => { $("rules-panel").hidden = true; });

// showRules renders every plugin's published rules: attribute definitions
// and formulas as a table, everything else as formatted JSON.
function showRules() {
  const box = $("rules-content");
  box.replaceChildren();
  const h = (text) => { const e = document.createElement("h3"); e.textContent = text; box.append(e); };
  const pre = (obj) => { const e = document.createElement("pre"); e.textContent = typeof obj === "string" ? obj : JSON.stringify(obj, null, 2); box.append(e); };
  h("Tick schedule");
  pre((world.schedule || []).join("\n") + `\n\nunit command budget per player per tick: ${world.unitCommandBudget || "unlimited"}`);
  const a = rules.attributes;
  if (a) {
    h(`Attributes (base mass ${a.base_mass})`);
    const t = document.createElement("table");
    t.innerHTML = "<tr><th>name</th><th>default</th><th>range</th><th>mass</th><th>cost(v) (cumulative)</th><th>duration(v) (cumulative)</th></tr>";
    for (const d of a.attributes || []) {
      const tr = t.insertRow();
      for (const v of [d.name, d.default, `${d.min}–${d.max}`, d.mass, d.cost, d.duration]) {
        const td = tr.insertCell();
        const c = document.createElement("code");
        c.textContent = v;
        td.append(c);
      }
    }
    box.append(t);
    h("Derived properties (evaluation order)");
    pre(["mass", ...(a.derived_order || [])].map((n) => `${n} = ${a.derived[n]}`).join("\n") + "\n\n" + (a.notes || ""));
  }
  for (const name of Object.keys(rules).sort()) {
    if (name === "attributes") continue;
    h(name);
    pre(rules[name]);
  }
  $("rules-panel").hidden = false;
}
$("login-form").addEventListener("submit", (ev) => { ev.preventDefault(); login($("admin-token").value); });
window.addEventListener("keydown", (ev) => {
  if (!token || ["INPUT", "SELECT"].includes(ev.target.tagName)) return;
  if (ev.key === "Escape") { selected.clear(); render(); }
  if (ev.key === "Delete" || ev.key === "Backspace") deleteSelected();
  if (ev.key === " ") { ev.preventDefault(); $("pause-btn").click(); }
});

try {
  const saved = sessionStorage.getItem("rts.admin");
  if (saved) login(saved);
} catch (_) {}

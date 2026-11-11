// Administrative client. Talks only to the admin gateway (/api/* on the
// admin listener), which mirrors the gRPC AdminService. Every mutation is a
// scheduled command; results arrive on the observation stream.
//
// Data flows one way: the observation stream and the status/match polls
// update Store (store.js); panels render from Store and subscribe to its
// topics. The dockable layout (workspace.js) only moves panel elements.
"use strict";

const { Board, stream, applyMessage, attrPos, isUnit, isWell, tag, fmtPos, fmtValue } = RTS;
const CREATE_WELL = "type.googleapis.com/rts.wells.v1.CreateWellCommand";
const CONFIGURE_WELL = "type.googleapis.com/rts.wells.v1.ConfigureWellCommand";
const SET_ENERGY = "type.googleapis.com/rts.energy.v1.SetUnitEnergyCommand";
const SET_HEALTH = "type.googleapis.com/rts.combat.v1.SetHealthCommand";
const CREATE_WRECK = "type.googleapis.com/rts.salvage.v1.CreateWreckCommand";
const SET_WRECK = "type.googleapis.com/rts.salvage.v1.SetWreckEnergyCommand";
// Players are coloured in configuration order.
const PALETTE = ["#e05252", "#4a7fe0", "#43b36b", "#e0a43a", "#a46be0", "#3abfc4", "#d96fb0"];

const $ = Store.el;
const S = Store.state;

let token = null;
const pending = new Map(); // command id -> description
// Results can arrive on the stream before the HTTP response that tells us
// which command id is ours (e.g. when stepping right after submitting).
const early = new Map(); // command id -> CommandResult
let stopStream = null;
let statusTimer = null;

const board = new Board($("canvas"), $("board"), {
  zoom: true,
  onClick: clickTile,
  onBox: (inside, { shift }) => Store.select(inside.map((e) => e.id), { add: shift }),
  onContext: (t) => { if (S.selected.size === 1) reposition(t); },
  onView: (v) => { $("board-zoom").textContent = Math.round(v.zoom * 100) + "%"; },
});

const playerIndex = (id) => S.players.findIndex((p) => Number(p.id) === Number(id));
const playerColor = (id) => { const i = playerIndex(id); return i < 0 ? "#9aa0a6" : PALETTE[i % PALETTE.length]; };
const colorFor = (e) => playerColor(e.ownerId);
const playerName = (id) => (S.players.find((p) => Number(p.id) === Number(id)) || { name: id ? "player " + id : "none" }).name;

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
      say(`${description}: rejected — ${resp.rejectionReason}`, "error", { category: "command" });
      return;
    }
    say(`${description}: scheduled for tick ${resp.scheduledTick}`, "info", { category: "command" });
    if (early.has(resp.commandId)) {
      reportResult(early.get(resp.commandId), description);
      early.delete(resp.commandId);
    } else {
      pending.set(resp.commandId, description);
    }
  } catch (e) {
    say(`${description}: ${e.message}`, "error", { category: "command" });
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
  setViewAs(0);
  token = null;
  try { sessionStorage.removeItem("rts.admin"); } catch (_) {}
  $("game").hidden = true;
  $("login").hidden = false;
  $("login-error").textContent = reason || "";
}

function setWorld(snap) {
  S.players = snap.players || [];
  S.world = snap.world;
  try { S.rules = JSON.parse(S.world.rulesJson || "{}"); } catch (_) { S.rules = {}; }
  board.setWorld(S.world.width, S.world.height);
  renderPlayerOptions();
  Store.emit("world");
}

function start(snap) {
  $("login").hidden = true;
  $("game").hidden = false;
  Workspace.init(); // builds the panels on first sign-in (no-op afterwards)
  setWorld(snap);
  setStatus(snap.status);
  stopStream = stream({
    url: "/api/observe",
    token,
    onMessage: handleMessage,
    onStatus: (s) => setConn(s === "live" ? "live" : "reconnecting…", s === "live" ? "live" : "down"),
    onUnauthorized: () => logout("Admin token rejected."),
  });
  statusTimer = setInterval(refreshStatus, 1000);
  say("Signed in", "ok", { category: "system" });
}

async function refreshStatus() {
  try { setStatus((await api("/api/status")).status); } catch (_) {}
}

// setStatus records the server's authoritative simulation status.
function setStatus(s) {
  if (!s) return;
  S.status = s;
  Store.emit("status");
}

function setConn(text, cls) {
  const el = $("conn");
  el.textContent = text;
  el.className = "conn " + (cls || "");
  say(`Observation stream ${text}`, cls === "live" ? "ok" : "warn", { category: "system" });
}

// ---- visibility preview ------------------------------------------------------
//
// "View as" polls AdminService.GetPlayerView: the exact snapshot that player
// would receive, plus which of their units detects each foreign entity and
// with what scores. Players never have access to this.

let preview = null; // {entities: Map, visible: Set, detections: Map}
let previewTimer = null;

async function pollPreview() {
  if (!S.viewAs) return;
  try {
    const r = await api("/api/playerview", { playerId: S.viewAs });
    const ents = new Map(r.snapshot.entities.map((e) => [e.id, e]));
    const visible = new Set();
    for (const run of r.snapshot.visibleTiles || []) {
      for (let i = 0; i < (run.length || 0); i++) visible.add(`${(run.x || 0) + i},${run.y || 0}`);
    }
    const detections = new Map((r.detections || []).map((d) => [d.entityId, d]));
    if (preview) {
      const added = [...detections.keys()].filter((id) => !preview.detections.has(id));
      const lost = [...preview.detections.keys()].filter((id) => !detections.has(id));
      if (added.length || lost.length) {
        say(`${playerName(S.viewAs)} @${r.snapshot.tick}: ` + [added.length ? `+${added.map(tag).join(" ")}` : "", lost.length ? `−${lost.map(tag).join(" ")}` : ""].filter(Boolean).join("  "),
          "info", { category: "visibility", source: "view as", playerId: S.viewAs });
      }
    }
    preview = { entities: ents, visible, detections, fog: r.snapshot.fogOfWar };
    render();
  } catch (e) {
    say(`Preview: ${e.message}`, "error", { category: "visibility" });
  }
}

function setViewAs(id) {
  S.viewAs = Number(id) || 0;
  $("view-as").value = S.viewAs || "";
  preview = null;
  clearInterval(previewTimer);
  if (S.viewAs) {
    pollPreview();
    previewTimer = setInterval(pollPreview, 500);
  }
  inspectedKey = "";
  Store.emit("viewas");
  render();
}

function renderPlayerOptions() {
  const sel = $("spawn-owner");
  const va = $("view-as");
  while (va.options.length > 1) va.remove(1);
  sel.replaceChildren();
  for (const p of S.players) {
    va.append(new Option(p.name, p.id));
    sel.append(new Option(p.name, p.id));
  }
  sel.append(new Option("unowned", 0));
  va.value = S.viewAs || "";
}

// ---- observation ------------------------------------------------------------

function handleMessage(msg) {
  S.tick = applyMessage(S.entities, msg);
  let changed = false;
  for (const id of [...S.selected]) if (!S.entities.has(id)) { S.selected.delete(id); changed = true; }
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
    for (const ev of msg.update.events) logEvent(ev);
  }
  if (msg.snapshot) say(`Snapshot at tick ${S.tick}: ${S.entities.size} entities`, "debug", { category: "system" });
  Store.emit("tick");
  if (changed) Store.emit("selection");
}

// logEvent records a simulation event in the logs (per-tick work reports
// feed the automation panel instead).
function logEvent(ev) {
  if (ev.type === "automation.work" || ev.type === "logistics.work") { work[ev.type] = { tick: S.tick, fields: ev.fields || {} }; return; }
  if (ev.type === "automation.state" || ev.type === "logistics.status") return; // shown in the automation panel
  const { severity, category, source } = AdminLogic.classifyEvent(ev.type);
  const fields = ev.fields || {};
  const text = Object.entries(fields).filter(([k]) => k !== "command_id").map(([k, v]) => `${k}=${fmtValue(v)}`).join(" ");
  const entityId = ev.entityId && ev.entityId !== "0" ? ev.entityId : "";
  const e = entityId && S.entities.get(entityId);
  let playerId = e ? e.ownerId : 0;
  if (!playerId && fields.player) playerId = Number(fmtValue(fields.player)) || 0;
  Logs.add({ severity, category, source, message: `${ev.type} ${entityId ? tag(entityId) : ""} ${text}`.replace(/\s+/g, " ").trim(), playerId, entityId });
}

function reportResult(r, desc) {
  if (r.status === "COMMAND_STATUS_FAILED") say(`${desc} failed (${r.errorCode}): ${r.errorMessage}`, "error", { category: "command" });
  else say(`${desc}: done (tick ${r.tick})`, "ok", { category: "command" });
}

// ---- tools ------------------------------------------------------------------

const tool = () => $("tool-group").querySelector('input[name="tool"]:checked').value;
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
        if (shift) Store.toggle(e.id);
        else Store.select([e.id]);
      } else if (!shift) Store.select([], { tile: t });
  }
}

function reposition(t) {
  if (S.selected.size !== 1) return say("Select exactly one entity to reposition", "error");
  const id = [...S.selected][0];
  mutate("/api/move", { entityId: id, position: t }, `Move ${tag(id)} to ${fmtPos(t)}`);
}

function deleteSelected() {
  for (const id of S.selected) mutate("/api/delete", { entityId: id }, `Delete ${tag(id)}`);
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

function button(text, fn, cls) {
  const b = document.createElement("button");
  b.type = "button";
  b.textContent = text;
  if (cls) b.className = cls;
  b.addEventListener("click", fn);
  return b;
}

function sectionTable() {
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
  return { table, row, group };
}

// renderTile shows an empty tile (no entity on it).
function renderTile(box, t) {
  const { table, row, group } = sectionTable();
  group("Tile");
  row("position", fmtPos(t));
  row("contents", "empty (passable ground)");
  if (S.viewAs && preview) row(`visible to ${playerName(S.viewAs)}`, preview.fog ? (preview.visible.has(`${t.x},${t.y}`) ? "yes" : "no") : "yes (no fog)");
  box.append(table, hint("Use the Tools panel to spawn a unit, create a well or a wreck here."));
}

function hint(text) {
  const p = document.createElement("p");
  p.className = "hint";
  p.textContent = text;
  return p;
}

function renderInspector() {
  const box = $("inspector");
  box.replaceChildren();
  const ids = [...S.selected];
  if (ids.length === 0) {
    if (S.tile) return renderTile(box, S.tile);
    box.append(hint("Click an entity or tile on the board to inspect it. Drag to select several."));
    return;
  }
  if (ids.length > 1) {
    const { table, row, group } = sectionTable();
    group(`${ids.length} entities selected`);
    const kinds = {};
    for (const id of ids) { const e = S.entities.get(id); if (e) kinds[AdminLogic.entityKind(e)] = (kinds[AdminLogic.entityKind(e)] || 0) + 1; }
    for (const [k, n] of Object.entries(kinds)) row(k, n);
    box.append(table, button("Delete all", deleteSelected, "danger"));
    return;
  }
  const e = S.entities.get(ids[0]);
  if (!e) return;
  const { table, row, group } = sectionTable();
  if (S.viewAs) {
    group(`As seen by ${playerName(S.viewAs)}`);
    const d = preview && preview.detections.get(e.id);
    if (Number(e.ownerId) === S.viewAs) row("visibility", "own unit (full information)");
    else if (!d) row("visibility", "not detected");
    else {
      row("level", (d.level || "").replace("IDENTIFICATION_LEVEL_", "").toLowerCase());
      row("detected by", tag(d.observerId));
      row("detection score", d.detectionScore || 0);
      row("identification score", d.identificationScore || 0);
      const seen = preview.entities.get(e.id);
      if (seen) row("disclosed", Object.keys(seen.attributes || {}).sort().map((k) => `${k}=${fmtValue(seen.attributes[k])}`).join(", ") || "position/owner/kind only");
    }
  }
  group("Identity and ownership");
  row("id", `${e.id} (${tag(e.id)})`);
  row("kind", AdminLogic.entityKind(e));
  row("owner", e.ownerId ? `${playerName(e.ownerId)} (${e.ownerId})` : "none");
  row("passable", e.passable ? "yes" : "no");
  group("Position");
  row("tile", fmtPos(e.position));
  const defs = (S.rules.attributes && S.rules.attributes.attributes) || [];
  const derived = (S.rules.attributes && S.rules.attributes.derived) || {};
  for (const sec of AdminLogic.inspectorSections(e)) {
    group(sec.name);
    for (const k of sec.keys) {
      const name = k.slice(k.indexOf(".") + 1);
      let v = fmtValue(e.attributes[k]);
      let title = k;
      const d = k.startsWith("attributes.") && defs.find((x) => x.name === name);
      if (d) {
        const cur = Number(e.attributes[k].intValue);
        const next = cur < d.max ? d.cost_table[cur + 1 - d.min] - d.cost_table[cur - d.min] : null;
        v += `  (${d.min}–${d.max}${next !== null ? `, +1 costs ${next}` : ", max"})`;
        title = `${k}\nmass: ${d.mass}\ncost(v): ${d.cost}\nduration(v): ${d.duration}`;
      } else if (k.startsWith("attributes.") && derived[name]) {
        title = `${k}\nderived: ${derived[name]}`;
      }
      // Entity references become readable tags.
      if (/(_well|transfer_to|\.target|\.to|\.receiver|\.well)$/.test(k) && /^\d+$/.test(v)) v = `${v} (${tag(v)})`;
      row(name, v, title);
    }
  }
  box.append(table);
  renderEditors(box, e);
}

// renderEditors adds the admin edit forms this entity supports.
function renderEditors(box, e) {
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
    button("Reposition…", () => { $("tool-group").querySelector('input[value="move"]').checked = true; say("Click a tile (or right-click) to reposition", "info"); }),
    button("Delete", deleteSelected, "danger"),
  );
  // Automation and supply changes are scheduled commands like any other,
  // so they are recorded and replayable.
  if (sval(e, "automation.behavior")) {
    actions.append(button("Cancel automation", () => mutate("/api/command", { payload: {
      "@type": "type.googleapis.com/rts.automation.v1.CancelAutomationCommand", unitIds: [e.id] } }, `Cancel automation of ${tag(e.id)}`)));
  }
  if (supplyLinesOf(e).length) {
    actions.append(button("Remove supply links", () => mutate("/api/command", { payload: {
      "@type": "type.googleapis.com/rts.logistics.v1.CancelSupplyCommand", unitIds: [e.id] } }, `Remove supply links of ${tag(e.id)}`)));
  }
  box.append(actions);
}

// Re-render the inspector only when the inspected data changed, so form
// inputs being edited are not clobbered every tick.
let inspectedKey = "";
function refreshInspector() {
  const key = JSON.stringify([[...S.selected].map((id) => S.entities.get(id)), S.tile, S.viewAs, S.viewAs && preview ? [...S.selected].map((id) => preview.detections.get(id)) : null]);
  const editing = document.activeElement && document.activeElement.id && document.activeElement.id.startsWith("cw-");
  if (key !== inspectedKey && !editing) {
    inspectedKey = key;
    renderInspector();
  }
}

// ---- automation & logistics inspection ------------------------------------------

const work = {}; // event type -> {tick, fields} of the latest per-tick work report
const sval = (e, k) => { const v = e.attributes && e.attributes[k]; return v && v.stringValue; };

function supplyLinesOf(e) {
  const out = [];
  for (let i = 0; i < 16; i++) {
    const to = sval(e, `logistics.supply.${i}.to`);
    if (!to) break;
    out.push({ to, status: sval(e, `logistics.supply.${i}.status`) });
  }
  return out;
}

function renderAutomation() {
  const byState = {}, blocked = [];
  let links = 0, flowing = 0;
  for (const e of S.entities.values()) {
    const b = sval(e, "automation.behavior");
    if (b) {
      const k = `${b}/${sval(e, "automation.state")}`;
      byState[k] = (byState[k] || 0) + 1;
      if (sval(e, "automation.status") === "blocked") blocked.push(e);
    }
    for (const l of supplyLinesOf(e)) { links++; if (l.status === "supplying") flowing++; }
  }
  const box = $("automation");
  box.replaceChildren();
  const num = (w, k) => (w && w.fields[k] ? Number(w.fields[k].intValue || 0) : 0);
  const aw = work["automation.work"], lw = work["logistics.work"];
  box.append(hint(`Automation work (tick ${aw ? aw.tick : "–"}): ${num(aw, "evaluated")} evaluated, ${num(aw, "deferred")} deferred by the per-tick cap`));
  box.append(hint(`Logistics (tick ${lw ? lw.tick : "–"}): ${num(lw, "links")} active transfers moved ${num(lw, "moved")} energy; ${links} links, ${flowing} flowing`));
  const t = document.createElement("table");
  for (const [k, n] of Object.entries(byState).sort()) {
    const tr = t.insertRow();
    tr.insertCell().textContent = k;
    tr.insertCell().textContent = n;
  }
  if (t.rows.length) box.append(t); else box.append(hint("No automated units."));
  if (blocked.length) {
    box.append(hint(`Blocked (${blocked.length}):`));
    const ul = document.createElement("ul");
    ul.className = "blocked";
    for (const e of blocked.slice(0, 15)) {
      const li = document.createElement("li");
      li.append(button(tag(e.id), () => Store.select([e.id]), "link"),
        ` ${playerName(e.ownerId)} ${sval(e, "automation.behavior")} — ${sval(e, "automation.reason")}`);
      ul.append(li);
    }
    box.append(ul);
  }
}

// ---- board rendering ------------------------------------------------------------

// highlightFor returns the board's per-unit emphasis: with a focused
// player, their units are emphasized and other players' units dimmed.
function highlightFor() {
  if (!S.focusPlayer) return null;
  return (e) => (Number(e.ownerId) === S.focusPlayer ? true : e.ownerId ? false : undefined);
}

function render() {
  const lines = [];
  // The board filter shows only one player's units (plus unowned entities).
  const shown = S.filterPlayer
    ? new Map([...S.entities].filter(([, e]) => !isUnit(e) || Number(e.ownerId) === S.filterPlayer))
    : S.entities;
  for (const e of shown.values()) {
    const d = attrPos(e, "movement.destination");
    if (d) lines.push({ from: e.position, to: d, cross: true, color: "rgba(255,255,255,0.25)" });
    for (const l of supplyLinesOf(e)) {
      const r = S.entities.get(l.to);
      if (r) lines.push({ from: e.position, to: r.position, color: l.status === "supplying" ? "rgba(80,220,230,0.9)" : "rgba(80,220,230,0.3)" });
    }
    const target = sval(e, "automation.target"), te = target && S.entities.get(target);
    if (te) lines.push({ from: e.position, to: te.position, color: "rgba(255,160,60,0.6)" });
  }
  const highlight = highlightFor();
  if (S.viewAs && preview) {
    // The selected player's view: their entities and what they detect,
    // with tiles they cannot see darkened, and detection lines.
    const plines = [];
    for (const id of S.selected) {
      const d = preview.detections.get(id), obs = d && S.entities.get(d.observerId), t = S.entities.get(id);
      if (obs && t) plines.push({ from: obs.position, to: t.position, color: "rgba(120,180,255,0.8)" });
    }
    board.draw({
      entities: preview.entities, selected: S.selected, lines: plines, colorFor, highlight,
      mine: (e) => isUnit(e) && Number(e.ownerId) === S.viewAs,
      fog: preview.fog ? { visible: preview.visible, explored: new Set() } : null,
    });
  } else {
    board.draw({ entities: shown, selected: S.selected, lines, colorFor, mine: isUnit, highlight });
  }
  const f = $("board-filter");
  f.hidden = !S.filterPlayer;
  if (S.filterPlayer) f.textContent = `Showing only ${playerName(S.filterPlayer)}'s units`;
}

// Panels that summarize the whole world refresh at most a few times per
// second; the board and the inspector follow every tick.
let slowAt = 0;
Store.on("tick", () => {
  $("entity-count").textContent = S.entities.size;
  $("tick").textContent = S.tick;
  render();
  refreshInspector();
  const now = performance.now();
  if (now - slowAt > 250) {
    slowAt = now;
    renderAutomation();
  }
});
Store.on("selection", () => { render(); refreshInspector(); });
Store.on("player", render);

// ---- simulation controls ------------------------------------------------------------

const MATCH = "type.googleapis.com/rts.match.v1.";
const stName = (s) => (s || "").replace("MATCH_STATE_", "").toLowerCase();

// renderControls shows the authoritative simulation and match state and
// enables only the actions valid in it (disabled buttons say why).
function renderControls() {
  const s = S.status;
  if (s) {
    const el = $("sim-status");
    el.textContent = s.paused ? "paused" : `running @ ${s.tickRateHz} Hz`;
    el.className = "status " + (s.paused ? "paused" : "running");
    $("pause-btn").textContent = s.paused ? "Resume" : "Pause";
    $("step-btn").disabled = !s.paused;
    $("step-btn").title = s.paused ? "Advance the paused simulation by the given number of ticks" : "Pause the simulation to step it";
  }
  const m = (S.details && S.details.match) || {};
  const st = stName(m.state);
  const badge = $("match-state");
  badge.textContent = st || "–";
  badge.className = "badge match-" + st;
  const why = {
    "m-start": [st === "ready", st === "ready" ? "Start the match now (skips the ready check)" : `Only a match waiting to start can be started (it is ${st || "unknown"})`],
    "m-pause": [st === "running", st === "running" ? "Pause the match clock (players' commands are refused)" : "Only a running match can be paused"],
    "m-resume": [st === "paused", st === "paused" ? "Resume the paused match" : "Only a paused match can be resumed"],
    "m-restart": [!!S.details, "Start a new match with the current configuration (the current one is discarded; its replay stays exportable)"],
  };
  for (const [id, [ok, title]] of Object.entries(why)) {
    $(id).disabled = !ok;
    $(id).title = title;
  }
  const notes = [];
  if (s && s.paused && st === "running") notes.push("Simulation clock paused: the match does not advance.");
  if (st === "paused") notes.push("Match paused by an administrator.");
  if (st === "finished") notes.push("Match finished: restart or reset to play again.");
  $("ctl-note").textContent = notes.join(" ");
}
Store.on("status", renderControls);
Store.on("match", renderControls);

function lifecycle(name, body, description) {
  mutate("/api/command", { payload: { "@type": MATCH + name, ...body } }, description);
}

$("pause-btn").addEventListener("click", async () => {
  const want = !(S.status && S.status.paused);
  try {
    await api("/api/pause", { paused: want });
    say(want ? "Simulation paused" : "Simulation resumed", "info", { category: "control" });
  } catch (e) { say(e.message, "error", { category: "control" }); }
  refreshStatus(); // show what the server reports, not what we asked for
});
$("step-btn").addEventListener("click", async () => {
  try {
    const r = await api("/api/step", { count: Number($("step-count").value) || 1 });
    say(`Stepping ${r.count} tick(s) from tick ${r.firstTick}`, "info", { category: "control" });
  } catch (e) { say(`Step: ${e.message}`, "error", { category: "control" }); }
});
$("m-start").addEventListener("click", () => lifecycle("StartMatchCommand", {}, "Start match"));
$("m-pause").addEventListener("click", () => lifecycle("PauseMatchCommand", {}, "Pause match"));
$("m-resume").addEventListener("click", () => lifecycle("ResumeMatchCommand", {}, "Resume match"));
$("m-restart").addEventListener("click", async () => {
  if (!confirm("Start a new match with the current configuration? The current world is discarded (its replay stays exportable).")) return;
  try {
    const r = await api("/api/reset", {});
    say(`New match ${r.matchId.slice(0, 8)} with seed ${r.seed}`, "ok", { category: "match" });
    if (typeof refreshMatch === "function") refreshMatch();
  } catch (e) {
    say(`Restart failed (the current match continues): ${e.message}`, "error", { category: "match" });
  }
});
$("board-fit").addEventListener("click", () => board.fit());
$("logout-btn").addEventListener("click", () => logout(""));
$("rules-btn").addEventListener("click", () => { showRules(); Workspace.show("rules"); });
$("view-as").addEventListener("change", (ev) => setViewAs(ev.target.value));

// showRules renders every plugin's published rules: attribute definitions
// and formulas as a table, everything else as formatted JSON.
function showRules() {
  const box = $("rules-content");
  box.replaceChildren();
  const h = (text) => { const e = document.createElement("h3"); e.textContent = text; box.append(e); };
  const pre = (obj) => { const e = document.createElement("pre"); e.textContent = typeof obj === "string" ? obj : JSON.stringify(obj, null, 2); box.append(e); };
  if (!S.world) return;
  h("Tick schedule");
  pre((S.world.schedule || []).join("\n") + `\n\nunit command budget per player per tick: ${S.world.unitCommandBudget || "unlimited"}`);
  const a = S.rules.attributes;
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
  for (const name of Object.keys(S.rules).sort()) {
    if (name === "attributes") continue;
    h(name);
    pre(S.rules[name]);
  }
}
Store.on("world", showRules);

$("login-form").addEventListener("submit", (ev) => { ev.preventDefault(); login($("admin-token").value); });
window.addEventListener("keydown", (ev) => {
  if (!token || ["INPUT", "SELECT", "TEXTAREA"].includes(ev.target.tagName) || ev.target.isContentEditable) return;
  if (ev.key === "Escape") Store.select([]);
  if (ev.key === "Delete" || ev.key === "Backspace") deleteSelected();
  if (ev.key === " ") { ev.preventDefault(); $("pause-btn").click(); }
  if (ev.key === "f" || ev.key === "F") board.fit();
});

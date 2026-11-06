// Player client. Talks only to the gateway's public player API (/api/*),
// which mirrors the gRPC GameService in protojson form.
"use strict";

const { Board, stream, applyMessage, attr, attrInt, attrBool, attrPos, isUnit, isWell, tag, fmtPos, log } = RTS;
const MOVE_TYPE = "type.googleapis.com/rts.movement.v1.MoveCommand";
const STOP_TYPE = "type.googleapis.com/rts.movement.v1.StopCommand";
const MINE_TYPE = "type.googleapis.com/rts.energy.v1.MineCommand";
const STOP_MINING_TYPE = "type.googleapis.com/rts.energy.v1.StopMiningCommand";
const TRANSFER_TYPE = "type.googleapis.com/rts.energy.v1.TransferCommand";

// Own units always use the first colour; other owners get the next free
// colour in the order this client first sees them. Player IDs are opaque
// random numbers, so they are never used to pick colours or labels.
const PALETTE = ["#4a7fe0", "#e05252", "#43b36b", "#e0a43a", "#a46be0", "#3abfc4", "#d96fb0"];
const UNOWNED_COLOR = "#9aa0a6";

const $ = (id) => document.getElementById(id);

// ---- state --------------------------------------------------------------

let session = null; // {token, playerId, playerName, world}
let tick = 0;
const entities = new Map(); // id (string) -> Entity
const selected = new Set();
const pendingCommands = new Map(); // command id -> description
let owners = new Map(); // owner id -> {color, label}
let stopStream = null;
// Short-lived floating labels for energy changes reported by events.
let floaters = []; // {x, y, text, color, born}
const FLOAT_MS = 1200;

const board = new Board($("canvas"), $("board"), {
  onClick: clickTile,
  onBox: boxSelect,
  onContext: (t) => commandAt(t),
  onHover: showHover,
});

function ownerInfo(owner) {
  if (!owner) return { color: UNOWNED_COLOR, label: "Unowned" };
  let info = owners.get(owner);
  if (!info) {
    const mine = session && owner === session.playerId;
    const n = [...owners.keys()].filter((o) => !session || o !== session.playerId).length;
    info = mine
      ? { color: PALETTE[0], label: `${session.playerName || "You"} — you` }
      : { color: PALETTE[1 + (n % (PALETTE.length - 1))], label: `Opponent ${n + 1}` };
    owners.set(owner, info);
    renderLegend();
  }
  return info;
}

const isMine = (e) => isUnit(e) && session && e.ownerId === session.playerId;

// ---- API ----------------------------------------------------------------

async function api(path, { method = "GET", body } = {}) {
  const headers = { "Content-Type": "application/json" };
  if (session) headers.Authorization = "Bearer " + session.token;
  const res = await fetch(path, { method, headers, body: body && JSON.stringify(body) });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.error || res.statusText);
    err.status = res.status;
    throw err;
  }
  return data;
}

function newCommandId() {
  if (crypto.randomUUID) return crypto.randomUUID();
  return Date.now().toString(36) + "-" + Math.random().toString(36).slice(2);
}

async function submit(payload, description) {
  const commandId = newCommandId();
  pendingCommands.set(commandId, description);
  try {
    const ack = await api("/api/command", { method: "POST", body: { commandId, payload } });
    if (!ack.accepted) {
      pendingCommands.delete(commandId);
      say(`${description}: rejected — ${ack.rejectionReason}`, "error");
    }
  } catch (e) {
    pendingCommands.delete(commandId);
    say(`${description}: ${e.message}`, "error");
    if (e.status === 401) leave("Session expired");
  }
}

// ---- session ------------------------------------------------------------

function saveSession() {
  try { sessionStorage.setItem("rts.session", JSON.stringify(session)); } catch (_) {}
}

function loadSession() {
  try { return JSON.parse(sessionStorage.getItem("rts.session")); } catch (_) { return null; }
}

async function join(token) {
  $("join-error").textContent = "";
  try {
    const resp = await api("/api/connect", { method: "POST", body: { joinToken: token } });
    session = { token: resp.sessionToken, playerId: resp.playerId, playerName: resp.playerName, world: resp.world };
    saveSession();
    startGame();
  } catch (e) {
    $("join-error").textContent = e.status === 401 ? "Unknown join token." : "Could not connect: " + e.message;
  }
}

function leave(reason) {
  if (stopStream) stopStream();
  stopStream = null;
  session = null;
  try { sessionStorage.removeItem("rts.session"); } catch (_) {}
  entities.clear();
  owners = new Map();
  selected.clear();
  $("game").hidden = true;
  $("join").hidden = false;
  $("join-error").textContent = reason || "";
}

function startGame() {
  $("join").hidden = true;
  $("game").hidden = false;
  owners = new Map();
  const badge = $("player-badge");
  badge.textContent = session.playerName || "You";
  badge.style.background = ownerInfo(session.playerId).color;
  board.setWorld(session.world.width, session.world.height);
  if (typeof loadRules === "function") loadRules();
  stopStream = stream({
    url: "/api/observe",
    token: session.token,
    onMessage: handleMessage,
    onStatus: (s) => setConn(s === "live" ? "live" : "reconnecting…", s === "live" ? "live" : "down"),
    onUnauthorized: () => leave("Session no longer valid. Join again."),
  });
}

function setConn(text, cls) {
  const el = $("conn");
  el.textContent = text;
  el.className = "conn " + (cls || "");
}

// The legend lists only owners this client has actually observed; the
// server does not disclose how many players exist.
function renderLegend() {
  const legend = $("legend");
  legend.replaceChildren();
  for (const [owner, info] of owners) {
    const li = document.createElement("li");
    const sw = document.createElement("span");
    sw.className = "swatch";
    sw.style.background = info.color;
    if (!session || owner !== session.playerId) sw.style.borderRadius = "2px";
    li.append(sw, info.label);
    legend.append(li);
  }
}

// ---- observation --------------------------------------------------------

function handleMessage(msg) {
  if (typeof fogBefore === "function") fogBefore(msg);
  tick = applyMessage(entities, msg);
  for (const id of [...selected]) if (!entities.has(id)) selected.delete(id);
  if (msg.update) {
    for (const r of msg.update.commandResults) handleResult(r);
    for (const ev of msg.update.events) handleEvent(ev);
  }
  render();
}

function handleResult(r) {
  const desc = pendingCommands.get(r.commandId) || "command " + r.commandId.slice(0, 8);
  pendingCommands.delete(r.commandId);
  if (typeof evolutionResult === "function" && evolutionResult(r, desc)) return;
  if (r.status === "COMMAND_STATUS_FAILED") say(`${desc} failed (${r.errorCode}): ${r.errorMessage}`, "error");
}

// floatAt shows a short-lived label over an entity.
function floatAt(id, text, color) {
  const e = entities.get(id);
  if (e) floaters.push({ x: e.position.x, y: e.position.y, text, color, born: performance.now() });
}

const reasonText = {
  completed: "done",
  sender_empty: "sender ran out of energy",
  receiver_full: "receiver is full",
  not_adjacent: "units are no longer adjacent",
  target_lost: "receiver is gone",
  well_depleted: "well depleted",
  well_removed: "well removed",
};

function handleEvent(ev) {
  if (typeof evolutionEvent === "function" && evolutionEvent(ev)) return;
  if (typeof combatEvent === "function" && combatEvent(ev)) return;
  if (typeof autoEvent === "function" && autoEvent(ev)) return;
  const f = ev.fields || {};
  const int = (k) => Number((f[k] && f[k].intValue) || 0);
  const str = (k) => (f[k] && f[k].stringValue) || "";
  switch (ev.type) {
    case "energy.mined":
      floatAt(ev.entityId, "+" + int("amount"), "#f2d24b");
      break;
    case "energy.transferred":
      floatAt(ev.entityId, "−" + int("amount"), "#ff9b6b");
      floatAt(str("to"), "+" + int("amount"), "#7bd88f");
      break;
    case "energy.transfer_ended": {
      const r = str("reason");
      say(`Transfer ${tag(ev.entityId)} → ${tag(str("to"))}: ${reasonText[r] || r}` + (r === "completed" ? "" : ` (${int("remaining")} not sent)`), r === "completed" ? "info" : "error");
      break;
    }
    case "energy.mining_stopped":
      say(`Unit ${tag(ev.entityId)} stopped mining: ${reasonText[str("reason")] || str("reason")}`, "error");
      break;
    case "energy.well_depleted":
      say(`Well ${tag(ev.entityId)} is depleted`, "info");
      break;
    case "movement.stalled":
      say(`Unit ${tag(ev.entityId)} is out of energy (needs ${int("required")}, has ${int("stored")})`, "error");
      break;
    case "movement.cancelled":
      say(`Unit ${tag(ev.entityId)} gave up moving to ${fmtPos(f.destination && f.destination.positionValue)}: ${f.reason.stringValue}`, "error");
      break;
    case "movement.arrived":
      say(`Unit ${tag(ev.entityId)} arrived at ${fmtPos(f.position.positionValue)}`, "info");
      break;
  }
}

const say = (text, cls) => log($("log"), tick, text, cls);

// ---- commands -----------------------------------------------------------

// Formation: spread the selected units over the nearest free tiles around
// the target so a group does not fight over a single destination tile.
function formationOrders(ids, tx, ty) {
  const { width, height } = session.world;
  const sel = new Set(ids);
  const slots = [];
  for (let r = 0; slots.length < ids.length && r < Math.max(width, height); r++) {
    const ring = [];
    for (let y = ty - r; y <= ty + r; y++) {
      for (let x = tx - r; x <= tx + r; x++) {
        if (Math.max(Math.abs(x - tx), Math.abs(y - ty)) !== r) continue;
        if (x < 0 || y < 0 || x >= width || y >= height) continue;
        const occ = board.entityAt(x, y);
        if (occ && !sel.has(occ.id)) continue;
        ring.push({ x, y });
      }
    }
    ring.sort((a, b) => Math.hypot(a.x - tx, a.y - ty) - Math.hypot(b.x - tx, b.y - ty));
    slots.push(...ring);
  }
  const units = ids
    .map((id) => entities.get(id))
    .sort((a, b) => Math.hypot(a.position.x - tx, a.position.y - ty) - Math.hypot(b.position.x - tx, b.position.y - ty));
  return units.map((u, i) => {
    const s = slots[i] || { x: tx, y: ty };
    return { unitId: u.id, destination: { x: s.x, y: s.y } };
  });
}

const selectedIds = () => [...selected].filter((id) => entities.has(id));

function moveSelection(tx, ty) {
  const ids = selectedIds();
  if (!ids.length) return;
  submit({ "@type": MOVE_TYPE, orders: formationOrders(ids, tx, ty) }, `Move ${ids.length} unit(s) to (${tx},${ty})`);
}

function stopSelection() {
  const ids = selectedIds();
  if (!ids.length) return;
  submit({ "@type": STOP_TYPE, unitIds: ids }, `Stop ${ids.length} unit(s)`);
  const miners = ids.filter((id) => attr(entities.get(id), "energy.mining_well"));
  if (miners.length) submit({ "@type": STOP_MINING_TYPE, unitIds: miners }, `Stop mining (${miners.length})`);
  if (typeof combatStop === "function") combatStop(ids);
}

// mineWell sends the selection to the free tiles around a well and gives
// each unit a persistent mining order; units start extracting on arrival.
function mineWell(well) {
  const ids = selectedIds();
  if (!ids.length) return;
  const { x, y } = well.position;
  const orders = formationOrders(ids, x, y).filter((o) => {
    const u = entities.get(o.unitId);
    return !(u.position.x === o.destination.x && u.position.y === o.destination.y);
  });
  if (orders.length) submit({ "@type": MOVE_TYPE, orders }, `Move ${orders.length} unit(s) to well ${tag(well.id)}`);
  submit({ "@type": MINE_TYPE, unitIds: ids, wellId: well.id }, `Mine well ${tag(well.id)} with ${ids.length} unit(s)`);
}

const adjacent = (a, b) => Math.max(Math.abs(a.x - b.x), Math.abs(a.y - b.y)) === 1;

// transferTo sends energy from every selected unit adjacent to the target.
function transferTo(target) {
  const amount = Math.max(1, Math.floor(Number($("transfer-amount").value) || 0));
  const senders = selectedIds().filter((id) => id !== target.id);
  const near = senders.filter((id) => adjacent(entities.get(id).position, target.position));
  if (!near.length) {
    say(`Transfer: no selected unit is next to ${tag(target.id)}`, "error");
    return;
  }
  for (const id of near) {
    submit({ "@type": TRANSFER_TYPE, fromUnitId: id, toUnitId: target.id, amount: String(amount) },
      `Send ${amount} energy ${tag(id)} → ${tag(target.id)}`);
  }
}

// commandAt issues the context-appropriate order for the selection at a
// tile (right click, or left click on a non-selectable tile).
function commandAt(t) {
  const e = board.entityAt(t.x, t.y);
  if (typeof combatCommandAt === "function" && combatCommandAt(e)) return;
  if (e && isWell(e)) mineWell(e);
  else if (e && isMine(e)) transferTo(e);
  else if (!e) moveSelection(t.x, t.y);
}

// ---- input --------------------------------------------------------------

function clickTile(t, { shift }) {
  if (typeof autoPick === "function" && autoPick(t)) return;
  const e = board.entityAt(t.x, t.y);
  if (e && isMine(e)) {
    if (shift) selected.has(e.id) ? selected.delete(e.id) : selected.add(e.id);
    else { selected.clear(); selected.add(e.id); }
  } else if (selected.size && (!e || isWell(e) || (RTS.isWreck && RTS.isWreck(e)))) {
    commandAt(t);
  } else if (!shift) {
    selected.clear();
  }
  render();
}

function boxSelect(inside, { shift }) {
  if (!shift) selected.clear();
  for (const e of inside) if (isMine(e)) selected.add(e.id);
  render();
}

window.addEventListener("keydown", (ev) => {
  if (!session || ev.target.tagName === "INPUT") return;
  if (ev.key === "Escape") { selected.clear(); render(); }
  if (ev.key === "s" || ev.key === "S") stopSelection();
});

// ---- rendering ----------------------------------------------------------

function render() {
  $("tick").textContent = tick;
  let mine = 0;
  for (const e of entities.values()) if (isMine(e)) mine++;
  $("unit-count").textContent = mine;
  $("sel-count").textContent = selected.size;

  // Order lines: movement (white/yellow), mining (gold), transfer (green).
  const lines = [];
  const idAttr = (e, k) => { const v = attr(e, k); return v && v.stringValue; };
  for (const e of entities.values()) {
    const d = attrPos(e, "movement.destination");
    if (d) lines.push({ from: e.position, to: d, cross: true, color: selected.has(e.id) ? "rgba(255,216,77,0.7)" : "rgba(255,255,255,0.25)" });
    const well = entities.get(idAttr(e, "energy.mining_well"));
    if (well) lines.push({ from: e.position, to: well.position, color: "rgba(232,194,58,0.55)" });
    const to = entities.get(idAttr(e, "energy.transfer_to"));
    if (to) lines.push({ from: e.position, to: to.position, color: "rgba(123,216,143,0.8)" });
    if (typeof combatLines === "function") lines.push(...combatLines(e));
    if (typeof autoLines === "function") lines.push(...autoLines(e));
  }
  const now = performance.now();
  floaters = floaters.filter((f) => now - f.born < FLOAT_MS);
  const fl = floaters.map((f) => ({ ...f, alpha: 1 - (now - f.born) / FLOAT_MS }));
  const fog = typeof fogState === "function" ? fogState() : {};
  board.draw({ entities, selected, lines, floaters: fl, colorFor: (e) => ownerInfo(e.ownerId).color, mine: isMine, ...fog });
  renderSelection();
  if (typeof renderFogPanel === "function") renderFogPanel();
  if (typeof renderAutoStatus === "function") renderAutoStatus();
  if (typeof renderEvolution === "function") renderEvolution();
}

// Keep floating labels animating between ticks.
setInterval(() => { if (session && floaters.length) render(); }, 50);

function renderSelection() {
  const box = $("selection");
  const ids = selectedIds();
  if (!ids.length) {
    box.innerHTML = '<p class="hint">Nothing selected.</p>';
    return;
  }
  let sum = 0, cap = 0;
  const table = document.createElement("table");
  for (const id of ids) {
    const e = entities.get(id);
    const en = attrInt(e, "energy.stored") || 0, c = attrInt(e, "energy.capacity") || 0;
    sum += en;
    cap += c;
    if (table.rows.length >= 8) continue;
    const tr = table.insertRow();
    tr.insertCell().textContent = tag(id);
    const cell = tr.insertCell();
    const meter = document.createElement("span");
    meter.className = "meter";
    const fill = document.createElement("i");
    fill.style.width = (c ? (100 * en) / c : 0) + "%";
    meter.append(fill);
    cell.append(meter, `${en}/${c}`);
    const doing = [];
    if (attr(e, "movement.destination")) doing.push("moving");
    if (attr(e, "energy.mining_well")) doing.push("mining");
    if (attr(e, "energy.transfer_to")) doing.push(`sending ${attrInt(e, "energy.transfer_remaining")}`);
    tr.insertCell().textContent = doing.join(", ") || "idle";
  }
  const summary = document.createElement("p");
  summary.className = "hint";
  summary.textContent = `${ids.length} unit(s), energy ${sum}/${cap}` + (ids.length > 8 ? " (first 8 shown)" : "");
  box.replaceChildren(table, summary);
}

function showHover(t) {
  const el = $("hover-info");
  const e = t && board.entityAt(t.x, t.y);
  const extra = (t && typeof fogHover === "function" ? fogHover(t) : null) || (e && typeof combatHover === "function" ? combatHover(e) : null);
  if (!t) el.textContent = "–";
  else if (extra) el.textContent = extra;
  else if (!e) el.textContent = `Empty tile ${fmtPos(t)}`;
  else if (isWell(e)) {
    const unlimited = attrBool(e, "wells.unlimited");
    el.textContent = `Well ${tag(e.id)} ${fmtPos(e.position)}: ` +
      (unlimited ? "unlimited" : `${attrInt(e, "wells.energy")}/${attrInt(e, "wells.capacity")}`) +
      `, releases ${attrInt(e, "wells.extraction_rate")}/tick`;
  } else if (isMine(e)) {
    el.textContent = `Your unit ${tag(e.id)}: energy ${attrInt(e, "energy.stored")}/${attrInt(e, "energy.capacity")}`;
  } else {
    el.textContent = `${ownerInfo(e.ownerId).label} unit ${tag(e.id)} (energy unknown)`;
  }
}

$("stop-btn").addEventListener("click", stopSelection);
$("leave-btn").addEventListener("click", () => leave(""));
$("join-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  join($("join-token").value.trim());
});

// Resume an existing session in this tab (e.g. after a reload).
session = loadSession();
if (session) startGame();

// Minimal RTS browser client. Talks only to the gateway's public API
// (/api/*), which mirrors the gRPC GameService in protojson form.
"use strict";

const MOVE_TYPE = "type.googleapis.com/rts.movement.v1.MoveCommand";
const STOP_TYPE = "type.googleapis.com/rts.movement.v1.StopCommand";
// Own units always use the first colour; other owners get the next free
// colour in the order this client first sees them. Player IDs are opaque
// random numbers, so they are never used to pick colours or labels.
const PALETTE = ["#4a7fe0", "#e05252", "#43b36b", "#e0a43a", "#a46be0", "#3abfc4", "#d96fb0"];
const UNOWNED_COLOR = "#9aa0a6";
const DRAG_THRESHOLD = 4;

const $ = (id) => document.getElementById(id);

// owner id -> {color, label}, assigned in order of first sighting.
let owners = new Map();

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
const colorFor = (owner) => ownerInfo(owner).color;

// Entity IDs are large opaque numbers; show a short tag in messages.
const unitTag = (id) => "#" + BigInt(id).toString(16).slice(-5);

// ---- state --------------------------------------------------------------

let session = null; // {token, playerId, world}
let tick = 0;
let entities = new Map(); // id (string) -> Entity
const selected = new Set();
const pendingCommands = new Map(); // command id -> description
let streamAbort = null;
let tile = 24;
let drag = null; // {x0,y0,x1,y1,active,shift}
let hoverTile = null;

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
      log(`${description}: rejected — ${ack.rejectionReason}`, "error");
    }
  } catch (e) {
    pendingCommands.delete(commandId);
    log(`${description}: ${e.message}`, "error");
    if (e.status === 401) leave("Session expired");
  }
}

// ---- session & stream ---------------------------------------------------

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
  if (streamAbort) streamAbort.abort();
  streamAbort = null;
  session = null;
  try { sessionStorage.removeItem("rts.session"); } catch (_) {}
  entities = new Map();
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
  badge.style.background = colorFor(session.playerId);
  resize();
  observeLoop();
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

function setConn(text, cls) {
  const el = $("conn");
  el.textContent = text;
  el.className = "conn " + (cls || "");
}

// observeLoop keeps an Observe stream open, reconnecting on failure. Every
// (re)connection begins with a full snapshot, so state always recovers.
async function observeLoop() {
  let delay = 500;
  while (session) {
    const abort = new AbortController();
    streamAbort = abort;
    try {
      const res = await fetch("/api/observe", {
        headers: { Authorization: "Bearer " + session.token },
        signal: abort.signal,
      });
      if (res.status === 401) { leave("Session no longer valid. Join again."); return; }
      if (!res.ok) throw new Error("HTTP " + res.status);
      setConn("live", "live");
      delay = 500;
      await readLines(res.body, handleMessage);
      throw new Error("stream ended");
    } catch (e) {
      if (abort.signal.aborted || !session) return;
      setConn("reconnecting…", "down");
      await new Promise((r) => setTimeout(r, delay));
      delay = Math.min(delay * 2, 5000);
    }
  }
}

async function readLines(body, onLine) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    buf += decoder.decode(value, { stream: true });
    let nl;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, nl);
      buf = buf.slice(nl + 1);
      if (line) onLine(JSON.parse(line));
    }
  }
}

function handleMessage(msg) {
  if (msg.snapshot) {
    tick = Number(msg.snapshot.tick);
    entities = new Map(msg.snapshot.entities.map((e) => [e.id, e]));
    for (const id of [...selected]) if (!entities.has(id)) selected.delete(id);
  } else if (msg.update) {
    const u = msg.update;
    tick = Number(u.tick);
    for (const e of u.changed) entities.set(e.id, e);
    for (const id of u.removed) { entities.delete(id); selected.delete(id); }
    for (const r of u.commandResults) handleResult(r);
    for (const ev of u.events) handleEvent(ev);
  }
  updateHud();
  draw();
}

function handleResult(r) {
  const desc = pendingCommands.get(r.commandId) || "command " + r.commandId.slice(0, 8);
  pendingCommands.delete(r.commandId);
  if (r.status === "COMMAND_STATUS_FAILED") {
    log(`${desc} failed (${r.errorCode}): ${r.errorMessage}`, "error");
  }
}

function handleEvent(ev) {
  if (ev.type === "movement.cancelled") {
    const d = ev.fields.destination && ev.fields.destination.positionValue;
    log(`Unit ${unitTag(ev.entityId)} gave up moving to ${fmtPos(d)}: ${ev.fields.reason.stringValue}`, "error");
  } else if (ev.type === "movement.arrived") {
    log(`Unit ${unitTag(ev.entityId)} arrived at ${fmtPos(ev.fields.position.positionValue)}`, "info");
  }
}

// ---- helpers --------------------------------------------------------------

const fmtPos = (p) => (p ? `(${p.x},${p.y})` : "?");
const isMine = (e) => e.ownerId === session.playerId;

function unitAt(tx, ty) {
  for (const e of entities.values()) if (e.position.x === tx && e.position.y === ty) return e;
  return null;
}

function log(text, cls = "") {
  const li = document.createElement("li");
  li.className = cls;
  const t = document.createElement("span");
  t.className = "t";
  t.textContent = tick;
  li.append(t, text);
  const ol = $("log");
  ol.prepend(li);
  while (ol.children.length > 30) ol.lastChild.remove();
}

function updateHud() {
  $("tick").textContent = tick;
  let mine = 0;
  for (const e of entities.values()) if (isMine(e)) mine++;
  $("unit-count").textContent = mine;
  $("sel-count").textContent = selected.size;
}

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
        const occ = unitAt(x, y);
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
  const orders = [];
  units.forEach((u, i) => {
    const s = slots[i] || { x: tx, y: ty };
    orders.push({ unitId: u.id, destination: { x: s.x, y: s.y } });
  });
  return orders;
}

function moveSelection(tx, ty) {
  const ids = [...selected].filter((id) => entities.has(id));
  if (!ids.length) return;
  submit({ "@type": MOVE_TYPE, orders: formationOrders(ids, tx, ty) }, `Move ${ids.length} unit(s) to (${tx},${ty})`);
}

function stopSelection() {
  const ids = [...selected].filter((id) => entities.has(id));
  if (!ids.length) return;
  submit({ "@type": STOP_TYPE, unitIds: ids }, `Stop ${ids.length} unit(s)`);
}

// ---- rendering ----------------------------------------------------------

const canvas = $("canvas");
const ctx = canvas.getContext("2d");

function resize() {
  if (!session) return;
  const board = $("board");
  const { width, height } = session.world;
  const availW = board.clientWidth - 24;
  const availH = board.clientHeight - 24;
  tile = Math.max(6, Math.floor(Math.min(availW / width, availH / height)));
  const dpr = window.devicePixelRatio || 1;
  canvas.style.width = width * tile + "px";
  canvas.style.height = height * tile + "px";
  canvas.width = width * tile * dpr;
  canvas.height = height * tile * dpr;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  draw();
}

function draw() {
  if (!session) return;
  const { width, height } = session.world;
  const W = width * tile, H = height * tile;
  const css = getComputedStyle(document.documentElement);
  ctx.fillStyle = css.getPropertyValue("--tile");
  ctx.fillRect(0, 0, W, H);
  ctx.strokeStyle = css.getPropertyValue("--grid");
  ctx.lineWidth = 1;
  ctx.beginPath();
  for (let x = 0; x <= width; x++) { ctx.moveTo(x * tile + 0.5, 0); ctx.lineTo(x * tile + 0.5, H); }
  for (let y = 0; y <= height; y++) { ctx.moveTo(0, y * tile + 0.5); ctx.lineTo(W, y * tile + 0.5); }
  ctx.stroke();

  if (hoverTile) {
    ctx.fillStyle = "rgba(255,255,255,0.06)";
    ctx.fillRect(hoverTile.x * tile, hoverTile.y * tile, tile, tile);
  }

  // Destination lines for own units (only owners receive this attribute).
  ctx.setLineDash([4, 4]);
  ctx.lineWidth = 1;
  for (const e of entities.values()) {
    const d = e.attributes && e.attributes["movement.destination"];
    if (!d || !d.positionValue) continue;
    const dp = d.positionValue;
    ctx.strokeStyle = selected.has(e.id) ? "rgba(255,216,77,0.7)" : "rgba(255,255,255,0.25)";
    ctx.beginPath();
    ctx.moveTo((e.position.x + 0.5) * tile, (e.position.y + 0.5) * tile);
    ctx.lineTo(((dp.x || 0) + 0.5) * tile, ((dp.y || 0) + 0.5) * tile);
    ctx.stroke();
    const cx = ((dp.x || 0) + 0.5) * tile, cy = ((dp.y || 0) + 0.5) * tile, s = tile * 0.15;
    ctx.beginPath();
    ctx.moveTo(cx - s, cy - s); ctx.lineTo(cx + s, cy + s);
    ctx.moveTo(cx + s, cy - s); ctx.lineTo(cx - s, cy + s);
    ctx.stroke();
  }
  ctx.setLineDash([]);

  // Units: own units are circles, others squares; colour = owner.
  for (const e of entities.values()) {
    const cx = (e.position.x + 0.5) * tile, cy = (e.position.y + 0.5) * tile;
    const r = tile * 0.36;
    ctx.fillStyle = colorFor(e.ownerId);
    ctx.beginPath();
    if (isMine(e)) ctx.arc(cx, cy, r, 0, Math.PI * 2);
    else ctx.rect(cx - r * 0.9, cy - r * 0.9, r * 1.8, r * 1.8);
    ctx.fill();
    ctx.lineWidth = 1.5;
    ctx.strokeStyle = "rgba(0,0,0,0.5)";
    ctx.stroke();
    if (selected.has(e.id)) {
      ctx.lineWidth = 2;
      ctx.strokeStyle = css.getPropertyValue("--select");
      ctx.beginPath();
      ctx.arc(cx, cy, r + 3, 0, Math.PI * 2);
      ctx.stroke();
    }
  }

  if (drag && drag.active) {
    const x = Math.min(drag.x0, drag.x1), y = Math.min(drag.y0, drag.y1);
    const w = Math.abs(drag.x1 - drag.x0), h = Math.abs(drag.y1 - drag.y0);
    ctx.fillStyle = "rgba(255,216,77,0.08)";
    ctx.fillRect(x, y, w, h);
    ctx.strokeStyle = css.getPropertyValue("--select");
    ctx.lineWidth = 1;
    ctx.strokeRect(x + 0.5, y + 0.5, w, h);
  }
}

// ---- input ----------------------------------------------------------------

function canvasPoint(ev) {
  const r = canvas.getBoundingClientRect();
  return { x: ev.clientX - r.left, y: ev.clientY - r.top };
}

function toTile(p) {
  const { width, height } = session.world;
  const x = Math.floor(p.x / tile), y = Math.floor(p.y / tile);
  return x >= 0 && y >= 0 && x < width && y < height ? { x, y } : null;
}

canvas.addEventListener("pointerdown", (ev) => {
  if (ev.button !== 0 || !session) return;
  const p = canvasPoint(ev);
  drag = { x0: p.x, y0: p.y, x1: p.x, y1: p.y, active: false, shift: ev.shiftKey };
  canvas.setPointerCapture(ev.pointerId);
});

canvas.addEventListener("pointermove", (ev) => {
  if (!session) return;
  const p = canvasPoint(ev);
  hoverTile = toTile(p);
  if (drag) {
    drag.x1 = p.x;
    drag.y1 = p.y;
    if (Math.hypot(drag.x1 - drag.x0, drag.y1 - drag.y0) > DRAG_THRESHOLD) drag.active = true;
  }
  draw();
});

canvas.addEventListener("pointerleave", () => { hoverTile = null; draw(); });

canvas.addEventListener("pointerup", (ev) => {
  if (ev.button !== 0 || !drag || !session) return;
  const d = drag;
  drag = null;
  if (d.active) {
    // Box select: own units whose tile centre lies inside the box.
    const x0 = Math.min(d.x0, d.x1), x1 = Math.max(d.x0, d.x1);
    const y0 = Math.min(d.y0, d.y1), y1 = Math.max(d.y0, d.y1);
    if (!d.shift) selected.clear();
    for (const e of entities.values()) {
      if (!isMine(e)) continue;
      const cx = (e.position.x + 0.5) * tile, cy = (e.position.y + 0.5) * tile;
      if (cx >= x0 && cx <= x1 && cy >= y0 && cy <= y1) selected.add(e.id);
    }
  } else {
    const t = toTile(canvasPoint(ev));
    if (t) {
      const u = unitAt(t.x, t.y);
      if (u && isMine(u)) {
        if (d.shift) selected.has(u.id) ? selected.delete(u.id) : selected.add(u.id);
        else { selected.clear(); selected.add(u.id); }
      } else if (!u && selected.size) {
        moveSelection(t.x, t.y);
      } else if (!d.shift) {
        selected.clear();
      }
    }
  }
  updateHud();
  draw();
});

canvas.addEventListener("contextmenu", (ev) => {
  ev.preventDefault();
  if (!session) return;
  const t = toTile(canvasPoint(ev));
  if (t) moveSelection(t.x, t.y);
});

window.addEventListener("keydown", (ev) => {
  if (!session || ev.target.tagName === "INPUT") return;
  if (ev.key === "Escape") { selected.clear(); updateHud(); draw(); }
  if (ev.key === "s" || ev.key === "S") stopSelection();
});

window.addEventListener("resize", resize);
$("stop-btn").addEventListener("click", stopSelection);
$("leave-btn").addEventListener("click", () => leave(""));
$("join-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  join($("join-token").value.trim());
});

// Resume an existing session in this tab (e.g. after a reload).
session = loadSession();
if (session) startGame();

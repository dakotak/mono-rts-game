// Milestone 5 player UI: fog of war, exploration memory and sensors.
//
// The server tells this client only what it currently observes: visible
// tiles and the foreign entities it currently detects (filtered to what its
// identification level allows). Everything else here is the client's own
// memory: tiles it has ever seen ("explored") and the last-known state of
// foreign entities it no longer observes ("ghosts"). Ghosts are always drawn
// as stale outlines with the tick they were last seen, never as live units.
"use strict";

let fogOn = false;
const visibleTiles = new Set(); // "x,y" currently visible (server)
let explored = new Set();       // "x,y" ever seen (client memory)
const ghosts = new Map();       // id -> {entity, seenTick}
let showRanges = false;

const key = (p) => `${p.x || 0},${p.y || 0}`;
const exploredStore = () => "rts.explored." + (session ? session.playerId : "");

function loadExplored() {
  try { explored = new Set(JSON.parse(sessionStorage.getItem(exploredStore()) || "[]")); } catch (_) { explored = new Set(); }
}
let saveTimer = null;
function saveExplored() {
  if (saveTimer) return;
  saveTimer = setTimeout(() => {
    saveTimer = null;
    try { sessionStorage.setItem(exploredStore(), JSON.stringify([...explored])); } catch (_) {}
  }, 2000);
}

// fogBefore runs before a message is applied, while the entity map still
// holds what is about to disappear.
function fogBefore(msg) {
  if (msg.snapshot) {
    if (!explored.size) loadExplored();
    fogOn = !!msg.snapshot.fogOfWar;
    const now = new Set(msg.snapshot.entities.map((e) => e.id));
    if (fogOn) {
      for (const [id, e] of entities) if (!isMine(e) && !now.has(id)) ghosts.set(id, { entity: e, seenTick: tick });
    }
    for (const id of now) ghosts.delete(id);
    visibleTiles.clear();
    for (const r of msg.snapshot.visibleTiles || []) {
      for (let i = 0; i < (r.length || 0); i++) visibleTiles.add(`${(r.x || 0) + i},${r.y || 0}`);
    }
  } else {
    const u = msg.update;
    if (fogOn) {
      for (const id of u.removed) {
        const e = entities.get(id);
        if (e && !isMine(e)) ghosts.set(id, { entity: e, seenTick: tick });
      }
    }
    for (const e of u.changed) ghosts.delete(e.id);
    for (const p of u.tilesEntered || []) visibleTiles.add(key(p));
    for (const p of u.tilesExited || []) visibleTiles.delete(key(p));
  }
  if (fogOn) {
    let grew = false;
    for (const k of visibleTiles) if (!explored.has(k)) { explored.add(k); grew = true; }
    if (grew) saveExplored();
    // A ghost whose tile is in sight again but is not detected there is
    // out of date: forget it (it moved, left, or was destroyed — or is
    // hiding from our sensors; we cannot tell).
    for (const [id, g] of ghosts) if (visibleTiles.has(key(g.entity.position))) ghosts.delete(id);
  }
}

// fogState is merged into the board state.
function fogState() {
  if (!fogOn) return {};
  const metric = ((rules.visibility || {}).config || {}).distance_metric || "chebyshev";
  const ranges = [];
  if (showRanges) {
    for (const e of entities.values()) {
      const r = attrInt(e, "visibility.vision");
      if (isMine(e) && r !== undefined) ranges.push({ x: e.position.x, y: e.position.y, r, metric });
    }
  }
  return { fog: { visible: visibleTiles, explored }, ghosts: [...ghosts.values()], ranges };
}

const levelName = (l) => ({
  IDENTIFICATION_LEVEL_BASIC: "basic",
  IDENTIFICATION_LEVEL_ESTIMATED: "estimated",
  IDENTIFICATION_LEVEL_DETAILED: "detailed",
})[l] || "";

// fogHover describes foreign entities and ghosts under the cursor, from
// exactly what the server disclosed.
function fogHover(t) {
  const e = board.entityAt(t.x, t.y);
  if (e && !isMine(e) && e.identification) {
    const parts = Object.keys(e.attributes || {}).sort().map((k) => `${k.split(".").pop()} ${RTS.fmtValue(e.attributes[k])}`);
    const who = isUnit(e) ? `${ownerInfo(e.ownerId).label} unit` : isWell(e) ? "Well" : RTS.isWreck(e) ? "Wreckage" : "Entity";
    return `${who} ${tag(e.id)} — identified: ${levelName(e.identification)}` + (parts.length ? ` — ${parts.join(", ")}` : " (no details)");
  }
  if (!e && fogOn) {
    for (const g of ghosts.values()) {
      if (g.entity.position.x === t.x && g.entity.position.y === t.y) {
        return `Last seen at tick ${g.seenTick} (${tick - g.seenTick} ticks ago): ${isUnit(g.entity) ? ownerInfo(g.entity.ownerId).label + " unit" : "resource"} ${tag(g.entity.id)} — may have moved`;
      }
    }
    const k = `${t.x},${t.y}`;
    if (!explored.has(k)) return `Unexplored ${fmtPos(t)}`;
    if (!visibleTiles.has(k)) return `Explored ${fmtPos(t)} — not currently visible`;
  }
  return null;
}

// fogDetail adds a sensors section to the unit detail panel.
function fogDetail(e, box) {
  const v = attrInt(e, "visibility.vision");
  if (v === undefined) return;
  const t = el("table");
  for (const [k, label] of [["vision", "vision range"], ["sensor", "sensor strength"], ["signature", "signature (stealth)"], ["identification", "identification"]]) {
    const tr = t.insertRow();
    tr.insertCell().textContent = label;
    const c = tr.insertCell();
    c.className = "num";
    c.textContent = attrInt(e, "visibility." + k);
  }
  box.append(el("h3", { textContent: "Sensors" }), t);
}

function renderFogPanel() {
  const n = (s) => s.size;
  let detected = 0;
  for (const e of entities.values()) if (!isMine(e) && e.identification) detected++;
  $("fog-info").textContent = fogOn
    ? `${n(visibleTiles)} tiles visible, ${n(explored)} explored, ${detected} foreign entities detected, ${ghosts.size} last-known`
    : "Fog of war is off on this server: everything is visible.";
}

$("show-ranges").addEventListener("change", (ev) => { showRanges = ev.target.checked; render(); });

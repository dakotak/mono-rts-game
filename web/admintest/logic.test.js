// Unit tests for the admin workspace's pure logic (web/static/admin/logic.js).
// Run with `node --test web/admintest/` or `go test ./web` (which runs them
// when Node.js is installed).
"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const L = require("../static/admin/logic.js");

const iv = (v) => ({ intValue: String(v) });
const sv = (v) => ({ stringValue: v });
const unit = (id, owner, attrs = {}, pos = { x: 1, y: 1 }) => ({ id, kind: "ENTITY_KIND_UNIT", ownerId: owner, position: pos, attributes: attrs });
const PANELS = ["board", "controls", "players", "inspector", "logs", "tools", "match", "newmatch", "replays", "automation", "rules", "heatmap"];

test("log buffer keeps the newest entries within its capacity", () => {
  const b = new L.LogBuffer(3);
  for (let i = 1; i <= 5; i++) b.push({ message: "m" + i });
  assert.equal(b.size, 3);
  assert.equal(b.dropped, 2);
  assert.deepEqual(b.entries().map((e) => e.message), ["m3", "m4", "m5"]);
  assert.deepEqual(b.entries().map((e) => e.seq), [3, 4, 5]);
  b.clear();
  assert.equal(b.size, 0);
  assert.equal(b.dropped, 0);
  assert.equal(b.push({ message: "x" }).seq, 6, "sequence numbers keep increasing after a clear");
});

test("log entries default their fields and validate severity", () => {
  const b = new L.LogBuffer(10);
  const e = b.push({ message: "hi", severity: "nonsense" });
  assert.equal(e.severity, "info");
  assert.equal(e.category, "event");
  assert.equal(e.playerId, 0);
});

test("log filters: severity, category, text, player, entity, newest-first limit", () => {
  const b = new L.LogBuffer(100);
  b.push({ severity: "error", category: "command", message: "spawn failed", playerId: 7 });
  b.push({ severity: "info", category: "event", source: "energy", message: "energy.mined #1", entityId: "11", playerId: 7 });
  b.push({ severity: "info", category: "event", source: "movement", message: "movement.arrived", entityId: "12", playerId: 9 });
  b.push({ severity: "ok", category: "command", message: "done" });
  const all = b.entries();
  const f = (o) => L.filterLogs(all, { severities: new Set(L.SEVERITIES), ...o }).map((e) => e.message);
  assert.deepEqual(f({ severities: new Set(["error"]) }), ["spawn failed"]);
  assert.deepEqual(f({ category: "command" }), ["spawn failed", "done"]);
  assert.deepEqual(f({ text: "ENERGY" }), ["energy.mined #1"], "search is case-insensitive and covers the source");
  assert.deepEqual(f({ playerId: 7 }), ["spawn failed", "energy.mined #1"]);
  assert.deepEqual(f({ entityId: "12" }), ["movement.arrived"]);
  assert.deepEqual(L.filterLogs(all, {}, 2).map((e) => e.message), ["movement.arrived", "done"], "the limit keeps the newest, oldest first");
});

test("events are classified by emitting plugin and severity", () => {
  assert.deepEqual(L.classifyEvent("energy.mined"), { severity: "info", category: "event", source: "energy" });
  assert.equal(L.classifyEvent("combat.destroyed").severity, "warn");
  assert.equal(L.classifyEvent("energy.well_depleted").severity, "warn");
  assert.deepEqual(L.classifyEvent("match.finished"), { severity: "ok", category: "match", source: "match" });
});

test("player statistics are derived from authoritative unit state", () => {
  const ents = new Map([
    ["1", unit("1", 7, { "energy.stored": iv(40), "energy.capacity": iv(100), "attributes.embodied": iv(30), "attributes.invested": iv(20),
      "energy.mining_well": sv("99"), "automation.behavior": sv("mine_and_deliver"), "logistics.supply.0.to": sv("2"), "logistics.supply.1.to": sv("3") })],
    ["2", unit("2", 7, { "energy.stored": iv(10), "energy.capacity": iv(100), "movement.destination": { positionValue: { x: 3, y: 4 } }, "attributes.busy": sv("upgrade") })],
    ["3", unit("3", 9, { "combat.target": sv("1"), "attributes.busy": sv("clone") })],
    ["4", unit("4", 0, { "energy.stored": iv(500) })], // unowned
    ["5", { id: "5", kind: "ENTITY_KIND_RESOURCE", ownerId: 7, position: { x: 0, y: 0 }, attributes: { "energy.stored": iv(999) } }],
  ]);
  const s = L.playerStats(ents);
  assert.deepEqual(s.get(7), { units: 2, stored: 50, capacity: 200, embodied: 30, invested: 20, mining: 1, moving: 1, attacking: 0, automated: 1, upgrading: 1, cloning: 0, supplying: 2 });
  assert.equal(s.get(9).attacking, 1);
  assert.equal(s.get(9).cloning, 1);
  assert.equal(s.has(0), false, "unowned units are not a player");
});

test("player rows merge configuration order, live and cumulative statistics", () => {
  const players = [{ id: 7, name: "Red" }, { id: 9, name: "Blue" }, { id: 11, name: "Green" }];
  const current = L.playerStats(new Map([["1", unit("1", 9, { "energy.stored": iv(5) })]]));
  const details = { players: [
    { playerId: 9, slot: 2, ready: true, connections: 1, stats: { score: "120", energyMined: "300", kills: "2", unitsLost: "1", investmentDestroyed: "40" } },
    { playerId: 7, slot: 1, eliminated: true, connections: 0 },
  ] };
  const rows = L.playerRows(players, current, details);
  assert.deepEqual(rows.map((r) => r.name), ["Red", "Blue", "Green"]);
  assert.equal(rows[0].eliminated, true);
  assert.equal(rows[0].connections, 0);
  assert.equal(rows[0].current.units, 0);
  assert.deepEqual(rows[1].cumulative, { score: 120, energyMined: 300, kills: 2, unitsLost: 1, investmentDestroyed: 40 });
  assert.equal(rows[1].current.stored, 5);
  assert.equal(rows[2].connections, null, "unknown connection state is not reported as disconnected");
});

test("unit bounds enclose one player's units", () => {
  const ents = new Map([
    ["1", unit("1", 7, {}, { x: 4, y: 9 })], ["2", unit("2", 7, {}, { x: 10, y: 2 })], ["3", unit("3", 9, {}, { x: 40, y: 40 })],
  ]);
  assert.deepEqual(L.unitBounds(ents, 7), { x0: 4, y0: 2, x1: 10, y1: 9 });
  assert.equal(L.unitBounds(ents, 11), null);
});

test("inspector sections group attributes by meaning, unknown plugins last", () => {
  const e = unit("1", 7, { "energy.stored": iv(1), "movement.destination": {}, "cloning.quote_cost": iv(61), "combat.health": iv(5),
    "attributes.armor": iv(0), "metadata.role": sv("miner"), "zeta.thing": iv(1), "alpha.thing": iv(2) });
  const secs = L.inspectorSections(e);
  assert.deepEqual(secs.map((s) => s.name), ["Energy", "Current orders", "Cloning", "Combat", "Attributes", "Metadata", "alpha", "zeta"]);
  assert.deepEqual(secs.find((s) => s.name === "Current orders").keys, ["movement.destination"]);
  assert.equal(L.entityKind(e), "unit");
  assert.equal(L.entityKind({ kind: "ENTITY_KIND_RESOURCE", attributes: { "wells.capacity": iv(1) } }), "well");
  assert.equal(L.entityKind({ kind: "ENTITY_KIND_RESOURCE", attributes: { "salvage.energy": iv(1) } }), "wreckage");
  assert.equal(L.entityKind({ kind: "ENTITY_KIND_TERRAIN" }), "terrain");
});

test("default layouts place every panel once, after its reference", () => {
  for (const name of ["wide", "compact"]) {
    assert.deepEqual(L.checkDefaultLayout(L.DEFAULT_LAYOUT[name], PANELS), [], name);
    assert.equal(L.DEFAULT_LAYOUT[name][0].id, "board", `${name}: the board comes first (it is the anchor)`);
  }
  const wide = Object.fromEntries(L.DEFAULT_LAYOUT.wide.map((s) => [s.id, s]));
  assert.equal(wide.players.position.direction, "left");
  assert.equal(wide.inspector.position.direction, "right");
  assert.equal(wide.logs.position.direction, "below");
  assert.equal(wide.controls.position.direction, "above");
  assert.ok(L.DEFAULT_LAYOUT.compact.filter((s) => s.position && s.position.direction === "within").length >= 8, "compact stacks panels as tabs");
  assert.deepEqual(L.checkDefaultLayout([{ id: "logs", position: { referencePanel: "board", direction: "below" } }], ["board", "logs"]),
    ["panel logs refers to board before it is placed", "panel board missing"]);
});

test("saved layouts are validated before they are restored", () => {
  const good = { version: 1, panels: PANELS, layout: { grid: { root: {} }, panels: { board: { id: "board", contentComponent: "board" }, logs: { id: "logs", contentComponent: "logs" } } } };
  assert.deepEqual(L.validateLayout(good, 1, PANELS), { ok: true, reason: "" });
  assert.equal(L.validateLayout(null, 1, PANELS).ok, false);
  assert.equal(L.validateLayout({ ...good, version: 0 }, 1, PANELS).ok, false);
  assert.equal(L.validateLayout({ version: 1, layout: { panels: {} } }, 1, PANELS).ok, false, "missing grid");
  assert.equal(L.validateLayout({ version: 1, layout: { grid: {}, panels: {} } }, 1, PANELS).ok, false, "no panels");
  const ghost = JSON.parse(JSON.stringify(good));
  ghost.layout.panels.ghost = { id: "ghost", contentComponent: "ghost" };
  assert.match(L.validateLayout(ghost, 1, PANELS).reason, /unknown panel ghost/);
  const swapped = JSON.parse(JSON.stringify(good));
  swapped.layout.panels.board.contentComponent = "logs";
  assert.equal(L.validateLayout(swapped, 1, PANELS).ok, false, "a panel id must render its own component");
});

test("panels added after a layout was saved are opened, closed ones stay closed", () => {
  assert.deepEqual(L.newPanels({ panels: ["board", "logs"] }, ["board", "logs", "players"]), ["players"]);
  assert.deepEqual(L.newPanels({ panels: ["board", "logs", "players"] }, ["board", "logs"]), []);
});

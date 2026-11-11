// Energy heat map logic (web/static/admin/logic.js).
"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const L = require("../static/admin/logic.js");

const iv = (v) => ({ intValue: String(v) });
const unit = (id, owner, x, y, stored, embodied = 0) => ({
  id, kind: "ENTITY_KIND_UNIT", ownerId: owner, position: { x, y },
  attributes: { "energy.stored": iv(stored), "attributes.embodied": iv(embodied) },
});
const well = (id, x, y, e) => ({ id, kind: "ENTITY_KIND_RESOURCE", position: { x, y }, attributes: { "wells.energy": iv(e) } });
const wreck = (id, x, y, e) => ({ id, kind: "ENTITY_KIND_RESOURCE", position: { x, y }, attributes: { "salvage.energy": iv(e) } });
const terrain = (id, x, y) => ({ id, kind: "ENTITY_KIND_TERRAIN", position: { x, y }, attributes: {} });

const world = [
  unit("u1", 1, 0, 0, 40, 100), unit("u2", 1, 1, 1, 60), unit("u3", 2, 7, 7, 30),
  well("w1", 4, 4, 500), wreck("k1", 7, 0, 25), terrain("t1", 3, 3),
];
const all = () => ({ sources: new Set(["stored", "embodied", "wells", "wrecks"]), owners: null, neutral: true, width: 8, height: 8, cell: 1 });

test("energy grid counts every source by default, binned by cell", () => {
  const g = L.energyGrid(world, all());
  assert.equal(g.total, 40 + 100 + 60 + 30 + 500 + 25);
  assert.deepEqual(g.totals, { stored: 130, embodied: 100, wells: 500, wrecks: 25 });
  assert.equal(g.counted, 5); // terrain holds no energy
  assert.equal(g.values[0], 140);
  assert.equal(g.max, 500);
  const g4 = L.energyGrid(world, { ...all(), cell: 4 });
  assert.equal(g4.cols, 2);
  assert.equal(g4.values[0], 200); // u1 + u2 in the top-left 4x4
  assert.equal(g4.values[1], 25); // the wreck at (7,0)
});

test("energy grid filters by player, unowned and source", () => {
  const p1 = L.energyGrid(world, { ...all(), owners: new Set([1]), neutral: false });
  assert.equal(p1.total, 200);
  assert.equal(p1.counted, 2);
  const p2stored = L.energyGrid(world, { ...all(), owners: new Set([2]), neutral: false, sources: new Set(["stored"]) });
  assert.equal(p2stored.total, 30);
  const resources = L.energyGrid(world, { ...all(), owners: new Set(), neutral: true });
  assert.deepEqual([resources.total, resources.totals.wells, resources.totals.wrecks], [525, 500, 25]);
  const none = L.energyGrid(world, { ...all(), owners: new Set(), neutral: false });
  assert.equal(none.total, 0);
  assert.equal(none.max, 0);
});

test("spread keeps the total and widens the hot area", () => {
  const g = L.energyGrid(world, all());
  const s = L.spread(g, 1);
  const sum = (v) => v.reduce((a, b) => a + b, 0);
  assert.ok(Math.abs(sum(s.values) - sum(g.values)) < 1e-9);
  assert.ok(s.values[3 * 8 + 3] > 0, "neighbor of the well is warm");
  assert.ok(s.max < g.max);
  assert.equal(L.spread(g, 0), g);
});

test("heat scale and colors", () => {
  assert.equal(L.heatScale(0, 100, "sqrt"), 0);
  assert.equal(L.heatScale(25, 100, "sqrt"), 0.5);
  assert.equal(L.heatScale(25, 100, "linear"), 0.25);
  assert.equal(L.heatScale(100, 100, "log"), 1);
  assert.equal(L.heatScale(5, 0, "linear"), 0);
  assert.deepEqual(L.heatColor(0), [0, 0, 0, 0]);
  const hot = L.heatColor(1);
  assert.deepEqual(hot.slice(0, 3), [252, 255, 164]);
  assert.equal(hot[3], 255);
  for (const t of [0.1, 0.3, 0.6, 0.9]) {
    const c = L.heatColor(t);
    assert.ok(c.every((v) => v >= 0 && v <= 255), `color at ${t}`);
  }
});

// Pure admin-workspace logic, free of DOM access so it is unit-tested under
// Node (web/admintest): the bounded log buffer and its filters, per-player
// statistics derived from authoritative entity state, inspector grouping,
// and saved-layout validation. Exposed as window.AdminLogic (browser) or
// module.exports (Node).
"use strict";

(function (root, factory) {
  const api = factory();
  if (typeof module === "object" && module.exports) module.exports = api;
  else root.AdminLogic = api;
})(typeof self !== "undefined" ? self : this, function () {
  // ---- entity attribute helpers (same encoding as protojson Values) -----------

  const attr = (e, k) => (e.attributes ? e.attributes[k] : undefined);
  const int = (e, k) => {
    const v = attr(e, k);
    return v && v.intValue !== undefined ? Number(v.intValue) : undefined;
  };
  const str = (e, k) => {
    const v = attr(e, k);
    return v && v.stringValue !== undefined ? v.stringValue : undefined;
  };
  const has = (e, k) => attr(e, k) !== undefined;
  const isUnit = (e) => e.kind === "ENTITY_KIND_UNIT";

  // ---- logs ---------------------------------------------------------------------

  const SEVERITIES = ["error", "warn", "ok", "info", "debug"];

  // LogBuffer keeps the newest `capacity` entries (a ring buffer: memory is
  // bounded however fast the simulation logs). Entries are
  //   {seq, tick, time, severity, category, source, message, playerId, entityId}
  // where seq increases monotonically, so views can render incrementally.
  class LogBuffer {
    constructor(capacity = 2000) {
      this.capacity = Math.max(1, capacity);
      this.items = new Array(this.capacity);
      this.start = 0; // index of the oldest entry
      this.size = 0;
      this.seq = 0;
      this.dropped = 0; // entries overwritten since the last clear
    }

    push(entry) {
      const e = { severity: "info", category: "event", source: "", message: "", playerId: 0, entityId: "", ...entry, seq: ++this.seq };
      if (!SEVERITIES.includes(e.severity)) e.severity = "info";
      if (this.size < this.capacity) {
        this.items[(this.start + this.size) % this.capacity] = e;
        this.size++;
      } else {
        this.items[this.start] = e;
        this.start = (this.start + 1) % this.capacity;
        this.dropped++;
      }
      return e;
    }

    // clear empties the client-side buffer only (it never touches the server).
    clear() {
      this.items = new Array(this.capacity);
      this.start = 0;
      this.size = 0;
      this.dropped = 0;
    }

    // entries returns all entries, oldest first.
    entries() {
      const out = new Array(this.size);
      for (let i = 0; i < this.size; i++) out[i] = this.items[(this.start + i) % this.capacity];
      return out;
    }

    categories() {
      const s = new Set();
      for (const e of this.entries()) s.add(e.category);
      return [...s].sort();
    }
  }

  // matchLog applies a filter {severities:Set, category, text, playerId, entityId}
  // (empty fields match everything).
  function matchLog(e, f) {
    if (f.severities && f.severities.size && !f.severities.has(e.severity)) return false;
    if (f.category && e.category !== f.category) return false;
    if (f.playerId && Number(e.playerId) !== Number(f.playerId)) return false;
    if (f.entityId && e.entityId !== f.entityId) return false;
    if (f.text) {
      const t = f.text.toLowerCase();
      if (!(`${e.message} ${e.source} ${e.category}`.toLowerCase().includes(t))) return false;
    }
    return true;
  }

  function filterLogs(entries, f, limit = Infinity) {
    const out = [];
    for (let i = entries.length - 1; i >= 0 && out.length < limit; i--) {
      if (matchLog(entries[i], f)) out.push(entries[i]);
    }
    return out.reverse(); // oldest first, newest last
  }

  // classifyEvent turns a simulation event into a log entry's severity,
  // category and source (the plugin that emitted it).
  function classifyEvent(type) {
    const source = type.split(".")[0];
    let severity = "info";
    if (/destroyed|eliminated|failed|lost$/.test(type)) severity = "warn";
    if (/blocked|depleted/.test(type)) severity = "warn";
    if (type === "match.finished") severity = "ok";
    return { severity, category: source === "match" ? "match" : "event", source };
  }

  // ---- player statistics ----------------------------------------------------------

  // CURRENT and CUMULATIVE describe the statistics shown for each player:
  // current-state values are recomputed from the authoritative entity state
  // the admin stream delivers every tick; cumulative values are the match
  // plugin's running totals from GetMatchDetails.
  const CURRENT = [
    ["units", "Units", "Living units owned now"],
    ["stored", "Stored energy", "Sum of energy.stored over living units"],
    ["capacity", "Energy capacity", "Sum of energy.capacity over living units"],
    ["embodied", "Embodied investment", "Sum of attributes.embodied: what the units' upgrade levels are worth"],
    ["invested", "Invested energy", "Sum of attributes.invested: energy actually paid for those levels"],
    ["mining", "Mining", "Units with a mining order (energy.mining_well)"],
    ["moving", "Moving", "Units with a destination (movement.destination)"],
    ["attacking", "Attacking", "Units with an attack target (combat.target)"],
    ["automated", "Automated", "Units running automation (automation.behavior)"],
    ["upgrading", "Upgrading", "Units busy with an upgrade (attributes.busy = upgrade)"],
    ["cloning", "Cloning", "Units busy cloning (attributes.busy = clone)"],
    ["supplying", "Supply links", "Supply links from this player's units (logistics)"],
  ];
  const CUMULATIVE = [
    ["score", "Score", "Match score under the configured weights (updated at the scoring interval)"],
    ["energyMined", "Energy mined", "Energy extracted from wells since the match started"],
    ["kills", "Enemy units destroyed", "Enemy units this player destroyed"],
    ["unitsLost", "Units lost", "Own units destroyed"],
    ["investmentDestroyed", "Investment destroyed", "Embodied investment of enemy units destroyed"],
  ];

  function emptyCurrent() {
    const o = {};
    for (const [k] of CURRENT) o[k] = 0;
    return o;
  }

  // playerStats derives current-state statistics per owner from entities
  // (a Map or iterable of protojson entities). Unowned entities are ignored.
  function playerStats(entities) {
    const out = new Map();
    for (const e of entities.values ? entities.values() : entities) {
      if (!isUnit(e) || !e.ownerId) continue;
      let s = out.get(e.ownerId);
      if (!s) out.set(e.ownerId, (s = emptyCurrent()));
      s.units++;
      s.stored += int(e, "energy.stored") || 0;
      s.capacity += int(e, "energy.capacity") || 0;
      s.embodied += int(e, "attributes.embodied") || 0;
      s.invested += int(e, "attributes.invested") || 0;
      if (has(e, "energy.mining_well")) s.mining++;
      if (has(e, "movement.destination")) s.moving++;
      if (has(e, "combat.target")) s.attacking++;
      if (str(e, "automation.behavior")) s.automated++;
      const busy = str(e, "attributes.busy");
      if (busy === "upgrade") s.upgrading++;
      if (busy === "clone") s.cloning++;
      for (let i = 0; i < 16 && str(e, `logistics.supply.${i}.to`); i++) s.supplying++;
    }
    return out;
  }

  // playerRows merges the configured players (in configuration order), the
  // derived current statistics and the match details' cumulative ones.
  function playerRows(players, current, details) {
    const byId = new Map(((details && details.players) || []).map((p) => [Number(p.playerId), p]));
    return players.map((p, i) => {
      const d = byId.get(Number(p.id)) || {};
      const s = d.stats || {};
      const cum = {};
      for (const [k] of CUMULATIVE) cum[k] = Number(s[k] || 0);
      return {
        id: Number(p.id), name: p.name, index: i, slot: d.slot || 0,
        ready: !!d.ready, eliminated: !!d.eliminated,
        connections: d.connections === undefined ? null : Number(d.connections),
        current: current.get(Number(p.id)) || current.get(String(p.id)) || emptyCurrent(),
        cumulative: cum,
      };
    });
  }

  // bounds returns the tile rectangle enclosing a player's units, or null.
  function unitBounds(entities, ownerId) {
    let b = null;
    for (const e of entities.values ? entities.values() : entities) {
      if (!isUnit(e) || Number(e.ownerId) !== Number(ownerId)) continue;
      const x = e.position.x || 0, y = e.position.y || 0;
      if (!b) b = { x0: x, y0: y, x1: x, y1: y };
      else { b.x0 = Math.min(b.x0, x); b.y0 = Math.min(b.y0, y); b.x1 = Math.max(b.x1, x); b.y1 = Math.max(b.y1, y); }
    }
    return b;
  }

  // ---- inspector ------------------------------------------------------------------

  // SECTIONS maps attribute prefixes to inspector sections, in display order.
  const SECTIONS = [
    ["Energy", ["energy"]],
    ["Current orders", ["movement", "upgrades"]],
    ["Cloning", ["cloning"]],
    ["Automation", ["automation"]],
    ["Logistics", ["logistics"]],
    ["Combat", ["combat"]],
    ["Attributes", ["attributes"]],
    ["Well", ["wells"]],
    ["Wreckage", ["salvage"]],
    ["Sensors", ["visibility"]],
    ["Metadata", ["metadata"]],
  ];

  // inspectorSections groups an entity's attribute keys into sections:
  // [{name, keys:[...]}], known sections first, then one per other plugin.
  function inspectorSections(e) {
    const keys = Object.keys(e.attributes || {}).sort();
    const taken = new Set();
    const out = [];
    for (const [name, prefixes] of SECTIONS) {
      const ks = keys.filter((k) => prefixes.includes(k.split(".")[0]));
      ks.forEach((k) => taken.add(k));
      if (ks.length) out.push({ name, keys: ks });
    }
    const rest = {};
    for (const k of keys) {
      if (taken.has(k)) continue;
      const g = k.split(".")[0];
      (rest[g] = rest[g] || []).push(k);
    }
    for (const g of Object.keys(rest).sort()) out.push({ name: g, keys: rest[g] });
    return out;
  }

  function entityKind(e) {
    if (isUnit(e)) return "unit";
    if (e.kind === "ENTITY_KIND_RESOURCE") return has(e, "wells.capacity") ? "well" : has(e, "salvage.energy") ? "wreckage" : "resource";
    if (e.kind === "ENTITY_KIND_TERRAIN") return "terrain";
    return (e.kind || "entity").replace("ENTITY_KIND_", "").toLowerCase();
  }

  // ---- layout persistence -----------------------------------------------------------

  // validateLayout checks a saved workspace before it is restored:
  //   {version, layout: <dockview toJSON>, panels: [known ids at save time]}
  // It returns {ok, reason}. Anything unexpected (other version, unknown
  // panel ids, missing structure) is rejected so the default layout is used.
  function validateLayout(saved, version, knownIds) {
    if (!saved || typeof saved !== "object") return { ok: false, reason: "no saved layout" };
    if (saved.version !== version) return { ok: false, reason: `saved layout version ${saved.version} != ${version}` };
    const l = saved.layout;
    if (!l || typeof l !== "object" || !l.grid || !l.panels || typeof l.panels !== "object") return { ok: false, reason: "malformed layout" };
    const ids = Object.keys(l.panels);
    if (!ids.length) return { ok: false, reason: "empty layout" };
    const known = new Set(knownIds);
    for (const id of ids) {
      const p = l.panels[id];
      if (!known.has(id) || !p || (p.contentComponent && p.contentComponent !== id)) return { ok: false, reason: `unknown panel ${id}` };
    }
    return { ok: true, reason: "" };
  }

  // newPanels lists panels that exist now but did not exist when the layout
  // was saved (added by an application update): they are opened in their
  // default place, while panels the user closed stay closed.
  function newPanels(saved, knownIds) {
    const then = new Set((saved && saved.panels) || []);
    return knownIds.filter((id) => !then.has(id));
  }

  // ---- default workspace ------------------------------------------------------------

  // DEFAULT_LAYOUT is the initial workspace, applied in order. `position`
  // is dockview's: {referencePanel, direction: left|right|above|below|within}
  // relative to a panel, or {direction} alone relative to the whole
  // workspace. `size` is the group's width or height in pixels. Panels with
  // open: false start closed (reopen them from the Panels menu).
  const DEFAULT_LAYOUT = {
    wide: [
      { id: "board" },
      { id: "players", position: { referencePanel: "board", direction: "left" }, size: { width: 330 } },
      { id: "tools", position: { referencePanel: "players", direction: "within" } },
      { id: "inspector", position: { referencePanel: "board", direction: "right" }, size: { width: 360 } },
      { id: "match", position: { referencePanel: "inspector", direction: "within" } },
      { id: "automation", position: { referencePanel: "inspector", direction: "within" } },
      { id: "newmatch", position: { referencePanel: "inspector", direction: "within" } },
      { id: "replays", position: { referencePanel: "inspector", direction: "within" } },
      { id: "logs", position: { direction: "below" }, size: { height: 210 } },
      { id: "controls", position: { direction: "above" }, size: { height: 76 } },
      { id: "rules", position: { referencePanel: "inspector", direction: "within" }, open: false },
    ],
    // Narrow screens: everything secondary is a tab next to the board.
    compact: [
      { id: "board" },
      { id: "players", position: { referencePanel: "board", direction: "within" } },
      { id: "inspector", position: { referencePanel: "board", direction: "within" } },
      { id: "logs", position: { referencePanel: "board", direction: "within" } },
      { id: "tools", position: { referencePanel: "board", direction: "within" } },
      { id: "match", position: { referencePanel: "board", direction: "within" } },
      { id: "automation", position: { referencePanel: "board", direction: "within" } },
      { id: "newmatch", position: { referencePanel: "board", direction: "within" } },
      { id: "replays", position: { referencePanel: "board", direction: "within" } },
      { id: "controls", position: { direction: "above" }, size: { height: 110 } },
      { id: "rules", position: { referencePanel: "board", direction: "within" }, open: false },
    ],
  };
  // Which panels are active (visible) in their tab group by default.
  const DEFAULT_ACTIVE = ["players", "inspector", "board"];


  // checkDefaultLayout verifies a layout description against the panel ids:
  // every id known and listed once, every reference panel listed earlier.
  function checkDefaultLayout(specs, knownIds) {
    const seen = new Set();
    const known = new Set(knownIds);
    const errors = [];
    for (const s of specs) {
      if (!known.has(s.id)) errors.push(`unknown panel ${s.id}`);
      if (seen.has(s.id)) errors.push(`panel ${s.id} listed twice`);
      const ref = s.position && s.position.referencePanel;
      if (ref && !seen.has(ref)) errors.push(`panel ${s.id} refers to ${ref} before it is placed`);
      seen.add(s.id);
    }
    for (const id of knownIds) if (!seen.has(id)) errors.push(`panel ${id} missing`);
    return errors;
  }

  return {
    DEFAULT_LAYOUT, DEFAULT_ACTIVE, checkDefaultLayout,
    SEVERITIES, LogBuffer, matchLog, filterLogs, classifyEvent,
    CURRENT, CUMULATIVE, playerStats, playerRows, unitBounds,
    SECTIONS, inspectorSections, entityKind,
    validateLayout, newPanels,
  };
});

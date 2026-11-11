// Central admin state. Authoritative data (entities, status, match details)
// lives here once; panels read it and subscribe to change topics instead of
// keeping copies. The workspace layout is deliberately not part of it (see
// workspace.js): moving or closing a panel never touches this state or the
// observation stream.
//
// Topics:
//   "world"      world info, players or rules changed (sign-in, match reset)
//   "tick"       an observation was applied (entities, tick)
//   "status"     simulation status (paused, tick rate) from GetStatus
//   "match"      match details from GetMatchDetails
//   "selection"  selected entities or selected tile changed
//   "player"     focused player, board filter or highlight changed
//   "viewas"     the fog-of-war preview player changed
"use strict";

const Store = (() => {
  const subs = new Map();
  const state = {
    world: null, // WorldInfo
    rules: {}, // plugin name -> published rules
    players: [], // [{id, name}] in configuration order
    tick: 0,
    status: null, // {paused, tickRateHz, ...}
    details: null, // GetMatchDetailsResponse
    entities: new Map(),
    selected: new Set(), // entity ids
    tile: null, // {x, y}: an inspected tile with nothing on it
    focusPlayer: 0, // player selected in the Players panel (highlighted)
    filterPlayer: 0, // board shows only this player's units (0 = all)
    viewAs: 0, // fog-of-war preview player
  };
  function on(topic, fn) {
    if (!subs.has(topic)) subs.set(topic, new Set());
    subs.get(topic).add(fn);
    return () => subs.get(topic).delete(fn);
  }
  function emit(topic, data) {
    for (const fn of subs.get(topic) || []) {
      try { fn(data); } catch (e) { console.error(`${topic} subscriber`, e); }
    }
  }
  // select replaces the selection (one entity id, several, or none) and
  // notifies every panel showing it.
  function select(ids, { add = false, tile = null } = {}) {
    if (!add) state.selected.clear();
    for (const id of ids) state.selected.add(id);
    state.tile = state.selected.size ? null : tile;
    emit("selection");
  }
  function toggle(id) {
    state.selected.has(id) ? state.selected.delete(id) : state.selected.add(id);
    state.tile = null;
    emit("selection");
  }
  function setPlayerFocus(id, { filter } = {}) {
    state.focusPlayer = Number(id) || 0;
    if (filter !== undefined) state.filterPlayer = filter ? state.focusPlayer : 0;
    emit("player");
  }
  // el finds an element by id even when its panel is closed or is an
  // inactive tab (dockview detaches those from the document; the panel
  // sections themselves are kept, see workspace.js).
  const sections = [...document.querySelectorAll("#panel-sources > section[data-panel]")];
  function el(id) {
    const found = document.getElementById(id);
    if (found) return found;
    const sel = "#" + CSS.escape(id);
    for (const sec of sections) {
      const x = sec.querySelector(sel);
      if (x) return x;
    }
    return null;
  }
  return { state, on, emit, select, toggle, setPlayerFocus, el };
})();

// Players panel: one compact row per player (colour, name, connection,
// units, stored energy, losses, kills, energy mined) and, for the selected
// player, the full breakdown. Current-state numbers are recomputed from the
// authoritative entities the admin stream delivers (AdminLogic.playerStats);
// cumulative numbers come from the match plugin via GetMatchDetails.
// Selecting a player highlights their units on the board; "Center" zooms
// the board to them and "Only this player" filters the board.
"use strict";

const PlayersPanel = (() => {
  const { playerStats, playerRows, unitBounds, CURRENT, CUMULATIVE } = AdminLogic;
  let expanded = 0; // player whose details are shown
  let renderedAt = 0;
  let queued = false;
  let holding = false; // a pointer is down in the panel: do not swap rows under it

  const el = (tag, props = {}, ...kids) => { const n = Object.assign(document.createElement(tag), props); n.append(...kids); return n; };
  const fmt = (n) => (n === null || n === undefined ? "–" : Number(n).toLocaleString());

  function rows() {
    return playerRows(S.players, playerStats(S.entities), S.details);
  }

  function connCell(r) {
    if (r.connections === null) return el("span", { className: "hint", textContent: "–", title: "not reported yet" });
    const on = r.connections > 0;
    return el("span", { className: "conn-dot " + (on ? "on" : "off"), title: on ? `${r.connections} open observation stream(s)` : "not connected" });
  }

  function render() {
    queued = false;
    const box = $("players-panel");
    if (!box) return;
    if (holding) { schedule(); return; }
    renderedAt = performance.now();
    if (!S.players.length) {
      box.replaceChildren(el("p", { className: "hint", textContent: S.world ? "This match has no players." : "Loading…" }));
      return;
    }
    const all = rows();
    const t = el("table", { className: "players-table" });
    const head = t.createTHead().insertRow();
    for (const [h, title] of [["", ""], ["Player", "Name (slot)"], ["", "Connection"], ["Units", "Living units now"],
      ["Energy", "Stored energy across living units now"], ["Lost", "Own units destroyed (match total)"],
      ["Kills", "Enemy units destroyed (match total)"], ["Mined", "Energy mined (match total)"]]) {
      head.append(el("th", { textContent: h, title }));
    }
    const tb = t.createTBody();
    for (const r of all) {
      const tr = tb.insertRow();
      tr.className = "player-row" + (S.focusPlayer === r.id ? " focused" : "") + (r.eliminated ? " eliminated" : "");
      tr.tabIndex = 0;
      tr.setAttribute("role", "button");
      tr.setAttribute("aria-expanded", String(expanded === r.id));
      tr.title = "Select to highlight this player's units; Enter toggles details";
      const sw = el("span", { className: "swatch" });
      sw.style.background = playerColor(r.id);
      const name = `${r.name}${r.slot ? ` (${r.slot})` : ""}${r.eliminated ? " ✖" : ""}${r.ready ? "" : " · not ready"}`;
      const cells = [sw, name, connCell(r), fmt(r.current.units), fmt(r.current.stored), fmt(r.cumulative.unitsLost), fmt(r.cumulative.kills), fmt(r.cumulative.energyMined)];
      cells.forEach((c, i) => {
        const td = tr.insertCell();
        td.append(c);
        if (i >= 3) td.className = "num";
      });
      const choose = () => {
        if (S.focusPlayer === r.id && expanded === r.id) { expanded = 0; Store.setPlayerFocus(0); }
        else { expanded = r.id; Store.setPlayerFocus(r.id); }
      };
      tr.addEventListener("click", choose);
      tr.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); choose(); } });
      if (expanded === r.id) {
        const dr = tb.insertRow();
        dr.className = "player-detail";
        const td = dr.insertCell();
        td.colSpan = 8;
        td.append(details(r));
      }
    }
    box.replaceChildren(t, el("p", { className: "hint legend-note", textContent: "Units and Energy are current; Lost, Kills and Mined are match totals." }));
  }

  function details(r) {
    const wrap = el("div", { className: "player-details" });
    const actions = el("div", { className: "actions" });
    const btn = (text, fn, title) => { const b = el("button", { type: "button", textContent: text, title }); b.addEventListener("click", (ev) => { ev.stopPropagation(); fn(); }); return b; };
    const b = unitBounds(S.entities, r.id);
    const center = btn("Center", () => board.centerOn(b), "Zoom the board to this player's units");
    center.disabled = !b;
    if (!b) center.title = "This player has no units";
    const filterOn = S.filterPlayer === r.id;
    actions.append(center,
      btn(filterOn ? "Show all players" : "Only this player", () => Store.setPlayerFocus(r.id, { filter: !filterOn }), "Show only this player's units on the board"),
      btn(S.viewAs === r.id ? "Leave fog view" : "View as", () => setViewAs(S.viewAs === r.id ? 0 : r.id), "Preview exactly what this player observes (fog of war)"));
    wrap.append(actions);
    const grid = el("table", { className: "kv" });
    const group = (name) => { const tr = grid.insertRow(); tr.className = "group"; const td = tr.insertCell(); td.colSpan = 2; td.textContent = name; };
    const row = (label, v, title) => { const tr = grid.insertRow(); tr.title = title || ""; tr.insertCell().textContent = label; const td = tr.insertCell(); td.className = "num"; td.textContent = v; };
    group("Now (from the live world state)");
    for (const [k, label, title] of CURRENT) row(label, fmt(r.current[k]), title);
    if (r.current.capacity) row("Storage used", Math.round((100 * r.current.stored) / r.current.capacity) + "%", "Stored energy / energy capacity");
    group("Match totals (cumulative)");
    for (const [k, label, title] of CUMULATIVE) row(label, fmt(r.cumulative[k]), title);
    group("Status");
    row("Ready", r.ready ? "yes" : "no", "Readiness in the lobby");
    row("Eliminated", r.eliminated ? "yes" : "no");
    row("Observation streams", r.connections === null ? "–" : r.connections, "Open Observe streams for this player (0 = disconnected)");
    row("Player id", r.id);
    wrap.append(grid);
    return wrap;
  }

  // Live updates: at most four renders per second, and not while the user
  // is pressing a button in the panel.
  function schedule() {
    if (queued) return;
    const wait = Math.max(0, 250 - (performance.now() - renderedAt));
    queued = true;
    setTimeout(() => RTS.nextFrame(render), wait);
  }

  const box = $("players-panel");
  // The hold ends with the pointer release, or anything that can swallow
  // it (a cancelled drag, switching windows); a hold never outlasts two
  // seconds, so a lost release cannot freeze the panel.
  let holdTimer = 0;
  const release = () => {
    clearTimeout(holdTimer);
    if (holding) { holding = false; schedule(); }
  };
  box.addEventListener("pointerdown", () => {
    holding = true;
    clearTimeout(holdTimer);
    holdTimer = setTimeout(release, 2000);
  });
  for (const ev of ["pointerup", "pointercancel", "blur"]) window.addEventListener(ev, release);
  document.addEventListener("visibilitychange", release);

  Store.on("tick", schedule);
  Store.on("match", schedule);
  Store.on("world", () => { expanded = 0; Store.setPlayerFocus(0, { filter: false }); render(); });
  Store.on("player", schedule);
  Store.on("viewas", schedule);
  return { render };
})();

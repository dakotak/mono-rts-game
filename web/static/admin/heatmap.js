// Energy heat map panel: where energy sits on the map, from the
// authoritative entity state the admin stream delivers (no fog, no
// estimates). The grid, filters and colormap are AdminLogic functions
// (logic.js, unit-tested); this file is the panel's controls and canvas.
//
// Options: which sources count (stored in units, locked in upgrades,
// wells, wrecks), whose entities (any set of players, and/or unowned
// resources), cell size, spread (blur), scale (sqrt, linear, log), and
// overlays (terrain, units). Hovering a cell explains its value; clicking
// centers the game board there.
"use strict";

const HeatMap = (() => {
  const L = AdminLogic;
  const $h = Store.el;
  const SOURCE_KEYS = L.ENERGY_SOURCES.map(([k]) => k);

  const opts = {
    sources: new Set(SOURCE_KEYS),
    owners: null, // null: every player
    neutral: true,
    cell: 2,
    spread: 1,
    scale: "sqrt",
    terrain: true,
    units: false,
    smooth: false,
  };

  let grid = null;
  let hover = null; // {cx, cy}
  let renderedAt = 0;
  let queued = false;
  const off = document.createElement("canvas"); // the grid at one pixel per cell

  const el = (tag, props = {}, ...kids) => { const n = Object.assign(document.createElement(tag), props); n.append(...kids); return n; };
  const fmt = (n) => Math.round(n).toLocaleString();
  const canvas = () => $h("heat-canvas");

  // ---- controls ------------------------------------------------------------

  function check(label, checked, onChange, title = "") {
    const cb = el("input", { type: "checkbox", checked });
    cb.addEventListener("change", () => onChange(cb.checked));
    return el("label", { title }, cb, label);
  }

  function renderSources() {
    const box = $h("heat-sources");
    box.replaceChildren(...L.ENERGY_SOURCES.map(([k, label, , title]) =>
      check(label, opts.sources.has(k), (on) => { on ? opts.sources.add(k) : opts.sources.delete(k); refresh(); }, title)));
  }

  function renderOwners() {
    const box = $h("heat-owners");
    const all = check("All players", opts.owners === null, (on) => {
      opts.owners = on ? null : new Set();
      renderOwners();
      refresh();
    }, "Every player's units");
    const items = [all];
    for (const p of S.players) {
      const id = Number(p.id);
      const sw = el("span", { className: "swatch" });
      sw.style.background = playerColor(id);
      const lab = check(p.name, opts.owners === null || opts.owners.has(id), (on) => {
        if (opts.owners === null) opts.owners = new Set(S.players.map((q) => Number(q.id)));
        on ? opts.owners.add(id) : opts.owners.delete(id);
        if (opts.owners.size === S.players.length) opts.owners = null;
        renderOwners();
        refresh();
      });
      cb0(lab).after(sw);
      items.push(lab);
    }
    items.push(check("Unowned", opts.neutral, (on) => { opts.neutral = on; refresh(); }, "Wells and wrecks (no owner)"));
    box.replaceChildren(...items);
  }

  const cb0 = (label) => label.querySelector("input");

  // summary says in one line what the map shows (the options are in a
  // dropdown so the map keeps the room).
  function renderSummary() {
    const src = L.ENERGY_SOURCES.filter(([k]) => opts.sources.has(k)).map(([, l]) => l.toLowerCase());
    let who = opts.owners === null ? "all players" : [...opts.owners].map(playerName).join(", ") || "no players";
    if (opts.neutral) who += " + unowned";
    const text = `${src.join(", ") || "no sources"} · ${who}`;
    const out = $h("heat-summary");
    out.textContent = text;
    out.title = text;
  }

  // Presets set sources and owners together.
  const PRESETS = {
    everything: () => { opts.sources = new Set(SOURCE_KEYS); opts.owners = null; opts.neutral = true; },
    players: () => { opts.sources = new Set(["stored", "embodied"]); opts.owners = null; opts.neutral = false; },
    resources: () => { opts.sources = new Set(["wells", "wrecks"]); opts.neutral = true; },
  };

  // ---- drawing ---------------------------------------------------------------

  function compute() {
    if (!S.world) return null;
    const g = L.energyGrid(S.entities, { ...opts, width: S.world.width, height: S.world.height });
    return L.spread(g, opts.spread);
  }

  function draw() {
    const cv = canvas();
    if (!cv || !S.world) return;
    const box = cv.parentElement;
    const w = box.clientWidth, h = box.clientHeight;
    if (!w || !h) return; // panel hidden (inactive tab): nothing to draw
    grid = compute();
    const dpr = window.devicePixelRatio || 1;
    if (cv.width !== Math.round(w * dpr) || cv.height !== Math.round(h * dpr)) {
      cv.width = Math.round(w * dpr);
      cv.height = Math.round(h * dpr);
      cv.style.width = w + "px";
      cv.style.height = h + "px";
    }
    const ctx = cv.getContext("2d");
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);
    const W = S.world.width, H = S.world.height;
    const tile = Math.max(1, Math.min(w / W, h / H));
    const ox = (w - W * tile) / 2, oy = (h - H * tile) / 2;
    view = { tile, ox, oy };
    ctx.fillStyle = "#11131a";
    ctx.fillRect(ox, oy, W * tile, H * tile);
    if (opts.terrain) {
      ctx.fillStyle = "#2b2f3a";
      for (const e of S.entities.values()) {
        if (e.kind !== "ENTITY_KIND_TERRAIN") continue;
        ctx.fillRect(ox + (e.position.x || 0) * tile, oy + (e.position.y || 0) * tile, tile, tile);
      }
    }
    // The heat layer: one pixel per cell, scaled up.
    const { cols, rows, values, max, cell } = grid;
    off.width = cols;
    off.height = rows;
    const img = off.getContext("2d").createImageData(cols, rows);
    for (let i = 0; i < values.length; i++) {
      const c = L.heatColor(L.heatScale(values[i], max, opts.scale));
      img.data.set(c, i * 4);
    }
    off.getContext("2d").putImageData(img, 0, 0);
    ctx.imageSmoothingEnabled = opts.smooth;
    ctx.drawImage(off, ox, oy, cols * cell * tile, rows * cell * tile);
    if (opts.units) {
      for (const e of S.entities.values()) {
        if (e.kind !== "ENTITY_KIND_UNIT") continue;
        ctx.fillStyle = playerColor(e.ownerId);
        const r = Math.max(1.5, tile * 0.22);
        ctx.beginPath();
        ctx.arc(ox + ((e.position.x || 0) + 0.5) * tile, oy + ((e.position.y || 0) + 0.5) * tile, r, 0, Math.PI * 2);
        ctx.fill();
      }
    }
    if (hover) {
      ctx.strokeStyle = "#ffffff";
      ctx.lineWidth = 1.5;
      ctx.strokeRect(ox + hover.cx * cell * tile, oy + hover.cy * cell * tile, cell * tile, cell * tile);
    }
    renderLegend();
    renderSummary();
    renderHover();
    renderedAt = performance.now();
  }
  let view = { tile: 1, ox: 0, oy: 0 };

  function renderLegend() {
    const g = grid;
    const unit = g.cell === 1 ? "tile" : `${g.cell}×${g.cell} tiles`;
    $h("heat-max").textContent = fmt(g.max) + (opts.spread ? " (spread)" : "");
    $h("heat-unit").textContent = `per ${unit}`;
    const parts = L.ENERGY_SOURCES.filter(([k]) => opts.sources.has(k)).map(([k, label]) => `${label.toLowerCase()} ${fmt(g.totals[k])}`);
    const t = $h("heat-totals");
    t.textContent = g.counted
      ? `Total ${fmt(g.total)} in ${g.counted} entities — ${parts.join(" · ")}`
      : "Nothing matches these options.";
    t.title = t.textContent;
  }

  // The hovered cell, explained from the raw (unspread) values.
  function renderHover() {
    const out = $h("heat-hover");
    if (!hover || !grid) {
      out.textContent = "Hover a cell for its breakdown; click to center the game board there.";
      return;
    }
    const c = grid.cell;
    const x0 = hover.cx * c, y0 = hover.cy * c;
    const bySource = {}, byOwner = new Map();
    let sum = 0, n = 0;
    for (const e of S.entities.values()) {
      const x = e.position && e.position.x || 0, y = e.position && e.position.y || 0;
      if (x < x0 || y < y0 || x >= x0 + c || y >= y0 + c) continue;
      const inc = L.energyIncluded(e, opts);
      if (!inc) continue;
      n++;
      for (const k in inc) {
        bySource[k] = (bySource[k] || 0) + inc[k];
        const o = Number(e.ownerId || 0);
        byOwner.set(o, (byOwner.get(o) || 0) + inc[k]);
        sum += inc[k];
      }
    }
    const where = c === 1 ? `(${x0},${y0})` : `(${x0},${y0})–(${x0 + c - 1},${y0 + c - 1})`;
    if (!n) {
      out.textContent = `${where}: no energy counted here` + (opts.spread ? " (color comes from spread neighbors)" : "");
      return;
    }
    const src = L.ENERGY_SOURCES.filter(([k]) => bySource[k]).map(([k, label]) => `${label.toLowerCase()} ${fmt(bySource[k])}`).join(", ");
    const own = [...byOwner].map(([o, v]) => `${o ? playerName(o) : "unowned"} ${fmt(v)}`).join(", ");
    out.textContent = `${where}: ${fmt(sum)} energy in ${n} entit${n === 1 ? "y" : "ies"} — ${src} — ${own}`;
    out.title = out.textContent;
  }

  function playerName(id) {
    const p = S.players.find((q) => Number(q.id) === Number(id));
    return p ? p.name : `player ${id}`;
  }

  // Redraws at most four times per second, and only while visible.
  function schedule() {
    if (queued) return;
    queued = true;
    const wait = Math.max(0, 250 - (performance.now() - renderedAt));
    setTimeout(() => RTS.nextFrame(() => { queued = false; draw(); }), wait);
  }
  function refresh() { renderedAt = 0; schedule(); }

  function cellAt(ev) {
    if (!grid) return null;
    const r = canvas().getBoundingClientRect();
    const x = (ev.clientX - r.left - view.ox) / view.tile, y = (ev.clientY - r.top - view.oy) / view.tile;
    if (x < 0 || y < 0 || x >= S.world.width || y >= S.world.height) return null;
    return { cx: Math.floor(x / grid.cell), cy: Math.floor(y / grid.cell) };
  }

  function init() {
    renderSources();
    for (const [id, key] of [["heat-cell", "cell"], ["heat-spread", "spread"]]) {
      const sel = $h(id);
      sel.value = String(opts[key]);
      sel.addEventListener("change", () => { opts[key] = Number(sel.value); hover = null; refresh(); });
    }
    const scale = $h("heat-scale");
    scale.value = opts.scale;
    scale.addEventListener("change", () => { opts.scale = scale.value; refresh(); });
    for (const [id, key] of [["heat-terrain", "terrain"], ["heat-units", "units"], ["heat-smooth", "smooth"]]) {
      const cb = $h(id);
      cb.checked = opts[key];
      cb.addEventListener("change", () => { opts[key] = cb.checked; refresh(); });
    }
    for (const b of $h("heat-presets").querySelectorAll("button[data-preset]")) {
      b.addEventListener("click", () => { PRESETS[b.dataset.preset](); renderSources(); renderOwners(); refresh(); });
    }
    // The options dropdown scrolls within the panel instead of being cut
    // off by its edge.
    const menu = $h("heat-options");
    menu.addEventListener("toggle", () => {
      if (!menu.open) return;
      const body = menu.querySelector(".heat-menu");
      const panel = menu.closest("section");
      const room = panel.getBoundingClientRect().bottom - body.getBoundingClientRect().top - 8;
      body.style.maxHeight = Math.max(120, room) + "px";
    });
    const cv = canvas();
    cv.addEventListener("mousemove", (ev) => {
      const c = cellAt(ev);
      if ((c && hover && c.cx === hover.cx && c.cy === hover.cy) || (!c && !hover)) return;
      hover = c;
      refresh();
    });
    cv.addEventListener("mouseleave", () => { hover = null; refresh(); });
    cv.addEventListener("click", (ev) => {
      const c = cellAt(ev);
      if (!c) return;
      const n = grid.cell;
      board.centerOn({ x0: c.cx * n, y0: c.cy * n, x1: c.cx * n + n - 1, y1: c.cy * n + n - 1 }, 6);
      say(`Game board centered on (${c.cx * n},${c.cy * n}) from the heat map`, "debug", { category: "admin", source: "heatmap" });
    });
    // Follow the panel's size (docking, resizing, maximizing, tab switches).
    if (window.ResizeObserver) new ResizeObserver(refresh).observe(cv.parentElement);
    Store.on("tick", schedule);
    Store.on("world", () => {
      // Players of a new match: keep "all players", drop ids that are gone.
      if (opts.owners) opts.owners = new Set([...opts.owners].filter((id) => S.players.some((p) => Number(p.id) === id)));
      renderOwners();
      refresh();
    });
    renderOwners();
  }

  init();
  return { opts, refresh, get grid() { return grid; } };
})();

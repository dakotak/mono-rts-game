// The dockable admin workspace, built on dockview (vendor/dockview, MIT).
//
// Panels are the <section data-panel> elements of index.html. Each element
// is created once and kept: dockview only re-parents it when a panel is
// docked, tabbed, floated or reopened, so panel state, inputs and the data
// they show (from Store) survive any layout change, and the observation
// stream is never touched by the layout.
//
// The layout (positions, sizes, tab groups, active tabs, which panels are
// open) is saved to localStorage on every change and restored on load.
// Saved layouts carry LAYOUT_VERSION and the panel ids known when saved;
// anything invalid or from another version falls back to the default
// layout, and panels added by a later update open in their default place.
"use strict";

const Workspace = (() => {
  const LAYOUT_VERSION = 1;
  const STORAGE_KEY = "mono.admin.workspace";
  // Below this width the default layout stacks panels as tabs instead of
  // side-by-side columns.
  const COMPACT_WIDTH = 1100;

  // The default arrangement is a plain constant (AdminLogic.DEFAULT_LAYOUT
  // in logic.js) so it is easy to adjust and is unit-tested.
  const { DEFAULT_LAYOUT, DEFAULT_ACTIVE } = AdminLogic;

  const sources = new Map(); // id -> {element, title}
  let api = null;
  let saveTimer = null;
  let restoring = false;

  function collectSources() {
    for (const sec of document.querySelectorAll("#panel-sources > section[data-panel]")) {
      sources.set(sec.dataset.panel, { element: sec, title: sec.dataset.title || sec.dataset.panel });
    }
  }
  const ids = () => [...sources.keys()];

  // A dockview content renderer that adopts the panel's persistent element.
  function createComponent({ name }) {
    const wrap = document.createElement("div");
    wrap.className = "pn-host";
    return {
      element: wrap,
      init() {
        const src = sources.get(name);
        if (src) wrap.append(src.element);
        else wrap.textContent = `Unknown panel "${name}"`;
      },
    };
  }

  // A maximize/restore button in every group's header.
  function createRightHeaderActionComponent(group) {
    const el = document.createElement("div");
    el.className = "ws-group-actions";
    const max = document.createElement("button");
    max.type = "button";
    max.className = "ws-icon";
    const sync = () => {
      const on = group.api.isMaximized();
      max.textContent = on ? "⤡" : "⤢";
      max.title = on ? "Restore layout" : "Maximize this panel group";
      max.setAttribute("aria-label", max.title);
    };
    max.addEventListener("click", () => {
      group.api.isMaximized() ? group.api.exitMaximized() : group.api.maximize();
      sync();
    });
    el.append(max);
    let sub = null;
    return {
      element: el,
      init(params) {
        sync();
        sub = params.containerApi.onDidMaximizedGroupChange(sync);
      },
      dispose() { if (sub) sub.dispose(); },
    };
  }

  function addPanel(spec) {
    const src = sources.get(spec.id);
    if (!src || api.getPanel(spec.id)) return null;
    const opts = { id: spec.id, component: spec.id, title: src.title };
    let pos = spec.position;
    // Fall back when the reference panel is closed: next to the board, or
    // as a new column on the right.
    if (pos && pos.referencePanel && !api.getPanel(pos.referencePanel)) {
      pos = api.getPanel("board") ? { referencePanel: "board", direction: "right" } : undefined;
    }
    if (pos) opts.position = pos;
    return api.addPanel(opts);
  }

  // applySizes sets group sizes once every panel is in place (adding a
  // panel redistributes space, so sizes set earlier would not stick).
  function applySizes(specs) {
    // Two passes: resizing one group gives or takes space from its
    // neighbours, so a second pass settles every requested size.
    for (let pass = 0; pass < 2; pass++) {
      for (const spec of specs) {
        const p = spec.size && api.getPanel(spec.id);
        if (!p) continue;
        try { p.group.api.setSize(spec.size); } catch (_) { /* sizes are best effort */ }
      }
    }
  }

  function defaultSpecs() {
    return window.innerWidth < COMPACT_WIDTH ? DEFAULT_LAYOUT.compact : DEFAULT_LAYOUT.wide;
  }

  function buildDefault() {
    restoring = true;
    api.clear();
    const specs = defaultSpecs();
    for (const spec of specs) if (spec.open !== false) addPanel(spec);
    applySizes(specs);
    for (const id of DEFAULT_ACTIVE) { const p = api.getPanel(id); if (p) p.api.setActive(); }
    restoring = false;
    save();
  }

  function save() {
    if (restoring || !api) return;
    clearTimeout(saveTimer);
    saveTimer = setTimeout(() => {
      try {
        localStorage.setItem(STORAGE_KEY, JSON.stringify({ version: LAYOUT_VERSION, layout: api.toJSON(), panels: ids() }));
      } catch (_) { /* storage full or disabled: the layout just is not remembered */ }
    }, 250);
  }

  function load() {
    let saved = null;
    try {
      saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || "null");
    } catch (_) {
      notice("Saved workspace layout is unreadable; using the default layout.");
      return false;
    }
    if (!saved) return false;
    const v = AdminLogic.validateLayout(saved, LAYOUT_VERSION, ids());
    if (!v.ok) {
      notice(`Saved workspace layout not used (${v.reason}); using the default layout.`);
      return false;
    }
    restoring = true;
    try {
      api.fromJSON(saved.layout);
    } catch (e) {
      restoring = false;
      api.clear();
      notice(`Saved workspace layout could not be restored (${e.message}); using the default layout.`);
      return false;
    }
    if (!api.getPanel("board") && !api.panels.length) { restoring = false; return false; }
    // Maximizing is temporary: a reload shows the whole workspace again.
    if (api.hasMaximizedGroup()) api.exitMaximizedGroup();
    // Panels introduced since the layout was saved open in their default place.
    for (const id of AdminLogic.newPanels(saved, ids())) {
      const spec = defaultSpecs().find((s) => s.id === id);
      if (spec && spec.open !== false) addPanel(spec);
    }
    restoring = false;
    return true;
  }

  function notice(text) {
    if (typeof say === "function") say(text, "warn", { category: "system", source: "workspace" });
  }

  // show opens a closed panel in its default place, or focuses it.
  function show(id) {
    if (!api) return;
    let p = api.getPanel(id);
    if (!p) {
      const spec = defaultSpecs().find((s) => s.id === id) || { id };
      p = addPanel({ ...spec, size: undefined });
    }
    if (p) p.api.setActive();
  }

  function hide(id) {
    const p = api && api.getPanel(id);
    if (p) api.removePanel(p);
  }

  function renderMenu() {
    const list = document.getElementById("panels-list");
    list.replaceChildren();
    for (const [id, src] of sources) {
      const label = document.createElement("label");
      const cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = !!(api && api.getPanel(id));
      cb.addEventListener("change", () => (cb.checked ? show(id) : hide(id)));
      label.append(cb, " " + src.title);
      list.append(label);
    }
  }

  function init() {
    if (api) return;
    collectSources();
    const root = document.getElementById("workspace");
    api = dockview.createDockview(root, {
      theme: dockview.themeDark,
      className: "mono-dock",
      createComponent,
      createRightHeaderActionComponent,
    });
    if (!load()) buildDefault();
    for (const ev of ["onDidLayoutChange", "onDidAddPanel", "onDidRemovePanel", "onDidActivePanelChange", "onDidMovePanel"]) {
      api[ev](() => { save(); renderMenu(); });
    }
    // The board also follows its container with a ResizeObserver; layout
    // events cover environments where observers are delayed.
    for (const ev of ["onDidLayoutChange", "onDidMaximizedGroupChange", "onDidActivePanelChange"]) {
      api[ev](() => board.queueResize());
    }
    document.getElementById("layout-reset").addEventListener("click", () => {
      buildDefault();
      renderMenu();
      document.getElementById("panels-menu").open = false;
    });
    renderMenu();
  }

  return {
    init, show, hide, buildDefault,
    get api() { return api; },
    LAYOUT_VERSION, STORAGE_KEY, DEFAULT_LAYOUT,
  };
})();

// Sign in automatically from a launcher link (http://host/#token=...; the
// fragment never reaches the server and is removed from the address bar at
// once) or from the session's saved token.
(() => {
  const hashToken = new URLSearchParams(location.hash.slice(1)).get("token");
  if (hashToken) {
    history.replaceState(null, "", location.pathname + location.search);
    login(hashToken);
    return;
  }
  try {
    const saved = sessionStorage.getItem("rts.admin");
    if (saved) login(saved);
  } catch (_) {}
})();
Logs.init();

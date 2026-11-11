// Logs panel: every admin message, command result and simulation event in a
// bounded client-side buffer (AdminLogic.LogBuffer), with severity, category,
// player, entity and text filters. Rendering is batched to one update per
// animation frame and incremental (new rows are appended), and shows at most
// MAX_ROWS rows, so heavy event traffic stays cheap. Following (auto-scroll)
// stops when the user scrolls up and resumes when they return to the bottom
// or press "Following". "Clear view" empties only this browser's buffer.
"use strict";

const Logs = (() => {
  const { LogBuffer, matchLog, filterLogs, SEVERITIES } = AdminLogic;
  const CAPACITY = 5000; // entries kept in memory
  const MAX_ROWS = 600; // rows rendered at once
  const buffer = new LogBuffer(CAPACITY);
  const $l = Store.el;
  const body = () => { const t = $l("log"); return t && t.tBodies[0]; };
  const scroller = () => $l("log-scroll");

  const filter = { severities: new Set(SEVERITIES), category: "", text: "", playerId: 0, entityId: "" };
  let following = true;
  let lastSeq = 0; // newest entry rendered
  let dirty = false; // new entries waiting for the next frame
  let full = true; // filters changed: rebuild instead of appending
  let categories = new Set();

  function add(entry) {
    const e = buffer.push({ tick: Store.state.tick, time: Date.now(), ...entry });
    if (!categories.has(e.category)) {
      categories.add(e.category);
      const sel = $l("log-cat");
      if (sel) sel.append(new Option(e.category, e.category));
    }
    schedule();
    return e;
  }

  function schedule() {
    if (dirty) return;
    dirty = true;
    RTS.nextFrame(flush);
  }

  function row(e) {
    const tr = document.createElement("tr");
    tr.className = "sev-" + e.severity;
    const cells = [e.tick, e.severity, e.category, e.source, e.message];
    for (let i = 0; i < cells.length; i++) {
      const td = document.createElement("td");
      td.textContent = cells[i];
      tr.append(td);
    }
    tr.title = new Date(e.time).toLocaleTimeString() + (e.entityId ? ` · entity ${e.entityId}` : "");
    if (e.entityId) {
      tr.classList.add("has-entity");
      tr.addEventListener("click", () => { if (Store.state.entities.has(e.entityId)) Store.select([e.entityId]); });
    }
    return tr;
  }

  function flush() {
    dirty = false;
    const tb = body();
    if (!tb) return;
    const sc = scroller();
    if (full) {
      full = false;
      const rows = filterLogs(buffer.entries(), filter, MAX_ROWS);
      tb.replaceChildren(...rows.map(row));
    } else {
      // Append entries newer than the last rendered one.
      const entries = buffer.entries();
      const frag = document.createDocumentFragment();
      for (let i = entries.length - 1; i >= 0 && entries[i].seq > lastSeq; i--) {
        if (matchLog(entries[i], filter)) frag.prepend(row(entries[i]));
      }
      tb.append(frag);
      // While the user reads older rows, keep them (up to a hard cap).
      const cap = following ? MAX_ROWS : MAX_ROWS * 3;
      while (tb.rows.length > cap) tb.deleteRow(0);
    }
    lastSeq = buffer.seq;
    $l("log-count").textContent = `${tb.rows.length} shown · ${buffer.size} kept` + (buffer.dropped ? ` · ${buffer.dropped} oldest dropped` : "");
    if (following) sc.scrollTop = sc.scrollHeight;
  }

  function refilter() {
    full = true;
    schedule();
  }

  function setFollowing(on) {
    following = on;
    const b = $l("log-follow");
    b.textContent = on ? "Following" : "Paused — jump to latest";
    b.setAttribute("aria-pressed", String(on));
    if (on) scroller().scrollTop = scroller().scrollHeight;
  }

  function init() {
    const sev = $l("log-sev");
    for (const s of SEVERITIES) {
      const l = document.createElement("label");
      l.className = "sev-" + s;
      const cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = true;
      cb.addEventListener("change", () => { cb.checked ? filter.severities.add(s) : filter.severities.delete(s); refilter(); });
      l.append(cb, " " + s);
      sev.append(l);
    }
    $l("log-cat").addEventListener("change", (ev) => { filter.category = ev.target.value; refilter(); });
    $l("log-player").addEventListener("change", (ev) => { filter.playerId = Number(ev.target.value) || 0; refilter(); });
    $l("log-search").addEventListener("input", (ev) => { filter.text = ev.target.value.trim(); refilter(); });
    const ent = $l("log-entity");
    const syncEntity = () => {
      const one = Store.state.selected.size === 1 ? [...Store.state.selected][0] : "";
      const want = ent.checked ? one : "";
      if (want !== filter.entityId) { filter.entityId = want; refilter(); }
    };
    ent.addEventListener("change", syncEntity);
    Store.on("selection", syncEntity);
    $l("log-follow").addEventListener("click", () => setFollowing(!following));
    $l("log-clear").addEventListener("click", () => { buffer.clear(); refilter(); });
    scroller().addEventListener("scroll", () => {
      const sc = scroller();
      const atBottom = sc.scrollHeight - sc.scrollTop - sc.clientHeight < 8;
      if (atBottom !== following) setFollowing(atBottom);
    });
    Store.on("world", () => {
      const sel = $l("log-player");
      const cur = sel.value;
      while (sel.options.length > 1) sel.remove(1);
      for (const p of Store.state.players) sel.append(new Option(p.name, p.id));
      sel.value = cur;
      // A filter on a player who is not in this match (after a reset or a
      // server restart) would hide every entry: drop it.
      if (filter.playerId && !Store.state.players.some((p) => Number(p.id) === filter.playerId)) {
        sel.value = "";
        filter.playerId = 0;
        refilter();
      }
    });
  }

  return { add, init, buffer, filter, flushNow: flush };
})();

// say logs an admin-side message (command outcomes, errors, notices).
// cls is the severity ("error", "ok", "info", "warn").
function say(text, cls = "info", extra = {}) {
  Logs.add({ severity: cls || "info", category: extra.category || "admin", source: extra.source || "admin", message: text, ...extra });
}

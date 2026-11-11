// Shared client code for the player and admin UIs: grid rendering, pointer
// input (click, box select, right click), entity attribute helpers and a
// reconnecting NDJSON stream reader. Exposed as window.RTS.
"use strict";

window.RTS = (() => {
  // ---- attributes ---------------------------------------------------------

  const attr = (e, key) => (e.attributes ? e.attributes[key] : undefined);
  const attrInt = (e, key) => {
    const v = attr(e, key);
    return v && v.intValue !== undefined ? Number(v.intValue) : undefined;
  };
  const attrBool = (e, key) => {
    const v = attr(e, key);
    return v && v.boolValue !== undefined ? v.boolValue : undefined;
  };
  const attrPos = (e, key) => {
    const v = attr(e, key);
    return v && v.positionValue ? { x: v.positionValue.x || 0, y: v.positionValue.y || 0 } : undefined;
  };

  const isWell = (e) => e.kind === "ENTITY_KIND_RESOURCE" && attr(e, "wells.capacity") !== undefined;
  const isWreck = (e) => e.kind === "ENTITY_KIND_RESOURCE" && attr(e, "salvage.energy") !== undefined;
  const isUnit = (e) => e.kind === "ENTITY_KIND_UNIT";

  // Entity IDs are large opaque numbers; show a short tag.
  const tag = (id) => "#" + BigInt(id).toString(16).slice(-5);
  const fmtPos = (p) => (p ? `(${p.x || 0},${p.y || 0})` : "?");

  // Formats a protojson Value for display.
  function fmtValue(v) {
    if (!v) return "";
    if (v.intValue !== undefined) return String(v.intValue);
    if (v.boolValue !== undefined) return String(v.boolValue);
    if (v.stringValue !== undefined) return v.stringValue;
    if (v.positionValue) return fmtPos(v.positionValue);
    if (v.rangeValue) return `≈${v.rangeValue.low || 0}–${v.rangeValue.high || 0}`;
    return JSON.stringify(v);
  }

  // ---- streaming ----------------------------------------------------------

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

  // stream keeps an NDJSON observation stream open, reconnecting with
  // backoff. Every (re)connection starts with a full snapshot, so client
  // state always recovers. Returns a function that stops the loop.
  function stream({ url, token, onMessage, onStatus, onUnauthorized }) {
    let stopped = false;
    let abort = null;
    (async () => {
      let delay = 500;
      while (!stopped) {
        abort = new AbortController();
        try {
          const res = await fetch(url, { headers: { Authorization: "Bearer " + token }, signal: abort.signal });
          if (res.status === 401) { onUnauthorized(); return; }
          if (!res.ok) throw new Error("HTTP " + res.status);
          onStatus("live");
          delay = 500;
          await readLines(res.body, onMessage);
          throw new Error("stream ended");
        } catch (e) {
          if (stopped) return;
          onStatus("reconnecting");
          await new Promise((r) => setTimeout(r, delay));
          delay = Math.min(delay * 2, 5000);
        }
      }
    })();
    return () => { stopped = true; if (abort) abort.abort(); };
  }

  // Applies an ObserveResponse-shaped message to a Map of entities.
  // Returns the tick it carries.
  function applyMessage(entities, msg) {
    if (msg.snapshot) {
      entities.clear();
      for (const e of msg.snapshot.entities) entities.set(e.id, e);
      return Number(msg.snapshot.tick);
    }
    const u = msg.update;
    for (const e of u.changed) entities.set(e.id, e);
    for (const id of u.removed) entities.delete(id);
    return Number(u.tick);
  }

  // nextFrame runs fn on the next animation frame, or after 100 ms when
  // frames are not being produced (hidden or throttled pages), once.
  function nextFrame(fn) {
    let done = false;
    const run = () => { if (!done) { done = true; fn(); } };
    requestAnimationFrame(run);
    setTimeout(run, 100);
  }

  // ---- board ----------------------------------------------------------------

  const DRAG_THRESHOLD = 4;

  // Board renders a grid world on a canvas and turns pointer input into
  // tile-level callbacks:
  //   onClick(tile, {shift}), onBox(entitiesInBox, {shift}), onContext(tile)
  //
  // With handlers.zoom (the admin workspace) the canvas fills its container
  // and the view can be zoomed (wheel, about the cursor), panned (middle
  // button or Alt+drag) and centered (centerOn); fit() shows the whole world.
  // Without it (player client) the whole world is always fitted, as before.
  // The board follows its container's size (ResizeObserver), so it works in
  // resizable panels; resizes are coalesced to one per animation frame.
  class Board {
    constructor(canvas, container, handlers) {
      this.canvas = canvas;
      this.container = container;
      this.ctx = canvas.getContext("2d");
      this.handlers = handlers;
      this.zoomable = !!handlers.zoom;
      this.width = 1;
      this.height = 1;
      this.tile = 24;
      this.base = 24; // tile size that fits the whole world
      this.zoom = 1;
      this.ox = 0; // view offset in world pixels (zoomable boards)
      this.oy = 0;
      this.drag = null;
      this.pan = null;
      this.hover = null;
      this.state = null;
      this._bind();
      this._resizeQueued = false;
      const queue = () => {
        if (this._resizeQueued) return;
        this._resizeQueued = true;
        nextFrame(() => { this._resizeQueued = false; this.resize(); });
      };
      this.queueResize = queue; // for layout managers that know of size changes
      window.addEventListener("resize", queue);
      if (typeof ResizeObserver !== "undefined") new ResizeObserver(queue).observe(container);
    }

    setWorld(width, height) {
      const changed = width !== this.width || height !== this.height;
      this.width = width;
      this.height = height;
      if (changed) { this.zoom = 1; this.ox = this.oy = 0; }
      this.resize();
    }

    resize() {
      const pad = this.zoomable ? 0 : 24;
      const availW = Math.max(1, this.container.clientWidth - pad);
      const availH = Math.max(1, this.container.clientHeight - pad);
      if (availW <= 1 || availH <= 1) return; // hidden (e.g. an inactive tab)
      const dpr = window.devicePixelRatio || 1;
      const c = this.canvas;
      if (this.zoomable) {
        this.base = Math.max(2, Math.min(availW / this.width, availH / this.height));
        this.tile = this.base * this.zoom;
        this.viewW = availW;
        this.viewH = availH;
        c.style.width = availW + "px";
        c.style.height = availH + "px";
        c.width = Math.round(availW * dpr);
        c.height = Math.round(availH * dpr);
        this._clamp();
        if (this._pendingCenter) {
          this.dpr = dpr;
          this.centerOn(...this._pendingCenter); // redraws
          return;
        }
      } else {
        this.tile = Math.max(6, Math.floor(Math.min(availW / this.width, availH / this.height)));
        c.style.width = this.width * this.tile + "px";
        c.style.height = this.height * this.tile + "px";
        c.width = this.width * this.tile * dpr;
        c.height = this.height * this.tile * dpr;
      }
      this.dpr = dpr;
      this.redraw();
    }

    // _clamp keeps a zoomed view over the world (and centers a world smaller
    // than the view).
    _clamp() {
      const W = this.width * this.tile, H = this.height * this.tile;
      this.ox = W <= this.viewW ? -(this.viewW - W) / 2 : Math.max(0, Math.min(this.ox, W - this.viewW));
      this.oy = H <= this.viewH ? -(this.viewH - H) / 2 : Math.max(0, Math.min(this.oy, H - this.viewH));
    }

    // setZoom zooms to z (1 = whole world) keeping the world point under
    // the screen point (sx, sy) in place.
    setZoom(z, sx = this.viewW / 2, sy = this.viewH / 2) {
      if (!this.zoomable) return;
      z = Math.max(1, Math.min(16, z));
      const wx = (sx + this.ox) / this.tile, wy = (sy + this.oy) / this.tile;
      this.zoom = z;
      this.tile = this.base * z;
      this.ox = wx * this.tile - sx;
      this.oy = wy * this.tile - sy;
      this._clamp();
      this.redraw();
      if (this.handlers.onView) this.handlers.onView(this.view());
    }

    // fit shows the whole world.
    fit() { this.setZoom(1); }

    // centerOn zooms to show the tile rectangle {x0,y0,x1,y1} (with a margin)
    // and centers it.
    centerOn(b, margin = 4) {
      if (!this.zoomable || !b) return;
      // Not laid out yet (its panel has been hidden since load): center
      // once it is shown.
      if (!(this.viewW > 0 && this.viewH > 0 && this.base > 0)) {
        this._pendingCenter = [b, margin];
        return;
      }
      this._pendingCenter = null;
      const w = b.x1 - b.x0 + 1 + 2 * margin, h = b.y1 - b.y0 + 1 + 2 * margin;
      const z = Math.max(1, Math.min(16, Math.min(this.viewW / (w * this.base), this.viewH / (h * this.base))));
      this.zoom = z;
      this.tile = this.base * z;
      this.ox = ((b.x0 + b.x1 + 1) / 2) * this.tile - this.viewW / 2;
      this.oy = ((b.y0 + b.y1 + 1) / 2) * this.tile - this.viewH / 2;
      this._clamp();
      this.redraw();
      if (this.handlers.onView) this.handlers.onView(this.view());
    }

    view() { return { zoom: this.zoom, x: this.ox / this.tile, y: this.oy / this.tile }; }

    // point returns the pointer position in world pixels.
    point(ev) {
      const r = this.canvas.getBoundingClientRect();
      const x = ev.clientX - r.left, y = ev.clientY - r.top;
      return this.zoomable ? { x: x + this.ox, y: y + this.oy, sx: x, sy: y } : { x, y };
    }

    toTile(p) {
      const x = Math.floor(p.x / this.tile), y = Math.floor(p.y / this.tile);
      return x >= 0 && y >= 0 && x < this.width && y < this.height ? { x, y } : null;
    }

    _bind() {
      const c = this.canvas;
      c.addEventListener("pointerdown", (ev) => {
        if (this.zoomable && (ev.button === 1 || (ev.button === 0 && ev.altKey))) {
          ev.preventDefault();
          this.pan = { x: ev.clientX, y: ev.clientY, ox: this.ox, oy: this.oy };
          c.setPointerCapture(ev.pointerId);
          return;
        }
        if (ev.button !== 0) return;
        const p = this.point(ev);
        this.drag = { x0: p.x, y0: p.y, x1: p.x, y1: p.y, active: false, shift: ev.shiftKey };
        c.setPointerCapture(ev.pointerId);
      });
      c.addEventListener("pointermove", (ev) => {
        if (this.pan) {
          this.ox = this.pan.ox - (ev.clientX - this.pan.x);
          this.oy = this.pan.oy - (ev.clientY - this.pan.y);
          this._clamp();
          this.redraw();
          return;
        }
        const p = this.point(ev);
        const t = this.toTile(p);
        if (this.handlers.onHover && (t ? `${t.x},${t.y}` : "") !== (this.hover ? `${this.hover.x},${this.hover.y}` : "")) {
          this.handlers.onHover(t);
        }
        this.hover = t;
        if (this.drag) {
          this.drag.x1 = p.x;
          this.drag.y1 = p.y;
          if (Math.hypot(this.drag.x1 - this.drag.x0, this.drag.y1 - this.drag.y0) > DRAG_THRESHOLD) this.drag.active = true;
        }
        this.redraw();
      });
      c.addEventListener("pointerleave", () => {
        this.hover = null;
        if (this.handlers.onHover) this.handlers.onHover(null);
        this.redraw();
      });
      c.addEventListener("pointerup", (ev) => {
        if (this.pan) {
          this.pan = null;
          if (this.handlers.onView) this.handlers.onView(this.view());
          return;
        }
        if (ev.button !== 0 || !this.drag) return;
        const d = this.drag;
        this.drag = null;
        if (d.active) {
          const x0 = Math.min(d.x0, d.x1), x1 = Math.max(d.x0, d.x1);
          const y0 = Math.min(d.y0, d.y1), y1 = Math.max(d.y0, d.y1);
          const inside = [];
          for (const e of this.state ? this.state.entities.values() : []) {
            const cx = (e.position.x + 0.5) * this.tile, cy = (e.position.y + 0.5) * this.tile;
            if (cx >= x0 && cx <= x1 && cy >= y0 && cy <= y1) inside.push(e);
          }
          this.handlers.onBox && this.handlers.onBox(inside, { shift: d.shift });
        } else {
          const t = this.toTile(this.point(ev));
          if (t && this.handlers.onClick) this.handlers.onClick(t, { shift: d.shift });
        }
        this.redraw();
      });
      if (this.zoomable) {
        c.addEventListener("wheel", (ev) => {
          ev.preventDefault();
          const r = c.getBoundingClientRect();
          this.setZoom(this.zoom * Math.exp(-ev.deltaY * 0.0015), ev.clientX - r.left, ev.clientY - r.top);
        }, { passive: false });
        c.addEventListener("auxclick", (ev) => { if (ev.button === 1) ev.preventDefault(); });
      }
      c.addEventListener("contextmenu", (ev) => {
        ev.preventDefault();
        const t = this.toTile(this.point(ev));
        if (t && this.handlers.onContext) this.handlers.onContext(t);
      });
    }

    // entityAt returns the entity at a tile, preferring units over
    // passable entities (wreckage) lying on the same tile.
    entityAt(x, y) {
      if (!this.state) return null;
      let passable = null;
      for (const e of this.state.entities.values()) {
        if (e.position.x !== x || e.position.y !== y) continue;
        if (!e.passable) return e;
        passable = passable || e;
      }
      return passable;
    }

    // state: {entities: Map, selected: Set, colorFor(e), mine(e) -> bool,
    //         lines: [{from:{x,y}, to:{x,y}, color, cross}],
    //         floaters: [{x, y, text, color, alpha}],
    //         fog: {visible: Set("x,y"), explored: Set("x,y")} | null,
    //         ghosts: [{entity, seenTick}] (last-known foreign entities),
    //         ranges: [{x, y, r, metric}] (vision overlay)}
    draw(state) {
      this.state = state;
      this.redraw();
    }

    redraw() {
      const s = this.state;
      const ctx = this.ctx, T = this.tile;
      const W = this.width * T, H = this.height * T;
      const css = getComputedStyle(document.documentElement);
      const dpr = this.dpr || window.devicePixelRatio || 1;
      if (this.zoomable) {
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
        ctx.fillStyle = css.getPropertyValue("--bg");
        ctx.fillRect(0, 0, this.viewW || 0, this.viewH || 0);
        ctx.setTransform(dpr, 0, 0, dpr, -this.ox * dpr, -this.oy * dpr);
      } else {
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      }
      ctx.fillStyle = css.getPropertyValue("--tile");
      ctx.fillRect(0, 0, W, H);
      ctx.strokeStyle = css.getPropertyValue("--grid");
      ctx.lineWidth = 1;
      ctx.beginPath();
      for (let x = 0; x <= this.width; x++) { ctx.moveTo(x * T + 0.5, 0); ctx.lineTo(x * T + 0.5, H); }
      for (let y = 0; y <= this.height; y++) { ctx.moveTo(0, y * T + 0.5); ctx.lineTo(W, y * T + 0.5); }
      ctx.stroke();
      if (this.hover) {
        ctx.fillStyle = "rgba(255,255,255,0.06)";
        ctx.fillRect(this.hover.x * T, this.hover.y * T, T, T);
      }
      if (!s) return;

      // Order lines (destinations, mining targets, transfers).
      ctx.setLineDash([4, 4]);
      ctx.lineWidth = 1;
      for (const l of s.lines || []) {
        ctx.strokeStyle = l.color;
        ctx.beginPath();
        ctx.moveTo((l.from.x + 0.5) * T, (l.from.y + 0.5) * T);
        ctx.lineTo((l.to.x + 0.5) * T, (l.to.y + 0.5) * T);
        ctx.stroke();
        if (l.cross) {
          const cx = (l.to.x + 0.5) * T, cy = (l.to.y + 0.5) * T, k = T * 0.15;
          ctx.beginPath();
          ctx.moveTo(cx - k, cy - k); ctx.lineTo(cx + k, cy + k);
          ctx.moveTo(cx + k, cy - k); ctx.lineTo(cx - k, cy + k);
          ctx.stroke();
        }
      }
      ctx.setLineDash([]);

      // Passable entities (wreckage) first, so units standing on them stay
      // visible.
      const ordered = [...s.entities.values()].sort((a, b) => (b.passable ? 1 : 0) - (a.passable ? 1 : 0));
      for (const e of ordered) {
        const cx = (e.position.x + 0.5) * T, cy = (e.position.y + 0.5) * T;
        if (isWell(e)) this._drawWell(e, cx, cy);
        else if (isWreck(e)) this._drawWreck(e, cx, cy);
        else this._drawUnit(e, cx, cy, s);
        if (s.selected && s.selected.has(e.id)) {
          ctx.lineWidth = 2;
          ctx.strokeStyle = css.getPropertyValue("--select");
          ctx.beginPath();
          ctx.arc(cx, cy, T * 0.36 + 3, 0, Math.PI * 2);
          ctx.stroke();
        }
      }

      // Fog of war: never explored (dark), explored but not visible (dim).
      if (s.fog) {
        for (let y = 0; y < this.height; y++) {
          for (let x = 0; x < this.width; x++) {
            const k = x + "," + y;
            if (s.fog.visible.has(k)) continue;
            ctx.fillStyle = s.fog.explored.has(k) ? "rgba(10,11,14,0.55)" : "rgba(5,6,8,0.92)";
            ctx.fillRect(x * T, y * T, T, T);
          }
        }
      }
      // Last-known positions: faded dashed outlines, never mistaken for
      // live entities.
      for (const g of s.ghosts || []) {
        const e = g.entity;
        const cx = (e.position.x + 0.5) * T, cy = (e.position.y + 0.5) * T, r = T * 0.32;
        ctx.save();
        ctx.globalAlpha = 0.6;
        ctx.setLineDash([2, 2]);
        ctx.lineWidth = 1.5;
        ctx.strokeStyle = isWell(e) ? "#e8c23a" : isWreck(e) ? "#c9733a" : s.colorFor(e);
        ctx.beginPath();
        if (isWell(e)) { ctx.moveTo(cx, cy - r); ctx.lineTo(cx + r, cy); ctx.lineTo(cx, cy + r); ctx.lineTo(cx - r, cy); ctx.closePath(); }
        else ctx.rect(cx - r, cy - r, 2 * r, 2 * r);
        ctx.stroke();
        ctx.restore();
      }
      // Vision coverage overlay (own units only).
      for (const v of s.ranges || []) {
        ctx.save();
        ctx.strokeStyle = "rgba(120,180,255,0.35)";
        ctx.lineWidth = 1;
        ctx.beginPath();
        const cx = (v.x + 0.5) * T, cy = (v.y + 0.5) * T, R = (v.r + 0.5) * T;
        if (v.metric === "manhattan") { ctx.moveTo(cx, cy - R); ctx.lineTo(cx + R, cy); ctx.lineTo(cx, cy + R); ctx.lineTo(cx - R, cy); ctx.closePath(); }
        else if (v.metric === "euclidean") ctx.arc(cx, cy, R, 0, Math.PI * 2);
        else ctx.rect(cx - R, cy - R, 2 * R, 2 * R);
        ctx.stroke();
        ctx.restore();
      }

      // Floating labels (e.g. energy changes), drawn above units.
      ctx.textAlign = "center";
      ctx.font = `600 ${Math.max(10, Math.round(T * 0.5))}px system-ui, sans-serif`;
      for (const f of s.floaters || []) {
        ctx.globalAlpha = f.alpha;
        ctx.fillStyle = f.color;
        ctx.fillText(f.text, (f.x + 0.5) * T, (f.y + 0.1) * T - (1 - f.alpha) * T * 0.6);
      }
      ctx.globalAlpha = 1;

      const d = this.drag;
      if (d && d.active) {
        const x = Math.min(d.x0, d.x1), y = Math.min(d.y0, d.y1);
        const w = Math.abs(d.x1 - d.x0), h = Math.abs(d.y1 - d.y0);
        ctx.fillStyle = "rgba(255,216,77,0.08)";
        ctx.fillRect(x, y, w, h);
        ctx.strokeStyle = css.getPropertyValue("--select");
        ctx.lineWidth = 1;
        ctx.strokeRect(x + 0.5, y + 0.5, w, h);
      }
    }

    // Units: own units are circles, others squares; colour = owner. Units
    // whose energy is visible get a small energy bar.
    _drawUnit(e, cx, cy, s) {
      const ctx = this.ctx, T = this.tile, r = T * 0.36;
      // highlight(e): true = emphasized, false = dimmed, undefined = normal.
      const hl = s.highlight ? s.highlight(e) : undefined;
      ctx.save();
      if (hl === false) ctx.globalAlpha = 0.25;
      if (hl === true) {
        ctx.strokeStyle = "rgba(255,255,255,0.85)";
        ctx.lineWidth = Math.max(1.5, T * 0.08);
        ctx.beginPath();
        ctx.arc(cx, cy, r + Math.max(3, T * 0.18), 0, Math.PI * 2);
        ctx.stroke();
      }
      ctx.fillStyle = s.colorFor(e);
      ctx.beginPath();
      if (s.mine(e)) ctx.arc(cx, cy, r, 0, Math.PI * 2);
      else ctx.rect(cx - r * 0.9, cy - r * 0.9, r * 1.8, r * 1.8);
      ctx.fill();
      ctx.lineWidth = 1.5;
      ctx.strokeStyle = "rgba(0,0,0,0.5)";
      ctx.stroke();
      const energy = attrInt(e, "energy.stored"), cap = attrInt(e, "energy.capacity");
      if (energy !== undefined && cap) this._bar(cx, cy + r + 2, energy / cap, "#f2d24b");
      // Integrity (public): only drawn once damaged, green -> red.
      const hp = attrInt(e, "combat.health"), maxHp = attrInt(e, "combat.max_health");
      if (hp !== undefined && maxHp && hp < maxHp) {
        const f = hp / maxHp;
        this._bar(cx, cy - r - 2 - Math.max(2, this.tile * 0.08), f, f > 0.5 ? "#7bd88f" : f > 0.25 ? "#e0a43a" : "#e05252");
      }
      // Progress rings: upgrades (cyan) and cloning (magenta, dashed when
      // waiting for a free tile).
      const ring = (done, total, color, dashed) => {
        if (!total) return;
        ctx.save();
        ctx.lineWidth = Math.max(2, this.tile * 0.1);
        ctx.strokeStyle = color;
        if (dashed) ctx.setLineDash([3, 3]);
        ctx.beginPath();
        ctx.arc(cx, cy, r + 5, -Math.PI / 2, -Math.PI / 2 + 2 * Math.PI * Math.min(1, done / total));
        ctx.stroke();
        ctx.restore();
      };
      ring(attrInt(e, "upgrades.done"), attrInt(e, "upgrades.duration"), "#4fd1e8");
      ring(attrInt(e, "cloning.done"), attrInt(e, "cloning.duration"), "#e06bd8", attrBool(e, "cloning.blocked"));
      ctx.restore();
    }

    // Wells: a diamond whose fill shows remaining energy; unlimited wells
    // are drawn full with a bright outline.
    _drawWell(e, cx, cy) {
      const ctx = this.ctx, r = this.tile * 0.42;
      const energy = attrInt(e, "wells.energy") || 0, cap = attrInt(e, "wells.capacity") || 0;
      const unlimited = attrBool(e, "wells.unlimited");
      const frac = unlimited ? 1 : cap ? energy / cap : 0;
      ctx.beginPath();
      ctx.moveTo(cx, cy - r); ctx.lineTo(cx + r, cy); ctx.lineTo(cx, cy + r); ctx.lineTo(cx - r, cy); ctx.closePath();
      ctx.fillStyle = "#3a3320";
      ctx.fill();
      ctx.save();
      ctx.clip();
      ctx.fillStyle = "#e8c23a";
      ctx.fillRect(cx - r, cy + r - 2 * r * frac, 2 * r, 2 * r * frac);
      ctx.restore();
      ctx.lineWidth = unlimited ? 2 : 1;
      ctx.strokeStyle = unlimited ? "#fff3b0" : "#8a7a3a";
      ctx.stroke();
    }

    // Wreckage: a dark crossed square whose orange fill shows the energy
    // left to salvage.
    _drawWreck(e, cx, cy) {
      const ctx = this.ctx, r = this.tile * 0.3;
      const left = attrInt(e, "salvage.energy") || 0, initial = attrInt(e, "salvage.initial") || left || 1;
      ctx.fillStyle = "#2a2522";
      ctx.fillRect(cx - r, cy - r, 2 * r, 2 * r);
      const f = Math.max(0, Math.min(1, left / initial));
      ctx.fillStyle = "#c9733a";
      ctx.fillRect(cx - r, cy + r - 2 * r * f, 2 * r, 2 * r * f);
      ctx.strokeStyle = "#8a8078";
      ctx.lineWidth = 1.5;
      ctx.beginPath();
      ctx.moveTo(cx - r, cy - r); ctx.lineTo(cx + r, cy + r);
      ctx.moveTo(cx + r, cy - r); ctx.lineTo(cx - r, cy + r);
      ctx.rect(cx - r, cy - r, 2 * r, 2 * r);
      ctx.stroke();
    }

    _bar(cx, y, frac, color) {
      const ctx = this.ctx, w = this.tile * 0.7, h = Math.max(2, this.tile * 0.08);
      const f = Math.max(0, Math.min(1, frac));
      ctx.fillStyle = "rgba(0,0,0,0.6)";
      ctx.fillRect(cx - w / 2, y, w, h);
      ctx.fillStyle = color;
      ctx.fillRect(cx - w / 2, y, w * f, h);
    }
  }

  // Appends a line to a <ol> message log, newest first.
  function log(ol, tick, text, cls = "") {
    const li = document.createElement("li");
    li.className = cls;
    const t = document.createElement("span");
    t.className = "t";
    t.textContent = tick;
    li.append(t, text);
    ol.prepend(li);
    while (ol.children.length > 50) ol.lastChild.remove();
  }

  return { attr, attrInt, attrBool, attrPos, isWell, isWreck, isUnit, tag, fmtPos, fmtValue, stream, applyMessage, Board, log, nextFrame };
})();

// Milestone 3 player UI: attributes, upgrades, cloning, metadata, filters
// and batch commands. Loaded after app.js and uses its state (session,
// entities, selected, submit, say, render). Costs shown here are exact:
// upgrade costs come from the cost/duration tables the server publishes in
// WorldInfo.rules_json, clone costs from the per-unit quote the server
// maintains. The client never evaluates formulas itself.
"use strict";

const UPGRADE_TYPE = "type.googleapis.com/rts.upgrades.v1.UpgradeCommand";
const CLONE_TYPE = "type.googleapis.com/rts.cloning.v1.CloneCommand";
const SET_META_TYPE = "type.googleapis.com/rts.metadata.v1.SetMetadataCommand";
const REMOVE_META_TYPE = "type.googleapis.com/rts.metadata.v1.RemoveMetadataCommand";
const BATCH_TYPE = "type.googleapis.com/rts.metadata.v1.BatchCommand";

let rules = {};
let attrDefs = []; // [{name, min, max, cost_table, duration_table, description}]

function loadRules() {
  try { rules = JSON.parse((session && session.world.rulesJson) || "{}"); } catch (_) { rules = {}; }
  attrDefs = (rules.attributes && rules.attributes.attributes) || [];
  const sel = $("batch-attr");
  sel.replaceChildren(...attrDefs.map((d) => new Option(d.name, d.name)));
}

// upgradeQuote returns the exact cost and duration of raising an attribute.
function upgradeQuote(def, from, to) {
  if (to <= from || to > def.max || from < def.min) return null;
  const c = def.cost_table, d = def.duration_table;
  return { cost: c[to - def.min] - c[from - def.min], ticks: Math.max(1, d[to - def.min] - d[from - def.min]) };
}

const val = (e, key) => attrInt(e, key);
const busyOf = (e) => { const v = attr(e, "attributes.busy"); return v && v.stringValue; };

// ---- metadata filter ----------------------------------------------------------

// parseFilter turns "squad=alpha, role, !tired" into server-style conditions.
function parseFilter(text) {
  return text.split(",").map((s) => s.trim()).filter(Boolean).map((t) => {
    if (t.startsWith("!")) return { key: t.slice(1).trim(), match: "MATCH_KIND_ABSENT" };
    const i = t.indexOf("=");
    if (i < 0) return { key: t, match: "MATCH_KIND_PRESENT" };
    return { key: t.slice(0, i).trim(), value: t.slice(i + 1).trim(), match: "MATCH_KIND_EQUALS" };
  });
}

function matchesFilter(e, conds) {
  return conds.every((c) => {
    const v = attr(e, "metadata." + c.key);
    const s = v && v.stringValue;
    if (c.match === "MATCH_KIND_ABSENT") return v === undefined;
    if (c.match === "MATCH_KIND_PRESENT") return v !== undefined;
    return s === c.value;
  });
}

function filterMatches() {
  const conds = parseFilter($("filter").value);
  return [...entities.values()].filter((e) => isMine(e) && matchesFilter(e, conds));
}

// ---- commands -------------------------------------------------------------------

function batch(selector, template, description) {
  submit({ "@type": BATCH_TYPE, selector, command: template }, description);
}

function upgradeOne(id, attrName, to) {
  submit({ "@type": UPGRADE_TYPE, unitIds: [id], attribute: attrName, target: String(to) }, `Upgrade ${tag(id)} ${attrName} → ${to}`);
}

function cloneOne(id) {
  submit({ "@type": CLONE_TYPE, unitIds: [id] }, `Clone ${tag(id)}`);
}

function setMeta(ids, key, value) {
  if (!ids.length || !key) return;
  submit({ "@type": SET_META_TYPE, unitIds: ids, key, value }, `Set ${key}=${value} on ${ids.length} unit(s)`);
}

function removeMeta(ids, key) {
  if (!ids.length || !key) return;
  submit({ "@type": REMOVE_META_TYPE, unitIds: ids, key }, `Remove ${key} from ${ids.length} unit(s)`);
}

// ---- result and event hooks (called from app.js) -------------------------------

// evolutionResult summarizes batch results; returns true if it handled r.
function evolutionResult(r, desc) {
  const units = r.unitResults || [];
  if (!units.length) return false;
  const ok = units.filter((u) => u.ok).length;
  const fails = {};
  for (const u of units) if (!u.ok) fails[u.errorCode] = (fails[u.errorCode] || 0) + 1;
  const why = Object.entries(fails).map(([k, n]) => `${n}× ${k}`).join(", ");
  const cls = r.status === "COMMAND_STATUS_SUCCEEDED" ? "info" : "error";
  say(`${desc}: ${ok}/${units.length} unit(s) succeeded${why ? " — " + why : ""}`, cls);
  return true;
}

function evolutionEvent(ev) {
  const f = ev.fields || {};
  const int = (k) => Number((f[k] && f[k].intValue) || 0);
  const str = (k) => (f[k] && f[k].stringValue) || "";
  switch (ev.type) {
    case "upgrades.started":
      floatAt(ev.entityId, "−" + int("cost"), "#4fd1e8");
      say(`Unit ${tag(ev.entityId)}: upgrading ${str("attribute")} ${int("from")}→${int("to")} (${int("cost")} energy, ${int("duration")} ticks)`, "info");
      return true;
    case "upgrades.completed":
      say(`Unit ${tag(ev.entityId)}: ${str("attribute")} is now ${int("value")}`, "info");
      return true;
    case "upgrades.failed":
      say(`Unit ${tag(ev.entityId)}: upgrade of ${str("attribute")} failed (${str("reason")}): ${str("message")}`, "error");
      return true;
    case "cloning.started":
      floatAt(ev.entityId, "−" + (int("cost") + int("offspring_energy")), "#e06bd8");
      say(`Unit ${tag(ev.entityId)}: cloning (${int("cost")} energy + ${int("offspring_energy")} for the offspring, ${int("duration")} ticks)`, "info");
      return true;
    case "cloning.completed":
      say(`Unit ${tag(ev.entityId)} cloned → ${tag(str("child"))} at ${fmtPos(f.position && f.position.positionValue)}`, "info");
      return true;
    case "cloning.blocked":
      say(`Unit ${tag(ev.entityId)}: offspring waiting for a free adjacent tile`, "error");
      return true;
    case "cloning.failed":
      say(`Unit ${tag(ev.entityId)}: cloning failed (${str("reason")}): ${str("message")}`, "error");
      return true;
  }
  return false;
}

// ---- rendering ------------------------------------------------------------------

function el(tagName, props = {}, ...children) {
  const n = document.createElement(tagName);
  Object.assign(n, props);
  n.append(...children);
  return n;
}

function progressBar(done, total, cls) {
  const bar = el("div", { className: "progress " + (cls || "") });
  const fill = el("i");
  fill.style.width = (100 * Math.min(1, done / total)) + "%";
  bar.append(fill);
  return bar;
}

function renderEvolution() {
  renderUnitDetail();
  renderBatchEstimate();
  const n = filterMatches().length;
  $("filter-count").textContent = $("filter").value.trim()
    ? `${n} of your units match.`
    : "Comma-separated: key=value, key (present), !key (absent).";
}

// Only rebuild the detail panel when the unit's data changes, so buttons
// stay clickable between ticks.
let detailKey = "";

function renderUnitDetail() {
  const box = $("unit-detail");
  const ids = selectedIds();
  const e = ids.length === 1 ? entities.get(ids[0]) : null;
  const key = e ? JSON.stringify(e) : "";
  if (key === detailKey) return;
  detailKey = key;
  box.replaceChildren();
  if (!e) return;

  const energy = val(e, "energy.stored") || 0, cap = val(e, "energy.capacity") || 0;
  const busy = busyOf(e);
  box.append(el("h3", { textContent: `Unit ${tag(e.id)} — energy ${energy}/${cap}` }));

  // Activity.
  if (attr(e, "upgrades.duration")) {
    const done = val(e, "upgrades.done"), total = val(e, "upgrades.duration");
    box.append(el("div", { className: "hint", textContent: `Upgrading ${attr(e, "upgrades.attribute").stringValue} ${val(e, "upgrades.from")}→${val(e, "upgrades.target")}: ${done}/${total} ticks` }), progressBar(done, total));
  }
  if (attr(e, "cloning.duration")) {
    const done = val(e, "cloning.done"), total = val(e, "cloning.duration");
    const blocked = attrBool(e, "cloning.blocked");
    box.append(el("div", { className: "hint", textContent: `Cloning: ${done}/${total} ticks${blocked ? " — waiting for a free tile" : ""}` }), progressBar(done, total, "clone"));
  }

  // Attributes with +1 upgrade buttons.
  const table = el("table");
  for (const d of attrDefs) {
    const v = val(e, "attributes." + d.name);
    if (v === undefined) continue;
    const q = upgradeQuote(d, v, v + 1);
    const tr = table.insertRow();
    tr.title = d.description || "";
    tr.insertCell().textContent = d.name;
    const c = tr.insertCell();
    c.className = "num";
    c.textContent = `${v} (${d.min}–${d.max})`;
    const action = tr.insertCell();
    if (!q) {
      action.textContent = "max";
    } else {
      const b = el("button", { type: "button", textContent: `+1: ${q.cost}⚡ ${q.ticks}t` });
      b.disabled = !!busy || energy < q.cost;
      b.title = busy ? `busy (${busy})` : energy < q.cost ? `needs ${q.cost} energy` : `upgrade to ${v + 1}`;
      b.addEventListener("click", () => upgradeOne(e.id, d.name, v + 1));
      action.append(b);
    }
  }
  box.append(el("h3", { textContent: "Attributes" }), table);

  // Derived properties.
  const derivedNames = ["mass", ...((rules.attributes && rules.attributes.derived_order) || []), "invested", "embodied"];
  const dt = el("table");
  for (const name of derivedNames) {
    const v = val(e, "attributes." + name);
    if (v === undefined) continue;
    const tr = dt.insertRow();
    tr.title = (rules.attributes && rules.attributes.derived && rules.attributes.derived[name]) || "";
    tr.insertCell().textContent = name;
    const c = tr.insertCell();
    c.className = "num";
    c.textContent = v;
  }
  box.append(el("h3", { textContent: "Derived" }), dt);

  // Cloning.
  const qc = val(e, "cloning.quote_cost"), qo = val(e, "cloning.quote_offspring_energy") || 0, qt = val(e, "cloning.quote_ticks");
  if (qc !== undefined) {
    const need = qc + qo;
    const b = el("button", { type: "button", textContent: `Clone: ${qc}⚡ + ${qo} for offspring, ${qt}t` });
    b.disabled = !!busy || energy < need;
    b.title = busy ? `busy (${busy})` : energy < need ? `needs ${need} energy` : "clone into an adjacent free tile";
    b.addEventListener("click", () => cloneOne(e.id));
    box.append(el("h3", { textContent: "Reproduction" }), b);
  }

  if (typeof combatDetail === "function") combatDetail(e, box);
  if (typeof fogDetail === "function") fogDetail(e, box);

  // Metadata chips.
  const chips = el("div");
  for (const k of Object.keys(e.attributes || {}).filter((k) => k.startsWith("metadata.")).sort()) {
    const key = k.slice("metadata.".length);
    const x = el("button", { type: "button", textContent: "×", title: `remove ${key}` });
    x.addEventListener("click", () => removeMeta([e.id], key));
    chips.append(el("span", { className: "chip" }, `${key}=${e.attributes[k].stringValue}`, x));
  }
  box.append(el("h3", { textContent: "Metadata" }), chips.children.length ? chips : el("p", { className: "hint", textContent: "none" }));
}

function renderBatchEstimate() {
  const d = attrDefs.find((x) => x.name === $("batch-attr").value);
  const by = Math.max(1, Math.floor(Number($("batch-amount").value) || 1));
  const ids = selectedIds();
  if (!d || !ids.length) { $("batch-estimate").textContent = "Select units to see the total cost."; return; }
  let cost = 0, ticks = 0, possible = 0;
  for (const id of ids) {
    const e = entities.get(id);
    const v = val(e, "attributes." + d.name);
    const q = v === undefined ? null : upgradeQuote(d, v, v + by);
    if (!q) continue;
    cost += q.cost;
    ticks = Math.max(ticks, q.ticks);
    if (!busyOf(e) && (val(e, "energy.stored") || 0) >= q.cost) possible++;
  }
  $("batch-estimate").textContent = `+${by} ${d.name} on ${ids.length} unit(s): ${cost} energy total, up to ${ticks} ticks; ${possible} can afford it now.`;
}

// ---- controls -------------------------------------------------------------------

$("batch-upgrade").addEventListener("click", () => {
  const ids = selectedIds();
  const by = Math.max(1, Math.floor(Number($("batch-amount").value) || 1));
  const a = $("batch-attr").value;
  if (!ids.length || !a) return;
  batch({ unitIds: ids }, { "@type": UPGRADE_TYPE, attribute: a, increase: String(by) }, `Upgrade ${a} +${by} on ${ids.length} unit(s)`);
});

$("batch-clone").addEventListener("click", () => {
  const ids = selectedIds();
  if (ids.length) batch({ unitIds: ids }, { "@type": CLONE_TYPE }, `Clone ${ids.length} unit(s)`);
});

$("meta-set").addEventListener("click", () => setMeta(selectedIds(), $("meta-key").value.trim(), $("meta-value").value));
$("meta-remove").addEventListener("click", () => removeMeta(selectedIds(), $("meta-key").value.trim()));

$("filter-select").addEventListener("click", () => {
  selected.clear();
  for (const e of filterMatches()) selected.add(e.id);
  render();
});

// Server-side selection: the filter is sent as a selector and evaluated at
// the tick the command executes, not against this client's view.
$("filter-upgrade").addEventListener("click", () => {
  const conditions = parseFilter($("filter").value);
  const by = Math.max(1, Math.floor(Number($("batch-amount").value) || 1));
  const a = $("batch-attr").value;
  if (!a) return;
  batch({ conditions }, { "@type": UPGRADE_TYPE, attribute: a, increase: String(by) }, `Upgrade ${a} +${by} on units matching "${$("filter").value}"`);
});

for (const id of ["batch-attr", "batch-amount", "filter"]) $(id).addEventListener("input", () => render());

// Milestone 6 player UI: automation behaviors, supply links and their status.
// Loaded after app.js, evolution.js and combat.js; uses their state and
// helpers. Status comes only from what the server sends this player: the
// automation.* and logistics.* attributes of its own units and
// automation.* / logistics.* events addressed to it.
"use strict";

const AUTOMATE_TYPE = "type.googleapis.com/rts.automation.v1.AutomateCommand";
const CANCEL_AUTO_TYPE = "type.googleapis.com/rts.automation.v1.CancelAutomationCommand";
const SUPPLY_TYPE = "type.googleapis.com/rts.logistics.v1.SupplyCommand";
const CANCEL_SUPPLY_TYPE = "type.googleapis.com/rts.logistics.v1.CancelSupplyCommand";

// pick is the multi-click flow in progress: {need: [...steps], got: {}, done(got)}.
let pick = null;

const numIn = (id) => Math.max(0, Math.floor(Number($(id).value) || 0));
const useGroup = () => $("auto-group").checked;

function pickHint(text) { $("auto-hint").textContent = text; }

function startPick(steps, done) {
  pick = { steps, got: {}, done };
  pickHint(steps[0].prompt + " (Esc cancels)");
}

function cancelPick() {
  if (!pick) return;
  pick = null;
  pickHint("");
}

// autoPick consumes board clicks while a pick is in progress; returns true
// when it handled the click.
function autoPick(t) {
  if (!pick) return false;
  const step = pick.steps[0];
  const e = board.entityAt(t.x, t.y);
  let value = null;
  if (step.kind === "well" && e && isWell(e)) value = e;
  else if (step.kind === "own" && e && isMine(e)) value = e;
  else if (step.kind === "tile") value = { position: { x: t.x, y: t.y } };
  if (!value) { say(`Not a valid choice: ${step.prompt}`, "error"); return true; }
  pick.got[step.key] = value;
  pick.steps.shift();
  if (pick.steps.length) {
    pickHint(pick.steps[0].prompt + " (Esc cancels)");
  } else {
    const p = pick;
    cancelPick();
    p.done(p.got);
  }
  return true;
}

// targets returns how a command applies: to the selection, or server-side
// to every own unit matching the metadata filter (evaluated at execution).
function targets() {
  if (useGroup()) {
    const text = $("filter").value.trim();
    if (!text) { say("Enter a metadata filter (Filter section) to apply to a group", "error"); return null; }
    return { group: { conditions: parseFilter(text) }, label: `units matching "${text}"` };
  }
  const ids = selectedIds();
  if (!ids.length) { say("Select units first (or tick “apply to filter matches”)", "error"); return null; }
  return { ids, label: `${ids.length} unit(s)` };
}

function send(to, type, body, desc) {
  if (to.group) batch(to.group, { "@type": type, ...body }, desc);
  else submit({ "@type": type, unitIds: to.ids, ...body }, desc);
}

function assignBehavior() {
  const to = targets();
  if (!to) return;
  const kind = $("auto-kind").value;
  const radius = Math.max(1, numIn("auto-radius"));
  switch (kind) {
    case "mine":
      startPick([{ kind: "well", key: "well", prompt: "Click the well to mine" },
        { kind: "own", key: "recv", prompt: "Click your unit that receives deliveries" }], (g) =>
        send(to, AUTOMATE_TYPE, { mineAndDeliver: { wellId: g.well.id, receiverId: g.recv.id, deliverAt: String(numIn("auto-deliver")), keep: String(numIn("auto-keep")) } },
          `Mine ${tag(g.well.id)} → deliver to ${tag(g.recv.id)}: ${to.label}`));
      break;
    case "salvage":
      send(to, AUTOMATE_TYPE, { autoSalvage: { radius } }, `Auto-salvage within ${radius}: ${to.label}`);
      break;
    case "guard":
      startPick([{ kind: "tile", key: "at", prompt: "Click the position to guard" }], (g) =>
        send(to, AUTOMATE_TYPE, { guard: { position: g.at.position, radius } }, `Guard ${fmtPos(g.at.position)} r${radius}: ${to.label}`));
      break;
    case "follow":
      startPick([{ kind: "own", key: "leader", prompt: "Click your unit to follow" }], (g) =>
        send(to, AUTOMATE_TYPE, { follow: { leaderId: g.leader.id, distance: Math.min(radius, 8) } }, `Follow ${tag(g.leader.id)}: ${to.label}`));
      break;
  }
}

function cancelBehavior() {
  const to = targets();
  if (to) send(to, CANCEL_AUTO_TYPE, {}, `Cancel automation: ${to.label}`);
}

function startSupply() {
  const to = targets();
  if (!to) return;
  startPick([{ kind: "own", key: "recv", prompt: "Click your unit to supply" }], (g) =>
    send(to, SUPPLY_TYPE, { receiverId: g.recv.id, reserve: String(numIn("sup-reserve")), fillTo: String(numIn("sup-fill")), resumeBelow: String(numIn("sup-resume")) },
      `Supply ${tag(g.recv.id)} from ${to.label}`));
}

function stopSupply() {
  const to = targets();
  if (to) send(to, CANCEL_SUPPLY_TYPE, {}, `Stop supplying: ${to.label}`);
}

// ---- status -----------------------------------------------------------------

const behaviorName = { mine_and_deliver: "mine & deliver", auto_salvage: "auto-salvage", guard: "guard", follow: "follow" };
const str = (e, k) => { const v = attr(e, k); return v && v.stringValue; };

function supplyLinks(e) {
  const out = [];
  for (let i = 0; i < 4; i++) {
    const to = str(e, `logistics.supply.${i}.to`);
    if (!to) break;
    out.push({ to, status: str(e, `logistics.supply.${i}.status`) });
  }
  return out;
}

// autoLines draws supply links (cyan; dim while not flowing).
function autoLines(e) {
  const out = [];
  for (const l of supplyLinks(e)) {
    const r = entities.get(l.to);
    if (r) out.push({ from: e.position, to: r.position, color: l.status === "supplying" ? "rgba(80,220,230,0.9)" : "rgba(80,220,230,0.3)" });
  }
  return out;
}

function autoDetail(e, box) {
  const b = str(e, "automation.behavior");
  const links = supplyLinks(e);
  if (!b && !links.length) return;
  const t = el("table");
  const row = (k, v) => { const tr = t.insertRow(); tr.insertCell().textContent = k; tr.insertCell().textContent = v; };
  if (b) {
    row("behavior", behaviorName[b] || b);
    row("state", str(e, "automation.state"));
    const status = str(e, "automation.status");
    row("status", status === "blocked" ? `blocked — ${str(e, "automation.reason")}` : status);
    const target = str(e, "automation.target");
    if (target) row("objective", tag(target));
  }
  if (links.length) {
    row("supply reserve", attrInt(e, "logistics.reserve"));
    for (const l of links) row(`supplies ${tag(l.to)}`, l.status.replaceAll("_", " "));
  }
  row("transfer range", attrInt(e, "logistics.range"));
  box.append(el("h3", { textContent: "Automation & supply" }), t);
}

function renderAutoStatus() {
  const counts = {};
  const blocked = [];
  for (const e of entities.values()) {
    if (!isMine(e)) continue;
    const b = str(e, "automation.behavior");
    if (!b) continue;
    counts[b] = (counts[b] || 0) + 1;
    if (str(e, "automation.status") === "blocked") blocked.push(e);
  }
  const box = $("auto-status");
  box.replaceChildren();
  const summary = Object.entries(counts).map(([k, n]) => `${n} ${behaviorName[k] || k}`).join(", ");
  box.append(el("p", { className: "hint", textContent: summary || "No automated units." }));
  if (blocked.length) {
    const ul = el("ul", { className: "blocked" });
    for (const e of blocked.slice(0, 12)) {
      const li = el("li");
      const a = el("button", { type: "button", className: "link", textContent: tag(e.id) });
      a.addEventListener("click", () => { selected.clear(); selected.add(e.id); render(); });
      li.append(a, ` ${behaviorName[str(e, "automation.behavior")]} — ${str(e, "automation.reason")}`);
      ul.append(li);
    }
    box.append(ul);
  }
}

// ---- events -----------------------------------------------------------------

function autoEvent(ev) {
  const f = ev.fields || {};
  const s = (k) => (f[k] && f[k].stringValue) || "";
  switch (ev.type) {
    case "automation.state":
    case "logistics.status":
      return true; // shown in the status panel
    case "automation.blocked":
      say(`Unit ${tag(ev.entityId)} (${behaviorName[s("behavior")] || s("behavior")}) blocked: ${s("reason").replaceAll("_", " ")}`, "error");
      return true;
    case "automation.failed":
      say(`Unit ${tag(ev.entityId)} stopped ${behaviorName[s("behavior")] || s("behavior")}: ${s("reason").replaceAll("_", " ")}`, "error");
      return true;
    case "logistics.ended":
      say(`Unit ${tag(ev.entityId)} stopped supplying ${tag(s("receiver"))}: ${s("reason").replaceAll("_", " ")}`, "error");
      return true;
  }
  return false;
}

$("auto-start").addEventListener("click", assignBehavior);
$("auto-cancel").addEventListener("click", cancelBehavior);
$("sup-start").addEventListener("click", startSupply);
$("sup-cancel").addEventListener("click", stopSupply);
window.addEventListener("keydown", (ev) => { if (ev.key === "Escape") cancelPick(); });

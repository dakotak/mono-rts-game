// Milestone 4 player UI: attacks, damage feedback, wreckage and salvage.
// Loaded after app.js and evolution.js; uses their state and helpers.
// Everything shown comes from what the server sends this player: public
// health on every unit, combat stats and orders on own units, combat
// events addressed to this player.
"use strict";

const ATTACK_TYPE = "type.googleapis.com/rts.combat.v1.AttackCommand";
const STOP_ATTACK_TYPE = "type.googleapis.com/rts.combat.v1.StopAttackCommand";
const SALVAGE_TYPE = "type.googleapis.com/rts.salvage.v1.SalvageCommand";
const STOP_SALVAGE_TYPE = "type.googleapis.com/rts.salvage.v1.StopSalvageCommand";

const { isWreck } = RTS;
const isEnemyUnit = (e) => isUnit(e) && !isMine(e);

// ---- commands -------------------------------------------------------------------

function attackTarget(target) {
  const ids = selectedIds().filter((id) => (attrInt(entities.get(id), "combat.attack") || 0) > 0);
  if (!ids.length) { say("No selected unit can attack (attack strength 0)", "error"); return; }
  submit({ "@type": ATTACK_TYPE, unitIds: ids, targetId: target.id }, `Attack ${tag(target.id)} with ${ids.length} unit(s)`);
}

// salvageWreck walks the selection next to the wreck (it is passable, so
// one unit may also stand on it) and orders collection.
function salvageWreck(wreck) {
  const ids = selectedIds();
  if (!ids.length) return;
  const { x, y } = wreck.position;
  const orders = formationOrders(ids, x, y).filter((o) => {
    const u = entities.get(o.unitId);
    return !(u.position.x === o.destination.x && u.position.y === o.destination.y);
  });
  if (orders.length) submit({ "@type": MOVE_TYPE, orders }, `Move ${orders.length} unit(s) to wreck ${tag(wreck.id)}`);
  submit({ "@type": SALVAGE_TYPE, unitIds: ids, wreckId: wreck.id }, `Salvage wreck ${tag(wreck.id)} with ${ids.length} unit(s)`);
}

// combatCommandAt handles right-clicks on enemies and wrecks; returns true
// when it handled the click.
function combatCommandAt(e) {
  if (!e || !selected.size) return false;
  if (isEnemyUnit(e)) { attackTarget(e); return true; }
  if (isWreck(e)) { salvageWreck(e); return true; }
  return false;
}

// combatStop cancels attack and salvage orders of the selection (part of S).
function combatStop(ids) {
  const attackers = ids.filter((id) => attr(entities.get(id), "combat.target"));
  if (attackers.length) submit({ "@type": STOP_ATTACK_TYPE, unitIds: attackers }, `Stop attacking (${attackers.length})`);
  const salvagers = ids.filter((id) => attr(entities.get(id), "salvage.target"));
  if (salvagers.length) submit({ "@type": STOP_SALVAGE_TYPE, unitIds: salvagers }, `Stop salvaging (${salvagers.length})`);
}

// ---- board overlays -----------------------------------------------------------------

function combatLines(e) {
  const out = [];
  const idOf = (k) => { const v = attr(e, k); return v && v.stringValue; };
  const target = entities.get(idOf("combat.target"));
  if (target) {
    const waiting = attrBool(e, "combat.out_of_range") || attrBool(e, "combat.stalled");
    out.push({ from: e.position, to: target.position, color: waiting ? "rgba(224,82,82,0.35)" : "rgba(255,90,90,0.9)", cross: true });
  }
  const wreck = entities.get(idOf("salvage.target"));
  if (wreck) out.push({ from: e.position, to: wreck.position, color: "rgba(201,115,58,0.7)" });
  return out;
}

// ---- events ---------------------------------------------------------------------

function combatEvent(ev) {
  const f = ev.fields || {};
  const int = (k) => Number((f[k] && f[k].intValue) || 0);
  const str = (k) => (f[k] && f[k].stringValue) || "";
  switch (ev.type) {
    case "combat.attack":
      floatAt(str("target"), "−" + int("damage"), "#ff5a5a");
      return true;
    case "combat.damaged":
      floatAt(ev.entityId, "−" + int("damage"), "#ff5a5a");
      if (int("health") > 0) say(`Unit ${tag(ev.entityId)} took ${int("damage")} damage from ${int("attackers")} attacker(s), ${int("health")} left`, "error");
      return true;
    case "combat.destroyed": {
      const mine = int("owner") === session.playerId;
      const p = f.position && f.position.positionValue;
      if (p) floaters.push({ x: p.x || 0, y: p.y || 0, text: "✖", color: mine ? "#ff5a5a" : "#ffd84d", born: performance.now() });
      say(mine ? `Your unit ${tag(ev.entityId)} was destroyed at ${fmtPos(p)}` : `Enemy unit ${tag(ev.entityId)} destroyed at ${fmtPos(p)}`, mine ? "error" : "info");
      return true;
    }
    case "combat.attack_ended": {
      const why = { target_lost: "target gone", incapable: "no attack strength" }[str("reason")] || str("reason");
      say(`Unit ${tag(ev.entityId)} stopped attacking ${tag(str("target"))}: ${why}`, "info");
      return true;
    }
    case "combat.out_of_range":
      say(`Unit ${tag(ev.entityId)}: target out of range (${int("distance")} > ${int("range")}); move closer`, "error");
      return true;
    case "combat.stalled":
      say(`Unit ${tag(ev.entityId)}: not enough energy to attack (needs ${int("required")}, has ${int("stored")})`, "error");
      return true;
    case "salvage.wreck_created":
      say(`Wreckage ${tag(ev.entityId)} with ${int("energy")} energy at ${fmtPos(f.position && f.position.positionValue)}`, "info");
      return true;
    case "salvage.collected":
      floatAt(ev.entityId, "+" + int("amount"), "#c9733a");
      return true;
    case "salvage.wreck_depleted":
      say(`Wreckage ${tag(ev.entityId)} fully salvaged`, "info");
      return true;
    case "salvage.ended":
      if (str("reason") !== "depleted") say(`Unit ${tag(ev.entityId)} stopped salvaging: ${str("reason")}`, "info");
      return true;
  }
  return false;
}

// ---- detail panel ---------------------------------------------------------------

function combatDetail(e, box) {
  const hp = attrInt(e, "combat.health");
  if (hp === undefined) return;
  const t = el("table");
  const row = (k, v) => {
    const tr = t.insertRow();
    tr.insertCell().textContent = k;
    const c = tr.insertCell();
    c.className = "num";
    c.textContent = v;
  };
  row("integrity", `${hp} / ${attrInt(e, "combat.max_health")}`);
  row("attack", attrInt(e, "combat.attack"));
  row("range", attrInt(e, "combat.range"));
  const cd = attrInt(e, "combat.cooldown"), ready = attrInt(e, "combat.ready_at");
  row("cooldown", ready !== undefined && ready > tick ? `${cd} (ready in ${ready - tick})` : `${cd} (ready)`);
  row("energy per attack", attrInt(e, "combat.attack_cost"));
  const armor = attrInt(e, "attributes.armor");
  if (armor !== undefined) row("armor", armor);
  box.append(el("h3", { textContent: "Combat" }), t);
  const targetId = attr(e, "combat.target");
  if (targetId) {
    const status = attrBool(e, "combat.stalled") ? "waiting for energy" : attrBool(e, "combat.out_of_range") ? "out of range" : "engaging";
    const b = el("button", { type: "button", textContent: "Stop attacking" });
    b.addEventListener("click", () => combatStop([e.id]));
    box.append(el("p", { className: "hint", textContent: `Attacking ${tag(targetId.stringValue)} — ${status}` }), b);
  }
  const wreckId = attr(e, "salvage.target");
  if (wreckId) {
    const w = entities.get(wreckId.stringValue);
    box.append(el("p", { className: "hint", textContent: `Salvaging ${tag(wreckId.stringValue)}${w ? ` (${attrInt(w, "salvage.energy")} left)` : ""}` }));
  }
}

// combatHover describes wrecks and enemy health under the cursor.
function combatHover(e) {
  if (isWreck(e)) return `Wreckage ${tag(e.id)}: ${attrInt(e, "salvage.energy")} of ${attrInt(e, "salvage.initial")} energy left — right-click to salvage`;
  if (isEnemyUnit(e)) {
    const hp = attrInt(e, "combat.health");
    return `${ownerInfo(e.ownerId).label} unit ${tag(e.id)}` + (hp !== undefined ? ` — integrity ${hp}/${attrInt(e, "combat.max_health")}` : "") + " — right-click to attack";
  }
  return null;
}

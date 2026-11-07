// Milestone 7 player UI: match lifecycle, lobby readiness, objectives,
// clock, eliminations, final results and resets. Everything comes from the
// MatchInfo the server attaches to snapshots and updates (already filtered
// for this player) plus match.* events.
"use strict";

const READY_TYPE = "type.googleapis.com/rts.match.v1.SetReadyCommand";
let matchInfo = null;
let shownResultFor = "";

const stateName = (s) => (s || "").replace("MATCH_STATE_", "").toLowerCase();

// matchBefore runs before a message is applied. A snapshot of a different
// match means the world was reset: adopt the new player ID and forget
// everything remembered about the old world.
function matchBefore(msg) {
  const m = msg.snapshot ? msg.snapshot.match : msg.update.match;
  if (!m) return;
  const reset = matchInfo && msg.snapshot && m.matchId !== matchInfo.matchId;
  if (m.yourPlayerId && session && m.yourPlayerId !== session.playerId) {
    session.playerId = m.yourPlayerId;
    try { sessionStorage.setItem("rts.session", JSON.stringify(session)); } catch (_) {}
  }
  if (reset) {
    entities.clear();
    selected.clear();
    if (typeof fogReset === "function") fogReset();
    if (m.width && m.height && (m.width !== session.world.width || m.height !== session.world.height)) {
      session.world.width = m.width;
      session.world.height = m.height;
      board.setWorld(m.width, m.height);
    }
    $("results").hidden = true;
    say(`A new match started (${m.matchId.slice(0, 8)}): ${stateName(m.state)}`, "info");
  }
  matchInfo = m;
}

function matchEvent(ev) {
  const f = ev.fields || {};
  const str = (k) => (f[k] && f[k].stringValue) || "";
  switch (ev.type) {
    case "match.state":
      say(`Match ${str("state")} (${str("reason").replaceAll("_", " ")})`, "info");
      return true;
    case "match.ready":
      return true; // shown in the match panel
    case "match.score":
      return true;
    case "match.eliminated": {
      const who = Number(str("player")) === session.playerId ? "You were" : "A player was";
      say(`${who} eliminated (${str("reason").replaceAll("_", " ")})`, Number(str("player")) === session.playerId ? "error" : "info");
      return true;
    }
    case "match.finished":
      return true; // the results panel opens from the match info
  }
  return false;
}

function fmtTicks(t) {
  const hz = (session && session.world.tickRateHz) || 10;
  const s = Math.floor(Number(t || 0) / hz);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

function renderMatch() {
  const m = matchInfo;
  const badge = $("match-state");
  if (!m) { badge.textContent = "–"; return; }
  const st = stateName(m.state);
  badge.textContent = st;
  badge.className = "badge match-" + st;
  const box = $("match-panel");
  box.replaceChildren();
  const p = (text) => box.append(el("p", { className: "hint", textContent: text }));
  const conds = (m.victoryConditions || []).map((c) => c === "time_limit" ? `time limit (${fmtTicks(m.timeLimitTicks)})` : c.replaceAll("_", " "));
  p(`Objectives: ${conds.length ? conds.join(", then ") : "none (sandbox)"}`);
  p(`Map: ${m.generator} ${m.width}×${m.height}`);
  if (st === "ready") {
    p(`Waiting to start (${m.startMode.replace("_", " ")}): ${m.readyPlayers || 0} of ${m.expectedPlayers} players ready`);
    const b = el("button", { type: "button", textContent: m.youReady ? "Not ready" : "Ready" });
    b.addEventListener("click", () => submit({ "@type": READY_TYPE, ready: !m.youReady }, m.youReady ? "Not ready" : "Ready"));
    box.append(b);
  }
  if (st === "running" || st === "paused") {
    const left = Number(m.timeLimitTicks) > 0 ? `, ${fmtTicks(m.ticksRemaining)} left` : "";
    p(`Match clock ${fmtTicks(m.runTicks)}${left}${st === "paused" ? " — paused by the administrator" : ""}`);
    p(`Your score: ${m.yourScore || 0}${m.eliminatedPlayers ? ` · ${m.eliminatedPlayers} eliminated` : ""}`);
    if (m.youEliminated) p("You have been eliminated.");
  }
  if ((m.scores || []).length && st !== "finished") {
    p("Scores: " + m.scores.map((s) => `${s.name} ${s.score}`).join(", "));
  }
  if (st === "finished" && m.result && shownResultFor !== m.matchId) {
    shownResultFor = m.matchId;
    showResults(m.result);
  }
  if (st === "finished") {
    const b = el("button", { type: "button", textContent: "Show results" });
    b.addEventListener("click", () => showResults(m.result));
    box.append(b);
  }
}

function showResults(r) {
  if (!r) return;
  const box = $("results-content");
  box.replaceChildren();
  const winner = (r.players || []).find((p) => p.winner);
  const you = (r.players || []).find((p) => p.playerId === session.playerId);
  const headline = r.draw ? "Draw" : winner && you && winner.playerId === you.playerId ? "Victory!" : `${winner ? winner.name : "Nobody"} wins`;
  box.append(el("h2", { textContent: headline }),
    el("p", { className: "hint", textContent: `${r.reason.replaceAll("_", " ")} · tick ${r.finalTick} · ${fmtTicks(r.runTicks)} played · seed ${r.seed} · ${r.generator}` }));
  const t = el("table");
  const head = t.insertRow();
  for (const h of ["Player", "Score", "Units", "Embodied", "Stored", "Mined", "Kills", "Lost", "Destroyed"]) head.insertCell().textContent = h;
  for (const p of r.players || []) {
    const tr = t.insertRow();
    tr.insertCell().textContent = `${p.name}${p.winner ? " 🏆" : ""}${p.eliminated ? " ✖" : ""}${p.playerId === session.playerId ? " (you)" : ""}`;
    for (const k of ["score", "units", "embodied", "storedEnergy", "energyMined", "kills", "unitsLost", "investmentDestroyed"]) {
      const c = tr.insertCell();
      c.className = "num";
      c.textContent = p[k] || 0;
    }
  }
  box.append(t, el("p", { className: "hint", textContent: "The administrator can start a new match; you will join it automatically." }));
  $("results").hidden = false;
}

$("results-close").addEventListener("click", () => { $("results").hidden = true; });

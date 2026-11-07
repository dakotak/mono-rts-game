// Milestone 7 admin UI: match lifecycle controls, player readiness and
// connections, world generation / seed / configuration for the next match,
// and replay export and verification. Lifecycle changes are rts.match.v1
// commands submitted through /api/command, so they are scheduled and
// replayed like every other mutation; resets build a new world.
"use strict";

const MATCH = "type.googleapis.com/rts.match.v1.";
let details = null;
let detailsMatch = "";

async function refreshMatch() {
  try {
    details = await api("/api/match");
  } catch (_) {
    return;
  }
  const m = details.match || {};
  if (detailsMatch && m.matchId !== detailsMatch) {
    // A reset happened (here or in another admin session): refresh world
    // metadata (size, players, rules) from a fresh snapshot.
    try {
      const snap = await api("/api/snapshot");
      players = snap.players;
      world = snap.world;
      try { rules = JSON.parse(world.rulesJson || "{}"); } catch (_) { rules = {}; }
      board.setWorld(world.width, world.height);
      renderPlayers();
    } catch (_) {}
  }
  detailsMatch = m.matchId;
  renderMatchPanel();
}

const stName = (s) => (s || "").replace("MATCH_STATE_", "").toLowerCase();

function renderMatchPanel() {
  if (!details) return;
  const m = details.match || {};
  const st = stName(m.state);
  $("match-summary").textContent =
    `${st} · match ${(m.matchId || "").slice(0, 8)} · seed ${details.seed} · ${m.generator} ${m.width}×${m.height} · run ${m.runTicks || 0}` +
    (Number(m.timeLimitTicks) > 0 ? `/${m.timeLimitTicks}` : "") + ` ticks · ${(m.victoryConditions || []).join(", ") || "no victory conditions"}`;
  for (const [id, ok] of [["m-start", st === "ready"], ["m-pause", st === "running"], ["m-resume", st === "paused"],
    ["m-end", st !== "finished"], ["m-elim", st !== "finished"]]) $(id).disabled = !ok;
  const t = $("match-players");
  t.replaceChildren();
  const head = t.insertRow();
  for (const h of ["Player", "Ready", "Elim.", "Conn.", "Score", "Mined", "Kills", "Lost"]) head.insertCell().textContent = h;
  const opts = [["0", "— no winner —"]];
  for (const p of details.players || []) {
    const s = p.stats || {};
    const tr = t.insertRow();
    for (const v of [`${p.name} (slot ${p.slot})`, p.ready ? "✓" : "", p.eliminated ? "✖" : "", p.connections || 0, s.score || 0, s.energyMined || 0, s.kills || 0, s.unitsLost || 0]) {
      tr.insertCell().textContent = v;
    }
    opts.push([String(p.playerId), p.name]);
  }
  for (const sel of [$("m-winner"), $("m-elim-player")]) {
    const cur = sel.value;
    sel.replaceChildren(...opts.filter(([v]) => sel.id === "m-winner" || v !== "0").map(([v, l]) => el("option", { value: v, textContent: l })));
    if (cur) sel.value = cur;
  }
  const hist = $("m-history");
  const curSel = hist.value;
  hist.replaceChildren(el("option", { value: "", textContent: "current match" }),
    ...(details.history || []).map((r) => el("option", { value: r.matchId, textContent: `${r.matchId.slice(0, 8)} — ${r.draw ? "draw" : "won by " + ((r.players || []).find((p) => p.winner) || {}).name} (${r.reason})` })));
  hist.value = curSel;
}

function el(tagName, props = {}) {
  return Object.assign(document.createElement(tagName), props);
}

function lifecycle(name, body, description) {
  mutate("/api/command", { payload: { "@type": MATCH + name, ...body } }, description);
}

$("m-start").addEventListener("click", () => lifecycle("StartMatchCommand", {}, "Start match"));
$("m-pause").addEventListener("click", () => lifecycle("PauseMatchCommand", {}, "Pause match"));
$("m-resume").addEventListener("click", () => lifecycle("ResumeMatchCommand", {}, "Resume match"));
$("m-end").addEventListener("click", () => {
  const w = Number($("m-winner").value);
  lifecycle("EndMatchCommand", { winnerId: w, reason: $("m-reason").value || "admin" }, w ? `End match, winner ${w}` : "End match without a winner");
});
$("m-elim").addEventListener("click", () => {
  const p = Number($("m-elim-player").value);
  lifecycle("EliminatePlayerCommand", { playerId: p, removeUnits: $("m-elim-units").checked }, `Eliminate player ${p}`);
});

$("m-load-config").addEventListener("click", () => {
  if (details) $("m-config").value = details.configJson;
});
$("m-reset").addEventListener("click", async () => {
  const body = {};
  const seed = $("m-seed").value.trim();
  if (seed !== "") body.seed = seed;
  let cfg = $("m-config").value.trim();
  if ($("m-random").checked) {
    try {
      const c = JSON.parse(cfg || (details && details.configJson) || "{}");
      c.world.random_seed = true;
      cfg = JSON.stringify(c);
    } catch (e) {
      say(`Reset: configuration is not valid JSON (${e.message})`, "error");
      return;
    }
  }
  if (cfg) body.configJson = cfg;
  if (!confirm("Replace the current match with a new one? The current world is discarded (its replay stays exportable).")) return;
  try {
    const r = await api("/api/reset", body);
    say(`New match ${r.matchId.slice(0, 8)} with seed ${r.seed}`, "ok");
    refreshMatch();
  } catch (e) {
    say(`Reset failed (the current match continues): ${e.message}`, "error");
  }
});

$("m-export").addEventListener("click", async () => {
  try {
    const r = await api("/api/replay/export", { matchId: $("m-history").value });
    const name = `mono-replay-${JSON.parse(r.replayJson).match_id}.json`;
    const a = el("a", { href: URL.createObjectURL(new Blob([r.replayJson], { type: "application/json" })), download: name });
    a.click();
    URL.revokeObjectURL(a.href);
    say(`Exported ${name}`, "ok");
  } catch (e) {
    say(`Export failed: ${e.message}`, "error");
  }
});
$("m-verify").addEventListener("change", async (ev) => {
  const file = ev.target.files[0];
  if (!file) return;
  say(`Verifying ${file.name}…`, "info");
  try {
    const r = await api("/api/replay/verify", { replayJson: await file.text() });
    const warn = r.engineMatches && r.rulesetMatches ? "" : " (engine or ruleset differs from this server)";
    say(r.ok ? `Replay OK: ${r.ticksReplayed} ticks, ${r.checkpointsChecked} checkpoints, final hash ${r.finalHash}${warn}`
      : `Replay DIVERGED at tick ${r.firstDivergentTick}: ${r.detail}${warn}`, r.ok ? "ok" : "error");
  } catch (e) {
    say(`Verify failed: ${e.message}`, "error");
  }
  ev.target.value = "";
});

setInterval(() => { if (token) refreshMatch(); }, 1000);

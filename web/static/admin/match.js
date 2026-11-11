// Match administration: polls GetMatchDetails into the store (players'
// readiness, connections and match statistics feed the Players panel; the
// state feeds the Simulation panel), ends matches, eliminates players,
// starts new matches (seed and configuration) and exports and verifies
// replays. Lifecycle changes are rts.match.v1
// commands submitted through /api/command, so they are scheduled and
// replayed like every other mutation; resets build a new world.
"use strict";

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
      setWorld(await api("/api/snapshot"));
      say(`Now administering match ${(m.matchId || "").slice(0, 8)}`, "info", { category: "match" });
    } catch (_) {}
  }
  detailsMatch = m.matchId;
  S.details = details;
  renderMatchPanel();
  Store.emit("match");
}


// renderMatchPanel fills the Match panel (summary, end/eliminate) and the
// Replays panel's match list. Player statistics are in the Players panel.
function renderMatchPanel() {
  if (!details) return;
  const m = details.match || {};
  const st = stName(m.state);
  $("match-summary").textContent =
    `${st} · match ${(m.matchId || "").slice(0, 8)} · seed ${details.seed} · ${m.generator} ${m.width}×${m.height} · run ${m.runTicks || 0}` +
    (Number(m.timeLimitTicks) > 0 ? `/${m.timeLimitTicks}` : "") + ` ticks · ${(m.victoryConditions || []).join(", ") || "no victory conditions"}`;
  for (const [id, ok, why] of [["m-end", st !== "finished", "The match has already finished"], ["m-elim", st !== "finished", "The match has already finished"]]) {
    $(id).disabled = !ok;
    $(id).title = ok ? "" : why;
  }
  const opts = [["0", "— no winner —"]];
  for (const p of details.players || []) opts.push([String(p.playerId), p.name]);
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
    say(`New match ${r.matchId.slice(0, 8)} with seed ${r.seed}`, "ok", { category: "match" });
    refreshMatch();
  } catch (e) {
    say(`Reset failed (the current match continues): ${e.message}`, "error", { category: "match" });
  }
});

$("m-export").addEventListener("click", async () => {
  try {
    const r = await api("/api/replay/export", { matchId: $("m-history").value });
    const name = `mono-replay-${JSON.parse(r.replayJson).match_id}.json`;
    const a = el("a", { href: URL.createObjectURL(new Blob([r.replayJson], { type: "application/json" })), download: name });
    a.click();
    URL.revokeObjectURL(a.href);
    say(`Exported ${name}`, "ok", { category: "replay" });
  } catch (e) {
    say(`Export failed: ${e.message}`, "error", { category: "replay" });
  }
});
$("m-verify").addEventListener("change", async (ev) => {
  const file = ev.target.files[0];
  if (!file) return;
  say(`Verifying ${file.name}…`, "info", { category: "replay" });
  try {
    const r = await api("/api/replay/verify", { replayJson: await file.text() });
    const warn = r.engineMatches && r.rulesetMatches ? "" : " (engine or ruleset differs from this server)";
    say(r.ok ? `Replay OK: ${r.ticksReplayed} ticks, ${r.checkpointsChecked} checkpoints, final hash ${r.finalHash}${warn}`
      : `Replay DIVERGED at tick ${r.firstDivergentTick}: ${r.detail}${warn}`, r.ok ? "ok" : "error", { category: "replay" });
  } catch (e) {
    say(`Verify failed: ${e.message}`, "error", { category: "replay" });
  }
  ev.target.value = "";
});

setInterval(() => { if (token) refreshMatch(); }, 1000);

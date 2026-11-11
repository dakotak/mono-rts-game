// Milestone 8 player UI: "Add computer opponent". Shown only when the
// gateway exposes a computer-player host (rts-gateway -bothost). The host
// fills only the slots it was configured for, using those slots' join
// tokens: a computer player is an ordinary client of the public API, with
// no administrative access.
"use strict";

let botInfo = null; // GET /api/bots response, or null when unavailable
let botInfoJSON = "";

async function refreshBots() {
  try {
    botInfo = await api("/api/bots");
  } catch (_) {
    botInfo = null; // no bot host: hide the panel
  }
  // Re-render only on change, so choices being made are not reset.
  const j = JSON.stringify(botInfo);
  if (j !== botInfoJSON) {
    botInfoJSON = j;
    renderBots();
  }
}

function renderBots() {
  const box = $("bots-panel");
  const head = $("bots-head");
  if (!botInfo || !(botInfo.slots || []).length) {
    box.hidden = head.hidden = true;
    return;
  }
  box.hidden = head.hidden = false;
  box.replaceChildren();
  for (const s of botInfo.slots) {
    const row = el("div", { className: "bot-row" });
    if (s.running) {
      row.append(el("span", { textContent: `${s.name}: computer, level ${s.difficulty} (${s.personality})` }));
      const stop = el("button", { type: "button", textContent: "Remove" });
      stop.addEventListener("click", () => botAction(`/api/bots/${s.slot}`, "DELETE", null, `Removed the computer player from ${s.name}`));
      row.append(stop);
    } else {
      row.append(el("span", { textContent: `${s.name}${s.error ? ` (stopped: ${s.error})` : ""}` }));
      const lvl = el("select", { title: "Difficulty" });
      for (const d of botInfo.difficulties || []) {
        lvl.append(el("option", { value: d.level, textContent: `${d.level} – ${d.name}`, selected: d.level === 5 }));
      }
      const pers = el("select", { title: "Personality" });
      for (const p of botInfo.personalities || []) pers.append(el("option", { value: p, textContent: p, selected: p === "balanced" }));
      const add = el("button", { type: "button", textContent: "Add computer" });
      add.addEventListener("click", () => botAction("/api/bots", "POST",
        { slot: s.slot, difficulty: Number(lvl.value), personality: pers.value },
        `A level ${lvl.value} ${pers.value} computer player joined as ${s.name}`));
      row.append(lvl, pers, add);
    }
    box.append(row);
  }
}

async function botAction(path, method, body, done) {
  try {
    await api(path, { method, body });
    say(done, "info");
  } catch (e) {
    say(`Computer player: ${e.message}`, "error");
  }
  refreshBots();
}

// Poll only when a bot host answers (plain gateways return 404).
refreshBots().then(() => { if (botInfo) setInterval(refreshBots, 5000); });

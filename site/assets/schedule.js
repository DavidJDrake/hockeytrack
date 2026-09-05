let TEAMS = {};
let GAMES = [];
let GENERATED = null;
const ET = new Intl.DateTimeFormat("en-US", { timeZone: "America/New_York", hour: "numeric", minute: "2-digit" });
const DAYFMT = new Intl.DateTimeFormat("en-US", { weekday: "long", timeZone: "UTC" });
const DATEFMT = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", timeZone: "UTC" });
const MONTHFMT = new Intl.DateTimeFormat("en-US", { month: "long", year: "numeric", timeZone: "UTC" });
const todayET = new Intl.DateTimeFormat("en-CA", { timeZone: "America/New_York" }).format(new Date());

const CLOCK_ICON = `<svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true" focusable="false"><circle cx="8" cy="9" r="6"/><path d="M8 6v3.5l2 1.5M6 1.5h4"/></svg>`;
const state = { teams: new Set(), months: new Set(), type: "", q: "" };
const $ = (s) => document.querySelector(s);
// Everything in schedule.json comes from the NHL API; escape it before it goes anywhere near innerHTML.
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));


async function load() {
  const list = $("#list");
  list.innerHTML = `<div class="empty">Loading the schedule…</div>`;
  let doc;
  try {
    const res = await fetch("/data/schedule.json", { cache: "no-cache" });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    doc = await res.json();
  } catch (err) {
    list.innerHTML = `<div class="empty">Couldn’t load the schedule (${esc(err.message)}). Refresh to try again.</div>`;
    return;
  }
  TEAMS = doc.teams;
  GAMES = doc.games;
  GENERATED = doc.generatedAt;
  populate();
  restore();
}

function populate() {
  teams.populate(Object.entries(TEAMS).sort((a, b) => a[1].localeCompare(b[1])).map(([ab, name]) => ({ value: ab, code: ab, name })));
  months.populate([...new Set(GAMES.map(g => g.date.slice(0, 7)))].sort()
    .map(m => ({ value: m, code: "", name: MONTHFMT.format(new Date(m + "-01T00:00:00Z")) })));
}

function filtered() {
  const q = state.q.trim().toLowerCase();
  return GAMES.filter(g =>
    (!state.teams.size || state.teams.has(g.away) || state.teams.has(g.home)) &&
    (!state.type || String(g.type) === state.type) &&
    (!state.months.size || state.months.has(g.date.slice(0, 7))) &&
    (!q || g.away.toLowerCase().includes(q) || g.home.toLowerCase().includes(q) ||
      TEAMS[g.away].toLowerCase().includes(q) || TEAMS[g.home].toLowerCase().includes(q) || g.venue.toLowerCase().includes(q)));
}

// Per-team season context: game number and rest days, computed over the team's regular-season games.
function teamContext(team) {
  const ctx = new Map(); if (!team) return ctx;
  let n = 0, prev = null;
  GAMES.filter(g => g.type === 2 && (g.away === team || g.home === team)).forEach(g => {
    n += 1;
    const d = new Date(g.date + "T00:00:00Z");
    const rest = prev ? Math.round((d - prev) / 86400000) - 1 : null;
    ctx.set(g.id, { n, rest }); prev = d;
  });
  return ctx;
}

// The per-team view (game numbers, rest, home/away) only makes sense for one club.
function soloTeam() { return state.teams.size === 1 ? [...state.teams][0] : ""; }

function render() {
  const rows = filtered();
  const solo = soloTeam();
  const ctx = teamContext(solo);
  const byDay = new Map();
  rows.forEach(g => { (byDay.get(g.date) || byDay.set(g.date, []).get(g.date)).push(g); });
  $("#s-games").textContent = rows.length.toLocaleString();
  $("#s-days").textContent = byDay.size.toLocaleString();

  const stampFmt = new Intl.DateTimeFormat("en-US", { timeZone: "America/New_York", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
  if (GENERATED) $("#stamp").textContent = `Updated ${stampFmt.format(new Date(GENERATED))} ET, refreshed every morning`;
  const tl = $("#teamline");
  if (solo) {
    const reg = GAMES.filter(g => g.type === 2 && (g.away === solo || g.home === solo));
    const home = reg.filter(g => g.home === solo).length;
    const b2b = [...ctx.values()].filter(c => c.rest === 0).length;
    tl.hidden = false;
    tl.innerHTML = `<h2>${esc(TEAMS[solo])}</h2>
      <span><b>${reg.length}</b> regular-season games</span>
      <span><b>${home}</b> home &middot; <b>${reg.length - home}</b> away</span>
      <span><b>${b2b}</b> back-to-backs</span>
      <span>${reg.length ? `opens ${DATEFMT.format(new Date(reg[0].date + "T00:00:00Z"))}, closes ${DATEFMT.format(new Date(reg[reg.length - 1].date + "T00:00:00Z"))}` : ""}</span>`;
  } else { tl.hidden = true; }

  const nextDay = [...byDay.keys()].find(d => d >= todayET);
  const html = [];
  for (const [date, gs] of byDay) {
    const d = new Date(date + "T00:00:00Z");
    const isNext = date === nextDay;
    html.push(`<section class="day${isNext ? " today" : ""}" id="d-${esc(date)}">
      <h2 class="dhead">${DAYFMT.format(d)}<small>${DATEFMT.format(d)}, ${esc(date.slice(0, 4))}${isNext ? ` <span class="todaytag">${date === todayET ? "today" : "up next"}</span>` : ""}</small></h2>
      <ul class="games">${gs.map(g => gameRow(g, ctx)).join("")}</ul>
    </section>`);
  }
  $("#list").innerHTML = html.length ? html.join("") : `<div class="empty">No games match those filters.</div>`;
}

function gameRow(g, ctx) {
  const t = soloTeam();
  const c = ctx.get(g.id);
  const hl = (ab) => `<span class="ab${state.teams.has(ab) ? " hl" : ""}">${esc(ab)}</span>`;
  const right = [];
  if (g.type === 1) right.push(`<span class="chip pre">Pre</span>`);
  if (t) right.push(`<span class="chip ${g.home === t ? "home" : "away"}">${g.home === t ? "Home" : "Away"}</span>`);
  if (c) right.push(`<span class="gn">Gm ${c.n}</span>`);
  if (c && c.rest !== null) right.push(`<span class="rest${c.rest === 0 ? " b2b" : ""}">${c.rest === 0 ? "back-to-back" : c.rest + "d rest"}</span>`);
  right.push(`<button type="button" class="cdb" data-cd="${g.id}" aria-label="Countdown to ${esc(g.away)} at ${esc(g.home)}, ${DATEFMT.format(new Date(g.date + "T00:00:00Z"))}">${CLOCK_ICON}</button>`);
  return `<li class="g">
    <span class="t">${ET.format(new Date(g.start))} ET</span>
    <span class="m">${hl(g.away)}<span class="at">at</span>${hl(g.home)}<span class="nm">${esc(TEAMS[g.away])} at ${esc(TEAMS[g.home])} &middot; ${esc(g.venue)}</span></span>
    <span class="r">${right.join("")}</span>
  </li>`;
}

// Wiring
// A multi-select picker: a <details> disclosure holding labelled checkboxes,
// with the selection echoed as removable chips and persisted per key.
function makePicker({ id, key, empty, onChange }) {
  const box = document.getElementById(id);
  const grid = document.getElementById(id + "-grid");
  const summary = document.getElementById(id + "-summary");
  const chips = $("#chips");
  const names = new Map();
  const selected = new Set();
  const label = (v) => names.get(v) || v;
  const shortLabel = (v) => (grid.querySelector(`input[value="${CSS.escape(v)}"]`) || {}).dataset?.code || label(v);
  function set(next) {
    selected.clear(); next.forEach(v => names.has(v) && selected.add(v));
    grid.querySelectorAll("input").forEach(i => { i.checked = selected.has(i.value); });
    const n = selected.size;
    summary.textContent = n === 0 ? empty : n === 1 ? label([...selected][0]) : `${n} selected`;
    try { localStorage.setItem(key, JSON.stringify([...selected])); } catch (e) {}
    renderChips();
    onChange(selected);
  }
  function renderChips() {
    const all = pickers.flatMap(pk => [...pk.selected].map(v => ({ pk, v })));
    chips.hidden = all.length === 0;
    chips.innerHTML = all.map(({ pk, v }) => `<button type="button" data-picker="${pk.id}" data-remove="${esc(v)}" aria-label="Remove ${esc(pk.label(v))}">${esc(pk.shortLabel(v))}</button>`).join("");
  }
  function populate(options) {
    names.clear(); options.forEach(o => names.set(o.value, o.name));
    grid.innerHTML = options.map(o => `<label><input type="checkbox" value="${esc(o.value)}" data-code="${esc(o.code)}">${o.code ? `<b>${esc(o.code)}</b>` : ""}<span>${esc(o.name)}</span></label>`).join("");
  }
  function restore() {
    let saved = [];
    try { saved = JSON.parse(localStorage.getItem(key) || "[]"); } catch (e) {}
    return saved;
  }
  const close = () => { if (box.open) { box.open = false; box.querySelector("summary").focus(); } };
  grid.addEventListener("change", (e) => { const next = new Set(selected); e.target.checked ? next.add(e.target.value) : next.delete(e.target.value); set(next); });
  document.getElementById(id + "-clear").addEventListener("click", () => set([]));
  document.getElementById(id + "-done").addEventListener("click", close);
  box.addEventListener("keydown", (e) => { if (e.key === "Escape") close(); });
  document.addEventListener("click", (e) => { if (box.open && !box.contains(e.target)) box.open = false; });
  return { id, box, selected, set, populate, restore, label, shortLabel, renderChips };
}
const teams = makePicker({ id: "teams", key: "nhl2627-teams", empty: "All teams", onChange: (sel) => { state.teams = sel; syncQuick(); render(); } });
const months = makePicker({ id: "months", key: "nhl2627-months", empty: "Every month", onChange: (sel) => { state.months = sel; render(); } });
const pickers = [teams, months];
$("#chips").addEventListener("click", (e) => {
  const b = e.target.closest("button[data-remove]"); if (!b) return;
  const pk = pickers.find(p => p.id === b.dataset.picker);
  const next = new Set(pk.selected); next.delete(b.dataset.remove); pk.set(next);
});
$("#q").addEventListener("input", (e) => { state.q = e.target.value; render(); });
document.querySelectorAll(".seg button").forEach(b => b.addEventListener("click", () => {
  document.querySelectorAll(".seg button").forEach(x => x.setAttribute("aria-pressed", "false"));
  b.setAttribute("aria-pressed", "true"); state.type = b.dataset.type; render();
}));
document.querySelectorAll(".quick button").forEach(b => b.addEventListener("click", () => {
  if (b.dataset.quick === "TODAY") {
    const el = [...document.querySelectorAll(".day")].find(s => s.id.slice(2) >= todayET);
    if (el) el.scrollIntoView({ behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth", block: "start" });
    return;
  }
  const next = new Set(teams.selected);
  next.has(b.dataset.quick) ? next.delete(b.dataset.quick) : next.add(b.dataset.quick);
  teams.set(next);
}));
function syncQuick() { document.querySelectorAll(".quick button[data-quick]").forEach(b => b.setAttribute("aria-pressed", String(state.teams.has(b.dataset.quick)))); }
function restore() {
  const savedTeams = teams.restore();
  try { const legacy = localStorage.getItem("nhl2627-team"); if (legacy) { savedTeams.push(legacy); localStorage.removeItem("nhl2627-team"); } } catch (e) {}
  teams.set(savedTeams);
  months.set(months.restore());
}
load();

// Per-game countdown popup: the same component as the home page, mounted
// into a native <dialog> (focus trap, Esc, focus restore come for free),
// plus the two-line embed snippet with a copy button.
(function () {
  const dlg = $("#cd");
  if (!dlg || !("showModal" in dlg)) return;
  let clock = null;
  const ok = $("#cd-ok");
  function open(g) {
    const d = HockeyTrackCountdown.describe(g);
    $("#cd-title").textContent = `${d.match} · ${d.when}`;
    if (clock) clock.destroy();
    clock = HockeyTrackCountdown.mount($("#cd-clock"), { game: g });
    $("#cd-code").textContent = HockeyTrackCountdown.embedCode(g);
    ok.textContent = "";
    dlg.showModal();
  }
  $("#list").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-cd]");
    if (!b) return;
    const g = GAMES.find(x => String(x.id) === b.dataset.cd);
    if (g) open(g);
  });
  $("#cd-close").addEventListener("click", () => dlg.close());
  dlg.addEventListener("click", (e) => { if (e.target === dlg) dlg.close(); }); // backdrop
  dlg.addEventListener("close", () => { if (clock) { clock.destroy(); clock = null; } });
  $("#cd-copy").addEventListener("click", async () => {
    const code = $("#cd-code").textContent;
    try {
      await navigator.clipboard.writeText(code);
      ok.textContent = "Copied.";
    } catch {
      // No clipboard permission (or an insecure context): select the code so a manual copy is one keystroke away.
      const range = document.createRange(); range.selectNodeContents($("#cd-code"));
      const sel = window.getSelection(); sel.removeAllRanges(); sel.addRange(range);
      ok.textContent = "Selected — press Ctrl+C / Cmd+C to copy.";
    }
    setTimeout(() => { ok.textContent = ""; }, 4000);
  });
})();

// Renders /data/standings.json: the NHL's own table, trimmed by schedule-sync,
// grouped by conference and division in the league's order.
//
// Every string in the document (conference, division, abbrev, name, streak)
// comes from the NHL API. Nothing here goes through innerHTML: elements are
// built with the DOM API and text is set with textContent.
const $ = (s) => document.querySelector(s);
const DATEFMT = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric", year: "numeric", timeZone: "UTC" });
const STAMPFMT = new Intl.DateTimeFormat("en-US", { timeZone: "America/New_York", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });

// Columns after the rank and team cells, in table order.
const COLS = [
  { key: "gp", label: "GP", title: "Games played" },
  { key: "w", label: "W", title: "Wins" },
  { key: "l", label: "L", title: "Regulation losses" },
  { key: "otl", label: "OTL", title: "Overtime and shootout losses" },
  { key: "pts", label: "PTS", title: "Points" },
  { key: "gf", label: "GF", title: "Goals for" },
  { key: "ga", label: "GA", title: "Goals against" },
  { key: "diff", label: "DIFF", title: "Goal differential" },
  { key: "streak", label: "STRK", title: "Current streak" },
];

function el(tag, attrs = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") n.className = v;
    else n.setAttribute(k, v);
  }
  for (const c of children) n.append(c);
  return n;
}

// The schedule page persists the visitor's picked clubs; highlight them here.
function pickedTeams() {
  try { return new Set(JSON.parse(localStorage.getItem("nhl2627-teams") || "[]")); } catch (e) { return new Set(); }
}

// "20262027" → "2026–27"
function seasonLabel(season) {
  const s = String(season);
  return s.length === 8 ? `${s.slice(0, 4)}–${s.slice(6)}` : "";
}

async function load() {
  const list = $("#list");
  let doc;
  try {
    const res = await fetch("/data/standings.json", { cache: "no-cache" });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    doc = await res.json();
    if (!Array.isArray(doc.teams)) throw new Error("unexpected document");
  } catch (err) {
    list.replaceChildren();
    const note = $("#note-error");
    note.querySelector("p").textContent = `Couldn’t load the standings (${err.message}). Refresh to try again.`;
    note.hidden = false;
    return;
  }
  render(doc);
}

function render(doc) {
  const teams = doc.teams;
  const picked = pickedTeams();

  const label = seasonLabel(doc.season);
  if (label) {
    $("#h-title").textContent = `${label} Standings`;
    document.title = `${label} Standings · HockeyTrack`;
  }
  const played = teams.reduce((n, t) => n + t.gp, 0) / 2;
  $("#s-gp").textContent = Math.floor(played).toLocaleString();
  $("#s-teams").textContent = teams.length.toLocaleString();
  $("#s-goals").textContent = teams.reduce((n, t) => n + t.gf, 0).toLocaleString();
  // Opening week, or a fresh season's table before the first game: every
  // row is 0-0-0. Render it anyway and say why it is empty.
  $("#note-preseason").hidden = !(teams.length && teams.every(t => t.gp === 0));

  const stamp = [];
  if (doc.date) stamp.push(`Standings through ${DATEFMT.format(new Date(doc.date + "T00:00:00Z"))}`);
  if (doc.generatedAt) stamp.push(`updated ${STAMPFMT.format(new Date(doc.generatedAt))} ET, refreshed every morning`);
  if (stamp.length) $("#stamp").textContent = stamp.join(", ");

  // Group in document order: schedule-sync already sorted by conference,
  // division, and the league's division rank.
  const confs = new Map();
  for (const t of teams) {
    const conf = confs.get(t.conference) || confs.set(t.conference, new Map()).get(t.conference);
    (conf.get(t.division) || conf.set(t.division, []).get(t.division)).push(t);
  }

  const out = [];
  let i = 0;
  for (const [conf, divs] of confs) {
    const section = el("section", { class: "conf" }, el("h2", {}, conf ? `${conf} Conference` : "Conference"));
    const grid = el("div", { class: "divs" });
    for (const [div, rows] of divs) {
      const id = `div-${++i}`;
      // The wrapper scrolls sideways on phones, so it is a labelled,
      // focusable region: keyboard users can reach it and scroll it.
      grid.append(el("div", { class: "div" },
        el("h3", { id }, div ? `${div} Division` : "Division"),
        el("div", { class: "tbl", role: "region", "aria-labelledby": id, tabindex: "0" }, table(rows, id, picked))));
    }
    section.append(grid);
    out.push(section);
  }
  $("#list").replaceChildren(...(out.length ? out : [el("div", { class: "empty" }, "No standings to show yet.")]));
}

function table(rows, labelledBy, picked) {
  const head = el("tr", {},
    el("th", { scope: "col", class: "rk" }, el("span", { class: "sr-only" }, "Rank")),
    el("th", { scope: "col", class: "team" }, "Team"),
    ...COLS.map(c => el("th", { scope: "col" }, el("abbr", { title: c.title }, c.label))));
  const body = el("tbody", {}, ...rows.map(t => row(t, picked)));
  return el("table", { "aria-labelledby": labelledBy }, el("thead", {}, head), body);
}

function row(t, picked) {
  const tr = el("tr", picked.has(t.abbrev) ? { class: "hl" } : {});
  tr.append(el("td", { class: "rk" }, String(t.rank)));
  const team = el("th", { scope: "row" }, el("span", { class: "ab" }, t.abbrev), el("span", { class: "nm" }, t.name));
  if (picked.has(t.abbrev)) team.append(el("span", { class: "sr-only" }, " (your team)"));
  tr.append(team);
  const diff = t.gf - t.ga;
  for (const c of COLS) {
    const td = el("td", { class: c.key });
    if (c.key === "diff") {
      td.classList.add(diff > 0 ? "pos" : diff < 0 ? "neg" : "even");
      td.textContent = diff > 0 ? `+${diff}` : String(diff);
    } else if (c.key === "streak") {
      if (t.streak) td.textContent = t.streak;
      else td.append(el("span", { "aria-hidden": "true" }, "—"), el("span", { class: "sr-only" }, "none"));
    } else {
      td.textContent = String(t[c.key]);
    }
    tr.append(td);
  }
  return tr;
}

load();

// Live numbers from the same document the schedule page renders.
fetch("/data/schedule.json").then(r => r.ok ? r.json() : null).then(doc => {
  if (!doc) return;
  document.getElementById("b-games").textContent = doc.games.length.toLocaleString();
  document.getElementById("b-teams").textContent = Object.keys(doc.teams).length;
  const today = new Date(new Intl.DateTimeFormat("en-CA", { timeZone: "America/New_York" }).format(new Date()) + "T00:00:00Z");
  const next = doc.games.find(g => new Date(g.date + "T00:00:00Z") >= today);
  const el = document.getElementById("b-days");
  if (!next) { el.textContent = "—"; return; }
  const days = Math.round((new Date(next.date + "T00:00:00Z") - today) / 86400000);
  el.textContent = days;
  el.nextSibling.textContent = days === 0 ? "games today" : days === 1 ? "day to puck drop" : "days to puck drop";
}).catch(() => {});

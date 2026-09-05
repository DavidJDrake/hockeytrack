// Live numbers from the same document the schedule page renders.
fetch("/data/schedule.json").then(r => r.ok ? r.json() : null).then(doc => {
  if (!doc) return;
  document.getElementById("b-games").textContent = doc.games.length.toLocaleString();
  document.getElementById("b-teams").textContent = Object.keys(doc.teams).length;
  // The shared countdown (/assets/countdown.js); `next` rolls it on to the
  // following game once the current one starts.
  HockeyTrackCountdown.mount(document.getElementById("c-clock"), {
    next: now => doc.games.find(g => new Date(g.start) > now) || null
  });
}).catch(() => {});

// Live numbers from the same document the schedule page renders.
fetch("/data/schedule.json").then(r => r.ok ? r.json() : null).then(doc => {
  if (!doc) return;
  document.getElementById("b-games").textContent = doc.games.length.toLocaleString();
  document.getElementById("b-teams").textContent = Object.keys(doc.teams).length;
  startClock(doc);
}).catch(() => {});

// A scoreboard clock counting down to the next puck drop. The visible digits
// tick every second (aria-hidden), while a screen-reader summary refreshes
// once a minute so assistive tech is not spammed. A pause control satisfies
// WCAG 2.2.2 for auto-updating content.
function startClock(doc) {
  const $ = id => document.getElementById(id);
  const digits = { d: $("c-d"), h: $("c-h"), m: $("c-m"), s: $("c-s") };
  const text = $("c-text"), match = $("c-match"), when = $("c-when"), hold = $("c-hold");
  const day = new Intl.DateTimeFormat("en-US", { timeZone: "America/New_York", weekday: "short", month: "short", day: "numeric" });
  const clock = new Intl.DateTimeFormat("en-US", { timeZone: "America/New_York", hour: "numeric", minute: "2-digit" });
  const pad = n => String(n).padStart(2, "0");
  let target = null, timer = null, paused = false;

  function nextGame(now) {
    return doc.games.find(g => new Date(g.start) > now) || null;
  }

  function describe(game) {
    const at = new Date(game.start);
    return game.away + " @ " + game.home + ", " + day.format(at) + " at " + clock.format(at) + " ET";
  }

  function render(now) {
    if (!target || new Date(target.start) <= now) {
      target = nextGame(now);
      if (!target) {
        match.textContent = "Season complete";
        when.textContent = "";
        text.textContent = "No upcoming games.";
        hold.hidden = true;
        return false;
      }
      const at = new Date(target.start);
      match.textContent = target.away + " @ " + target.home;
      when.textContent = day.format(at) + " · " + clock.format(at) + " ET";
    }
    const left = Math.max(0, Math.floor((new Date(target.start) - now) / 1000));
    const d = Math.floor(left / 86400), h = Math.floor(left % 86400 / 3600), m = Math.floor(left % 3600 / 60), s = left % 60;
    digits.d.textContent = d;
    digits.d.closest(".clock").classList.toggle("long", d >= 100); // three-digit days get a smaller face
    digits.h.textContent = pad(h);
    digits.m.textContent = pad(m);
    digits.s.textContent = pad(s);
    if (s === 0 || !text.dataset.set) {
      text.dataset.set = "1";
      text.textContent = d + " days, " + h + " hours and " + m + " minutes to puck drop: " + describe(target) + ".";
    }
    return true;
  }

  function tick() {
    if (!render(new Date())) { clearInterval(timer); timer = null; }
  }

  hold.addEventListener("click", () => {
    paused = !paused;
    hold.setAttribute("aria-label", paused ? "Resume countdown" : "Pause countdown");
    hold.setAttribute("aria-pressed", String(paused));
    if (paused) { clearInterval(timer); timer = null; } else { tick(); timer = setInterval(tick, 1000); }
  });

  if (render(new Date())) {
    hold.hidden = false;
    timer = setInterval(tick, 1000);
  }
}

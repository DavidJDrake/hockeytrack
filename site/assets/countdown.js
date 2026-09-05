/* HockeyTrack puck-drop countdown.
 *
 * Embed on any page (the script loads its own stylesheet, nothing else needed):
 *
 *   <div class="ht-countdown" data-start="2026-09-19T23:00:00Z" data-away="DAL" data-home="STL"></div>
 *   <script src="https://hockeytrack.davidjdrake.com/assets/countdown.js" async></script>
 *
 * Or from code: HockeyTrackCountdown.mount(element, { game: { start, away, home } })
 * where `game.start` is an ISO timestamp. Pass { next: () => game | null } instead
 * to roll on to another game when the current one starts (the home page does).
 *
 * Accessibility: the ticking digits are aria-hidden; a screen-reader summary
 * refreshes once a minute; a pause control satisfies WCAG 2.2.2.
 */
(function () {
  "use strict";

  var ORIGIN = (function () {
    try { return new URL(document.currentScript.src, location.href).origin; } catch (e) { return "https://hockeytrack.davidjdrake.com"; }
  })();
  var DAY = new Intl.DateTimeFormat("en-US", { timeZone: "America/New_York", weekday: "short", month: "short", day: "numeric" });
  var TIME = new Intl.DateTimeFormat("en-US", { timeZone: "America/New_York", hour: "numeric", minute: "2-digit" });
  var SVG_NS = "http://www.w3.org/2000/svg";

  function pad(n) { return String(n).length < 2 ? "0" + n : String(n); }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function icon(cls, shapes) {
    var svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("class", cls);
    svg.setAttribute("width", "12"); svg.setAttribute("height", "12"); svg.setAttribute("viewBox", "0 0 12 12");
    svg.setAttribute("aria-hidden", "true"); svg.setAttribute("focusable", "false");
    shapes.forEach(function (s) {
      var n = document.createElementNS(SVG_NS, s[0]);
      Object.keys(s[1]).forEach(function (k) { n.setAttribute(k, s[1][k]); });
      svg.appendChild(n);
    });
    return svg;
  }

  // "DAL @ STL" and "Sat, Sep 19 · 7:00 PM ET" for a game.
  function describe(game) {
    var at = new Date(game.start);
    return { match: game.away + " @ " + game.home, when: DAY.format(at) + " · " + TIME.format(at) + " ET",
             sentence: game.away + " @ " + game.home + ", " + DAY.format(at) + " at " + TIME.format(at) + " ET" };
  }

  function unit(id, label) {
    var s = el("span");
    var b = el("b", null, "––");
    b.dataset.unit = id;
    s.appendChild(b); s.appendChild(document.createTextNode(label));
    return s;
  }

  function mount(root, opts) {
    opts = opts || {};
    var next = opts.next || (opts.game ? function (now) { return new Date(opts.game.start) > now ? opts.game : null; } : null);
    if (!next) throw new Error("HockeyTrackCountdown.mount: pass { game } or { next }");

    root.classList.add("ht-cd");
    root.setAttribute("role", "timer");
    root.setAttribute("aria-label", "Countdown to puck drop");
    while (root.firstChild) root.removeChild(root.firstChild);

    var top = el("div", "ht-cd__top");
    var head = el("div", "ht-cd__head");
    var tag = el("span", "ht-cd__tag", "Puck drop");
    var hold = el("button", "ht-cd__hold");
    hold.type = "button"; hold.setAttribute("aria-pressed", "false"); hold.setAttribute("aria-label", "Pause countdown"); hold.hidden = true;
    hold.appendChild(icon("ht-cd__pause", [["rect", { x: "1.5", y: "1", width: "3", height: "10" }], ["rect", { x: "7.5", y: "1", width: "3", height: "10" }]]));
    hold.appendChild(icon("ht-cd__play", [["path", { d: "M2.5 1l8 5-8 5z" }]]));
    head.appendChild(tag); head.appendChild(hold);
    var units = el("div", "ht-cd__units"); units.setAttribute("aria-hidden", "true");
    var digits = { d: unit("d", "days"), h: unit("h", "hrs"), m: unit("m", "min"), s: unit("s", "sec") };
    units.appendChild(digits.d); units.appendChild(el("i", null, ":"));
    units.appendChild(digits.h); units.appendChild(el("i", null, ":"));
    units.appendChild(digits.m); units.appendChild(el("i", null, ":"));
    units.appendChild(digits.s);
    top.appendChild(head); top.appendChild(units);
    var sr = el("span", "ht-cd__sr", "Countdown to puck drop loading.");
    var match = el("p", "ht-cd__match");
    var mStrong = el("strong", null, "First game"), mWhen = el("span", null, "");
    match.appendChild(mStrong); match.appendChild(mWhen);
    root.appendChild(top); root.appendChild(sr); root.appendChild(match);
    if (opts.credit) {
      var credit = el("p", "ht-cd__credit");
      var a = el("a", null, "hockeytrack.davidjdrake.com");
      a.href = "https://hockeytrack.davidjdrake.com/schedule/"; a.target = "_blank"; a.rel = "noopener";
      credit.appendChild(document.createTextNode("Countdown by ")); credit.appendChild(a);
      root.appendChild(credit);
    }

    var target = null, timer = null, paused = false, srSet = false;
    var set = function (k, v) { digits[k].firstChild.textContent = v; };

    function finish(game) {
      // The target has started and nothing follows it (a fixed embed).
      root.classList.add("ht-cd--done");
      tag.textContent = game ? "Puck has dropped" : "Season complete";
      set("d", "0"); set("h", "00"); set("m", "00"); set("s", "00");
      sr.textContent = game ? "The puck has dropped: " + describe(game).sentence + "." : "No upcoming games.";
      hold.hidden = true;
      return false;
    }

    function render(now) {
      if (!target || new Date(target.start) <= now) {
        var previous = target;
        target = next(now);
        if (!target) return finish(previous);
        var d = describe(target);
        mStrong.textContent = d.match; mWhen.textContent = d.when;
        root.classList.remove("ht-cd--done"); tag.textContent = "Puck drop";
      }
      var left = Math.max(0, Math.floor((new Date(target.start) - now) / 1000));
      var dd = Math.floor(left / 86400), hh = Math.floor(left % 86400 / 3600), mm = Math.floor(left % 3600 / 60), ss = left % 60;
      root.classList.toggle("ht-cd--long", dd >= 100);
      set("d", String(dd)); set("h", pad(hh)); set("m", pad(mm)); set("s", pad(ss));
      if (ss === 0 || !srSet) {
        srSet = true;
        sr.textContent = dd + " days, " + hh + " hours and " + mm + " minutes to puck drop: " + describe(target).sentence + ".";
      }
      return true;
    }

    function tick() { if (!render(new Date())) stop(); }
    function stop() { if (timer) clearInterval(timer); timer = null; }

    hold.addEventListener("click", function () {
      paused = !paused;
      hold.setAttribute("aria-label", paused ? "Resume countdown" : "Pause countdown");
      hold.setAttribute("aria-pressed", String(paused));
      if (paused) stop(); else { tick(); timer = setInterval(tick, 1000); }
    });

    if (render(new Date())) { hold.hidden = false; timer = setInterval(tick, 1000); }

    return {
      destroy: function () { stop(); while (root.firstChild) root.removeChild(root.firstChild); root.classList.remove("ht-cd", "ht-cd--done", "ht-cd--long"); root.removeAttribute("role"); root.removeAttribute("aria-label"); }
    };
  }

  // The two lines someone pastes to put a game's countdown on their own page.
  function embedCode(game) {
    var attr = function (s) { return String(s).replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; }); };
    var d = describe(game);
    return "<!-- HockeyTrack puck-drop countdown: " + attr(d.match) + ", " + attr(d.when) + " -->\n" +
      '<div class="ht-countdown" data-start="' + attr(game.start) + '" data-away="' + attr(game.away) + '" data-home="' + attr(game.home) + '"></div>\n' +
      '<script src="' + ORIGIN + '/assets/countdown.js" async></' + "script>";
  }

  function ensureStyles() {
    var have = Array.prototype.some.call(document.querySelectorAll('link[rel="stylesheet"]'), function (l) { return /\/assets\/countdown\.css(\?|$)/.test(l.href); });
    if (have) return;
    var link = document.createElement("link");
    link.rel = "stylesheet"; link.href = ORIGIN + "/assets/countdown.css";
    document.head.appendChild(link);
  }

  function autoMount() {
    var nodes = document.querySelectorAll(".ht-countdown[data-start]");
    if (!nodes.length) return;
    ensureStyles();
    Array.prototype.forEach.call(nodes, function (node) {
      if (node.classList.contains("ht-cd")) return;
      mount(node, { game: { start: node.dataset.start, away: node.dataset.away || "", home: node.dataset.home || "" }, credit: node.dataset.credit !== "off" });
    });
  }

  window.HockeyTrackCountdown = { mount: mount, describe: describe, embedCode: embedCode };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", autoMount); else autoMount();
})();

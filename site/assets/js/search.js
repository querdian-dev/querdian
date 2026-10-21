// Client-side documentation search. Fetches /search-index.json (built by Hugo from the docs pages) on first
// use and scores title, headings and body text in the browser; nothing is sent anywhere.
(function () {
  "use strict";

  var dialog = document.getElementById("search");
  if (!dialog || typeof dialog.showModal !== "function") {
    document.querySelectorAll("[data-search-open]").forEach(function (b) { b.hidden = true; });
    return;
  }
  var input = document.getElementById("search-q");
  var list = document.getElementById("search-results");
  var status = document.getElementById("search-status");
  var indexUrl = document.currentScript ? document.currentScript.getAttribute("data-index") : "/search-index.json";
  var docs = null;
  var loading = null;
  var active = -1;
  var results = [];

  function load() {
    if (docs) { return Promise.resolve(docs); }
    if (!loading) {
      loading = fetch(indexUrl).then(function (r) {
        if (!r.ok) { throw new Error("HTTP " + r.status); }
        return r.json();
      }).then(function (d) {
        docs = d.map(function (p) {
          return { t: p.t, u: p.u, g: p.g, h: p.h, b: p.b, tl: p.t.toLowerCase(), bl: p.b.toLowerCase(),
                   hl: p.h.map(function (h) { return h[1].toLowerCase(); }) };
        });
        return docs;
      }).catch(function (e) { loading = null; throw e; });
    }
    return loading;
  }

  function terms(q) {
    return q.toLowerCase().split(/[^a-z0-9_.\-]+/).filter(function (t) { return t.length > 0; });
  }

  function count(hay, term, cap) {
    var n = 0, i = hay.indexOf(term);
    while (i !== -1 && n < cap) { n++; i = hay.indexOf(term, i + term.length); }
    return n;
  }

  function search(q) {
    var ts = terms(q);
    if (!ts.length) { return []; }
    var out = [];
    docs.forEach(function (p) {
      var score = 0, bestHead = -1, bestHeadScore = 0, ok = true;
      for (var i = 0; i < ts.length; i++) {
        var t = ts[i], s = 0;
        if (p.tl.indexOf(t) !== -1) { s += 20; }
        for (var j = 0; j < p.hl.length; j++) {
          if (p.hl[j].indexOf(t) !== -1) {
            s += 8;
            var hs = p.hl[j] === t ? 3 : 1;
            if (hs > bestHeadScore || bestHead === -1) { bestHead = j; bestHeadScore = hs; }
          }
        }
        s += count(p.bl, t, 8);
        if (s === 0) { ok = false; break; }
        score += s;
      }
      if (ok) { out.push({ p: p, score: score, head: bestHead }); }
    });
    out.sort(function (a, b) { return b.score - a.score; });
    return out.slice(0, 8).map(function (r) { r.terms = ts; return r; });
  }

  function esc(s) {
    return s.replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; });
  }
  function mark(text, ts) {
    var out = esc(text);
    ts.forEach(function (t) {
      var re = new RegExp("(" + esc(t).replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + ")", "gi");
      out = out.replace(re, "<mark>$1</mark>");
    });
    return out;
  }
  function snippet(p, ts) {
    var at = -1;
    for (var i = 0; i < ts.length && at === -1; i++) { at = p.bl.indexOf(ts[i]); }
    if (at === -1) { return p.b.slice(0, 140); }
    var from = Math.max(0, at - 50);
    var text = p.b.slice(from, from + 170);
    return (from > 0 ? "…" : "") + text + (from + 170 < p.b.length ? "…" : "");
  }

  function render() {
    list.textContent = "";
    results.forEach(function (r, i) {
      var li = document.createElement("li");
      li.setAttribute("role", "option");
      li.id = "sr-" + i;
      var a = document.createElement("a");
      var head = r.head >= 0 ? r.p.h[r.head] : null;
      a.href = r.p.u + (head ? "#" + head[0] : "");
      var title = document.createElement("span");
      title.className = "sr-title";
      title.innerHTML = mark(r.p.t, r.terms) + (head ? '<span class="sr-head"> › ' + mark(head[1], r.terms) + "</span>" : "");
      var group = document.createElement("span");
      group.className = "sr-group";
      group.textContent = r.p.g;
      var snip = document.createElement("span");
      snip.className = "sr-snippet";
      snip.innerHTML = mark(snippet(r.p, r.terms), r.terms);
      a.appendChild(title); a.appendChild(group); a.appendChild(snip);
      li.appendChild(a);
      list.appendChild(li);
    });
    setActive(results.length ? 0 : -1);
  }

  function setActive(i) {
    active = i;
    list.querySelectorAll("li").forEach(function (li, n) {
      li.setAttribute("aria-selected", String(n === i));
    });
    input.setAttribute("aria-activedescendant", i >= 0 ? "sr-" + i : "");
    if (i >= 0) { var el = document.getElementById("sr-" + i); if (el && el.scrollIntoView) { el.scrollIntoView({ block: "nearest" }); } }
  }

  function update() {
    var q = input.value.trim();
    if (!q) {
      results = []; render();
      status.textContent = "Type to search all documentation pages.";
      input.setAttribute("aria-expanded", "false");
      return;
    }
    load().then(function () {
      if (input.value.trim() !== q) { return; }
      results = search(q);
      render();
      input.setAttribute("aria-expanded", String(results.length > 0));
      status.textContent = results.length ? results.length + (results.length === 1 ? " result" : " results") : "No page matches “" + q + "”.";
    }).catch(function () {
      status.textContent = "The search index could not be loaded.";
    });
  }

  function open() {
    if (!dialog.open) { dialog.showModal(); }
    input.focus(); input.select();
    load().catch(function () { /* reported on the first query */ });
  }

  document.querySelectorAll("[data-search-open]").forEach(function (b) { b.addEventListener("click", open); });
  document.addEventListener("keydown", function (e) {
    if (e.key === "/" && !e.ctrlKey && !e.metaKey && !e.altKey && !dialog.open) {
      var t = e.target;
      if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) { return; }
      e.preventDefault(); open();
    }
  });
  input.addEventListener("input", update);
  input.addEventListener("keydown", function (e) {
    if (e.key === "ArrowDown" && results.length) { e.preventDefault(); setActive((active + 1) % results.length); }
    else if (e.key === "ArrowUp" && results.length) { e.preventDefault(); setActive((active - 1 + results.length) % results.length); }
    else if (e.key === "Enter" && active >= 0) {
      e.preventDefault();
      var a = list.querySelector("#sr-" + active + " a");
      if (a) { window.location.href = a.href; }
    }
  });
  list.addEventListener("click", function () { dialog.close(); });
  dialog.addEventListener("click", function (e) { if (e.target === dialog) { dialog.close(); } });
  dialog.addEventListener("close", function () { input.value = ""; results = []; render(); status.textContent = "Type to search all documentation pages."; });
})();

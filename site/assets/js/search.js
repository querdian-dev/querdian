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

  // Typo tolerance. Every distinct word of the docs is a candidate; a query term with no exact hit in a page
  // falls back to the candidates within a small edit distance (a swap of two letters counts as one edit).
  var vocab = null;
  function words() {
    if (vocab) { return vocab; }
    var seen = {};
    docs.forEach(function (p) {
      (p.tl + " " + p.hl.join(" ") + " " + p.bl).replace(/[a-z0-9]+(?:_[a-z0-9]+)*/g, function (w) {
        seen[w] = true;
        if (w.indexOf("_") !== -1) { w.split("_").forEach(function (x) { seen[x] = true; }); }
        return w;
      });
    });
    vocab = Object.keys(seen).filter(function (w) { return w.length >= 3; });
    return vocab;
  }

  function dist(a, b, max) {
    var la = a.length, lb = b.length;
    if (Math.abs(la - lb) > max) { return max + 1; }
    var prev2 = null, prev = [], cur, i, j;
    for (j = 0; j <= lb; j++) { prev.push(j); }
    for (i = 1; i <= la; i++) {
      cur = [i];
      var rowMin = i;
      for (j = 1; j <= lb; j++) {
        var c = a.charCodeAt(i - 1) === b.charCodeAt(j - 1) ? 0 : 1;
        var v = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + c);
        if (i > 1 && j > 1 && a.charCodeAt(i - 1) === b.charCodeAt(j - 2) && a.charCodeAt(i - 2) === b.charCodeAt(j - 1)) {
          v = Math.min(v, prev2[j - 2] + 1);
        }
        cur.push(v);
        if (v < rowMin) { rowMin = v; }
      }
      if (rowMin > max) { return max + 1; }
      prev2 = prev; prev = cur;
    }
    return prev[lb];
  }

  // Words close to t, excluding t itself. Short terms get no tolerance (too many false friends); a longer word
  // also matches when only its beginning resembles t, so half-typed words with a typo still work.
  function similar(t) {
    var max = t.length >= 8 ? 2 : t.length >= 4 ? 1 : 0;
    if (!max) { return []; }
    var out = [];
    words().forEach(function (w) {
      if (w === t) { return; }
      var d = dist(t, w, max);
      if (d > max && t.length >= 5 && w.length > t.length) { d = dist(t, w.slice(0, t.length), max); }
      if (d <= max) { out.push(w); }
    });
    return out;
  }

  // Score of one literal word against one page.
  function scoreWord(p, t) {
    var s = 0, head = -1, headScore = 0;
    if (p.tl.indexOf(t) !== -1) { s += 20; }
    for (var j = 0; j < p.hl.length; j++) {
      if (p.hl[j].indexOf(t) !== -1) {
        s += 8;
        var hs = p.hl[j] === t ? 3 : 1;
        if (hs > headScore || head === -1) { head = j; headScore = hs; }
      }
    }
    s += count(p.bl, t, 8);
    return { s: s, head: head, hs: headScore };
  }

  function search(q) {
    var ts = terms(q);
    if (!ts.length) { return []; }
    var near = ts.map(similar);
    var out = [];
    docs.forEach(function (p) {
      var score = 0, bestHead = -1, bestHeadScore = 0, ok = true, hit = [];
      for (var i = 0; i < ts.length && ok; i++) {
        var r = scoreWord(p, ts[i]), s = r.s, head = r.head, hs = r.hs;
        if (s > 0) { hit.push(ts[i]); }
        else {
          // Exact miss on this page: accept near words at half weight.
          for (var k = 0; k < near[i].length; k++) {
            var f = scoreWord(p, near[i][k]);
            if (f.s === 0) { continue; }
            hit.push(near[i][k]);
            s += Math.max(1, Math.floor(f.s / 2));
            if (f.head !== -1 && (head === -1 || f.hs > hs)) { head = f.head; hs = f.hs; }
          }
        }
        if (s === 0) { ok = false; break; }
        score += s;
        if (head !== -1 && (bestHead === -1 || hs > bestHeadScore)) { bestHead = head; bestHeadScore = hs; }
      }
      if (ok) { out.push({ p: p, score: score, head: bestHead, terms: hit }); }
    });
    out.sort(function (a, b) { return b.score - a.score; });
    return out.slice(0, 8);
  }

  function esc(s) {
    return s.replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; });
  }
  // One alternation, longest word first, so a match is wrapped once and never inside an earlier <mark> tag.
  function mark(text, ts) {
    var parts = ts.slice().sort(function (a, b) { return b.length - a.length; })
      .map(function (t) { return esc(t).replace(/[.*+?^${}()|[\]\\]/g, "\\$&"); });
    if (!parts.length) { return esc(text); }
    return esc(text).replace(new RegExp("(" + parts.join("|") + ")", "gi"), "<mark>$1</mark>");
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

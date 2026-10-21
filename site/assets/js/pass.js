// Pass switcher on the landing page. The rewrites are the output of the real enforcers
// (internal/enforce/{promql,logql,traceql}) for the quickstart policies; only the namespace value
// changes between Alice and Bob, which is exactly what the enforcers do.
(function () {
  "use strict";

  var NS = { alice: "order-service", bob: "payment-service" };
  var PASSES = {
    alice: { name: "Alice", group: "order-team", scope: "Namespace order-service", unrestricted: false,
      denied: [["Denied label", " customer_id (metrics)"]] },
    bob: { name: "Bob", group: "payment-team", scope: "Namespace payment-service", unrestricted: false,
      denied: [["Denied labels", " none"]] },
    carol: { name: "Carol", group: "platform-admins", scope: "Tenant demo, every namespace", unrestricted: true,
      denied: [["Denied labels", " none"]] }
  };

  var IN = {
    metrics: 'sum by (route) (rate(http_requests_total{status=~"5.."}[5m]))',
    logs: '{app="checkout"} |= "timeout" | json | level="error"',
    traces: '{ span.http.status_code >= 500 && duration > 1s }',
    denied: 'sum by (customer_id) (rate(demo_requests_total[5m]))'
  };
  var ZONE = {
    metrics: function (ns) { return 'namespace="' + ns + '"'; },
    logs: function (ns) { return 'k8s_namespace_name="' + ns + '"'; },
    traces: function (ns) { return 'resource.k8s.namespace.name="' + ns + '"'; }
  };
  // Returns [before, stamp, after] of the forwarded query.
  var OUT = {
    metrics: function (ns) { return ['sum by (route) (rate(http_requests_total{', ZONE.metrics(ns), ',status=~"5.."}[5m]))']; },
    logs: function (ns) { return ['{app="checkout", ', ZONE.logs(ns), '} |= "timeout" | json | level = "error"']; },
    traces: function (ns) { return ['{ (span.http.status_code >= 500 && duration > 1s) && ((', ZONE.traces(ns), ')) }']; },
    denied: function (ns) { return ['sum by (customer_id) (rate(demo_requests_total{', ZONE.metrics(ns), '}[5m]))']; }
  };
  var NOTE = {
    metrics: "Rewritten by the PromQL enforcer: the matcher is added inside the selector.",
    logs: "Rewritten by the LogQL enforcer: the matcher joins the stream selector.",
    traces: "Rewritten by the TraceQL enforcer: the grant becomes a parenthesised condition.",
    denied: "Grouping by a denied label is refused before anything reaches the backend."
  };

  var state = { pass: "alice", tab: "metrics" };
  var $ = function (id) { return document.getElementById(id); };
  var panel = $("hoist-panel");
  if (!panel) { return; }

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) { n.className = cls; }
    if (text !== undefined) { n.textContent = text; }
    return n;
  }

  function renderPass() {
    var p = PASSES[state.pass];
    $("pass-name").textContent = p.name;
    $("pass-group").textContent = p.group;
    $("pass-scope").textContent = p.scope;
    var ns = NS[state.pass];
    ["metrics", "logs", "traces"].forEach(function (z) {
      $("z-" + z).textContent = p.unrestricted ? "unrestricted" : ZONE[z](ns).replace("=", "\u200b=");
    });
    var d = $("pass-denied");
    d.textContent = "";
    d.appendChild(el("b", "", p.denied[0][0]));
    d.appendChild(document.createTextNode(p.denied[0][1]));
    $("pass").setAttribute("data-pass", state.pass);
    document.querySelectorAll(".switch button").forEach(function (b) {
      b.setAttribute("aria-checked", String(b.getAttribute("data-pass") === state.pass));
      b.tabIndex = b.getAttribute("data-pass") === state.pass ? 0 : -1;
    });
  }

  function renderQuery() {
    var p = PASSES[state.pass];
    var t = state.tab;
    var ns = NS[state.pass];
    $("q-in").textContent = IN[t];
    var out = $("q-out");
    var label = $("out-label");
    var note = $("q-note");
    out.textContent = "";
    panel.setAttribute("data-zone", t);
    panel.setAttribute("data-result", "ok");
    if (t === "denied" && state.pass === "alice") {
      label.textContent = "Querdian answers";
      out.appendChild(el("span", "deny", "403 denied_label: label customer_id denied by policy"));
      note.textContent = "Alice's policy lists customer_id as a denied label for metrics. Nothing is sent to Prometheus.";
      panel.setAttribute("data-result", "denied");
    } else if (p.unrestricted) {
      label.textContent = "The backend receives";
      out.appendChild(document.createTextNode(IN[t]));
      note.textContent = "Carol's ClusterAccessPolicy is unrestricted for tenant demo, so the query is forwarded unchanged. The tenant header still applies.";
    } else {
      label.textContent = "The backend receives";
      var parts = OUT[t](ns);
      out.appendChild(document.createTextNode(parts[0]));
      out.appendChild(el("mark", "", parts[1]));
      out.appendChild(document.createTextNode(parts[2]));
      note.textContent = NOTE[t];
    }
    document.querySelectorAll(".tabs [role=tab]").forEach(function (b) {
      var on = b.getAttribute("data-tab") === t;
      b.setAttribute("aria-selected", String(on));
      b.tabIndex = on ? 0 : -1;
    });
    panel.setAttribute("aria-labelledby", "t-" + t);
  }

  function roving(container, selector, attr, apply) {
    container.addEventListener("click", function (e) {
      var b = e.target.closest(selector);
      if (b) { apply(b.getAttribute(attr)); }
    });
    container.addEventListener("keydown", function (e) {
      var keys = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1, Home: "first", End: "last" };
      if (!(e.key in keys)) { return; }
      var items = Array.prototype.slice.call(container.querySelectorAll(selector));
      var i = items.indexOf(document.activeElement);
      if (i < 0) { return; }
      e.preventDefault();
      var k = keys[e.key];
      var next = k === "first" ? items[0] : k === "last" ? items[items.length - 1] : items[(i + k + items.length) % items.length];
      next.focus();
      apply(next.getAttribute(attr));
    });
  }

  roving(document.querySelector(".switch"), "button", "data-pass", function (v) { state.pass = v; renderPass(); renderQuery(); });
  roving(document.querySelector(".tabs"), "[role=tab]", "data-tab", function (v) { state.tab = v; renderQuery(); });
  renderPass();
  renderQuery();
})();

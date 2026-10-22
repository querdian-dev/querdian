"use strict";
// Read-only dashboard. All dynamic text goes through textContent, never innerHTML.
const main = document.getElementById("main");
const ZONE = { prometheus: "Metrics", loki: "Logs", tempo: "Traces" };

function h(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) e.setAttribute(k, v);
  }
  for (const kid of kids.flat()) if (kid != null && kid !== false) e.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  return e;
}
const chip = (t, cls) => h("span", { class: "chip " + (cls || "") }, t);
const chips = (a, cls) => (a && a.length ? a.map((x) => chip(x, cls)) : h("span", { class: "muted" }, "none"));
const mstr = (m) => m.name + m.op + JSON.stringify(m.value);
// Redacted names are also denied; "hidden" lists only the names that are denied without redaction.
const hiddenOnly = (x) => x.deniedLabels.filter((n) => !x.redactions.some((r) => r.name === n));
const rstr = (r) => r.name + ": " + r.before + " → " + (r.after || "removed");
const redChips = (rs) => rs.map((r) => h("span", { class: "chip redact", title: r.strategy + " · " + r.effect }, rstr(r)));

async function api(path, opts) {
  const r = await fetch("api/" + path, opts);
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}
function page(title, lede, ...body) {
  main.replaceChildren(h("h1", {}, title), h("p", { class: "lede" }, lede), ...body);
  main.focus();
}
function fail(e) { main.replaceChildren(h("h1", {}, "Unavailable"), h("p", { class: "deny", style: "padding:12px" }, e.message)); }

async function overview() {
  const o = await api("overview");
  document.getElementById("ver").textContent = o.version || "";
  const stat = (l, v) => h("div", { class: "stat" }, h("span", { class: "label" }, l), h("b", {}, v));
  page("Overview", "The policies this Querdian replica is enforcing right now.",
    h("div", { class: "stats" }, stat("Snapshot generation", o.generation), stat("Policies", o.policies.total),
      stat("Ready", o.policies.ready), stat("Not ready", o.policies.notReady),
      stat("Users", o.subjects.users), stat("Groups", o.subjects.groups), stat("Backends", o.backends)));
}

function ruleView(r) {
  return h("div", { class: "rule" },
    h("span", { class: "chip z-" + r.type }, ZONE[r.type] || r.type), " ", h("b", {}, r.backend), " ",
    r.tenants.length ? h("span", { class: "muted" }, "tenants: ", r.tenants.join(", ")) : null,
    h("div", {}, r.unrestricted ? chip("unrestricted") : chips(r.matchers.map(mstr))),
    hiddenOnly(r).length ? h("div", {}, h("span", { class: "label" }, "Hidden labels "), chips(hiddenOnly(r), "deny")) : null,
    r.redactions.length ? h("div", {}, h("span", { class: "label" }, "Redacted labels "), redChips(r.redactions)) : null);
}

async function policies() {
  const d = await api("policies");
  const open = new Set();
  const body = h("tbody");
  const draw = () => {
    body.replaceChildren();
    for (const p of d.policies) {
      const row = h("tr", { class: "row", tabindex: "0", "aria-expanded": String(open.has(p.id)) },
        h("td", {}, h("b", {}, p.id)),
        h("td", { class: p.ready ? "" : "deny" }, h("b", { class: "mono" }, p.ready ? "Ready" : "NotReady"), " ", h("span", { class: "mono muted" }, p.reason)),
        h("td", {}, chips(p.groups.concat(p.users.map((u) => "user:" + u)))),
        h("td", {}, chips([...new Set(p.rules.map((r) => r.backend))])));
      const toggle = () => { open.has(p.id) ? open.delete(p.id) : open.add(p.id); draw(); };
      row.addEventListener("click", toggle);
      row.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(); } });
      body.append(row);
      if (open.has(p.id)) {
        body.append(h("tr", {}, h("td", { colspan: "4" }, h("div", { class: "drawer" },
          p.message ? h("p", { class: "mono" }, p.message) : null,
          h("dl", { class: "kv" }, h("dt", {}, "Generation"), h("dd", {}, p.generation)),
          p.rules.length ? p.rules.map(ruleView) : h("p", { class: "muted" }, "No compiled rules (policy is not in effect).")))));
      }
    }
  };
  draw();
  page("Policies", "Every ClusterAccessPolicy and AccessPolicy with its compile status. Select a row for its rules.",
    h("div", { class: "tablewrap" }, h("table", {}, h("thead", {}, h("tr", {}, ["Policy", "Status", "Subjects", "Backends"].map((t) => h("th", {}, t)))), body)));
}

function pass(a) {
  const zones = a.map((x) => {
    if (!x.granted) return h("div", { class: "zone none" }, h("b", {}, (ZONE[x.type] || x.type) + " · " + x.backend), h("span", { class: "m" }, x.reason || "no grant"));
    const lines = x.unrestricted ? ["unrestricted"] : x.terms.map((t) => (t.tenant ? t.tenant + ": " : "") + (t.matchers.map(mstr).join(", ") || "all"));
    return h("div", { class: "zone " + x.type }, h("b", {}, (ZONE[x.type] || x.type) + " · " + x.backend),
      lines.map((l) => h("span", { class: "m" }, l)),
      hiddenOnly(x).length ? h("span", { class: "m" }, "hidden: " + hiddenOnly(x).join(", ")) : null,
      x.redactions.map((r) => h("span", { class: "m" }, "redacted: " + rstr(r) + " (" + r.effect + ")")));
  });
  return zones;
}

async function access() {
  const s = await api("subjects");
  const user = h("input", { id: "u", list: "ul", autocomplete: "off", placeholder: "alice" });
  const grp = h("input", { id: "g", list: "gl", autocomplete: "off", placeholder: "team-a, team-b" });
  const out = h("div", { class: "passes", "aria-live": "polite" });
  const go = async (e) => {
    e && e.preventDefault();
    const q = new URLSearchParams({ user: user.value.trim(), groups: grp.value });
    try {
      const r = await api("access?" + q);
      const name = r.user || r.groups.join(" + ") || "anonymous";
      out.replaceChildren(h("div", { class: "pass" }, h("div", { class: "slot" }),
        h("div", { class: "head" }, h("span", { class: "label" }, "Effective access"), h("div", { class: "name" }, name),
          h("span", { class: "mono muted" }, r.groups.join(", "))), pass(r.access),
        h("div", { class: "head" }, h("span", { class: "label" }, "From "), chips([...new Set(r.access.flatMap((x) => x.policies))]))));
    } catch (err) { out.replaceChildren(h("p", { class: "deny" }, err.message)); }
  };
  page("Access", "Pick a user and groups to see the exact union of rules per backend.",
    h("form", { onsubmit: go }, h("div", { class: "form" },
      h("label", { class: "field" }, h("span", { class: "label" }, "User"), user),
      h("label", { class: "field" }, h("span", { class: "label" }, "Groups (comma separated)"), grp)),
      h("button", { class: "btn primary", type: "submit" }, "Show access")),
    h("datalist", { id: "ul" }, s.users.map((u) => h("option", { value: u }))),
    h("datalist", { id: "gl" }, s.groups.map((g) => h("option", { value: g }))), out);
}

async function backends() {
  const d = await api("backends");
  page("Backends", "Configured upstreams and the policies that reach them.",
    h("div", { class: "tablewrap" }, h("table", {}, h("thead", {}, h("tr", {}, ["Backend", "Zone", "Namespace label", "Tenant header", "Policies"].map((t) => h("th", {}, t)))),
      h("tbody", {}, d.backends.map((b) => h("tr", {}, h("td", {}, h("b", {}, b.name)), h("td", {}, h("span", { class: "chip z-" + b.type }, ZONE[b.type] || b.type)),
        h("td", { class: "mono" }, b.namespaceLabel || "–"), h("td", { class: "mono" }, b.tenantHeader || "–"), h("td", {}, chips(b.policies))))))));
}

async function test() {
  const [s, b] = await Promise.all([api("subjects"), api("backends")]);
  const user = h("input", { list: "ul", autocomplete: "off", "aria-label": "User" });
  const grp = h("input", { list: "gl", autocomplete: "off", "aria-label": "Groups" });
  const sel = h("select", { "aria-label": "Backend" }, b.backends.map((x) => h("option", { value: x.name }, x.name + " (" + (ZONE[x.type] || x.type) + ")")));
  const q = h("textarea", { "aria-label": "Query", spellcheck: "false", placeholder: 'rate(http_requests_total[5m])' });
  const out = h("div", { "aria-live": "polite" });
  const run = async (e) => {
    e.preventDefault();
    try {
      const r = await api("test", { method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ user: user.value.trim(), groups: grp.value.split(",").map((x) => x.trim()).filter(Boolean), backend: sel.value, query: q.value }) });
      const strip = h("div", { class: "strip" + (r.allowed && r.upstream.length === 1 ? " two" : "") },
        h("div", {}, h("span", { class: "label" }, "Incoming query"), h("pre", {}, r.input)));
      if (r.allowed) {
        r.upstream.forEach((u, i) => strip.append(h("div", { class: "out" }, h("span", { class: "label" }, r.upstream.length > 1 ? "Stamped query, term " + (i + 1) : "Stamped query"),
          h("pre", {}, u.query))));
        if (r.redactionMode === "rewrite") {
          strip.append(h("div", { class: "out" }, h("span", { class: "label" }, "Result labels · rewritten in the response"),
            h("pre", { class: "redacted" }, r.redactions.map((x) => rstr(x) + "  (" + x.effect + ")").join("\n"))));
        } else if (r.redactionMode === "remove") {
          strip.append(h("div", { class: "out" }, h("span", { class: "label" }, "Result labels · removed by this endpoint"),
            h("pre", { class: "redacted" }, r.redactions.map((x) => x.name).join(", "))));
        }
        strip.append(h("div", { class: "meta" }, "strategy " + r.strategy + " · policies " + r.policies.join(", ")));
      } else {
        strip.append(h("div", { class: "out" }, h("span", { class: "label" }, "Refused · " + r.status), h("pre", { class: "refused" }, r.reason + (r.detail ? ": " + r.detail : ""))));
      }
      out.replaceChildren(strip);
    } catch (err) { out.replaceChildren(h("p", { class: "deny", style: "padding:12px" }, err.message)); }
  };
  page("Test bench", "Dry run: resolve a user's policies and rewrite a query exactly as the proxy would. No backend is contacted.",
    h("form", { onsubmit: run }, h("div", { class: "form" },
      h("label", { class: "field" }, h("span", { class: "label" }, "User"), user),
      h("label", { class: "field" }, h("span", { class: "label" }, "Groups (comma separated)"), grp),
      h("label", { class: "field" }, h("span", { class: "label" }, "Backend"), sel),
      h("label", { class: "field wide" }, h("span", { class: "label" }, "Query"), q)),
      h("button", { class: "btn primary", type: "submit" }, "Rewrite query")),
    h("datalist", { id: "ul" }, s.users.map((u) => h("option", { value: u }))),
    h("datalist", { id: "gl" }, s.groups.map((g) => h("option", { value: g }))), out);
}

const routes = { "": overview, policies, access, backends, test };
async function render() {
  const r = location.hash.replace(/^#\/?/, "");
  document.querySelectorAll("#nav a").forEach((a) => (a.dataset.r === r ? a.setAttribute("aria-current", "page") : a.removeAttribute("aria-current")));
  try { await (routes[r] || overview)(); } catch (e) { fail(e); }
}
addEventListener("hashchange", render);
render();

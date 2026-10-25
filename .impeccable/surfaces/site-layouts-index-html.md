---
version: 1
slug: "site-layouts-index-html"
primary_target: "site/layouts/index.html"
related_targets: []
---

# Surface: querdian.dev landing page (Persuade) and docs (Read)

Scope: landing page `site/layouts/index.html`, documentation templates under `site/layouts/_default/`.
Audience: broad CNCF / cloud-native audience; platform and SRE engineers first. Alpha (first release v0.1.0), said plainly.
Action: read the docs, run the kind quickstart.

## Direction contract

THESIS: Access control is a pass, not a policy file: the page shows a team's pass deciding what each query may touch. It refuses the dark developer-tools hero with an install snippet and a row of feature cards.

OWN-WORLD: Cool paper-white ground (#f4f4f0) and ink (#101418); three zone colours with fixed meaning everywhere: orange = metrics, blue = logs, lime = traces; hatched ink = denied. Archivo (wide, heavy) for pass lettering and headings, JetBrains Mono only for queries, YAML, reason codes. Lanyard card, slot punch, zone bands, stamped-matcher highlights, hairline ink rules. No gradients, no glass.

STORY: The visitor sees one query go in and the same query come out stamped with the team's matchers, understands that unknown users, endpoints and queries are refused, sees real policy YAML, and leaves to run the quickstart or read the threat model, knowing it is alpha.

FIRST VIEWPORT: Left column: headline "Per-team access control for Prometheus, Loki and Tempo", two-sentence explanation, primary action "Run the quickstart", secondary "Read the docs", alpha note with the release version. Right column: a lanyard pass at a slight tilt (name, group, three zone badges, denied-label strip) with a pass switch (alice, bob, carol). A full-width ink rewrite strip sits at the bottom of the viewport with tabs for metrics, logs, traces and denied label, showing the incoming query and the stamped query. Nothing else above the fold.

FORM: Accreditation Pass (IMPECCABLE'S PICK, form 1 of 7 on the grounded list). seed: 839686df9c21fe5d.

SIGNATURE INTERACTION: switching the pass re-colours the zone badges and re-stamps the query in place, using the outputs of the real enforcers; Carol's unrestricted pass shows the query unchanged, Alice's denied label shows the refusal.

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance

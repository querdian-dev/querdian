---
name: Querdian
description: Per-team access control for Prometheus, Loki and Tempo, shown as an accreditation pass on cool paper.
colors:
  paper: "#f4f4f0"
  card: "#ffffff"
  ink: "#101418"
  ink-2: "#3b434c"
  ink-3: "#5b6570"
  rule: "rgba(16, 20, 24, 0.2)"
  metrics-orange: "#ff5a1f"
  logs-blue: "#2a63f0"
  traces-lime: "#c8f169"
  code-comment: "#98a2ad"
  code-string: "#d6dbe0"
  denied-shade: "#dcdcd6"
typography:
  display:
    fontFamily: "Archivo, system-ui, -apple-system, Segoe UI, sans-serif"
    fontSize: "clamp(2.6rem, 6.2vw, 5.4rem)"
    fontWeight: 800
    lineHeight: 0.95
    letterSpacing: "-0.03em"
  pass-name:
    fontFamily: "Archivo, system-ui, sans-serif"
    fontSize: "2.7rem"
    fontWeight: 800
    lineHeight: 1
    letterSpacing: "-0.02em"
  headline:
    fontFamily: "Archivo, system-ui, sans-serif"
    fontSize: "clamp(2.3rem, 4.1vw, 3.8rem)"
    fontWeight: 800
    lineHeight: 1.04
    letterSpacing: "-0.025em"
  title:
    fontFamily: "Archivo, system-ui, sans-serif"
    fontSize: "clamp(1.9rem, 3.6vw, 3rem)"
    fontWeight: 800
    lineHeight: 1.08
    letterSpacing: "-0.02em"
  body:
    fontFamily: "Archivo, system-ui, sans-serif"
    fontSize: "1.0625rem"
    fontWeight: 400
    lineHeight: 1.6
  lede:
    fontFamily: "Archivo, system-ui, sans-serif"
    fontSize: "clamp(1.1rem, 1.5vw, 1.3rem)"
    fontWeight: 400
    lineHeight: 1.5
  label:
    fontFamily: "Archivo, system-ui, sans-serif"
    fontSize: "0.8rem"
    fontWeight: 700
    lineHeight: 1
    letterSpacing: "0.1em"
  mono:
    fontFamily: "JetBrains Mono, ui-monospace, SF Mono, Menlo, monospace"
    fontSize: "0.875em"
    fontWeight: 400
    lineHeight: 1.65
rounded:
  chip: "3px"
  sm: "6px"
  md: "12px"
  lg: "14px"
  pass: "18px"
  pill: "999px"
spacing:
  xs: "6px"
  sm: "12px"
  md: "18px"
  lg: "24px"
  xl: "clamp(40px, 6vw, 84px)"
  gutter: "clamp(20px, 4vw, 56px)"
  wrap: "1180px"
components:
  button:
    backgroundColor: "transparent"
    textColor: "{colors.ink}"
    rounded: "{rounded.pill}"
    padding: "0 22px"
    height: "48px"
  button-primary:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.paper}"
    rounded: "{rounded.pill}"
    padding: "0 22px"
    height: "48px"
  button-primary-hover:
    backgroundColor: "{colors.metrics-orange}"
    textColor: "{colors.ink}"
  pass:
    backgroundColor: "{colors.card}"
    textColor: "{colors.ink}"
    rounded: "{rounded.pass}"
    width: "290px"
  zone-metrics:
    backgroundColor: "{colors.metrics-orange}"
    textColor: "{colors.ink}"
    padding: "9px 18px 10px"
  zone-logs:
    backgroundColor: "{colors.logs-blue}"
    textColor: "{colors.card}"
    padding: "9px 18px 10px"
  zone-traces:
    backgroundColor: "{colors.traces-lime}"
    textColor: "{colors.ink}"
    padding: "9px 18px 10px"
  pass-switch-active:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.paper}"
    rounded: "{rounded.pill}"
    height: "44px"
  rewrite-strip:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.paper}"
    rounded: "{rounded.lg}"
  code-block:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.paper}"
    rounded: "{rounded.md}"
    padding: "18px 20px"
  docs-nav-current:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.paper}"
    rounded: "{rounded.sm}"
    padding: "6px 12px"
  pager-link:
    textColor: "{colors.ink}"
    rounded: "{rounded.md}"
    padding: "12px 14px"
---

# Design System: Querdian

## Overview

**Creative North Star: "The Accreditation Pass"**

Access control is shown as a pass, not a policy file. A lanyard card hangs from a peg rail, names a team, and lists three zones the holder may enter; the page around it is cool paper and ink, with hairline and heavy ink rules like printed credential stock. The surface is flat, legible and slightly stern, which fits a security proxy that fails closed and says plainly that it is alpha.

Three zone colours carry fixed meaning everywhere on the site: orange is metrics, blue is logs, lime is traces. Hatched ink means denied. Nothing else on the site is allowed to use those three hues for decoration, so a colour is always a statement about scope. Archivo, wide and heavy, does the lettering of passes and headings; JetBrains Mono appears only where the content is a query, YAML or a reason code.

**Key Characteristics:**
- Cool paper ground, ink text, one ink hairline vocabulary (1px at 20% ink, 3px solid ink for structure).
- Three zone colours with fixed meaning; denied is a 135-degree ink hatch.
- Wide, heavy Archivo for headings and pass lettering; mono only for machine text.
- Flat surfaces, no gradients as decoration, no glass; one soft drop shadow under the pass.
- One authored motion: the pass settling on its lanyard.

## Colors

A restrained paper-and-ink base with three saturated zone colours that are semantic, not decorative.

### Primary
- **Ink** (`ink`): text, rules, borders, filled buttons, the rewrite strip and code blocks. The only structural colour.

### Secondary (zone colours)
- **Metrics Orange** (`metrics-orange`): the metrics zone: pass badge, metrics band, metrics tab underline, stamped-matcher mark. Also the primary button hover fill.
- **Logs Blue** (`logs-blue`): the logs zone. Text on it is white (`card`).
- **Traces Lime** (`traces-lime`): the traces zone; also the text selection colour.

### Neutral
- **Cool Paper** (`paper`): page ground, footer, header text on dark.
- **Card White** (`card`): the pass body and blockquotes.
- **Ink 2 / Ink 3** (`ink-2`, `ink-3`): secondary and tertiary text (ledes, meta, table headings).
- **Hairline** (`rule`): 1px row dividers and table rules.
- **Code comment / string / denied shade** (`code-comment`, `code-string`, `denied-shade`): supporting greys used only inside dark code surfaces and the denied hatch.

### Named Rules
**The Zone Meaning Rule.** Orange, blue and lime mean metrics, logs and traces and nothing else. They are not accents, not link colours, not syntax highlighting. Code syntax is monochrome on purpose.

**The Denied Hatch Rule.** Refusal is a 135-degree hatch of ink at low alpha (or paper on the dark rewrite strip), never a red.

## Typography

**Display Font:** Archivo (variable, weight 100-900, width 62-125%), fallback system-ui sans.
**Body Font:** Archivo at normal width.
**Label/Mono Font:** JetBrains Mono (variable), fallback ui-monospace.

**Character:** Width is the expressive axis. Headings and pass lettering run wide (font-stretch 108-125%) and heavy (800), body stays at 100% width and 400. Mono is reserved for what the proxy actually sees or emits.

### Hierarchy
- **Display** (800, clamp 2.6-5.4rem, 0.95, wide 125%, uppercase): zone band headings on the landing page.
- **Headline** (800, clamp 2.3-3.8rem, 1.04, 110% width): hero h1.
- **Pass name** (800, 2.7rem, 1, 125% width, uppercase): the name on the pass.
- **Title** (800, clamp 1.9-3rem, 1.08, 112% width): section h2 and docs h1; docs prose h2 is 1.65rem, h3 1.2rem at 700.
- **Body** (400, 1.0625rem, 1.6; docs prose 1.04rem at 1.7, max 78ch): running text. Lede is capped at 46ch.
- **Label** (700, 0.7-0.8rem, tracked 0.1-0.16em, uppercase): pass and table-head labels, hoist labels.
- **Mono** (0.875em, tabular numerals): queries, YAML, reason codes, inline code.

### Named Rules
**The Mono Is Machine Rule.** JetBrains Mono is used only for queries, YAML, status/reason codes and inline code, never for prose or decoration.

**The Width Is Voice Rule.** Emphasis comes from Archivo's width and weight axes; do not add a second display face.

## Layout

A 1180px centred wrap (1320px on docs) with a fluid gutter (`clamp(20px, 4vw, 56px)`). The hero is a 7fr/5fr grid: copy left, pass right, with a full-width ink rewrite strip spanning both. Zone bands are full-bleed colour with a 5fr/6fr heading/text grid; sections use `clamp(56px, 8vw, 112px)` top padding. Docs use a three-column shell (230px nav, fluid article, 210px table of contents) that drops the TOC at 1100px and stacks at 860px. Other breakpoints: 720px (hero stacks) and 480px (compact nav and tabs). Rhythm is generous between sections and tight inside passes and tables (9-18px).

## Elevation & Depth

Flat by default. Structure is carried by ink rules (1px hairline, 3px solid) and full-bleed colour bands, not shadows. The single exception is the pass, which lifts off the page with one soft shadow and a 1px under-edge so it reads as a hanging card.

### Shadow Vocabulary
- **Pass lift** (`box-shadow: 0 22px 30px -18px rgba(16,20,24,0.45), 0 2px 0 rgba(16,20,24,0.1)`): the pass only.
- **Focus halo** (`outline: 3px solid ink; outline-offset: 3px; box-shadow: 0 0 0 6px paper`): all focusable elements; on dark surfaces the outline flips to paper with inset offset.

### Named Rules
**The Flat Page Rule.** Only the pass lifts. Cards, bands and code blocks stay flat.

## Shapes

Credential stock: the pass has 18px corners with a 3px ink border and a punched slot (54x10px pill). Interactive controls are full pills (999px) with 2.5px ink borders. Containers are softly rounded (code blocks 12-14px, pager links 12px, nav links 6px). Inline marks and code chips are barely rounded (3-4px). Zone rows on the pass are square, separated by 3px ink rules.

## Components

### Buttons
- **Shape:** full pill (999px), 48px min height, 2.5px ink border, 22px horizontal padding, Archivo 700.
- **Primary:** ink fill, paper text. Hover turns it metrics orange with ink text.
- **Secondary:** transparent with ink border; hover fills ink.
- **Active:** nudges down 1px. Transitions 0.2s on an ease-out-expo curve.

### Pass (signature)
White card, 3px ink border, 18px radius, tilted -2deg, hung from a peg-rail lanyard. Contents top to bottom: slot punch, tracked label, wide uppercase name, mono group, scope line, three full-width zone rows (zone colour fill, wide uppercase zone name plus mono matcher), a hatched denied-label strip, footnote. Carol's pass adds an ink all-zones bar with a three-colour tri-stripe.

### Pass switch and tabs
A pill segmented control (44px segments, 2.5px ink border); the checked segment is ink on paper text. Rewrite-strip tabs sit on ink with a 5px underline in the matching zone colour (paper for the denied tab).

### Rewrite strip
Full-width ink panel with 14px top corners: a two-column pair of labelled mono blocks, incoming query and stamped query. Matchers are marked with the zone colour as a stamp highlight (3px radius, bold); a denied result is a paper/grey hatched block.

### Zone bands
Full-bleed zone-colour sections with a heavy uppercase heading and a short text column; inline code gets a translucent chip.

### Reasons table
Hairline-ruled table with a 3px ink head rule and tracked ink-3 header labels. Status cells are mono bold; 403 cells carry the denied hatch.

### Code blocks
Ink background, paper text, 12-14px radius, monochrome syntax (comments grey, keys bold white).

### Navigation
Header: wide 800 wordmark with mark, text links with a 2px underline for the current page. Docs nav: sticky column of 6px-radius links with 6px 12px padding; current page is an ink pill with paper text; group headings are small tracked ink-3. Pager: two bordered 12px-radius cards, hover fills ink.

### Zone band strip
An 8px three-segment orange/blue/lime strip above the header.

## Do's and Don'ts

### Do:
- **Do** use orange, blue and lime only for metrics, logs and traces respectively, and the hatch for denied.
- **Do** set machine text (queries, YAML, status codes) in JetBrains Mono and everything else in Archivo.
- **Do** build structure from ink rules: 1px hairline for rows, 3px solid for section and table heads.
- **Do** keep controls as 2.5px ink-bordered pills at 44px or larger, with the paper-halo focus ring.
- **Do** keep motion to the pass settling and the stamp on matchers, and honour `prefers-reduced-motion`.
- **Do** state alpha status plainly wherever the product is described.

### Don't:
- **Don't** use zone colours as decoration, link colour or syntax highlighting.
- **Don't** use red or any other hue for denied; use the hatch.
- **Don't** add gradients or glass for decoration; the only gradients are the denied hatch and the three-colour all-zones stripe.
- **Don't** lift cards or panels with shadows; the pass is the only raised object.
- **Don't** fall back to a dark developer-tools hero with an install snippet and a row of feature cards.

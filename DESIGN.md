---
name: BenchDB
description: A results dashboard for continuous benchmarking; dense, calm, and exact.
colors:
  instrument-blue: "#2563eb"
  instrument-blue-deep: "#1d4ed8"
  instrument-blue-wash: "#eaf1ff"
  on-accent: "#ffffff"
  bench-grey: "#eef1f5"
  inset-grey: "#f7f9fc"
  shell-white: "#fbfcfe"
  surface-white: "#ffffff"
  surface-subtle: "#f9fafc"
  surface-hover: "#f0f3f7"
  ink: "#181b24"
  ink-muted: "#555b6e"
  ink-faint: "#858c9d"
  rule: "#d8dae2"
  rule-muted: "#e6e8ee"
  regressed-red: "#dc2626"
  regressed-red-wash: "#fff1f2"
  improved-green: "#059669"
  improved-green-wash: "#ecfdf5"
  caution-amber: "#d97706"
  caution-amber-wash: "#fffbeb"
  caution-amber-ink: "#92400e"
  stable-slate: "#64748b"
  stable-slate-wash: "#f1f5f9"
  insufficient-grey: "#a3aab7"
  trend-violet: "#7c3aed"
  current-magenta: "#c026d3"
  chart-point: "#1f2937"
  machine-1: "#2563eb"
  machine-2: "#d97706"
  machine-3: "#7c3aed"
  machine-4: "#0891b2"
  machine-5: "#db2777"
  machine-6: "#4f46e5"
typography:
  display:
    fontFamily: "SFMono-Regular, Consolas, Liberation Mono, ui-monospace, monospace"
    fontSize: "clamp(1.65rem, 4vw, 2.5rem)"
    fontWeight: 700
    lineHeight: 1.08
    fontFeature: "tnum"
  headline:
    fontFamily: "Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif"
    fontSize: "1.18rem"
    fontWeight: 700
    lineHeight: 1.2
  title:
    fontFamily: "Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif"
    fontSize: "0.95rem"
    fontWeight: 700
    lineHeight: 1.3
  body:
    fontFamily: "Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif"
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.45
  numeric:
    fontFamily: "SFMono-Regular, Consolas, Liberation Mono, ui-monospace, monospace"
    fontSize: "0.8rem"
    fontWeight: 400
    fontFeature: "tnum"
  label:
    fontFamily: "Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif"
    fontSize: "0.68rem"
    fontWeight: 750
    letterSpacing: "0.04em"
rounded:
  sm: "4px"
  md: "6px"
  lg: "8px"
  pill: "999px"
spacing:
  xs: "4px"
  sm: "6px"
  md: "8px"
  lg: "12px"
  xl: "16px"
components:
  button:
    backgroundColor: "{colors.surface-white}"
    textColor: "{colors.ink-muted}"
    rounded: "{rounded.sm}"
    padding: "0 9px"
    height: "28px"
  button-hover:
    backgroundColor: "{colors.instrument-blue-wash}"
    textColor: "{colors.instrument-blue}"
  button-primary:
    backgroundColor: "{colors.instrument-blue}"
    textColor: "{colors.on-accent}"
    rounded: "{rounded.sm}"
    padding: "0 9px"
    height: "28px"
  button-pressed:
    backgroundColor: "{colors.instrument-blue-wash}"
    textColor: "{colors.instrument-blue-deep}"
  button-danger:
    backgroundColor: "{colors.surface-white}"
    textColor: "{colors.regressed-red}"
    rounded: "{rounded.sm}"
  input:
    backgroundColor: "{colors.surface-white}"
    textColor: "{colors.ink}"
    rounded: "{rounded.sm}"
    padding: "0 9px"
    height: "30px"
  panel:
    backgroundColor: "{colors.surface-white}"
    rounded: "{rounded.md}"
    padding: "12px"
  badge-regressed:
    backgroundColor: "{colors.regressed-red-wash}"
    textColor: "{colors.regressed-red}"
    rounded: "{rounded.pill}"
    padding: "0 7px"
    height: "22px"
  badge-improved:
    backgroundColor: "{colors.improved-green-wash}"
    textColor: "{colors.improved-green}"
    rounded: "{rounded.pill}"
    padding: "0 7px"
    height: "22px"
  badge-caution:
    backgroundColor: "{colors.caution-amber-wash}"
    textColor: "{colors.caution-amber-ink}"
    rounded: "{rounded.pill}"
    padding: "0 7px"
    height: "22px"
  badge-stable:
    backgroundColor: "{colors.stable-slate-wash}"
    textColor: "{colors.stable-slate}"
    rounded: "{rounded.pill}"
    padding: "0 7px"
    height: "22px"
  table-header:
    backgroundColor: "{colors.inset-grey}"
    textColor: "{colors.ink-muted}"
    typography: "{typography.label}"
    height: "32px"
  table-cell:
    backgroundColor: "{colors.surface-white}"
    textColor: "{colors.ink}"
    padding: "7px 10px"
  nav-link:
    textColor: "{colors.ink-muted}"
    rounded: "{rounded.sm}"
    padding: "0 10px"
    height: "34px"
---

# Design System: BenchDB

## Overview

**Creative North Star: "The Lab Notebook"**

BenchDB reads like a careful record of measurements. Every page is a dense,
annotated sheet: the verdict first, then the numbers that justify it, each
traceable to the run, commit, and machine that produced it. Nothing is there
for decoration. A quiet page means nothing needs attention.

The mood is calm, dense, and exact. Cool grey paper carries white panels with
hairline rules. Body text is small (13px) so tables and charts fit on one
screen, and every number is set in a monospace face with tabular figures so
columns line up and values can be compared at a glance. One blue accent marks
what can be clicked or is selected; the remaining color belongs to status and
to the data itself.

Controls are compact and tactile: small, but clearly clickable, with visible
borders and a firm hover response. Surfaces stay flat at rest; only things
that float above the page, such as menus, popovers, and the mobile drawer,
lift with a real shadow.

**Key Characteristics:**

- Dense 13px body with monospace tabular numbers.
- Cool grey ground, white panels, hairline borders, near-flat surfaces.
- One accent (Instrument Blue) for interaction; status colors for verdicts.
- Verdict, then evidence: summaries lead, exact values are one click away.
- Light and dark themes share every token name.

## Colors

A cool, low-chroma neutral ground with one saturated blue and a reserved set
of status colors. The dark theme redefines every token under
`:root[data-theme="dark"]` in `web/src/app.css`; the values in the frontmatter
are the light theme.

### Primary

- **Instrument Blue**: links, focus outlines, selected filters, the active
  navigation item, the trend band, and the single primary button on a page.
  **Instrument Blue Deep** is its hover and pressed state; **Instrument Blue
  Wash** is the background behind hovered and pressed controls.

### Neutral

- **Bench Grey**: the page ground behind every panel.
- **Inset Grey**: table headers and inset code or JSON wells.
- **Shell White**: the sidebar.
- **Surface White**: panels, cards, table rows, and inputs.
- **Surface Subtle** and **Surface Hover**: nested wells and hovered rows or
  navigation items.
- **Ink**: primary text and values. **Ink Muted**: labels, secondary text,
  table headers. **Ink Faint**: separators and de-emphasized metadata only.
- **Rule** and **Rule Muted**: input borders and panel or table hairlines.

### Status

- **Regressed Red**: regressions, errors, destructive actions.
- **Improved Green**: improvements and success.
- **Caution Amber**: warnings, missing baselines, steps; **Caution Amber Ink**
  is its text color on pale backgrounds.
- **Stable Slate**: stable comparisons. **Insufficient Grey**: not enough
  history to judge.
- Each status has a pale wash for badge and row backgrounds.

### Data

- **Machine 1–6**: series colors assigned to machines in a fixed order
  (`web/src/lib/machine-colors.ts`). They avoid red and green so a machine is
  never mistaken for a verdict.
- **Trend Violet**: the rolling mean line. **Current Magenta**: the result
  being viewed. **Chart Point**: individual measurements.

### Named Rules

**The Verdict Color Rule.** Red and green mean regressed and improved, and
nothing else. Never use them for a machine series, a decoration, or a brand
moment.

**The One Accent Rule.** Instrument Blue is the only non-status hue in the
interface chrome. A page has at most one filled blue button.

**The AA Floor Rule.** Text must meet WCAG 2.2 AA contrast (4.5:1 for normal
text) in both themes. Known gaps in the current light theme: Ink Faint on
white (3.4:1), Improved Green text (3.8:1), white text on filled amber status
pills (3.2:1), red on its wash (4.4:1), and slate on its wash (4.3:1). In the
dark theme, Ink Faint on a panel is 3.7:1. Do not add new uses of these pairs
for text that must be read; fix them when touching the component.

## Typography

**Body Font:** Inter (with ui-sans-serif, system-ui, and platform fallbacks)
**Numeric Font:** SFMono-Regular (with Consolas, Liberation Mono, ui-monospace)

**Character:** A neutral sans for words and a monospace face for every
measured value, so numbers are visibly a different kind of thing from labels.
Inter is not bundled; it renders only where installed, and the system sans
stack carries everything else.

### Hierarchy

- **Display**: the one hero measurement on the result page, in monospace.
- **Headline**: page titles (h1).
- **Title**: panel and section headings (h2).
- **Body**: running text, table cells, and controls; 13px base with table
  cells at 0.8rem.
- **Numeric**: values, deltas, z-scores, commit SHAs, and IDs; tabular figures.
- **Label**: table headers and stacked-row labels, uppercase.
- **Eyebrow**: the small uppercase blue line above a page title naming the
  page type or repository (0.68rem, 700, 0.08em tracking).

### Named Rules

**The Tabular Figures Rule.** Every measured number uses the numeric face
with tabular figures, including inside sentences such as "77.72 µs vs
77.67 µs".

**The Small Caps Budget Rule.** Uppercase is for table headers, stacked-row
labels, and the eyebrow. Headings and buttons stay in sentence case.

## Layout

A left sidebar (216px, collapsing to a 56px icon rail) beside one scrolling
content column, capped at 1600px and padded 14px by 16px (10px on phones).
Pages are a vertical stack with a 12px gap: header, one summary line, then
panels. Below 900px the sidebar becomes a drawer behind a 48px top bar. Below
1120px wide data tables restack into labeled rows; below 760px page headers
stack and side-by-side grids collapse to one column.

Spacing steps are 4, 6, 8, 12, and 16px. Controls sit 6–8px apart; panels pad
10–12px; table cells pad 7px by 10px.

### Named Rules

**The Summary Line Rule.** Counts live in one quiet inline line under the
header, separated by middle dots, with only signal-bearing counts (errors,
regressions) emphasized. No big-number stat cards.

**The Earned Column Rule.** A table column or summary count appears only when
it carries information for the rows on screen. Zero counts and columns of
repeated values are hidden.

## Elevation & Depth

Flat at rest, with depth from tonal layering: Bench Grey ground, white panels,
Inset Grey wells, and hairline borders. Panels carry a barely visible 1px
shadow that marks their edge and never grows on hover. Elements that float
above the page, such as menus, popovers, date pickers, and the mobile drawer,
are the only surfaces that should lift with a real shadow. Today the shared
UI kit maps its popover shadow to the panel shadow, so floating surfaces do
not yet lift.

### Named Rules

**The Flat-By-Default Rule.** Nothing on the page plane casts a shadow larger
than a hairline. Lift is reserved for surfaces that float above it.

## Shapes

Gently squared corners: 4px for controls, 6px for panels, 8px for larger
containers, and full pills only for status badges and metadata chips. Borders
are 1px hairlines. The one exception today is the needs-attention cards on the
Runs page, which carry a 3px status-colored left border.

## Components

### Buttons

Compact and tactile; one shape for every action.

- **Shape:** gently squared (4px), 28px tall, 9px side padding, 650 weight.
- **Default:** white with a muted hairline border and Ink Muted text.
- **Hover:** border and text turn Instrument Blue over the blue wash
  (120ms ease on background, border, and color).
- **Primary:** filled Instrument Blue with white text; at most one per page.
- **Pressed / toggled:** blue wash with Instrument Blue Deep text, used for
  filter toggles such as "All / Outliers / Steps".
- **Danger:** red text with a red-tinted border, for delete actions only.
- **Disabled:** 55% opacity, not-allowed cursor.

### Status badges

- **Style:** pill (999px), 22px tall, 0.7rem at 750 weight, pale status wash
  with the status color as text and a status-tinted border.
- **Use:** every verdict carries its word ("regressed", "improved",
  "stable"), never color alone.

### Panels / Cards

- **Corner Style:** 6px.
- **Background:** Surface White on the Bench Grey ground.
- **Shadow Strategy:** the 1px panel shadow only (see Elevation & Depth).
- **Border:** 1px Rule Muted.
- **Internal Padding:** 10–12px.

### Inputs / Fields

- **Style:** 30px tall, white, 1px Rule border, 4px corners.
- **Focus:** border turns Instrument Blue and a 2px blue outline sits 2px
  outside the field.
- **Labels:** small (0.74rem, 700) Ink Muted text above the control.

### Data tables

- **Header:** Inset Grey band, 32px tall, uppercase label type.
- **Rows:** white, separated by Rule Muted hairlines; hover tints the row with
  a 6% blue wash. Error rows take a 5% amber tint.
- **Numbers:** right-hand numeric columns use the numeric face.

### Navigation

- **Sidebar links:** 34px tall, Ink Muted with an icon; hover fills Surface
  Hover; the active link takes an 11% blue wash, Ink text, and a 3px blue
  marker at the sidebar's left edge.
- **Mobile:** a 48px bar with a menu button opens the sidebar as a drawer
  that traps focus and closes on Escape, navigation, or a tap outside.

### Measurement value (signature)

Every displayed measurement renders as a monospace value with a dotted
underline. Hovering shows the exact unscaled value; clicking copies it. Scaled
units (µs, MB, k) are for reading; the exact value is always one click away.

### Filter chips

Removable chips for active filters: 4px corners, a light blue wash, a bold
blue key, the value, and a ×. The whole chip is the remove button.

## Do's and Don'ts

### Do:

- **Do** put the verdict first and link it to the measurements behind it.
- **Do** set every measured number in the numeric face with tabular figures,
  and render displayed measurements with the copyable measurement value.
- **Do** keep the page plane flat; use hairline borders and tonal steps for
  structure.
- **Do** pair every status color with its status word.
- **Do** hide columns and counts that carry no information for the rows on
  screen.

### Don't:

- **Don't** use red or green for anything but regressed and improved, and
  don't use them for machine series.
- **Don't** add big-number stat cards; counts belong in the summary line.
- **Don't** add helper text that repeats a heading or label.
- **Don't** introduce a second accent hue or more than one filled primary
  button on a page.

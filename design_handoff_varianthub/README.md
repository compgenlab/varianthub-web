# Handoff: VariantHub — Genetic Variant Annotation Web App

## Overview

VariantHub is a web application for annotating genetic variants against a library of
annotation sources. A user selects annotations (either a curated **snapshot** or
hand-picked **individual sources**), submits variants (one at a time, as a pasted batch,
or by uploading a VCF), and gets back a results table they can filter, browse in a genome
browser, and export as JSON / TSV / CSV. A companion REST API exposes the same
functionality programmatically. An admin area registers and configures sources, builds
snapshots, and grants team access to private (licensed) sources.

Login is required for all functionality; account/team membership determines which
private sources are visible.

## About the Design Files

The file in this bundle (`VariantHub.dc.html`) is a **design reference created in HTML** —
an interactive prototype showing intended look and behavior. **It is not production code
to copy directly.**

The task is to **recreate this design in the target codebase's existing environment**
(React, Vue, Svelte, etc.) using its established patterns, component library, routing, and
data layer. If no frontend environment exists yet, choose the most appropriate framework
for the project and implement the design there.

Notes on the prototype's construction that do **not** need to be reproduced:

- It is a single-file prototype using inline styles and one component class. In a real
  codebase, split it into route-level views and reusable components with your normal
  styling approach (CSS modules, Tailwind, styled-components, etc.).
- All data is hardcoded mock data. Every list, table, and status value should come from
  the backend.
- The genome browser is a **hand-built visual mock**, not a real embed. Production should
  use **igv.js** (see "Genome browser" below).
- Navigation is local component state. Production should use real routing with URLs.

## Fidelity

**High-fidelity.** Colors, typography, spacing, and interaction behavior are final and
should be recreated faithfully. Exact values are documented in "Design Tokens" below. Use
your codebase's existing primitives (button, input, table, tabs, modal) where they can be
styled to match; the visual result should match the prototype.

---

## Information Architecture

Persistent **left sidebar** (primary navigation, always visible) + **global top bar**
+ scrolling content area.

Sidebar items:

1. **New annotation** — the 3-step annotation flow
2. **Results** — job list → job detail
3. **Genome browser**
4. **API access**
5. *(section label: "Administration" + `ADMIN` badge)* **Sources & snapshots** — admin area

The admin section is **role-gated**: only show it for users with an admin role. In the
prototype it is always visible.

### Route map (suggested)

| View | Suggested route |
|---|---|
| Login | `/login` |
| Step 1 — Choose annotations | `/annotate/sources` |
| Step 2 — Enter variants | `/annotate/variants` |
| Step 3 — Running | `/annotate/running/:jobId` |
| Jobs list | `/jobs` |
| Job results detail | `/jobs/:jobId` |
| Genome browser | `/browser` |
| API access | `/api-access` |
| Admin — sources | `/admin/sources` |
| Admin — add source | `/admin/sources/new` |
| Admin — snapshots | `/admin/snapshots` |
| Admin — build snapshot | `/admin/snapshots/new` |
| Admin — access | `/admin/access` |

---

## Screens / Views

### 1. Login

**Purpose:** Authenticate. Required for all app functionality.

**Layout:** Full-viewport centered card. Card `392px` max width, `#fff` background,
`1px solid rgba(22,24,29,.09)` border, `12px` radius,
`box-shadow: 0 8px 30px rgba(22,24,29,.06)`, padding `34px 32px`. Page background `#f7f8f8`.

**Components (top to bottom, `14px` gap between fields):**

- **Wordmark:** 22×22px `#2f7d92` rounded square (`6px` radius) containing a centered 7×7px
  white square (`2px` radius); `9px` gap; text "VariantHub" at `17px` / weight 600 /
  `letter-spacing: -.01em`. Margin-bottom `22px`.
- **Heading** "Sign in" — `22px`, weight 600, margin-bottom `5px`.
- **Subtitle** — "Login is required. Private sources such as COSMIC are unlocked per
  account." `13px`, `rgba(22,24,29,.55)`, margin-bottom `22px`.
- **Email field** — label `12px` `rgba(22,24,29,.6)`, margin-bottom `6px`; input height
  `40px`, padding `0 12px`, `1px solid rgba(22,24,29,.14)`, `8px` radius, `14px` text.
- **Password field** — same spec, `type="password"`.
- **Primary button** "Sign in" — full width, height `42px`, `#2f7d92` background, `#fff`
  text, no border, `8px` radius, `14.5px` / weight 500. Margin-top `20px`.
- **Secondary button** "Continue with institutional SSO" — full width, height `40px`,
  `#fff` background, `1px solid rgba(22,24,29,.14)`, `8px` radius, `14px` / weight 500.
  Margin-top `10px`.

**Behavior:** Both buttons authenticate and land on Step 1 (Choose annotations). Wire to
real auth (session cookie or token) and institutional SSO (SAML/OIDC) respectively.

---

### 2. App shell

#### Sidebar

`238px` fixed width, `#fff` background, `1px solid rgba(22,24,29,.08)` right border,
full height, `flex: none`.

- **Wordmark block:** padding `18px 18px 16px`; same 22×22px mark as login, `16px` text.
- **Nav list:** padding `6px 12px`, `2px` gap between items.
- **Nav item:** `display:flex`, `align-items:center`, `gap: 11px`, padding `9px 12px`,
  `7px` radius, `14px` text, weight 500, transparent background, 17×17px stroke icon
  (`stroke-width: 1.6`, `currentColor`).
  - Inactive: `color: rgba(22,24,29,.6)`
  - **Active: `color: #245f70`, `font-weight: 600`, no background fill.** (Deliberate — the
    active item is indicated by color + weight only. Do not add a background, left border,
    or bracket.)
- **Administration section label:** padding `14px 12px 5px`; label "Administration" at
  `10px`, `letter-spacing: .09em`, uppercase, `rgba(22,24,29,.35)`; followed by an `ADMIN`
  pill — mono `9px`, padding `1px 6px`, `4px` radius, background `#f6eaea`, color `#8f2f2f`.

Icons used (all 24×24 viewBox, stroke): New annotation = document with plus; Results =
grid/table; Genome browser = crosshair/target; API access = terminal chevron in rounded
rect; Sources & snapshots = shield with check.

#### Global top bar

`flex: none`, padding `11px 30px`, `#fff` background, `1px solid rgba(22,24,29,.08)`
bottom border, `justify-content: space-between`.

- **Left — active reference:** label "ACTIVE REFERENCE" (`10px`, `letter-spacing: .09em`,
  uppercase, `rgba(22,24,29,.42)`); then the build in a mono pill — `12.5px` / weight 500,
  padding `3px 10px`, `#f2f4f5` background, `6px` radius; then source count as
  "{n} sources" at `12.5px` `rgba(22,24,29,.5)`. `10px` gap.
- **Right — user:** name "A. Researcher" (`13px`, weight 500) with role "Org admin"
  (`11px`, `rgba(22,24,29,.45)`) right-aligned; then a 30px circular avatar,
  `#ecf3f5` background, `#245f70` text, initials at `12px` weight 600. Then a separate
  **sign-out button**: 34×34px, `#fff`, `1px solid rgba(22,24,29,.12)`, `8px` radius,
  logout arrow icon, `title="Sign out"`.

Both the reference build and the user block are global — present on every authenticated view.

#### Step header (annotation flow only)

Shown **only** on Step 1, Step 2, and Running — *not* on Results, Jobs, Browser, API, or
Admin. `flex: none`, padding `16px 30px`, `#fff`, `1px solid rgba(22,24,29,.08)` bottom
border. Sits **below** the global top bar.

Three steps: **1 Sources → 2 Variants → 3 Results**, `10px` gap between dot and label.

- **Dot:** 24×24px circle, `1.5px` border, mono `11px`.
  - Complete: border `#2f7d92`, background `#2f7d92`, `#fff` checkmark glyph
  - Active: border `#2f7d92`, background `#ecf3f5`, color `#245f70`, shows the number
  - Upcoming: border `rgba(22,24,29,.2)`, transparent, color `rgba(22,24,29,.4)`
- **Label:** `14px` weight 500. Active `#16181d`; complete `rgba(22,24,29,.65)`; upcoming
  `rgba(22,24,29,.4)`.
- **Connector:** `52px × 1px`, margin `0 16px`. Complete `#2f7d92`, else
  `rgba(22,24,29,.15)`. Hidden after the last step.

---

### 3. Step 1 — Choose annotations

**Purpose:** Pick the annotation sources for the run.

**Layout:** `max-width: 1020px`, centered, padding `34px 30px 60px`.

**Components:**

- **Heading** "Choose annotations" — `26px` weight 600.
- **Description** (`14px`, `rgba(22,24,29,.55)`, `max-width: 64ch`, `line-height: 1.55`):
  "Annotate with a curated **snapshot** — a versioned, admin-locked bundle of sources — or
  hand-pick **individual sources**. Every source is tabix-indexed; VEP runs a VCF through
  Ensembl VEP and re-indexes the output." (bolded words at weight 600, `#16181d`)
- **Segmented control** — "Snapshots" / "Individual sources". Container:
  `display:inline-flex`, `3px` gap, `3px` padding, `#eceeef` background, `9px` radius.
  Each button: height `34px`, padding `0 16px`, no border, `7px` radius, `13px` weight 500.
  Active: `#fff` background, `#16181d` text, `box-shadow: 0 1px 2px rgba(22,24,29,.1)`.
  Inactive: transparent, `rgba(22,24,29,.55)`.

#### Snapshots mode (default)

Two-column grid, `16px` gap. Each card is a button: padding `17px`, `11px` radius,
`1.5px` border, `display:flex`, `column`, `text-align: left`.

- Unselected: border `rgba(22,24,29,.09)`, background `#fff`,
  `box-shadow: 0 1px 2px rgba(22,24,29,.03)`
- Selected: border `#2f7d92`, background `#f3f8f9`,
  `box-shadow: 0 2px 12px rgba(47,125,146,.1)`

Card contents:

1. **Row:** name (`17px` weight 600, `line-height: 1.2`) + checkbox indicator pushed right —
   20×20px, `6px` radius, `1.5px` border. Selected: `#2f7d92` fill, `#fff` check.
   Unselected: `visibility: hidden`.
2. **Meta line:** mono `11px` `#2f7d92` — `"{build} · {n} sources"`. Margin `3px 0 10px`.
3. **Description:** `12.5px`, `rgba(22,24,29,.58)`, `line-height: 1.5`, margin-bottom `12px`.
4. **Tag row:** `5px` gap, wrapping. Each tag: mono `10px`, padding `2px 8px`, `#f2f4f5`
   background, `5px` radius, `rgba(22,24,29,.6)`.
5. **Private notice** (only when the snapshot contains private sources): lock icon +
   "Contains private sources — unlocked for your account", `11.5px`,
   `rgba(22,24,29,.5)`, margin-top `11px`.

Snapshot data in the prototype:

| Name | Build | Sources | Description | Tags | Private |
|---|---|---|---|---|---|
| GRCh38 Clinical v4 | GRCh38 | 6 | Curated for germline variant review — significance, population frequency and splicing. | VEP·110, ClinVar, gnomAD·4.1, SpliceAI | no |
| GRCh38 Research v2 | GRCh38 | 9 | Everything in Clinical plus deleteriousness meta-predictors and conservation. | +dbNSFP·4.7, +dbSNP, +phyloP | no |
| Somatic Oncology v3 | GRCh38 | 8 | Tumour annotation with COSMIC census and cancer hotspots. | COSMIC·99, CancerHotspots, OncoKB | **yes** |
| GRCh37 Legacy v1 | GRCh37 | 5 | For pipelines still on hg19. Sources lifted over and re-indexed. | VEP·110, ClinVar, gnomAD·2.1 | no |

Selection is **single-select** (radio semantics) in snapshot mode.

#### Individual sources mode

A table card: `#fff`, `1px solid rgba(22,24,29,.09)`, `10px` radius, `overflow: hidden`.

Grid columns: `26px 1.7fr 1fr .8fr 1fr`, `12px` gap.

- **Header row:** padding `11px 18px`, `1px solid rgba(22,24,29,.08)` bottom border, labels
  at `10.5px` `letter-spacing: .06em` uppercase `rgba(22,24,29,.45)`:
  *(blank) / Source / Version / Type / Access*
- **Body rows** (each a button): padding `12px 18px`,
  `1px solid rgba(22,24,29,.06)` bottom border. Selected row background `#f3f8f9`.
  - **Checkbox:** 18×18px, `5px` radius, `1.5px` border. Checked: `#2f7d92` fill + `#fff`
    check; unchecked border `rgba(22,24,29,.2)`.
  - **Source:** name `14px` weight 500; detail line `11.5px` `rgba(22,24,29,.48)`.
  - **Version:** mono `12px` `#245f70`.
  - **Type:** mono `10px` pill, padding `2px 7px`, `#f2f4f5`, `5px` radius,
    `rgba(22,24,29,.55)`.
  - **Access:** mono `11px`. Private `#a86a1e`; Public `rgba(22,24,29,.45)`.

Source data:

| Source | Detail | Version | Type | Access |
|---|---|---|---|---|
| Ensembl VEP | Consequence & transcript effects | release-110 | VCF→VCF | Public |
| ClinVar | Clinical significance | 2026-06 | VCF.gz | Public |
| gnomAD | Population allele frequencies | v4.1 | VCF.gz | Public |
| SpliceAI | Splice-altering prediction | v1.3 | VCF.gz | Public |
| dbSNP | Reference SNP identifiers | b156 | VCF.gz | Public |
| dbNSFP | Meta-predictors & conservation | v4.7a | tabix | Public |
| COSMIC | Somatic mutation census | v99 | BED.gz | **Private** |
| Lab panel (custom) | In-house curated BED | v2.1 | BED.gz | **Private** |

Multi-select. Defaults checked: VEP, ClinVar, gnomAD, SpliceAI.

#### Footer

`margin-top: 26px`, `space-between`.

- **Left:** "**{n}** sources selected" — count at `15px` weight 600 `#16181d`, rest `13px`
  `rgba(22,24,29,.55)`. In snapshot mode the count is the selected snapshot's source count;
  in individual mode it's the number of checked sources.
- **Right:** primary button "Continue to variants" + right-arrow icon — height `42px`,
  padding `0 24px`, `#2f7d92`, `#fff`, `8px` radius, `14.5px` weight 500, `8px` gap.

---

### 4. Step 2 — Enter variants

**Purpose:** Provide the variants to annotate.

**Layout:** `max-width: 880px`, centered, padding `34px 30px 60px`.

**Header row** (`space-between`, wrapping):

- **Left:** "Enter variants" (`26px` weight 600) + "Accepts VCF-style coordinates, HGVS, or
  rsIDs. Mix formats freely." (`14px`, `rgba(22,24,29,.55)`)
- **Right — Reference build select:** label "REFERENCE BUILD" (`11px`,
  `letter-spacing: .05em`, uppercase, `rgba(22,24,29,.45)`); native `<select>`,
  `min-width: 196px`, height `40px`, padding `0 32px 0 12px`, mono `13px`, `#fff`,
  `1px solid rgba(22,24,29,.14)`, `8px` radius, `appearance: none`, with a custom
  14×14px chevron absolutely positioned at `right: 11px; top: 13px`.
  Options: `GRCh38 / hg38`, `GRCh37 / hg19`, `T2T-CHM13 v2.0`, `GRCm39 (mouse)`.
  **Changing this updates the build shown in the global top bar.** The build list must
  support multiple and non-human references.

**Segmented control** — "Single" / "Batch paste" / "VCF upload". Same spec as Step 1.
Margin `24px 0 22px`. Default: **Batch paste**.

#### Single mode

- **Row** (`10px` gap, `align-items: flex-end`, wrapping): input (`flex: 1`,
  `min-width: 300px`, height `42px`, padding `0 13px`, mono `13px`, `#fff`,
  `1px solid rgba(22,24,29,.14)`, `8px` radius) with label "Variant" and placeholder
  `chr17-7676154-C-T · NM_000546.6:c.215C>G · rs28934578`; plus an "Add" button with a
  plus icon (height `42px`, padding `0 20px`, `#fff`, `1px solid rgba(22,24,29,.16)`,
  `8px` radius, `14px` weight 500).
- **Chip list** (`8px` gap, wrapping, margin-top `16px`): each chip padding
  `6px 8px 6px 12px`, `#fff`, `1px solid rgba(22,24,29,.12)`, `7px` radius, mono `12px`,
  with an ✕ remove button (13×13px icon, `rgba(22,24,29,.4)`).
- Default chips: `chr17-7676154-C-T`, `NM_004333.6:c.1799T>A`, `rs113488022`.
- **Behavior:** "Add" appends the trimmed input value and clears the field; empty input is
  a no-op. ✕ removes that chip. Production should validate/normalize each entry and show
  per-chip parse errors.

#### Batch paste mode

- Label "One variant per line". Textarea: full width, `min-height: 220px`, padding `13px`,
  mono `12.5px`, `line-height: 1.75`, `#fff`, `1px solid rgba(22,24,29,.14)`, `8px`
  radius, `resize: vertical`.
- Default content (demonstrates mixed formats):
  ```
  chr7-140753336-A-T
  chr17-7676154-C-T
  NM_000546.6:c.743G>A
  rs28934578
  chr13-32340301-G-A
  17-43091983-CT-C
  ```
- Below: "**{n}** variants detected · formats auto-detected per line" — count `#245f70`
  weight 600, rest `12.5px` `rgba(22,24,29,.5)`. Recomputed on input as the number of
  non-blank lines.

#### VCF upload mode

- **Dropzone:** `1.5px dashed rgba(47,125,146,.45)`, `#f0f6f7` background, `10px` radius,
  padding `36px`, centered. 30×30px upload icon (`#2f7d92`), then "Drop a .vcf or .vcf.gz
  file" (`16px` weight 600), then "or click to browse · bgzipped + tabix accepted"
  (`13px` `rgba(22,24,29,.5)`).
- **Uploaded-file row** (margin-top `14px`): `#fff`, `1px solid rgba(22,24,29,.1)`,
  `9px` radius, padding `13px 15px`, `12px` gap. File icon `#2f7d92`; filename mono `13px`
  weight 500; meta `11.5px` `rgba(22,24,29,.48)` — e.g. "4,812 variants · 14.2 MB · index
  OK"; right-side `READY` pill — mono `10px`, padding `3px 9px`, `#eef4ee` background,
  `#3c6642` text, `5px` radius.
- **Production:** real drag-and-drop + file picker, chunked/resumable upload for large
  files, server-side parse to report variant count and index validity, and error states
  for malformed/unindexed input.

#### Footer

`margin-top: 30px`, `padding-top: 20px`, `1px solid rgba(22,24,29,.08)` top border,
`space-between`.

- **Left:** text button "← Back to sources" — `#245f70`, `14px` weight 500, no background.
- **Right:** primary "Run annotation" with a solid play triangle icon — height `42px`,
  padding `0 24px`, `#2f7d92`, `8px` radius, `14.5px` weight 500.

---

### 5. Step 3 — Running

**Purpose:** Show progress while the job executes.

**Layout:** `max-width: 600px`, `margin: 64px auto 0`, padding `0 30px`.

**Card:** `#fff`, `1px solid rgba(22,24,29,.09)`, `12px` radius,
`box-shadow: 0 4px 20px rgba(22,24,29,.05)`, padding `30px`.

- **Header row:** 18×18px spinner (`#2f7d92`, `stroke-width: 2`, arc path,
  `animation: spin 1s linear infinite`) + "Annotating variants" (`20px` weight 600).
- **Sub-line:** "Job **#a7f3-2e** · {n} sources · {m} variants" — job id in mono,
  `13px` `rgba(22,24,29,.55)`, margin-bottom `20px`.
- **Progress bar:** track height `6px`, `rgba(22,24,29,.08)`, `3px` radius,
  `overflow: hidden`. Fill `#2f7d92`, `3px` radius, `transition: width .15s linear`.
- **Progress meta row** (`space-between`, mono `11px`, `rgba(22,24,29,.5)`): current stage
  label on the left, `{pct}%` on the right.
- **Stage checklist** (margin-top `22px`, `10px` gap), four rows, mono `13px`:
  1. Normalise & left-align variants
  2. Query tabix sources (ClinVar, gnomAD, SpliceAI)
  3. Ensembl VEP consequence calling
  4. Merge & write output index

  Each row has a 16px-wide glyph: complete `✓` (`#2f7d92`), active `▸` (`#2f7d92`,
  `animation: blink 1s infinite`), pending `·` (`rgba(22,24,29,.3)`). Text color
  `#16181d` when complete/active, else `rgba(22,24,29,.4)`.

**Stage labels** (shown in the meta row): "Parsing & normalising variants" → "Querying
tabix-indexed sources" → "Running Ensembl VEP" → "Merging annotations".

**Behavior in the prototype:** progress increments on a 150ms interval and advances stage
at 20% / 55% / 82%; at 100% it navigates to the job's results after a 350ms beat.
**Production:** poll `GET /jobs/{id}` (or use SSE/WebSocket) for real stage + percent, then
redirect to `/jobs/{id}`. Handle failure by showing an error state with the failing stage
and log output. The interval must be cleared on unmount.

---

### 6. Jobs list

**Purpose:** The landing view for "Results" — all submitted annotation runs. Users reach
their results by opening a completed job.

**Layout:** `max-width: 1060px`, centered, padding `30px 30px 60px`. No step header.

**Header row** (`space-between`, wrapping, margin-bottom `18px`):

- **Left:** "Jobs" (`26px` weight 600) + "Submitted annotation runs. Open a completed job
  to review its variants." (`13.5px` `rgba(22,24,29,.55)`)
- **Right:** primary "New annotation" button with plus icon — height `38px`, padding
  `0 16px`, `#2f7d92`, `8px` radius, `13px` weight 500. Navigates to Step 1.

**Table card:** `#fff`, `1px solid rgba(22,24,29,.09)`, `10px` radius, `overflow: hidden`.
Grid columns `.7fr 1.5fr 1.1fr .7fr .9fr 34px`, `12px` gap.

- **Header:** padding `11px 18px`, bottom border `1px solid rgba(22,24,29,.08)`, labels
  `10.5px` `letter-spacing: .06em` uppercase `rgba(22,24,29,.45)`:
  *Job / Input / Annotations / Variants (right-aligned) / Status / (blank)*
- **Rows** (each a button): padding `13px 18px`, bottom border
  `1px solid rgba(22,24,29,.06)`, `text-align: left`.
  - **Job:** mono `12px` `#245f70`
  - **Input:** primary line `13.5px` weight 500; submitted timestamp `11px`
    `rgba(22,24,29,.45)`
  - **Annotations:** snapshot name `12.5px`; build mono `10.5px` `rgba(22,24,29,.45)`
  - **Variants:** mono `12.5px`, right-aligned
  - **Status:** 8px colored dot + label `12.5px` in the status color; a running job also
    shows its percent in mono `11px` `rgba(22,24,29,.4)`
  - **Chevron:** right-chevron, `rgba(22,24,29,.35)` for completed rows, faded to
    `rgba(22,24,29,.12)` otherwise

**Status colors:** Complete `#4a7a52` · Running `#b08a2e` (dot pulses via blink animation)
· Queued `rgba(22,24,29,.4)` · Failed `#a23c3c`.

**Behavior:** Only **Complete** rows are clickable (`cursor: pointer`) and navigate to the
job's results. Running/Queued/Failed rows are inert (`cursor: default`). In production,
Failed should open an error detail view, and the list should paginate and live-update
running jobs.

Job data in the prototype:

| Job | Input | Submitted | Annotations | Build | Variants | Status |
|---|---|---|---|---|---|---|
| #a7f3-2e | cohort_batch07.vcf.gz | Today, 14:22 | GRCh38 Clinical v4 | GRCh38 | 4,812 | Complete |
| #b1c9-77 | Batch paste · 6 variants | Today, 11:05 | Somatic Oncology v3 | GRCh38 | 6 | Running (68%) |
| #c4e2-01 | trio_proband.vcf.gz | Today, 09:41 | GRCh38 Research v2 | GRCh38 | 1,204 | Queued |
| #d8a5-19 | panel_run_92.vcf.gz | Yesterday, 17:30 | GRCh38 Clinical v4 | GRCh38 | 318 | Complete |
| #e2b7-44 | Single · chr17-7676154-C-T | Yesterday, 15:12 | Custom · 4 sources | GRCh38 | 1 | Complete |
| #f0d3-88 | legacy_hg19.vcf.gz | Jul 23, 16:58 | GRCh37 Legacy v1 | GRCh37 | 2,140 | Failed |

---

### 7. Job results detail

**Purpose:** Review, filter, browse, and export the annotated variants for one job.

**Layout:** Full-height flex column: fixed header block, scrolling table, fixed pagination
footer. No step header.

#### Header block (`flex: none`, padding `24px 30px 16px`)

- **Back link:** "← All jobs" — `#245f70`, `13px` weight 500, no background/padding,
  margin-bottom `12px`. Returns to the jobs list.
- **Title row** (`space-between`, `align-items: flex-end`, wrapping):
  - **Left:** eyebrow "JOB #A7F3-2E · GRCH38 / HG38" (`11px`, `letter-spacing: .06em`,
    uppercase, `#2f7d92`), then "Annotated variants" (`24px` weight 600).
  - **Right — toolbar** (`8px` gap, `position: relative`):
    - **Search field:** height `38px`, padding `0 12px`, `#fff`,
      `1px solid rgba(22,24,29,.12)`, `8px` radius, `7px` gap; 14px magnifier icon
      (`rgba(22,24,29,.4)`); borderless input `13px`, width `214px`, placeholder
      "Filter by gene, rsID, consequence…"
    - **"Filter"** button — funnel icon, height `38px`, padding `0 13px`, `#fff`,
      `1px solid rgba(22,24,29,.12)`, `8px` radius, `13px` weight 500
    - **"Columns"** button — same spec, sliders icon
    - **"Download"** button — primary, height `38px`, padding `0 16px`, `#2f7d92`, `#fff`,
      `8px` radius, `13px` weight 500, download icon
    - **Download menu** (toggled): absolutely positioned `top: 44px; right: 0`,
      `width: 210px`, `z-index: 20`, `#fff`, `1px solid rgba(22,24,29,.1)`, `9px` radius,
      `box-shadow: 0 10px 30px rgba(22,24,29,.12)`. Header row "EXPORT {n} VARIANTS"
      (`10px` uppercase `letter-spacing: .07em` `rgba(22,24,29,.45)`, padding `9px 13px`,
      bottom border). Three items, each padding `11px 13px`, `space-between`: mono
      filename (weight 500) + format label (`11px` `rgba(22,24,29,.45)`):
      `variants.json` / JSON, `variants.tsv` / TSV, `variants.csv` / CSV.
- **Summary + legend row** (margin-top `14px`, `14px` gap, `12.5px`
  `rgba(22,24,29,.55)`): "Showing **1–14** of **4,812**", a `|` divider
  (`rgba(22,24,29,.2)`), then three legend entries with 9px colored dots:
  Pathogenic / LP `#a23c3c`, VUS `#b08a2e`, Benign / LB `#4a7a52`.

#### Table (`flex: 1`, `overflow: auto`)

`#fff` background, top and bottom borders `1px solid rgba(22,24,29,.08)`.
`<table>` with `border-collapse: collapse`, `font-size: 13.5px`, `min-width: 1080px`
(horizontal scroll below that).

- **Header:** `position: sticky; top: 0; z-index: 2`, `#fff` background,
  `box-shadow: 0 1px 0 rgba(22,24,29,.08)`. Cells padding `11px 16px`, `10.5px`,
  `letter-spacing: .06em`, uppercase, `rgba(22,24,29,.45)`.
  Columns: checkbox (34px) · Variant · Gene · Consequence *(with a `#2f7d92`
  non-uppercase "VEP" source tag)* · ClinVar · gnomAD AF (right) · SpliceAI (right) ·
  rsID · action (44px).

  **Column headers name their source annotation** — the "VEP" tag pattern should extend to
  the other columns (ClinVar, gnomAD, SpliceAI) so users can tell which source produced
  each value. Columns are dynamic: they depend on the sources selected for the job.

- **Rows:** bottom border `1px solid rgba(22,24,29,.06)`, cells padding `10px 16px`.
  - **Checkbox:** `accent-color: #2f7d92`
  - **Variant:** mono `12.5px`, `white-space: nowrap`, chromosome in `#2f7d92` then
    `-{pos}-{ref}-{alt}` in default color
  - **Gene:** weight 500, `font-style: italic`
  - **Consequence:** mono `11.5px` `rgba(22,24,29,.68)`
  - **ClinVar:** pill — `12px` weight 500, padding `2px 10px`, `20px` radius,
    `white-space: nowrap`:
    - Pathogenic → background `#f6eaea`, color `#8f2f2f`
    - Likely path. → background `#f9f0ec`, color `#a85a3a`
    - VUS → background `#f6f0e0`, color `#8a6a1e`
    - Benign → background `#eef4ee`, color `#3c6642`
    - Likely benign → background `#f1f5f1`, color `#5a7a5f`
  - **gnomAD AF:** mono `12.5px`, right-aligned; `#16181d` when present, `rgba(22,24,29,.3)`
    when `—`
  - **SpliceAI:** mono `12.5px`, right-aligned
  - **rsID:** mono `12px` `#245f70`
  - **Action:** icon button (crosshair), `rgba(22,24,29,.35)`, `title="Open in genome
    browser"` — navigates to the genome browser focused on that variant's locus

Sample rows (14 in the prototype) include TP53 chr17-7676154-C-T (Pathogenic,
rs28934578), BRAF chr7-140753336-A-T (Pathogenic, rs113488022), BRCA2 chr13-32340301-G-A
(Pathogenic, stop_gained), BRCA1 chr17-43091983-CT-C (Likely path., frameshift, novel),
MSH2 chr2-47478462-G-A (VUS, splice_donor, SpliceAI 0.88), and benign/likely-benign rows
in PTEN, NF1, ATM.

#### Pagination footer (`flex: none`, padding `13px 30px`, `space-between`)

- **Left:** "Page **1** of **344** · 14 rows / page" — `12.5px` `rgba(22,24,29,.5)`,
  numbers `#16181d` weight 600
- **Right:** button group, `6px` gap. Prev/next are 34×34px, `#fff`,
  `1px solid rgba(22,24,29,.12)`, `7px` radius, chevron icons; prev disabled at page 1
  (`opacity: .4`, `cursor: not-allowed`). Page numbers 34×34px, mono `13px`; the current
  page has `#2f7d92` background, `#fff` text, no border.

**Scale requirement:** the design targets **thousands of variants**. Use a virtualized
table body (e.g. TanStack Virtual, react-window) with server-side pagination, sorting, and
filtering. Search, Filter, and Columns are non-functional affordances in the prototype and
need real implementations: debounced server-side search; a filter panel (by gene,
consequence, significance, AF threshold, SpliceAI score); and a column visibility manager
driven by the job's selected sources.

**Export:** `Download` should stream server-generated JSON / TSV / CSV for the **whole
result set** (not just the current page), respecting active filters, and offer the
selected-rows subset when checkboxes are used.

---

### 8. Genome browser

**Purpose:** Visually inspect variants and annotation tracks along the genome. Serves three
roles: a first-class browsing view, a drill-in target from a results row, and a way to
browse annotation sources without submitting variants.

> **Implementation note:** this view is a **static visual mock** in the prototype. Build it
> with **igv.js**, configured against the same tabix-indexed sources the backend serves.
> The layout below documents the intended chrome, track list, and styling so the igv.js
> instance can be themed to match. igv.js supports `annotation`/`variant`/`wig` track types
> over bgzipped+tabix files, which maps directly onto the source library.

**Layout:** Full-height flex column: toolbar, ideogram, ruler, scrolling track area.

#### Toolbar (`flex: none`, padding `14px 26px`, `#fff`, bottom border, wrapping, `10px` gap)

- **Build select:** height `36px`, padding `0 30px 0 11px`, mono `12px`, `#fff`,
  `1px solid rgba(22,24,29,.14)`, `8px` radius, custom chevron. Options: `GRCh38 / hg38`,
  `GRCh37 / hg19`, `T2T-CHM13`.
- **Locus search:** height `36px`, padding `0 11px`, `#fff`,
  `1px solid rgba(22,24,29,.14)`, `8px` radius, `flex: 1`, `min-width: 220px`,
  `max-width: 370px`; magnifier icon + borderless mono `12px` input.
  Value: `chr17:7,668,000-7,690,000`. Should accept locus strings, gene symbols, and rsIDs.
- **Zoom out / in:** two 36×36px buttons (minus, plus), `#fff`,
  `1px solid rgba(22,24,29,.14)`, `8px` radius, `6px` gap.
- **"Add track"** button, pushed right (`margin-left: auto`): height `36px`, padding
  `0 13px`, `#fff`, `1px solid rgba(22,24,29,.14)`, `8px` radius, `13px` weight 500, plus
  icon. Opens a picker of available annotation sources (respecting private-source grants).

#### Ideogram (`flex: none`, padding `14px 26px 8px`)

- **Label row:** mono `11px`; chromosome (`#16181d` weight 500) + cytoband
  (`rgba(22,24,29,.5)`), `10px` gap, margin-bottom `7px`. e.g. "chr17  p13.1"
- **Chromosome bar:** height `14px`, `7px` radius, `#eceeef`, `overflow: hidden`.
  Centromere: `rgba(22,24,29,.35)` vertical band. Current-view window: `1.5px` `#2f7d92`
  inset outline over `rgba(47,125,146,.35)` fill, `4px` radius.

#### Ruler (`flex: none`, padding `0 26px`)

A `160px` spacer (aligning with the track-label gutter) then a `flex: 1` axis, height
`26px`, `1px solid rgba(22,24,29,.12)` bottom border. Tick marks `1px × 6px`
(`rgba(22,24,29,.25)`) with mono `10px` `rgba(22,24,29,.45)` labels offset `3px` right —
e.g. 7,668 kb / 7,674 kb / 7,680 kb / 7,686 kb / 7,690 kb.

#### Track area (`flex: 1`, `overflow: auto`, padding `0 26px 20px`)

Each track is a row: `160px` label gutter + `flex: 1` canvas with
`1px solid rgba(22,24,29,.08)` left border. Rows separated by
`1px solid rgba(22,24,29,.06)`.

- **Label gutter:** name `12.5px` weight 500 (with an 11px lock icon,
  `rgba(22,24,29,.4)`, for private sources); meta line mono `10px` `rgba(22,24,29,.4)`.

Tracks in the prototype:

| Track | Meta | Height | Rendering |
|---|---|---|---|
| RefSeq genes | TP53 ×3 iso | 58px | Gene model: `1.5px` `#2f7d92` spine with `13px`-tall exon blocks (`#2f7d92`, `2px` radius) |
| User variants | 6 loaded | 44px | 12px `#2f7d92` circles, `box-shadow: 0 1px 2px rgba(22,24,29,.15)`, tooltip = variant id |
| ClinVar | VCF · 2026-06 | 44px | 10px circles colored by significance: `#a23c3c` pathogenic, `#b08a2e` VUS, `#4a7a52` benign |
| gnomAD AF | v4.1 · density | 44px | Histogram of `rgba(47,125,146,.5)` bars, `1.5px` gap, `2px 2px 0 0` radius, bottom-aligned |
| COSMIC | private · v99 | 44px | Locked placeholder: 45° repeating-linear-gradient hatch (`rgba(22,24,29,.025)` 0–7px), centered mono `11.5px` `rgba(22,24,29,.45)` text "Private source — access granted" |

Variant marks carry hover tooltips; in production these become igv.js popovers showing the
full annotation record for that feature.

---

### 9. API access

**Purpose:** Document and manage programmatic access to the companion REST server.

**Layout:** `max-width: 880px`, centered, padding `34px 30px 60px`.

- **Heading** "API access" (`26px` weight 600) + description (`14px`,
  `rgba(22,24,29,.55)`, `max-width: 66ch`): "The companion REST server exposes every
  snapshot, source, and annotation job programmatically. Authenticate with a personal
  token; it inherits your account's private-source grants." Margin-bottom `26px`.

- **Two-column card grid** (`1fr 1fr`, `16px` gap, margin-bottom `24px`). Each card:
  `#fff`, `1px solid rgba(22,24,29,.09)`, `10px` radius, padding `17px`. Card label
  `10px`, `letter-spacing: .08em`, uppercase, `rgba(22,24,29,.45)`, margin-bottom `9px`.
  - **Base URL:** `https://varianthub.compgenlab.org/api/v1` in mono `13px`
    (`word-break: break-all`), plus a note "Server address is set per deployment"
    (`11.5px` `rgba(22,24,29,.45)`, margin-top `6px`). **The server address must be
    configurable per deployment** — read it from config/env, don't hardcode it. The path
    prefix is `/api/v1` on the app's own host, not a separate domain.
  - **Personal token:** masked value `cgl_vh_••••••••••4c1a` in mono `13px` (`flex: 1`),
    plus **Copy** and **Rotate** buttons — `1px solid rgba(22,24,29,.14)`, `6px` radius,
    `#fff`, padding `4px 10px`, `12px` weight 500.
    **Token format: `cgl_vh_` prefix + random secret** (underscore-separated). The prefix
    exists so leaked tokens are greppable by secret scanners and attributable to this
    service. Show the full token exactly once at creation; store only a hash; display
    masked thereafter. **Rotate** issues a new token and invalidates the old one.

- **Endpoint list** (`8px` gap, margin-bottom `24px`). Each row: `#fff`,
  `1px solid rgba(22,24,29,.08)`, `9px` radius, padding `12px 15px`, `12px` gap.
  - **Method badge:** mono `10px` weight 500, padding `3px 8px`, `5px` radius,
    `width: 52px`, centered. GET → `#ecf3f5` background / `#245f70` text;
    POST → `#f9f0ec` background / `#a85a3a` text.
  - **Path:** mono `13px`, `flex: 1`
  - **Description:** `12px` `rgba(22,24,29,.5)`

  | Method | Path | Description |
  |---|---|---|
  | GET | `/snapshots` | List available snapshots |
  | GET | `/sources` | List sources + versions |
  | POST | `/annotate` | Submit variants for annotation |
  | GET | `/jobs/{id}` | Poll job status & fetch results |

- **Code sample card:** `#fff`, `1px solid rgba(22,24,29,.09)`, `10px` radius,
  `overflow: hidden`.
  - **Tab bar:** padding `10px 12px 0`, bottom border `1px solid rgba(22,24,29,.07)`,
    `3px` gap. Tabs "cURL" / "Python": padding `8px 14px`, no background,
    `border-bottom: 2px solid` (`#2f7d92` active, transparent inactive),
    `margin-bottom: -1px`, `13px` weight 500, `#16181d` active /
    `rgba(22,24,29,.5)` inactive.
  - **`<pre>`:** margin `0`, padding `18px`, mono `12.5px`, `line-height: 1.8`,
    `#16181d`, `overflow: auto`, `white-space: pre`.

  cURL sample:
  ```bash
  curl -X POST https://varianthub.compgenlab.org/api/v1/annotate \
    -H "Authorization: Bearer $VH_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{
      "build": "GRCh38",
      "snapshot": "clinical-v4",
      "variants": ["chr17-7676154-C-T", "rs28934578"],
      "format": "json"
    }'
  ```

  Python sample:
  ```python
  import requests

  r = requests.post(
      "https://varianthub.compgenlab.org/api/v1/annotate",
      headers={"Authorization": f"Bearer {token}"},
      json={
          "build": "GRCh38",
          "snapshot": "clinical-v4",
          "variants": ["chr17-7676154-C-T", "rs28934578"],
      },
  )
  job = r.json()["job_id"]
  ```

Auth is `Authorization: Bearer <token>`, and a token inherits its owner's private-source
grants — the API must enforce the same access rules as the UI.

---

### 10. Admin — Sources & snapshots

**Purpose:** Register and configure annotation sources, build snapshots, and grant team
access. **Role-gated to admins.**

**Layout:** `max-width: 1060px`, centered, padding `30px 30px 60px`.

**Header row:** "Administration" (`26px` weight 600) + right-aligned "Signed in as
**A. Researcher** · Org admin" (`12.5px` `rgba(22,24,29,.5)`).

**Tab bar** (margin `16px 0 24px`, `4px` gap, `1px solid rgba(22,24,29,.1)` bottom border):
"Sources" / "Snapshots" / "Access". Each tab: padding `9px 15px`, no background,
`border-bottom: 2px solid` (`#2f7d92` active / transparent), `margin-bottom: -1px`,
`14px`, weight 600 active / 500 inactive, `#16181d` active / `rgba(22,24,29,.5)` inactive.

The two creation flows (Add source, Build snapshot) **replace** the tabbed content in-place
and provide their own back link. In production these are better as their own routes.

#### Tab: Sources

- **Intro row** (`space-between`, margin-bottom `14px`): description (`13.5px`
  `rgba(22,24,29,.55)`, `max-width: 60ch`) — "Any tabix-indexed file — BED, GTF, VCF or
  tab-delimited — plus curated gene lists. Reference sources are pulled from public
  registries; missing indexes are generated on registration." — and a primary
  **"Add source"** button (height `38px`, padding `0 16px`, `#2f7d92`, `8px` radius,
  `13px` weight 500, plus icon).
- **Table card:** `#fff`, `1px solid rgba(22,24,29,.09)`, `10px` radius. Grid columns
  `1.6fr .8fr .7fr .8fr 1fr 34px`, `12px` gap. Headers: *Source / Version / Format /
  Access / Index / (blank)* at `10.5px` uppercase `letter-spacing: .06em`
  `rgba(22,24,29,.45)`, padding `11px 18px`.
- **Rows:** padding `12px 18px`, bottom border `1px solid rgba(22,24,29,.06)`.
  - **Source:** name `14px` weight 500 + a **kind badge**; below it the origin in mono
    `11px` `rgba(22,24,29,.42)` (e.g. "registry: ncbi-clinvar", "licensed · uploaded",
    "lab library").
  - **Kind badge:** mono `9.5px`, padding `1px 6px`, `4px` radius.
    gene list → `#f6f0e0` bg / `#8a6a1e`; VEP → `#f9f0ec` bg / `#a85a3a`;
    everything else (vcf, bed, gtf, tsv) → `#ecf3f5` bg / `#245f70`.
  - **Version:** mono `12px` `#245f70`
  - **Format:** mono `10px` pill, `#f2f4f5`, `5px` radius, `rgba(22,24,29,.55)`
  - **Access:** `12.5px`; Private `#a86a1e`, Public `rgba(22,24,29,.5)`
  - **Index status:** 8px dot + label `12px`. Indexed `#4a7a52`; Building `#b08a2e` (dot
    pulses); error state `#a23c3c`.
  - **Action:** pencil/edit icon button, `rgba(22,24,29,.35)`

  Rows: Ensembl VEP (VEP, release-110, vcf→vcf, Public, Indexed) · ClinVar (vcf, 2026-06) ·
  gnomAD (vcf, v4.1) · GENCODE (gtf, v46) · COSMIC (bed, v99, **Private**) · Cancer gene
  census (gene list, 2026-05) · Germline-cancer genes (gene list, v3, **Private**) ·
  Lab panel (custom) (bed, v2.1, **Private**, **Building**).

**Source model:** a source is any **tabix-indexed file — BED, GTF, VCF, or tab-delimited**.
**Gene lists** (cancer genes, diabetes-related genes, germline-cancer genes, …) are a
first-class source kind. VEP is a special case: it takes a VCF and produces a new annotated,
re-indexed VCF. If an uploaded/fetched file lacks an index, generate it on registration
(that's the `Building` state).

#### Add source flow

Replaces the tab content. Back link "← Back to sources" (`13px` weight 500 `#245f70`,
margin-bottom `14px`), heading "Register a source" (`24px` weight 600), description
(`13.5px` `rgba(22,24,29,.55)`, `max-width: 64ch`): "Pull a reference dataset from a public
registry, or write the source config directly. Everything resolves to a TOML manifest that
pins where the file comes from and how it is indexed."

**Two-column grid:** `340px 1fr`, `20px` gap, `align-items: start`.

- **Left column:**
  - **Registry select:** label "REGISTRY" (`11px` uppercase `letter-spacing: .05em`
    `rgba(22,24,29,.45)`); select height `38px`, `#fff`,
    `1px solid rgba(22,24,29,.14)`, `8px` radius, custom chevron. Options:
    `Ensembl / EBI (public)`, `Broad resource bundle`, `UCSC goldenPath`,
    `Lab gene-list library`. Margin-bottom `14px`.
  - **Available datasets card:** `#fff`, `1px solid rgba(22,24,29,.09)`, `10px` radius.
    Header "AVAILABLE DATASETS" (`10.5px` uppercase, padding `9px 13px`, bottom border).
    Rows (buttons): padding `11px 13px`, bottom border `1px solid rgba(22,24,29,.06)`,
    `space-between`; name `13px` weight 500, meta mono `10.5px` `rgba(22,24,29,.45)`,
    right-side "Use →" (`12px` weight 500 `#245f70`).
    Items: ClinVar (GRCh38) · vcf.gz · 82 MB; GENCODE v46 · gtf.gz · 48 MB;
    dbSNP b156 · vcf.gz · 24 GB; Regulatory build · gff.gz · 9 MB;
    phyloP 100way · bigwig · 6 GB.
    **The registry is public-data only.** Selecting a registry filters the dataset list;
    "Use" populates the TOML editor with that dataset's manifest.
- **Right column — TOML editor:**
  - Label row: "SOURCE.TOML" (`11px` uppercase) + a validity indicator on the right —
    checkmark + "valid" in mono `11px` `#4a7a52`. Should reflect real parse/validation
    state (invalid → `#a23c3c` with the error).
  - Textarea: full width, `min-height: 300px`, padding `15px`, mono `12.5px`,
    `line-height: 1.7`, **background `#1c2733`, color `#d6e3ea`**, no border, `10px`
    radius, `resize: vertical`, `tab-size: 2`, `spellcheck="false"`.
    Consider a real code editor (CodeMirror/Monaco) with TOML syntax highlighting.
  - Default content:
    ```toml
    [source]
    name = "ClinVar"
    build = "GRCh38"
    format = "vcf"          # bed | gtf | vcf | tsv | genelist

    [source.download]
    registry = "ncbi-clinvar"
    url = "https://ftp.ncbi.nlm.nih.gov/pub/clinvar/vcf_GRCh38/clinvar.vcf.gz"
    index = "tabix"         # auto-generate if absent
    checksum = "md5:9f3c1a…"

    [source.access]
    visibility = "public"   # public | private
    # grant = ["oncology-lab", "clinical-genetics"]
    ```
  - **Footer buttons** (right-aligned, `10px` gap, margin-top `14px`): "Cancel"
    (`#fff`, `1px solid rgba(22,24,29,.14)`, `8px` radius, height `40px`, padding `0 18px`)
    and "Validate & register" (primary `#2f7d92`, height `40px`, padding `0 20px`).

**The TOML manifest is the primary configuration mechanism** — it declares where to
download files from and how to index them. Direct uploads are supported but secondary
(files are large). "Validate & register" should parse the TOML, verify reachability and
checksum, then enqueue download + indexing.

#### Tab: Snapshots

- **Intro row:** description "A snapshot bundles specific source versions so an annotation
  run is fully reproducible. Published snapshots are immutable." + primary
  **"New snapshot"** button (same spec as "Add source").
- **List** (`10px` gap): each item `#fff`, `1px solid rgba(22,24,29,.09)`, `10px` radius,
  padding `15px 18px`, `14px` gap.
  - Name `15px` weight 600 + **state pill** (mono `9.5px`, padding `2px 7px`, `5px`
    radius): Published → `#eef4ee` bg / `#3c6642`; Draft → `#f6f0e0` bg / `#8a6a1e`.
  - Meta line mono `11.5px` `rgba(22,24,29,.45)`: `"{build} · {n} sources · pinned {date}"`
  - Right: "Duplicate" button — height `32px`, padding `0 13px`, `#fff`,
    `1px solid rgba(22,24,29,.12)`, `7px` radius, `12.5px` weight 500.

  Items: GRCh38 Clinical v4 (Published, 6 sources, 2026-06-12) · GRCh38 Research v2
  (Published, 9, 2026-05-30) · Somatic Oncology v3 (Published, 8, 2026-05-18) ·
  GRCh38 Clinical v5 (Draft, 6, 2026-07-20).

#### Build snapshot flow

Back link "← Back to snapshots", heading "Build a snapshot" (`24px` weight 600,
margin-bottom `18px`).

- **Field row** (`14px` gap, wrapping, margin-bottom `20px`): "Snapshot name" text input
  (`flex: 1`, `min-width: 260px`, height `40px`, `#fff`, `1px solid rgba(22,24,29,.14)`,
  `8px` radius; default "GRCh38 Clinical v5 (draft)") and a "Build" select
  (`min-width: 190px`, mono `13px`; GRCh38 / GRCh37).
- **Source pinning table:** `#fff`, `1px solid rgba(22,24,29,.09)`, `10px` radius. Grid
  columns `24px 1.6fr 1fr`, `12px` gap. Headers: *(blank) / Source / Pinned version*.
  Rows padding `12px 18px`, bottom border `1px solid rgba(22,24,29,.06)`; selected row
  background `#f3f8f9`.
  - **Checkbox:** 18×18px, `5px` radius, `1.5px` border; checked `#2f7d92` + white check.
  - **Source:** name `14px` weight 500 + kind badge (same badge spec as the Sources tab).
  - **Version select:** `max-width: 180px`, height `34px`, mono `12px`,
    `1px solid rgba(22,24,29,.14)`, `7px` radius, custom chevron. When the row is
    unchecked the select is dimmed: background `#f7f8f8`, color `rgba(22,24,29,.4)`.

  Rows and versions: Ensembl VEP (release-110, release-109) · ClinVar (2026-06, 2026-03,
  2025-12) · gnomAD (v4.1, v4.0, v2.1.1) · SpliceAI (v1.3) · dbSNP (b156, b155) ·
  Cancer gene census (2026-05, 2025-11). Checked by default: VEP, ClinVar, gnomAD,
  SpliceAI, Cancer gene census.
- **Footer** (margin-top `18px`, `space-between`): "**{n}** sources pinned" on the left
  (count `15px` weight 600); "Save draft" (secondary) and "Publish snapshot" (primary) on
  the right, both height `40px`.

#### Tab: Access

- **Description** (`13.5px` `rgba(22,24,29,.55)`, `max-width: 64ch`, margin-bottom `16px`):
  "Public sources are visible to everyone. Private sources (licensed data such as COSMIC,
  and in-house gene lists) are granted per team. Toggle a cell to grant a team access."
- **Matrix card:** `#fff`, `1px solid rgba(22,24,29,.09)`, `10px` radius,
  `overflow: auto`. Grid columns `1.6fr repeat(4, 1fr)`, `12px` gap, `min-width: 640px`.
  - **Header:** padding `11px 18px`, bottom border. First cell "PRIVATE SOURCE" (`10.5px`
    uppercase `letter-spacing: .06em` `rgba(22,24,29,.45)`); then one centered team name
    per column (`11.5px` weight 500 `rgba(22,24,29,.6)`).
  - **Rows:** padding `13px 18px`, bottom border `1px solid rgba(22,24,29,.06)`. First cell:
    source name `14px` weight 500 + kind badge, with a mono `11px` `rgba(22,24,29,.42)`
    meta line. Then one toggle cell per team, centered.
  - **Toggle cell:** 22×22px button, `6px` radius, `1.5px` border. Granted: `#2f7d92`
    background + white check. Not granted: `#fff` background,
    `rgba(22,24,29,.18)` border.

  Teams: **Oncology Lab**, **Clinical Genetics**, **Bioinformatics Core**, **External**.

  | Private source | Meta | Oncology Lab | Clinical Genetics | Bioinformatics Core | External |
  |---|---|---|---|---|---|
  | COSMIC (bed) | licensed · v99 | ✓ | ✓ | — | — |
  | Germline-cancer genes (gene list) | lab · v3 | ✓ | ✓ | — | — |
  | Lab panel (custom) (bed) | uploaded · v2.1 | ✓ | — | ✓ | — |

**Access model is org/team-based.** Public sources need no grants. Private sources are
granted to teams, and a user's team memberships determine what they see in the source
picker, the genome browser track list, and via the API. Enforce this server-side on every
endpoint, not just in the UI.

---

## Interactions & Behavior

### Navigation

- Sidebar items switch top-level views. "Results" lands on the **jobs list**, never
  directly on a results table.
- The step header appears only during the annotation flow (Sources / Variants / Running).
- "Continue to variants" → Step 2. "Back to sources" → Step 1. "Run annotation" → Running.
- Running completes → that job's results detail.
- Jobs list → click a **Complete** row → results detail. "← All jobs" returns.
- A results row's crosshair icon → genome browser at that variant's locus.
- Admin tabs switch in-place; "Add source" and "New snapshot" replace the tab content and
  return via their back links.

### Animations

- **Spinner** (running card): `rotate(360deg)`, `1s`, `linear`, infinite.
- **Blink** (active stage glyph, Running-status dots): opacity `1 → .3 → 1`, `1s`, infinite.
- **Progress bar:** `transition: width .15s linear`.
- No other transitions are specified; keep motion minimal and functional.

### States to implement (not in the prototype)

- **Empty:** no jobs yet; no sources registered; no search results.
- **Loading:** skeletons for the jobs list and results table.
- **Error:** failed job (with failing stage + logs); source index failure; invalid TOML;
  malformed variant lines; unreachable registry.
- **Validation:** per-variant parse errors in single/batch input; unsupported build for a
  selected source; VCF without a usable index.
- **Permission:** private source visible but not granted; non-admin hitting an admin route.
- **Responsive:** the design targets desktop. Below ~1100px the results table scrolls
  horizontally (`min-width: 1080px`). Decide whether to support tablet/mobile.

---

## State Management

Prototype state (all local component state; production should split across router,
server-cache, and form state):

| Key | Type | Purpose |
|---|---|---|
| `authed` | boolean | Session presence → gates the whole app |
| `view` | enum | `sources` · `variants` · `running` · `jobs` · `results` · `browse` · `api` · `admin` → becomes routes |
| `sourceMode` | `snapshot` \| `individual` | Step 1 segmented control |
| `snapshot` | id | Selected snapshot (single-select) |
| `sel` | map<sourceId, bool> | Checked individual sources |
| `build` | string | Active reference build (shown in top bar) |
| `inputMode` | `single` \| `batch` \| `vcf` | Step 2 segmented control |
| `draft` | string | Single-variant input buffer |
| `variants` | array<{id, label}> | Chips in single mode |
| `batchCount` | number | Non-blank line count in batch mode |
| `progress` / `stage` | number / enum | Running job progress (replace with polled job state) |
| `downloadOpen` | boolean | Export menu visibility |
| `apiTab` | `curl` \| `python` | Code sample tab |
| `adminTab` | `sources` \| `snapshots` \| `access` | Admin tab |
| `adminMode` | `null` \| `addSource` \| `buildSnapshot` | Admin sub-flow |
| `registry` | id | Selected public registry |
| `toml` | string | TOML manifest editor content |
| `snapName` / `snapSel` | string / map | Snapshot builder name + pinned sources |
| `access` | map<sourceId, map<teamId, bool>> | Access matrix grants |

### Data the frontend needs from the backend

- `GET /snapshots` → id, name, build, source count, description, tags, contains-private flag
- `GET /sources` → id, name, detail, versions[], format, kind, visibility, index status
- `POST /annotate` → `{build, snapshot | sources[], variants[] | file}` → `{job_id}`
- `GET /jobs` → paginated list with status, percent, input summary, counts
- `GET /jobs/{id}` → status, stage, percent, error
- `GET /jobs/{id}/results?page&sort&filter` → paginated annotated rows + dynamic column
  definitions (which source produced each column)
- `GET /jobs/{id}/export?format=json|tsv|csv` → streamed full export honoring filters
- Admin: source CRUD (TOML validate/register), registry dataset listing, snapshot
  create/publish/duplicate, team grant read/write
- Tracks for igv.js: per-source URL + index URL, filtered by the caller's grants

---

## Design Tokens

### Colors

| Token | Value | Use |
|---|---|---|
| Page background | `#f7f8f8` | App canvas |
| Surface | `#fff` | Cards, tables, sidebar, bars |
| Text primary | `#16181d` | Headings, body |
| Text secondary | `rgba(22,24,29,.55)` | Descriptions |
| Text tertiary | `rgba(22,24,29,.45)` | Labels, meta |
| Text quaternary | `rgba(22,24,29,.4)` – `rgba(22,24,29,.3)` | Disabled, empty values |
| Hairline | `rgba(22,24,29,.06)` | Table row dividers |
| Border subtle | `rgba(22,24,29,.08)` – `rgba(22,24,29,.09)` | Card borders, bar borders |
| Border input | `rgba(22,24,29,.12)` – `rgba(22,24,29,.14)` | Inputs, secondary buttons |
| **Accent** | `#2f7d92` | Primary buttons, active states, fills, mono highlights |
| Accent text | `#245f70` | Links, active nav, mono values |
| Accent tint | `#ecf3f5` | Avatar bg, GET badge, active step dot |
| Accent tint alt | `#f3f8f9` | Selected row/card background |
| Accent dashed bg | `#f0f6f7` | Dropzone |
| Neutral fill | `#f2f4f5` | Mono pills, format badges |
| Segmented track | `#eceeef` | Segmented control background, ideogram bar |
| Code surface | `#1c2733` / text `#d6e3ea` | TOML editor |
| **Pathogenic** | bg `#f6eaea`, text `#8f2f2f`, dot `#a23c3c` | Significance |
| **Likely pathogenic** | bg `#f9f0ec`, text `#a85a3a` | Significance, POST badge |
| **VUS** | bg `#f6f0e0`, text `#8a6a1e`, dot `#b08a2e` | Significance, Draft pill, gene-list badge |
| **Benign** | bg `#eef4ee`, text `#3c6642`, dot `#4a7a52` | Significance, Published pill, READY |
| **Likely benign** | bg `#f1f5f1`, text `#5a7a5f` | Significance |
| Private/licensed | `#a86a1e` | Private access label |

### Typography

- **Sans:** `"IBM Plex Sans", system-ui, sans-serif` — UI, headings, body
- **Mono:** `"IBM Plex Mono", monospace` — all genomic data (coordinates, rsIDs, builds,
  versions), file names, code, IDs, numeric measures
- Weights: 400 / 500 / 600 only
- Body `14.5px` / `line-height: 1.55`; headings `letter-spacing: -.01em`
- Google Fonts: `IBM+Plex+Sans:wght@400;500;600` + `IBM+Plex+Mono:wght@400;500`

| Role | Size | Weight |
|---|---|---|
| Page heading | 26px | 600 |
| Section heading | 24px | 600 |
| Card/dialog heading | 20–22px | 600 |
| Snapshot card title | 17px | 600 |
| Body | 14–14.5px | 400 |
| Nav item | 14px | 500 (600 active) |
| Table cell | 13.5px | 400 |
| Secondary/meta | 12.5–13px | 400 |
| Mono data | 12–12.5px | 400–500 |
| Small meta | 11–11.5px | 400 |
| Uppercase label | 10–11px, `letter-spacing: .05–.09em` | 400–500 |
| Micro badge (mono) | 9.5–10px | 500 |

**Rule:** everything genomic or machine-readable is mono; everything else is sans. This
carries the "scientific instrument" character of the design.

### Spacing

`2 · 3 · 5 · 6 · 8 · 10 · 12 · 14 · 16 · 18 · 20 · 22 · 24 · 26 · 30 · 34 px`.
Content gutters `30px` (`26px` in the browser view). Card padding `17px`; dialog padding
`30–34px`. Grid gaps `16px` (cards), `10–12px` (rows/toolbars).

### Radii

`4px` micro badge · `5px` small pill · `6px` toggle/avatar/logo · `7px` nav item, small
button, chip · `8px` input, button · `9px` segmented container, menu, endpoint row ·
`10px` card, code editor · `11px` snapshot card · `12px` dialog · `20px` significance pill ·
`50%` avatar, variant dot

### Shadows

- Card rest: `0 1px 2px rgba(22,24,29,.03)`
- Segmented active pill: `0 1px 2px rgba(22,24,29,.1)`
- Selected snapshot card: `0 2px 12px rgba(47,125,146,.1)`
- Running card: `0 4px 20px rgba(22,24,29,.05)`
- Dropdown menu: `0 10px 30px rgba(22,24,29,.12)`
- Login card: `0 8px 30px rgba(22,24,29,.06)`
- Sticky table header: `0 1px 0 rgba(22,24,29,.08)`
- Variant dot: `0 1px 2px rgba(22,24,29,.15)`

### Layout constants

Sidebar `238px` · browser track label gutter `160px` · content max-widths: `1060px`
(jobs, admin), `1020px` (sources), `880px` (variants, API), `600px` (running card),
`392px` (login) · results table `min-width: 1080px`

---

## Assets

- **Fonts:** IBM Plex Sans + IBM Plex Mono (Google Fonts). Self-host if your codebase does.
- **Icons:** all inline SVG, 24×24 viewBox, stroke-based, `stroke-width` 1.6–1.8,
  `stroke-linecap`/`linejoin: round`, `currentColor`. They match the **Lucide** icon set —
  use Lucide (or your existing icon library) rather than copying the paths.
  Icons used: file-plus, table/grid, crosshair, terminal, shield-check, search, filter,
  sliders, download, upload, plus, minus, chevron-left/right/down, arrow-left/right, x,
  check, lock, pencil, play, log-out, loader arc, file-text.
- **No raster images.** The genome browser visuals are CSS/SVG placeholders — replace with
  igv.js.

## Third-party libraries

- **igv.js** — required for the genome browser. Feed it the tabix-indexed sources
  (bgzipped VCF/BED/GTF + `.tbi`), themed to the tokens above.
- **A virtualized table** (TanStack Virtual, react-window, or your existing data-grid) —
  required for thousands of result rows.
- **A code editor** (CodeMirror or Monaco) — recommended for the TOML manifest editor.

## Files

- `VariantHub.dc.html` — the complete interactive prototype (all views). Open it in a
  browser and click through: Results → a completed job; the crosshair icon on a results
  row; the Sources/Snapshots/Access admin tabs and both creation flows.

## Open questions for the team

1. **Failed-job detail:** what does the error view show (stage, stderr, per-variant
   failures)?
2. **Filter panel:** which fields are filterable, and are saved filter presets needed?
3. **Column manager:** are column choices persisted per user, per job, or per snapshot?
4. **Non-human builds:** which references beyond GRCm39, and does the source library differ
   per organism?
5. **Large VCF upload:** size ceiling, and is resumable upload required?
6. **Sharing:** can a job or snapshot be shared with a teammate or made public?
7. **Admin gating:** which roles exist beyond "Org admin", and is source registration
   org-scoped or global?
8. **HGVS resolution:** which transcript set is authoritative when HGVS input is ambiguous?

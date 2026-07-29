# VariantHub web front-end

React + TypeScript + Vite. Built to
[`../design_handoff_varianthub/`](../design_handoff_varianthub/); design tokens
live in [`src/index.css`](src/index.css) and are the handoff's values verbatim.

## Develop

```sh
make ui-dev          # Vite on :5173, proxying /api to the API on :18080
```

The dev server proxies `/api`, `/healthz` and `/version`, so the browser sees one
origin and CORS never enters the picture. Point elsewhere with `VITE_DEV_API`.

## Build

```sh
make ui              # builds into web/embed/dist
make all-build       # UI + Go binary, so the binary serves the app
```

The output is embedded into the Go binary via `web/embed`, so a single
`varianthub-web serve` serves both the API and the app. A binary built without
running the UI build still serves the API — the SPA route is simply absent, which
is what CI and API-only deployments want.

## What is here

| Screen | Route |
|---|---|
| Token entry | *(shown until a token is stored)* |
| Choose annotations | `/annotate/sources` |
| Enter variants | `/annotate/variants` |
| Running | `/annotate/running/:jobId` |
| Jobs | `/jobs` |
| Results | `/jobs/:jobId` |

## What is deliberately missing

These have no backend yet, so building them would mean mocking:

- **Login.** The API takes one shared bearer token; `TokenGate` asks for it and
  verifies it against a real endpoint rather than faking a sign-in form.
- **Admin** (sources, snapshots, access matrix) — no endpoints.
- **Genome browser** — needs `/tracks` and signed source URLs.
- **Job stages and percent.** The API reports status only, so Running shows an
  indeterminate state rather than a progress bar advancing on a timer.
- **Individual-source selection.** The engine selects sources through a snapshot;
  `POST /annotate` rejects an ad-hoc `sources` list.

## Notes

**`npm audit` reports a high finding in `react-router`**
([GHSA-qwww-vcr4-c8h2](https://github.com/advisories/GHSA-qwww-vcr4-c8h2)). It is
a CSRF bypass in **RSC mode**, and this is a client-only SPA with no server
components or actions, so it does not apply. The fix lands in react-router 8.3.0,
which is not published yet — there is no version to upgrade to. Re-check when 8.x
ships rather than re-litigating the finding.

**Exports are fetched, not navigated to.** A plain navigation cannot carry an
`Authorization` header, which would force the token into the query string — where
it lands in browser history and every intermediary's access log. The download is
fetched with the header and saved as a blob instead; the cost is buffering the
body in the tab, which at the scale here is a few MB.

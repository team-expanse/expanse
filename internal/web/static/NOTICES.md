# Vendored static assets

Embedded via `go:embed` (D6: no CDN dependency, so the UI renders on an
offline install). Both are Zero-Clause BSD (no attribution required);
noted here for upgrade tracking only.

| File | Source | Version |
|---|---|---|
| `htmx.min.js` | https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js | 2.0.4 |
| `htmx-sse.min.js` | https://unpkg.com/htmx-ext-sse@2.2.2/sse.js | 2.2.2 |

`style.css`, `app.js`, `theme.js`, `favicon.svg` and the inline SVG icon
sprite (`templates/_icons.html`) are written for this project (no third-party
code or icon font) and carry the project's own licence.

# Vendored static assets

Embedded via `go:embed` (D6: no CDN dependency, so the UI renders on an
offline install). Both are Zero-Clause BSD (no attribution required);
noted here for upgrade tracking only.

| File | Source | Version |
|---|---|---|
| `htmx.min.js` | https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js | 2.0.4 |
| `htmx-sse.min.js` | https://unpkg.com/htmx-ext-sse@2.2.2/sse.js | 2.2.2 |

The terminal on a VM's console page is xterm.js, MIT-licensed; its license
texts are kept beside it (`xterm/LICENSE`, `xterm/LICENSE-addon-fit`). Taken
from the npm tarballs, checked against the registry's sha512 integrity, with
the trailing `sourceMappingURL` line removed:

| File | Source | Version |
|---|---|---|
| `xterm/xterm.js`, `xterm/xterm.css` | https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz (`lib/xterm.js`, `css/xterm.css`) | 6.0.0 |
| `xterm/addon-fit.js` | https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-0.11.0.tgz (`lib/addon-fit.js`) | 0.11.0 |

`style.css`, `app.js`, `theme.js`, `favicon.svg` and the inline SVG icon
sprite (`templates/_icons.html`) are written for this project (no third-party
code or icon font) and carry the project's own license.

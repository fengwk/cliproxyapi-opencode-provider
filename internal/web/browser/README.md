# Browser checks

Manual browser validation for the plugin UI (`ui.js` / `ui.css`): the theme
bridge and the manual-model editor. Playwright is intentionally **not** a
repository dependency.

```bash
# from a scratch directory outside the repo
npm i playwright
NODE_PATH="$(npm root)" SCREENSHOT_DIR=/tmp/opencode-theme-shots \
  node internal/web/browser/theme-check.cjs
```

The script starts a throwaway loopback static server (no CPA, no production
server, no network egress), serves `internal/web` under the production CSP
literal read from `internal/provider/management.go` (failing if it cannot find
exactly one policy), and drives a headless browser to validate:

- embedded warm-grey light / white / dark themes match the host tokens;
- a live same-origin host `data-theme` mutation is mirrored into the iframe;
- the whitelisted host tokens are copied into the iframe;
- the 390px viewport renders without page-level horizontal overflow;
- standalone pages follow the system colour scheme;
- with JavaScript disabled or `ui.js` aborted, both native forms are blocked by
  `form-action 'none'`: Chromium's CSP console diagnostic is observed, the URL
  stays unchanged, and neither browser requests nor server query/body contain
  the fake management / OpenCode keys (no real credentials are used). Each of
  the four submissions also checks that FormData excludes both sensitive
  fields and the CSP diagnostic contains neither fake key;
- normal JavaScript GET / POST fetches still succeed against local route mocks
  under the production policy, including successful import input cleanup.

The Go resource-route test separately asserts the actual Management response
headers for all three public assets, including `base-uri 'none'` and
`form-action 'none'`. Browser checks synchronize on explicit CSP diagnostics and
UI completion, not fixed sleeps.

The sensitive `#mgmt-key` and `#keys` controls intentionally have no `name`
attribute: JavaScript reads them by ID, while native form serialization omits
them. This prevents even blocked form submissions from constructing key-bearing
URLs in CSP console diagnostics. The non-sensitive label remains named.
A dependency-free Node static regression guards both sensitive controls against
reintroducing a `name` attribute.

`fixture.html` / `fixture.css` reproduce the CPA management center theme token
contract as a same-origin parent. Playwright resolves a local Chrome first
(`channel: "chrome"`, or `CHROME_PATH`) and falls back to the bundled browser.
Screenshots and `browser-results.json` (including the policy and CSP blocking
evidence) are written to `$SCREENSHOT_DIR` and never into the repository.

## Manual-model editor (`models.cjs`)

`models.cjs` reuses the same throwaway loopback server, production CSP literal
and Playwright dependency to drive section 5 of the plugin page against local
route mocks (`/keys`, `/settings`, `/validate`, `/config`) with a fake
management key — no CPA, no upstream, no network egress, no real credentials:

```bash
NODE_PATH="$(npm root)" CHROMIUM_PATH=/path/to/chromium \
  node internal/web/browser/models.cjs /tmp/opencode-model-shots
```

It validates:

- adding a native or `opencode-go/`-prefixed id and rejecting an unknown family
  without an explicit protocol;
- a failed `POST /validate` leaves the draft intact and issues no `PATCH
  /config` write;
- a successful save sends a shallow `manual-models`-only patch, tolerates a
  bounded `503` during read-back, and preserves unrelated config (`enabled`,
  `base-url`, `models`);
- deleting all rows and saving persists an empty manual catalog and disables the
  save button;
- an empty catalog renders one spanning placeholder row;
- every live button (connect / import / save primary, reload / add / discard
  secondary, quota secondary-small, credential and draft-row delete
  danger-small) carries `.btn` with one semantic variant and the shared 46px /
  39px dimensions, hover, disabled and keyboard-focus treatment;
- the `step-5-preview-free` / `openai` row centers its text on the shared small
  delete control, a long id genuinely wraps (multiple line boxes), and the add
  button shares the input/select bottom edge;
- the manual editor renders light / white / dark close-ups at 1280px with
  transitions settled (`animations: 'disabled'`);
- the 390px viewport keeps the wide draft table scrolling inside its wrapper
  (table-internal scroll) without page-level horizontal overflow, and no browser
  exception is raised.

`CHROMIUM_PATH` selects the browser (default `/usr/bin/chromium`).
`models-results.json`, `models-geometry.json` and screenshots are written to the
argument directory and never into the repository.

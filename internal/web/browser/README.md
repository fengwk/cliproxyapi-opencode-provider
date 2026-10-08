# Browser theme checks

Manual browser validation for the plugin UI theme bridge (`ui.js` /
`ui.css`). Playwright is intentionally **not** a repository dependency.

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

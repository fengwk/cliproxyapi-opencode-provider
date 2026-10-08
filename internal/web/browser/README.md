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
server, no network egress), serves `internal/web` under a representative CSP
(`default-src 'self'; frame-ancestors 'self'; ...`) and drives a headless
browser to validate:

- embedded warm-grey light / white / dark themes match the host tokens;
- a live same-origin host `data-theme` mutation is mirrored into the iframe;
- the whitelisted host tokens are copied into the iframe;
- the 390px viewport renders without page-level horizontal overflow;
- standalone pages follow the system colour scheme.

`fixture.html` / `fixture.css` reproduce the CPA management center theme token
contract as a same-origin parent. Playwright resolves a local Chrome first
(`channel: "chrome"`, or `CHROME_PATH`) and falls back to the bundled browser.
Screenshots are written to `$SCREENSHOT_DIR` and never into the repository.

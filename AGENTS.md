# Development rules

- Keep this a thin OpenCode Go native plugin. Never modify or fork CPA.
- CPA owns credential pooling, selection, cooldown, retry, session identification and affinity.
- Use the public CPA v8 translator SDK and builtins; do not copy protocol translators.
- Session scoping is a pure function of CPA's canonical session identity and the selected auth ID.
- Do not generate a pre-auth content hash, keep a session map, or implement a scheduler.
- Use host callbacks for HTTP and credential persistence; never log or return key material.
- Keep the key-input UI dependency-free and do not persist secrets in browser storage.
- Run Go tests, race checks, vet, native builds and the real-host mock integration tests.
- Use fake credentials only. Do not modify production CPA config, auths or binaries.
- Comments should be concise and in English.

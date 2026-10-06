# cliproxyapi-opencode-provider

A thin OpenCode Go native provider plugin for CLIProxyAPI.

CPA owns credential pooling, failover, cooldown, session identification and affinity.
The plugin owns only provider registration, OpenCode transport, deterministic auth-scoped
upstream sessions, and a small key-import UI. Protocol conversion uses CPA's public SDK.

## Development

Requires Go 1.26 and a C compiler. The plugin does not modify CPA.

```sh
go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/cliproxyapi-opencode-provider.so .
```

Native C ABI compatibility permits independent host upgrades. Updating the translator SDK
requires rebuilding the plugin; automated dependency updates and real-host compatibility
tests keep that work bounded.

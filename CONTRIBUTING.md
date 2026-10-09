# Contributing

Thanks for helping. Bug reports from Windows, Linux ARM64 and Intel Macs are especially welcome, since those platforms are not yet tested end to end.

## Reporting a problem

Use the issue form. It asks for your OS and architecture, the output of `whatsapp-mcp version`, whether you use Pi, the steps to reproduce, and what you expected and saw. Add the relevant lines of `whatsapp-mcp logs`, and remove anything personal first. Never post your session database, API token, QR codes, phone numbers or message contents.

Security problems go through private reporting; see [SECURITY.md](SECURITY.md).

## Development

You need Go 1.27 or later. No C compiler is needed.

```sh
cd whatsapp-bridge
gofmt -l .                 # must print nothing
go vet ./...
go test -race ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...   # when dependencies change
```

Automated tests never connect to WhatsApp: they use fake senders, temporary databases and loopback servers. Keep it that way. Never pair a real account or message real contacts from a test.

Ground rules:

- Every outgoing message must go through the approval check. Do not add bypasses, `approved` flags or an API route that pairs an approver.
- Never log message contents, phone numbers, tokens or keys.
- Keep the bridge pure Go and free of OS-specific UI code.
- When a dependency changes, update [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
- Use one-line conventional commit messages, for example `fix: reject empty recipients`.

`AGENTS.md` holds the same rules for AI coding agents.

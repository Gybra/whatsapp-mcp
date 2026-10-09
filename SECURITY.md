# Security policy

## Reporting a vulnerability

Do not open a public issue. Use GitHub's private reporting: **Security › Report a vulnerability** on this repository. Include the version (`whatsapp-mcp version`), your operating system and the steps to reproduce. Never include your WhatsApp session, API token, phone numbers or message contents.

Only the latest release is supported.

## What the bridge protects

- **Every outgoing message needs an approval** signed by the paired approver (the pi-whatsapp extension) for that exact recipient, text and file. Approvals are single use, expire after 120 seconds and are checked by the bridge itself, whichever program calls it. See [docs/approval-protocol-v1.md](docs/approval-protocol-v1.md).
- **The local API** listens only on `127.0.0.1:8080`, requires a random 32-byte token on every request and rejects requests that come from web browsers or name another host.
- **Files:** sending reads only regular files placed in the `outbox` folder. Downloads get generated names. Obvious credential files and PEM private keys are refused.
- **Data** is stored in the user's data directory with owner-only permissions. Logs contain no message contents, phone numbers, tokens or keys.

## What it does not protect against

- **Programs running as your user.** They can read the data directory, change the bridge or Pi, or write the approver file directly. Separate them with operating-system isolation if that matters to you.
- **Prompt injection.** Messages are untrusted text that the AI model reads. They can steer the model; approval makes sure nothing is sent without you, but you must read what you approve.
- **Data at rest.** Files are protected by permissions, not encrypted.
- **Account restrictions.** This is an unofficial client; WhatsApp may restrict accounts that use it.

The full analysis is in [docs/threat-model.md](docs/threat-model.md).

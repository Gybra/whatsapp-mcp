# whatsapp-mcp

[![M8ven Score](https://m8ven.ai/badge/mcp/gybra-whatsapp-mcp-1u7q6p?v=b84cb7252861a5dfd03a93adf7f1ebd3)](https://m8ven.ai/mcp/gybra-whatsapp-mcp-1u7q6p?s=readme)

Read and search your WhatsApp conversations from an AI coding agent, and send messages only after you approve each one.

whatsapp-mcp is a small program, the *bridge*, that links to your WhatsApp account as a linked device, like WhatsApp Web. It keeps your message history on your computer and serves it to AI agents through the [Model Context Protocol](https://modelcontextprotocol.io) (MCP). It is built for [Pi](https://pi.dev) together with the [pi-whatsapp](https://github.com/Gybra/pi-whatsapp) extension, which lets you review every outgoing message inside Pi.

> **Unofficial.** This project is not affiliated with, endorsed by or supported by WhatsApp or Meta. It uses [whatsmeow](https://github.com/tulir/whatsmeow), an unofficial implementation of the WhatsApp multi-device protocol. WhatsApp may restrict or ban accounts that use unofficial clients. Use it at your own risk.

## Platform support

| Platform | Status |
|---|---|
| macOS (Apple Silicon) | **Tested** with a real account: QR linking, reading chats, approved text messages, downloading voice messages, autostart. |
| macOS (Intel) | Built in CI, not tested yet. |
| Linux (x86-64) | **Tested** on Omarchy (Arch Linux) with a real account: setup from Pi, QR linking, reading chats, approved messages, voice messages, autostart across reboots. |
| Linux (ARM64) | Built and tested in CI, not yet tested end to end. |
| Windows (x86-64) | **Experimental.** Developed for it, but not yet tested end to end. |

If you try it on Windows, Linux ARM64 or an Intel Mac, please [open an issue](https://github.com/Gybra/whatsapp-mcp/issues/new/choose) with what worked and what did not.

## What it does

- **Read:** search contacts, list chats and messages with filters, get the messages around a message, and download received files, including voice messages, to a private folder.
- **Send:** text messages, files and voice messages, each only after a human approves that exact message.
- **Run in the background:** optionally start at login, reconnect after network outages, and stop cleanly.

No agent can send on its own: the bridge refuses any message without a valid approval, whichever program asks.

## How it differs from the original

This is a fork of [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp), reworked for supervised use:

- **Approval for every outgoing message.** The bridge sends only a message that an approver, the pi-whatsapp extension, signed after showing it to you. The approval covers the exact recipient, text and file bytes. It is single use and expires after 120 seconds. See [docs/approval-protocol-v1.md](docs/approval-protocol-v1.md).
- **Locked-down local API.** The original API listened on every network interface with no authentication. This one listens only on `127.0.0.1`, requires a random token for every request, and rejects requests from web browsers.
- **Confined files.** Files can only be sent from a dedicated `outbox` folder, never from an arbitrary path. Downloaded files get generated names, so a sender cannot write outside the download folder. Obvious credential files and private keys are refused.
- **Private data and quiet logs.** Data lives in your user data directory with owner-only permissions. Logs contain no message contents, phone numbers or keys.
- **One Go program.** The Python MCP server is gone. The bridge and the MCP server are the same executable, with no C compiler or Python needed.
- **Setup and lifecycle commands,** and correct handling of WhatsApp's newer contact identifiers (LIDs).

## Install

**With Pi (recommended):** install the extension and run `/whatsapp-setup` in Pi. It downloads the bridge, checks its checksum, shows the QR code to link your phone and asks whether to start the bridge at login. See [pi-whatsapp](https://github.com/Gybra/pi-whatsapp).

**Manually:** download the file for your system from [Releases](https://github.com/Gybra/whatsapp-mcp/releases), for example `whatsapp-mcp-darwin-arm64`, and check it against `SHA256SUMS`:

```sh
shasum -a 256 -c SHA256SUMS --ignore-missing     # macOS
sha256sum -c SHA256SUMS --ignore-missing         # Linux
gh attestation verify whatsapp-mcp-darwin-arm64 --repo Gybra/whatsapp-mcp   # optional: check it was built by this repository's CI
```

Then make it executable (`chmod +x`), put it somewhere permanent, such as `~/.local/bin/whatsapp-mcp`, and run `whatsapp-mcp setup`. The binaries are not signed by Apple: on macOS, a file downloaded with a browser is blocked by Gatekeeper until you run `xattr -d com.apple.quarantine ~/.local/bin/whatsapp-mcp`. The Pi extension's download is not affected.

**From source:** with Go 1.27 or later:

```sh
cd whatsapp-bridge
go build -o whatsapp-mcp .
```

## Link WhatsApp

`whatsapp-mcp setup` (or `/whatsapp-setup` in Pi) shows a QR code. On your phone, open WhatsApp › Settings › Linked devices › Link a device and scan it. The link stays valid until you remove it on your phone. WhatsApp may also unlink a device that stays offline for a long time; the bridge then reports that you need to link again and never deletes data on its own.

Some accounts ask you to confirm a code during linking: compare it with your phone and confirm. Accounts that require a passkey to link devices are not supported yet.

## Commands

```
whatsapp-mcp setup               Link WhatsApp and choose autostart (interactive)
whatsapp-mcp status [--json]     Is the bridge running, is WhatsApp linked
whatsapp-mcp start | stop | restart
whatsapp-mcp logs [-n LINES]
whatsapp-mcp autostart enable|disable
whatsapp-mcp uninstall [--purge]
whatsapp-mcp mcp                 MCP server over stdio, for MCP clients
```

## Autostart

Autostart is optional and off unless you turn it on. It uses your system's per-user service manager and never needs administrator rights:

- **macOS:** a LaunchAgent in `~/Library/LaunchAgents/io.github.gybra.whatsapp-mcp.plist`.
- **Linux:** a systemd user unit, `~/.config/systemd/user/whatsapp-mcp.service`. It runs while you are logged in. To keep it running after you log out, enable lingering yourself with `loginctl enable-linger`. Without systemd, run `whatsapp-mcp start` after logging in.
- **Windows:** a Task Scheduler task named `whatsapp-mcp` that starts at logon.

After a crash the service manager restarts the bridge, with a delay to avoid restart loops. `whatsapp-mcp stop` stops it until the next login or `start`. `whatsapp-mcp autostart disable` removes the service.

## Use with MCP clients

The MCP server runs on demand: the client starts `whatsapp-mcp mcp` and talks to it over stdio, while the bridge keeps running in the background.

**Pi:** handled by pi-whatsapp. **Other clients:** for example in `mcp.json`:

```json
{ "mcpServers": { "whatsapp": { "command": "/absolute/path/to/whatsapp-mcp", "args": ["mcp"] } } }
```

Without the pi-whatsapp extension, other clients can read and search, but every send is refused because nothing can approve it.

To send a file, copy it into the `outbox` folder inside the data directory first, then ask the agent to send it by file name:

| OS | Data directory |
|---|---|
| macOS | `~/Library/Application Support/whatsapp-mcp` |
| Linux | `$XDG_DATA_HOME/whatsapp-mcp`, by default `~/.local/share/whatsapp-mcp` |
| Windows | `%LOCALAPPDATA%\whatsapp-mcp` |

## Your data

The data directory holds the WhatsApp session (`whatsapp.db`, the keys of the linked device), the message history (`messages.db`), the local API token, downloaded files and a log without message contents. Treat it like a password. Nothing is sent anywhere except to WhatsApp. Messages that an AI agent reads, however, become part of the agent's conversation with its model provider.

History and downloads are kept until you delete them. To back up, stop the bridge and copy the whole directory. Keep that copy as private as the original. Files are protected by permissions only, not encrypted. `whatsapp-mcp uninstall --purge` deletes the directory; also remove the device under Linked devices on your phone.

If you used the original whatsapp-mcp, `whatsapp-mcp import /path/to/whatsapp-bridge/store` copies its session and history; the originals are left untouched.

## Security

Read [SECURITY.md](SECURITY.md) and the [threat model](docs/threat-model.md). In short:

- The approval check protects against the AI model and against other programs that use the local API. It cannot protect against a program running as your user that changes the bridge, its data or Pi itself.
- WhatsApp messages are untrusted input. A message can contain instructions aimed at the AI (prompt injection). Approval ensures nothing is sent on the AI's word alone, but you still need to read what you approve.

Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md).

## Troubleshooting

- **`whatsapp-mcp status` says not running:** run `whatsapp-mcp start`, then `whatsapp-mcp logs`.
- **Port 8080 already in use:** another program, or a bridge from the original project, uses it. Stop it and start again.
- **Needs action / logged out:** WhatsApp ended the session or another client replaced it. Run `whatsapp-mcp setup` to link again.
- **Sending says the session is not paired:** in Pi, confirm the pairing question or run `/whatsapp-pair`.
- **Messages missing after linking:** WhatsApp sends history in batches. Leave the bridge running for a while.

## License

The source code is MIT licensed; see [LICENSE](LICENSE). The release binaries include GPL-3.0 and MPL-2.0 libraries, so they are distributed under the terms of the GPL-3.0; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

Based on [whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) by Luke Harries.

# Manual verification

CI runs the unit and integration tests on macOS, Linux and Windows with WhatsApp mocked. It cannot show that linking, receiving and sending work with a real account, or that autostart works on a real desktop. These checks need a person. Record the date, OS, architecture and version for each run.

Use your own account and send test messages only to yourself or to someone who agreed to receive them. Never attach logs without checking that they contain no personal data.

## Without a WhatsApp account (dry run)

`whatsapp-bridge/manual_test.go` serves the real API, approval and outbox checks with fictional chats and a sender that only prints what it would have sent:

```sh
cd whatsapp-bridge
go test -tags manual -run TestManualDryRunBridge -timeout 0 -v .
```

Then in Pi, with the extension loaded (`pi -e /path/to/pi-whatsapp`):

1. `/whatsapp-setup /path/to/whatsapp-mcp` registers the MCP server. The dry-run bridge reports itself as linked and running.
2. Ask: "What are Ada's latest WhatsApp messages?" Pi reads the demo chats.
3. Ask Pi to send Ada (12025550123) a message. Pi asks once to approve messages from this session. Check that the review shows the recipient and the full text, and that *Cancel* is selected.
4. Cancel: Pi says nothing was sent, and the bridge prints nothing.
5. Ask again and approve: the bridge prints `DRY RUN, nothing sent: text to 12025550123@s.whatsapp.net`.
6. Copy a small PDF into the outbox and ask Pi to send it. The review offers a private copy of the exact file to open.
7. Ask Pi to read `api-token` in the bridge data directory: the call is blocked.
8. Quit Pi: the pairing is revoked; `whatsapp-mcp status --json` shows an empty `approver_id`.

## With a WhatsApp account

| Check | macOS | Linux | Windows |
|---|---|---|---|
| `whatsapp-mcp setup` or `/whatsapp-setup` shows a QR code that the phone scans | | | |
| The device appears under Linked devices on the phone | | | |
| History arrives; `list_chats` and `list_messages` show real chats | | | |
| A message received while the bridge runs appears in `list_messages` | | | |
| A text message to yourself, approved in Pi, arrives exactly as reviewed | | | |
| A file and a voice message from the outbox arrive | | | |
| `download_media` saves a received photo and voice message under `downloads/` | | | |
| Declining the review sends nothing | | | |
| `autostart enable`, log out and in: the bridge runs | | | |
| Kill the bridge process: it restarts after about 30 s | | | |
| `whatsapp-mcp stop`: it stays stopped | | | |
| Turn the network off for a minute and on again: the bridge reconnects | | | |
| `whatsapp-mcp uninstall` removes autostart and keeps the data | | | |

Mark a platform as tested in the README only after every row passes on it.

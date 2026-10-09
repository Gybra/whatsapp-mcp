# Threat model

## What is worth protecting

- **The WhatsApp session** (`whatsapp.db`): whoever has it can read and send as you.
- **The message history and downloads** (`messages.db`, `downloads/`).
- **Your account's reputation**: messages sent in your name.
- **Files on your computer** that could be sent by mistake.

## Who could misuse the bridge

| Actor | Can | Protection |
|---|---|---|
| The AI model, manipulated by a message or document | Call the MCP tools, run Pi's file and shell tools | Sends need your approval in Pi; the pi-whatsapp extension blocks its file and shell tools from the bridge's data and pairing command. |
| Another local program using the API | Send HTTP requests to `127.0.0.1:8080` | Token on every request; sends still need a signed approval; no API route can create an approver. |
| A web page you visit | Send requests to localhost | Requests with an `Origin` or a host other than `127.0.0.1:8080` are rejected (also DNS rebinding). |
| Someone on your network | Connect to your computer | The API listens only on loopback. |
| A sender on WhatsApp | Choose message text, contact names and file names | Treated as data, never as instructions; file names never become paths. |
| A program running as your user | Read and write all your files | **Not protected.** Use operating-system isolation if needed. |

## Approval flow

1. The agent asks to send a message through the `send_*` MCP tools.
2. The Pi extension asks the bridge to freeze the exact message. The bridge resolves the recipient, reads the file once and keeps those bytes.
3. The extension shows you the frozen message in full, with control and invisible characters made visible.
4. Only if you choose *Approve and send* does the extension sign it, with a key that exists only in that Pi process.
5. The bridge checks the signature, the expiry and that nothing changed, spends the approval and sends the frozen bytes.

Pairing the Pi session's key needs your confirmation in Pi once per Pi process. It cannot be done through the API.

## Residual risks

- **Same-user programs** can bypass everything above. This includes Pi's own shell tool used in ways the extension's checks do not recognise; those checks are a guard rail, not a sandbox.
- **Misleading messages:** approval proves you saw the message, not that it is wise to send.
- **Uncertain sends:** if the connection drops while sending, the bridge reports that delivery is uncertain. It never resends automatically, so check WhatsApp before asking again.
- **Unofficial protocol:** WhatsApp can change its protocol, break the bridge or restrict the account.
- **Data reaching model providers:** messages the agent reads are sent to its model provider and stored in Pi's session files.

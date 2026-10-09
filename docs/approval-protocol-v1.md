# Approval protocol v1

Every outgoing WhatsApp operation (text, file or voice message) is sent only after a human approves that exact operation. The bridge enforces this: it refuses to send without a valid proof, whichever client calls it. This document describes the wire format shared by the bridge and an approver such as the [`pi-whatsapp`](https://github.com/Gybra/pi-whatsapp) Pi extension.

Status: the approval core is implemented and tested without a WhatsApp connection. Sending remains disabled until the enrollment flow, the HTTP API and the reviewer UI pass their own tests.

## Roles

- **Bridge**: owns the WhatsApp session. It validates and freezes each proposed operation, and verifies the approval before sending.
- **Approver**: an interactive component holding an Ed25519 key pair for one session. It shows the user the frozen operation and signs it only after a direct confirmation.
- **Enrollment**: the user authorizes the approver's public key from a local terminal with `whatsapp-mcp setup --pair-pi`. The bridge stores only the public key. The pairing has no expiry: the private key lives only in the approver process, which revokes the pairing when it exits; after a crash the stored public key is useless and the next pairing replaces it. There is no API route to enroll a key: the API bearer token cannot create or replace the approver.

## Flow

1. The client submits a `SendInput`. The bridge validates it, resolves the recipient and freezes the exact text and attachment bytes. Nothing is uploaded to WhatsApp yet.
2. The bridge returns a `DraftView`: the exact payload bytes the approver must display, and a challenge to sign.
3. The approver checks the challenge against the payload and its own key, shows the full payload to the user and, after confirmation, signs the challenge.
4. The client sends the original `SendInput` together with an `ApprovalProof`. The bridge verifies the signature, the expiry, the approver and that the input is byte-for-byte the one it froze, then sends the frozen bytes.

## Wire types

All JSON field names are as listed. Binary values are standard base64 with padding; other spellings are rejected.

`SendInput`

| Field | Type | Notes |
|---|---|---|
| `kind` | string | `text`, `file` or `voice` |
| `recipient` | string | as submitted by the client |
| `message` | string | text, or caption for media; UTF-8, at most 32 KiB |
| `media_id` | string | empty for `text` |

`ApprovalPayload`, the reviewed content, serialized in exactly this field order:

| Field | Type | Notes |
|---|---|---|
| `version` | number | `1` |
| `draft_id` | string | 32 lowercase hex characters |
| `recipient_jid` | string | resolved WhatsApp recipient |
| `message` | string | identical to `SendInput.message` |
| `kind` | string | identical to `SendInput.kind` |
| `attachment` | object or `null` | `id`, `name`, `mime`, `size` (bytes) and `sha256` (lowercase hex) of the frozen file, at most 16 MiB |
| `expires_at` | number | Unix seconds |

`DraftView`: `draft_id`, `payload_b64` (the exact payload bytes), `challenge_b64`, `approver_id`, `expires_at`.

`ApprovalProof`: `draft_id`, `signature_b64`.

The approver must parse and display the payload bytes it received. It must not rebuild them from its own data structures.

## Challenge and signature

```
challenge = ASCII("whatsapp-mcp/approval/v1") || 0x00
         || nonce            (32 random bytes)
         || SHA-256(payload)  (32 bytes)
         || SHA-256(approver public key, raw 32 bytes)
```

`approver_id` is the lowercase hex SHA-256 of the raw public key. The approver verifies that the challenge starts with the domain, that it contains the hash of the payload it displays and the hash of its own public key, and that `expires_at` has not passed. The signature is Ed25519 over the challenge bytes.

## Bridge guarantees

- A draft expires 120 seconds after creation. A proof presented at or after `expires_at` is rejected.
- A proof is accepted once. A valid signature is spent even if the accompanying input differs from the frozen one, so it cannot be replayed with corrected arguments. A retry after a failed or uncertain send needs a new approval.
- A proof is bound to its draft, its nonce and the enrolled key. A signature for one draft does not authorize another one, even with identical content.
- Replacing or removing the approver invalidates pending drafts. So do revocation and bridge shutdown.
- At most 32 drafts can wait for approval at once.
- Drafts are kept in memory only.

The test vector in [`whatsapp-bridge/testdata/approval-v1.json`](../whatsapp-bridge/testdata/approval-v1.json) uses the public RFC 8032 test key and a fictional recipient. Both implementations must accept it and reject any modified byte.

## Limits

This protocol protects the bridge's supported interfaces: the MCP tools and the local HTTP API. It does not protect against a process that runs as the same operating-system user and can modify the bridge, its data directory or the approver itself. Protecting against that requires operating-system isolation, which this project does not provide.

Approval proves that the user confirmed what was displayed. It cannot prevent a user from approving a misleading message. The approver must therefore show the complete content, with control and bidirectional characters made visible.

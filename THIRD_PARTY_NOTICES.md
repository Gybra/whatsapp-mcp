# Third-party notices

This project's own source code, and the upstream [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) code it is derived from, are licensed under the MIT License (see [LICENSE](LICENSE)).

A compiled `whatsapp-mcp` executable also contains the Go modules listed below, each under its own license. **The executable as a whole is therefore not MIT-only.** In particular, `go.mau.fi/libsignal` is licensed under the GPL-3.0, and `whatsmeow` and `go.mau.fi/util` under the MPL-2.0. Release binaries of this project are therefore distributed under the terms of the GPL-3.0, and each release links to the exact tagged source code it was built from. Anyone redistributing binaries must meet the same terms. This file is an inventory, not legal advice.

Modules compiled into the bridge (from `go list -deps`, with each license identified from the module's own license file):

| Module | Version | License |
|---|---|---|
| filippo.io/edwards25519 | v1.2.0 | BSD-3-Clause |
| github.com/beeper/argo-go | v1.1.2 | MIT |
| github.com/coder/websocket | v1.8.15 | ISC |
| github.com/dustin/go-humanize | v1.0.1 | MIT |
| github.com/elliotchance/orderedmap/v3 | v3.1.0 | MIT |
| github.com/google/jsonschema-go | v0.4.3 | MIT |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/mattn/go-colorable | v0.1.14 | MIT |
| github.com/mattn/go-isatty | v0.0.24 | MIT |
| github.com/modelcontextprotocol/go-sdk | v1.8.0 | Apache-2.0 for new contributions, MIT for earlier ones (see its LICENSE) |
| github.com/ncruces/go-strftime | v1.0.0 | MIT |
| github.com/petermattis/goid | v0.0.0-20260820044319-269ab09b5261 | Apache-2.0 |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause |
| github.com/rs/zerolog | v1.35.1 | MIT |
| github.com/segmentio/asm | v1.1.3 | MIT |
| github.com/segmentio/encoding | v0.5.4 | MIT |
| github.com/vektah/gqlparser/v2 | v2.5.27 | MIT |
| github.com/yosida95/uritemplate/v3 | v3.0.2 | BSD-3-Clause |
| go.mau.fi/libsignal | v0.2.2 | **GPL-3.0** |
| go.mau.fi/util | v0.10.1 | MPL-2.0 |
| go.mau.fi/whatsmeow | v0.0.0-20261007111105-c386243a72ba | MPL-2.0 |
| golang.org/x/crypto | v0.57.0 | BSD-3-Clause |
| golang.org/x/exp | v0.0.0-20260908205506-85c1c2202aba | BSD-3-Clause |
| golang.org/x/net | v0.60.0 | BSD-3-Clause |
| golang.org/x/oauth2 | v0.35.0 | BSD-3-Clause |
| golang.org/x/sync | v0.23.0 | BSD-3-Clause |
| golang.org/x/sys | v0.49.0 | BSD-3-Clause |
| golang.org/x/term | v0.46.0 | BSD-3-Clause |
| golang.org/x/text | v0.42.0 | BSD-3-Clause |
| golang.org/x/time | v0.15.0 | BSD-3-Clause |
| google.golang.org/protobuf | v1.36.12 | BSD-3-Clause |
| modernc.org/libc | v1.77.1 | BSD-3-Clause |
| modernc.org/mathutil | v1.7.1 | BSD-3-Clause |
| modernc.org/memory | v1.12.1 | BSD-3-Clause |
| modernc.org/sqlite | v1.60.1 | BSD-3-Clause (contains a transpiled copy of SQLite, which is in the public domain) |
| rsc.io/qr | v0.2.0 | BSD-3-Clause |

The Go standard library and runtime are licensed under the BSD-3-Clause license.

Update this table whenever `whatsapp-bridge/go.mod` changes.

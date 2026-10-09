package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `This build only supports pairing the Pi approver while the bridge is being hardened.

Usage:
  whatsapp-mcp setup --pair-pi   Trust the Pi session shown by /whatsapp-pair to approve outgoing messages
`

func main() {
	restrictUmask()
	os.Exit(run(os.Args[1:], newTerminalConsole(), os.Stderr))
}

func run(args []string, console TrustedConsole, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "setup" || args[1] != "--pair-pi" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	dataDir, err := defaultDataDir()
	if err == nil {
		err = RunPairingSetup(dataDir, console)
	}
	if err != nil {
		fmt.Fprintf(stderr, "Pairing failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr, "Approver paired. Run /whatsapp-pair again in Pi to confirm.")
	return 0
}

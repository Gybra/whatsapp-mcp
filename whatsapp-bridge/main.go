package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `Usage:
  whatsapp-mcp mcp               Serve the WhatsApp tools to an MCP client over stdin/stdout
  whatsapp-mcp setup --pair-pi   Trust the Pi session shown by /whatsapp-pair to approve outgoing messages
`

func main() {
	restrictUmask()
	os.Exit(run(os.Args[1:], newTerminalConsole(), os.Stderr))
}

func run(args []string, console TrustedConsole, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "mcp" {
		return runMCP(stderr)
	}
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

// runMCP serves MCP over stdio. stdout carries only the protocol; diagnostics go to stderr.
func runMCP(stderr io.Writer) int {
	dataDir, err := defaultDataDir()
	var token []byte
	if err == nil {
		token, err = LoadToken(dataDir)
	}
	if err != nil {
		fmt.Fprintln(stderr, "The WhatsApp bridge is not set up: run whatsapp-mcp setup first.")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := RunMCP(ctx, NewBridgeClient(token)); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "MCP server stopped:", err)
		return 1
	}
	return 0
}

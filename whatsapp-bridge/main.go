package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	restrictUmask()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := newCLI().Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

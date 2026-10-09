//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMediaSpecialFile(t *testing.T) {
	dir := privateDataDir(t)
	outboxFile(t, dir, "seed.pdf", samples["report.pdf"])
	if err := syscall.Mkfifo(filepath.Join(dir, "outbox", "pipe.pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := FreezeSend(context.Background(), dir, fileInput("pipe.pdf"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("FIFO not rejected as a non-regular file: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}

func TestMediaHardLink(t *testing.T) {
	dir := privateDataDir(t)
	outside := filepath.Join(dir, "outside.pdf")
	os.WriteFile(outside, samples["report.pdf"], 0o600)
	outboxFile(t, dir, "seed.pdf", samples["report.pdf"])
	if err := os.Link(outside, filepath.Join(dir, "outbox", "linked.pdf")); err != nil {
		t.Fatal(err)
	}
	if _, err := FreezeSend(context.Background(), dir, fileInput("linked.pdf")); err == nil {
		t.Fatal("hard-linked file accepted")
	}
}

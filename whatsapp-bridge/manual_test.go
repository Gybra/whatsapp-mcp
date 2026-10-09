//go:build manual

package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// dryRunSender stands in for WhatsApp during manual verification: it reports
// what would have been sent, without the content, and sends nothing.
type dryRunSender struct{}

func (dryRunSender) Send(_ context.Context, draft FrozenDraft) (SendReceipt, error) {
	size, digest := len(draft.Payload.Message), ""
	if a := draft.Payload.Attachment; a != nil {
		size, digest = int(a.Size), " sha256="+a.SHA256[:16]+"…"
	}
	fmt.Printf("DRY RUN, nothing sent: %s to %s (%d bytes%s)\n", draft.Payload.Kind, draft.Payload.RecipientJID, size, digest)
	return SendReceipt{Status: "sent", MessageID: fmt.Sprintf("DRYRUN%d", time.Now().UnixNano())}, nil
}

// TestManualDryRunBridge serves the real API on 127.0.0.1:8080 with real approval,
// pairing and outbox checks, demo chats and a dry-run sender. It never contacts WhatsApp.
//
//	go test -tags manual -run TestManualDryRunBridge -timeout 0 -v .
//
// It uses the real data directory for the API token, the approver pairing and the
// outbox, so the Pi extension and `whatsapp-mcp pair-pi` work unchanged.
// Stop it with Ctrl-C.
func TestManualDryRunBridge(t *testing.T) {
	dataDir, err := defaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(dataDir); err != nil {
		t.Fatal(err)
	}
	token, err := LoadOrCreateToken(dataDir, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	demo := seededStore(t) // fictional chats in a temporary directory
	approvals := NewApprovalManager(time.Now, rand.Reader, func() (ApproverLease, error) { return LoadApprover(dataDir) })
	defer approvals.Close()
	handler := NewAPI(APIConfig{
		Token:          token,
		RevokeApprover: func() error { return RevokeApprover(dataDir) },
		Status:         func() RuntimeStatus { return RuntimeStatus{State: "online", Connected: true} },
	}, approvals, func(ctx context.Context, in SendInput) (FrozenDraft, error) { return FreezeSend(ctx, dataDir, in) },
		dryRunSender{}, &storeReads{MessageStore: demo})
	listener, err := ListenLoopback()
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("Dry-run bridge on %s. Data directory: %s. Demo contacts: Ada (12025550123), Bob (12025550199).\n", apiAddress, dataDir)
	if err := RunHTTP(ctx, listener, handler); err != nil {
		t.Fatal(err)
	}
}

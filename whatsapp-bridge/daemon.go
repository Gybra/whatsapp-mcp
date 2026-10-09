package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	logFileName = "bridge.log"
	maxLogBytes = 5 * 1024 * 1024
)

// runBridge runs the bridge until ctx ends or a client asks it to stop. Holding
// the loopback port makes it the only bridge for this user. ready, if set, is
// called once the API accepts requests.
func runBridge(ctx context.Context, dataDir string, stderr io.Writer, ready func()) error {
	if err := EnsurePrivateDir(dataDir); err != nil {
		return err
	}
	token, err := LoadOrCreateToken(dataDir, rand.Reader)
	if err != nil {
		return err
	}
	logFile, err := openLogFile(dataDir)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(logFile, stderr), &slog.HandlerOptions{Level: slog.LevelInfo}))

	listener, err := ListenLoopback()
	if err != nil {
		logger.Error("could not start", "error", err)
		return err
	}
	store, err := OpenMessageStore(dataDir)
	if err != nil {
		listener.Close()
		return err
	}
	defer store.Close()
	runtime, err := NewWhatsAppRuntime(RuntimeConfig{DataDir: dataDir, Logger: logger}, store)
	if err != nil {
		listener.Close()
		return err
	}
	defer runtime.Close()
	approvals := NewApprovalManager(time.Now, rand.Reader, func() (ApproverLease, error) { return LoadApprover(dataDir) })
	defer approvals.Close()

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	handler := NewAPI(APIConfig{
		Token:          token,
		RevokeApprover: func() error { return RevokeApprover(dataDir) },
		Status:         runtime.Status,
		Shutdown:       stop,
	}, approvals, func(ctx context.Context, in SendInput) (FrozenDraft, error) { return FreezeSend(ctx, dataDir, in) }, runtime, runtime)

	switch err := runtime.Connect(ctx); {
	case errors.Is(err, ErrNotPaired):
		logger.Warn("not linked to WhatsApp yet; run whatsapp-mcp setup")
	case err != nil:
		logger.Warn("could not reach WhatsApp yet; retrying in the background")
	}
	logger.Info("bridge started", "address", apiAddress, "version", version)
	if ready != nil {
		ready()
	}
	err = RunHTTP(ctx, listener, handler)
	logger.Info("bridge stopped")
	return err
}

// openLogFile opens the private log, starting a new one when it grows past maxLogBytes.
func openLogFile(dataDir string) (*os.File, error) {
	path := filepath.Join(dataDir, logFileName)
	if info, err := os.Lstat(path); err == nil && info.Size() > maxLogBytes {
		os.Rename(path, path+".1")
	}
	if err := EnsurePrivateFile(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
}

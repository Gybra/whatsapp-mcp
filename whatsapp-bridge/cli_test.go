package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cliHarness struct {
	cli     *CLI
	out     bytes.Buffer
	errOut  bytes.Buffer
	runner  *commandLog
	spawned [][]string
	running bool
	linked  bool
	stopped int
	console *fakeConsole
}

func newCLIHarness(t *testing.T) *cliHarness {
	t.Helper()
	h := &cliHarness{runner: &commandLog{}, console: &fakeConsole{interactive: true}}
	home := t.TempDir()
	h.cli = &CLI{
		Stdin: strings.NewReader(""), Stdout: &h.out, Stderr: &h.errOut, Console: h.console,
		Command: func(ctx context.Context, name string, args ...string) error {
			for _, a := range args {
				if a == "bootstrap" || a == "kickstart" {
					h.running = true // the service manager starts the bridge
				}
			}
			return h.runner.run(ctx, name, args...)
		},
		Executable: filepath.Join(home, "bin", "whatsapp-mcp"),
		Service:    ServiceConfig{Platform: "darwin", BinaryPath: filepath.Join(home, "bin", "whatsapp-mcp"), Home: home, ConfigHome: filepath.Join(home, ".config"), UID: "501", TempDir: t.TempDir()},
		DataDir:    filepath.Join(home, "data"),
		Spawn: func(binary string, args ...string) error {
			h.spawned = append(h.spawned, append([]string{binary}, args...))
			h.running = true
			return nil
		},
		Status: func(context.Context) (APIStatus, error) {
			if !h.running {
				return APIStatus{}, ErrBridgeUnreachable
			}
			state := "unpaired"
			if h.linked {
				state = "online"
			}
			return APIStatus{State: state, Connected: h.linked}, nil
		},
		Shutdown: func(context.Context) error {
			if !h.running {
				return ErrBridgeUnreachable
			}
			h.running = false
			h.stopped++
			return nil
		},
		Link: func(_ context.Context, _ string, out func(LinkEvent), confirm func(string) (bool, error)) error {
			out(LinkEvent{Event: "qr", Modules: []string{"0000", "0110", "0110", "0000"}, ExpiresIn: 60})
			out(LinkEvent{Event: "linked", Message: "linked"})
			h.linked = true
			return nil
		},
		WaitStep: time.Millisecond,
	}
	return h
}

func (h *cliHarness) run(args ...string) int {
	h.out.Reset()
	h.errOut.Reset()
	return h.cli.Run(context.Background(), args)
}

func TestCLIUsage(t *testing.T) {
	h := newCLIHarness(t)
	for _, args := range [][]string{nil, {"bogus"}, {"autostart"}, {"autostart", "maybe"}, {"logs", "-n", "x"}, {"start", "extra"}} {
		if code := h.run(args...); code != 2 || !strings.Contains(h.errOut.String(), "Usage") {
			t.Errorf("%q: exit %d, %q", args, code, h.errOut.String())
		}
	}
	if code := h.run("version"); code != 0 || strings.TrimSpace(h.out.String()) != version {
		t.Errorf("version: %d %q", code, h.out.String())
	}
}

func TestCLIStartStopRestart(t *testing.T) {
	h := newCLIHarness(t)
	if code := h.run("stop"); code != 0 || !strings.Contains(h.out.String(), "not running") {
		t.Fatalf("stop when stopped: %d %q", code, h.out.String())
	}
	if code := h.run("start"); code != 0 || len(h.spawned) != 1 || strings.Join(h.spawned[0][1:], " ") != "bridge" {
		t.Fatalf("start: %d %v %q", code, h.spawned, h.errOut.String())
	}
	if code := h.run("start"); code != 0 || len(h.spawned) != 1 || !strings.Contains(h.out.String(), "already running") {
		t.Fatal("a second start must not launch a second bridge")
	}
	if code := h.run("restart"); code != 0 || h.stopped != 1 || len(h.spawned) != 2 {
		t.Fatalf("restart: %d stopped %d spawned %d", code, h.stopped, len(h.spawned))
	}
	if code := h.run("stop"); code != 0 || h.running {
		t.Fatal("stop failed")
	}

	// With autostart on, start goes through the service manager instead.
	h.run("autostart", "enable")
	h.running = false
	h.runner.calls = nil
	h.cli.Spawn = func(string, ...string) error { t.Fatal("spawned directly although a service is installed"); return nil }
	h.cli.Command = func(ctx context.Context, name string, args ...string) error {
		h.runner.run(ctx, name, args...)
		if len(args) > 0 && args[0] == "kickstart" {
			h.running = true
		}
		return nil
	}
	if code := h.run("start"); code != 0 || !strings.Contains(strings.Join(h.runner.calls[len(h.runner.calls)-1], " "), "kickstart") {
		t.Fatalf("start with autostart: %d %v", code, h.runner.calls)
	}

	h.running = false
	h.cli.Command = h.runner.run // the service never comes up
	h.cli.StartTimeout = 20 * time.Millisecond
	if code := h.run("start"); code != 1 || !strings.Contains(h.errOut.String(), "whatsapp-mcp logs") {
		t.Fatalf("a bridge that never answers must fail with a hint: %d %q", code, h.errOut.String())
	}
}

func TestCLIStatus(t *testing.T) {
	h := newCLIHarness(t)
	if code := h.run("status", "--json"); code != 0 {
		t.Fatal(h.errOut.String())
	}
	var status CLIStatus
	if err := json.Unmarshal(h.out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Running || status.Linked || status.Autostart || status.DataDir != h.cli.DataDir || status.Version != version {
		t.Fatalf("stopped status: %+v", status)
	}

	// Linked is read from the session database when the bridge is not running.
	EnsurePrivateDir(h.cli.DataDir)
	db, _ := sql.Open("sqlite", filepath.Join(h.cli.DataDir, "whatsapp.db"))
	db.Exec(`CREATE TABLE whatsmeow_device (jid TEXT PRIMARY KEY); INSERT INTO whatsmeow_device VALUES ('12025550123:1@s.whatsapp.net')`)
	db.Close()
	h.run("status", "--json")
	json.Unmarshal(h.out.Bytes(), &status)
	if !status.Linked {
		t.Fatalf("linked session not detected: %+v", status)
	}

	h.running, h.linked = true, true
	h.run("autostart", "enable")
	h.run("status", "--json")
	json.Unmarshal(h.out.Bytes(), &status)
	if !status.Running || status.State != "online" || !status.Autostart {
		t.Fatalf("running status: %+v", status)
	}
	h.run("status")
	if !strings.Contains(h.out.String(), "online") || strings.Contains(h.out.String(), "{") {
		t.Fatalf("text status: %q", h.out.String())
	}
}

func TestCLILogs(t *testing.T) {
	h := newCLIHarness(t)
	if code := h.run("logs"); code != 0 || !strings.Contains(h.out.String(), "No log") {
		t.Fatalf("no log: %d %q", code, h.out.String())
	}
	EnsurePrivateDir(h.cli.DataDir)
	var lines []string
	for i := range 150 {
		lines = append(lines, "line "+strings.Repeat("x", i%3)+string(rune('a'+i%26)))
	}
	os.WriteFile(filepath.Join(h.cli.DataDir, "bridge.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	h.run("logs", "-n", "3")
	if got := strings.Split(strings.TrimSpace(h.out.String()), "\n"); len(got) != 3 || got[2] != lines[149] {
		t.Fatalf("tail: %q", got)
	}
	h.run("logs")
	if got := strings.Split(strings.TrimSpace(h.out.String()), "\n"); len(got) != 100 {
		t.Fatalf("default tail: %d lines", len(got))
	}
}

func TestCLIAutostart(t *testing.T) {
	h := newCLIHarness(t)
	if code := h.run("autostart", "enable"); code != 0 {
		t.Fatal(h.errOut.String())
	}
	if _, err := os.Stat(servicePath(h.cli.Service)); err != nil {
		t.Fatal("LaunchAgent not written")
	}
	if code := h.run("autostart", "disable"); code != 0 {
		t.Fatal(h.errOut.String())
	}
	if _, err := os.Stat(servicePath(h.cli.Service)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("LaunchAgent not removed")
	}
}

func TestCLIPairPi(t *testing.T) {
	h := newCLIHarness(t)
	pub := newPublicKey(t)
	h.cli.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString(pub) + "\n")
	if code := h.run("pair-pi"); code != 0 {
		t.Fatal(h.errOut.String())
	}
	var out struct {
		ApproverID string `json:"approver_id"`
	}
	if json.Unmarshal(h.out.Bytes(), &out); out.ApproverID != approverID(pub) {
		t.Fatalf("output: %q", h.out.String())
	}
	if _, err := LoadApprover(h.cli.DataDir); err != nil {
		t.Fatal(err)
	}
	h.cli.Stdin = strings.NewReader("not a key")
	if code := h.run("pair-pi"); code != 1 {
		t.Fatal("bad key accepted")
	}
}

func TestCLILink(t *testing.T) {
	h := newCLIHarness(t)
	if code := h.run("link", "--json"); code != 0 {
		t.Fatal(h.errOut.String())
	}
	var events []LinkEvent
	for _, line := range strings.Split(strings.TrimSpace(h.out.String()), "\n") {
		var e LinkEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("stdout must be JSON lines: %q", line)
		}
		events = append(events, e)
	}
	if len(events) != 2 || events[0].Event != "qr" || events[1].Event != "linked" {
		t.Fatalf("events: %+v", events)
	}

	h.linked = false
	h.run("link")
	if !strings.Contains(h.out.String(), "\x1b[30;47m") || !strings.Contains(h.out.String(), "Linked devices") {
		t.Fatalf("terminal link must draw the QR and explain where to scan it: %q", h.out.String())
	}

	h.running = true
	if code := h.run("link", "--json"); code != 1 || !strings.Contains(h.errOut.String(), "whatsapp-mcp stop") {
		t.Fatalf("linking while the bridge runs must be refused: %d %q", code, h.errOut.String())
	}
}

func TestCLISetup(t *testing.T) {
	h := newCLIHarness(t)
	h.console.answers = []bool{true, true} // link now, autostart
	if code := h.run("setup"); code != 0 {
		t.Fatalf("setup: %q", h.errOut.String())
	}
	if !h.linked || !AutostartEnabled(context.Background(), h.cli.Service, h.runner.run) {
		t.Fatal("setup did not link and enable autostart")
	}
	if _, err := LoadToken(h.cli.DataDir); err != nil {
		t.Fatal("setup did not create the API token")
	}
	if !strings.Contains(h.out.String(), "pi install npm:pi-whatsapp") {
		t.Fatalf("setup must explain the Pi integration: %q", h.out.String())
	}

	// Rerunning keeps the existing session and asks nothing destructive.
	token, _ := LoadToken(h.cli.DataDir)
	h.console.answers, h.console.prompts = []bool{false}, nil
	h.cli.Link = func(context.Context, string, func(LinkEvent), func(string) (bool, error)) error {
		t.Fatal("setup relinked an already linked computer")
		return nil
	}
	h.console.answers = []bool{true, true, true} // would accept anything it asked
	h.run("setup")
	for _, prompt := range h.console.prompts {
		if strings.Contains(prompt, "Link this computer") {
			t.Fatal("setup offered to relink an already linked computer")
		}
	}
	if again, _ := LoadToken(h.cli.DataDir); !bytes.Equal(token, again) {
		t.Fatal("setup replaced the API token")
	}

	h2 := newCLIHarness(t)
	h2.console.interactive = false
	if code := h2.run("setup"); code != 1 || h2.linked {
		t.Fatal("setup without a terminal must refuse")
	}
	h3 := newCLIHarness(t)
	h3.console.answers = []bool{false, false} // decline linking and autostart
	if code := h3.run("setup"); code != 0 || h3.linked || AutostartEnabled(context.Background(), h3.cli.Service, h3.runner.run) {
		t.Fatal("declined steps were performed")
	}
}

func TestCLIUninstall(t *testing.T) {
	h := newCLIHarness(t)
	h.run("autostart", "enable")
	EnsurePrivateDir(h.cli.DataDir)
	os.WriteFile(filepath.Join(h.cli.DataDir, "whatsapp.db"), []byte("session"), 0o600)
	h.running = true
	if code := h.run("uninstall"); code != 0 {
		t.Fatal(h.errOut.String())
	}
	if h.running || AutostartEnabled(context.Background(), h.cli.Service, h.runner.run) {
		t.Fatal("uninstall left the bridge running or autostart enabled")
	}
	if _, err := os.Stat(filepath.Join(h.cli.DataDir, "whatsapp.db")); err != nil {
		t.Fatal("uninstall without --purge deleted data")
	}
	h.console.answers = []bool{false}
	h.run("uninstall", "--purge")
	if _, err := os.Stat(h.cli.DataDir); err != nil {
		t.Fatal("purge without confirmation deleted data")
	}
	h.console.interactive = false
	h.console.answers = []bool{true}
	if code := h.run("uninstall", "--purge"); code != 1 {
		t.Fatal("purge without a terminal must refuse")
	}
	h.console.interactive = true
	h.console.answers = []bool{true}
	if code := h.run("uninstall", "--purge"); code != 0 {
		t.Fatal(h.errOut.String())
	}
	if _, err := os.Stat(h.cli.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("confirmed purge kept the data")
	}
}

func TestBridgeDaemon(t *testing.T) {
	probe, err := net.Listen("tcp4", apiAddress)
	if err != nil {
		t.Skip("port 8080 is in use on this machine")
	}
	probe.Close()
	dataDir := filepath.Join(t.TempDir(), "data")
	var logs syncBuffer
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- runBridge(context.Background(), dataDir, &logs, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("bridge exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not start")
	}
	token, err := LoadToken(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	client := NewBridgeClient(token)
	status, err := client.Status(context.Background())
	if err != nil || status.State != "unpaired" {
		t.Fatalf("status: %+v %v", status, err)
	}
	if _, err := ListenLoopback(); err == nil {
		t.Fatal("a second bridge could start alongside the first")
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bridge exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not stop")
	}
	assertPrivate(t, filepath.Join(dataDir, "bridge.log"), false)
	if !strings.Contains(logs.String(), "bridge started") {
		t.Fatalf("logs: %q", logs.String())
	}
	if _, err := client.Status(context.Background()); !errors.Is(err, ErrBridgeUnreachable) {
		t.Fatalf("after stop: %v", err)
	}
}

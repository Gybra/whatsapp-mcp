package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeConsole struct {
	interactive bool
	pasted      string
	confirm     bool
	prompts     []string
}

func (c *fakeConsole) IsInteractive() bool { return c.interactive }

func (c *fakeConsole) ReadPublicKey() (string, error) { return c.pasted, nil }

func (c *fakeConsole) ConfirmFingerprint(fingerprint string) (bool, error) {
	c.prompts = append(c.prompts, fingerprint)
	return c.confirm, nil
}

func (c *fakeConsole) ConfirmImport(description string) (bool, error) {
	c.prompts = append(c.prompts, description)
	return c.confirm, nil
}

func privateDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Application Support", "whatsapp-mcp")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	return dir
}

func newPublicKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestDataDirResolution(t *testing.T) {
	home := filepath.FromSlash("/home/ada")
	cases := []struct {
		platform, localAppData, xdg, want string
	}{
		{"darwin", "", "", filepath.Join(home, "Library", "Application Support", "whatsapp-mcp")},
		{"linux", "", "", filepath.Join(home, ".local", "share", "whatsapp-mcp")},
		{"linux", "", filepath.FromSlash("/data/xdg"), filepath.Join(filepath.FromSlash("/data/xdg"), "whatsapp-mcp")},
		{"linux", "", "relative/xdg", filepath.Join(home, ".local", "share", "whatsapp-mcp")},
		{"windows", filepath.FromSlash("/AppData/Local"), "", filepath.Join(filepath.FromSlash("/AppData/Local"), "whatsapp-mcp")},
	}
	for _, c := range cases {
		got, err := ResolveDataDir(c.platform, home, c.localAppData, c.xdg)
		if err != nil || got != c.want {
			t.Errorf("%s xdg=%q: got %q, %v; want %q", c.platform, c.xdg, got, err, c.want)
		}
	}
	for _, bad := range [][2]string{{"darwin", ""}, {"linux", "relative/home"}, {"windows", ""}} {
		if _, err := ResolveDataDir(bad[0], bad[1], "", ""); err == nil {
			t.Errorf("%s with home %q: expected an error", bad[0], bad[1])
		}
	}
}

func TestPermissionPrivateDirAndFile(t *testing.T) {
	dir := privateDataDir(t)
	assertPrivate(t, dir, true)
	file := filepath.Join(dir, "secret")
	if err := EnsurePrivateFile(file); err != nil {
		t.Fatalf("EnsurePrivateFile: %v", err)
	}
	assertPrivate(t, file, false)
	if err := EnsurePrivateFile(file); err != nil {
		t.Fatalf("EnsurePrivateFile on existing file: %v", err)
	}
}

func TestPermissionRejectsUnsafePaths(t *testing.T) {
	dir := privateDataDir(t)
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("x"), 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := EnsurePrivateFile(link); err == nil {
		t.Error("symlinked file accepted")
	}
	if err := EnsurePrivateDir(link); err == nil {
		t.Error("symlinked directory accepted")
	}
	if err := EnsurePrivateFile(dir); err == nil {
		t.Error("directory accepted as a private file")
	}
	if err := EnsurePrivateDir(target); err == nil {
		t.Error("regular file accepted as a private directory")
	}
}

func TestDataSQLiteSidecarsArePrivate(t *testing.T) {
	dir := privateDataDir(t)
	store, err := OpenMessageStore(dir)
	if err != nil {
		t.Fatalf("OpenMessageStore: %v", err)
	}
	defer store.Close()
	if _, err := store.db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreChat(context.Background(), "12025550123@s.whatsapp.net", "Test", time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatalf("StoreChat: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		assertPrivate(t, filepath.Join(dir, entry.Name()), false)
	}
	if !strings.Contains(strings.Join(names, ","), "messages.db-wal") {
		t.Fatalf("expected a WAL sidecar, got %v", names)
	}
}

func TestTokenCreateAndReuse(t *testing.T) {
	dir := privateDataDir(t)
	first, err := LoadOrCreateToken(dir, rand.Reader)
	if err != nil || len(first) != 32 {
		t.Fatalf("first token: %d bytes, %v", len(first), err)
	}
	assertPrivate(t, filepath.Join(dir, "api-token"), false)
	second, err := LoadOrCreateToken(dir, rand.Reader)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("rerun changed the token: %v", err)
	}

	corrupt := privateDataDir(t)
	os.WriteFile(filepath.Join(corrupt, "api-token"), []byte("short"), 0o600)
	if _, err := LoadOrCreateToken(corrupt, rand.Reader); err == nil {
		t.Fatal("corrupt token accepted")
	}
	if data, _ := os.ReadFile(filepath.Join(corrupt, "api-token")); string(data) != "short" {
		t.Fatal("corrupt token was overwritten instead of reported")
	}

	failing := privateDataDir(t)
	if _, err := LoadOrCreateToken(failing, iotestErrReader{}); err == nil {
		t.Fatal("token created without randomness")
	}
	if _, err := os.Stat(filepath.Join(failing, "api-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed token creation left a file behind")
	}
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestPairApprover(t *testing.T) {
	pub := newPublicKey(t)

	dir := privateDataDir(t)
	console := &fakeConsole{interactive: true, confirm: true}
	if err := PairApprover(dir, pub, console); err != nil {
		t.Fatalf("PairApprover: %v", err)
	}
	if len(console.prompts) != 1 || console.prompts[0] != approverID(pub) {
		t.Fatalf("user was not shown the approver fingerprint: %v", console.prompts)
	}
	assertPrivate(t, filepath.Join(dir, "approver.json"), false)
	lease, err := LoadApprover(dir)
	if err != nil || lease.PublicKeyB64 != base64.StdEncoding.EncodeToString(pub) {
		t.Fatalf("LoadApprover = %+v, %v", lease, err)
	}
	if err := RevokeApprover(dir); err != nil {
		t.Fatalf("RevokeApprover: %v", err)
	}
	if _, err := LoadApprover(dir); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("after revoke: %v", err)
	}
	if err := RevokeApprover(dir); err != nil {
		t.Fatalf("second revoke: %v", err)
	}

	refusals := map[string]struct {
		console *fakeConsole
		key     []byte
	}{
		"no terminal":  {&fakeConsole{interactive: false, confirm: true}, pub},
		"user refuses": {&fakeConsole{interactive: true, confirm: false}, pub},
		"short key":    {&fakeConsole{interactive: true, confirm: true}, pub[:31]},
	}
	for name, c := range refusals {
		dir := privateDataDir(t)
		if err := PairApprover(dir, c.key, c.console); err == nil {
			t.Errorf("%s: pairing succeeded", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "approver.json")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: approver lease written", name)
		}
	}
}

func TestPairRunSetup(t *testing.T) {
	pub := newPublicKey(t)
	encoded := base64.StdEncoding.EncodeToString(pub)
	dir := privateDataDir(t)
	if err := RunPairingSetup(dir, &fakeConsole{interactive: true, confirm: true, pasted: "  " + encoded + "\n"}); err != nil {
		t.Fatalf("RunPairingSetup: %v", err)
	}
	if _, err := LoadApprover(dir); err != nil {
		t.Fatalf("lease not usable: %v", err)
	}
	for name, pasted := range map[string]string{
		"not base64": "%%%",
		"31 bytes":   base64.StdEncoding.EncodeToString(pub[:31]),
		"unpadded":   strings.TrimRight(encoded, "="),
	} {
		dir := privateDataDir(t)
		if err := RunPairingSetup(dir, &fakeConsole{interactive: true, confirm: true, pasted: pasted}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPairMalformedLease(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(newPublicKey(t))
	cases := map[string]string{
		"older expiring format": `{"public_key_b64":"` + key + `","expires_at":1800000100}`,
		"unknown field":         `{"public_key_b64":"` + key + `","admin":true}`,
		"bad key":               `{"public_key_b64":"AAAA"}`,
		"trailing data":         `{"public_key_b64":"` + key + `"} {}`,
		"not json":              `public key`,
	}
	for name, content := range cases {
		dir := privateDataDir(t)
		os.WriteFile(filepath.Join(dir, "approver.json"), []byte(content), 0o600)
		if _, err := LoadApprover(dir); !errors.Is(err, ErrNotPaired) {
			t.Errorf("%s: got %v, want ErrNotPaired", name, err)
		}
	}
	if _, err := LoadApprover(privateDataDir(t)); !errors.Is(err, ErrNotPaired) {
		t.Errorf("missing lease: got %v", err)
	}
}

func TestPairTokenCannotEnroll(t *testing.T) {
	dir := privateDataDir(t)
	if _, err := LoadOrCreateToken(dir, rand.Reader); err != nil {
		t.Fatal(err)
	}
	// Holding the API token gives no terminal: pairing must still be refused.
	if err := RunPairingSetup(dir, &fakeConsole{interactive: false, confirm: true, pasted: base64.StdEncoding.EncodeToString(newPublicKey(t))}); err == nil {
		t.Fatal("pairing without an interactive terminal succeeded")
	}
	if _, err := LoadApprover(dir); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("an approver exists after token-only setup: %v", err)
	}
}

func TestDataSetupKeepsSessionAndToken(t *testing.T) {
	dir := privateDataDir(t)
	session := filepath.Join(dir, "whatsapp.db")
	os.WriteFile(session, []byte("existing session"), 0o600)
	for range 2 {
		if _, err := LoadOrCreateToken(dir, rand.Reader); err != nil {
			t.Fatal(err)
		}
		store, err := OpenMessageStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		store.Close()
	}
	if data, _ := os.ReadFile(session); string(data) != "existing session" {
		t.Fatal("setup modified the WhatsApp session")
	}
}

func writeLegacyStore(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o700)
	messages, err := sql.Open("sqlite3", filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer messages.Close()
	if _, err := messages.Exec(messageSchema + `INSERT INTO chats VALUES ('12025550123@s.whatsapp.net', 'Legacy', '2025-03-01 10:00:00');`); err != nil {
		t.Fatal(err)
	}
	session, err := sql.Open("sqlite3", filepath.Join(dir, "whatsapp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Exec(`CREATE TABLE whatsmeow_device (jid TEXT PRIMARY KEY); INSERT INTO whatsmeow_device VALUES ('12025550123:1@s.whatsapp.net');`); err != nil {
		t.Fatal(err)
	}
}

func TestImportLegacy(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "old checkout", "whatsapp-bridge", "store")
	writeLegacyStore(t, legacy)
	before, _ := os.ReadFile(filepath.Join(legacy, "whatsapp.db"))

	dir := privateDataDir(t)
	console := &fakeConsole{interactive: true, confirm: true}
	if err := ImportLegacy(legacy, dir, console); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if len(console.prompts) != 1 || !strings.Contains(console.prompts[0], "Stop") {
		t.Fatalf("user was not asked to confirm with the old bridge stopped: %v", console.prompts)
	}
	for _, name := range []string{"messages.db", "whatsapp.db"} {
		assertPrivate(t, filepath.Join(dir, name), false)
	}
	store, err := OpenMessageStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	store.db.QueryRow("SELECT name FROM chats").Scan(&name)
	store.Close()
	if name != "Legacy" {
		t.Fatalf("imported chat name = %q", name)
	}
	if after, _ := os.ReadFile(filepath.Join(legacy, "whatsapp.db")); !bytes.Equal(before, after) {
		t.Fatal("import modified the legacy session, which is the user's backup")
	}

	if err := ImportLegacy(legacy, dir, console); err == nil {
		t.Fatal("import overwrote an existing session")
	}

	refusals := map[string]*fakeConsole{
		"no terminal":  {interactive: false, confirm: true},
		"user refuses": {interactive: true, confirm: false},
	}
	for name, console := range refusals {
		dest := privateDataDir(t)
		if err := ImportLegacy(legacy, dest, console); err == nil {
			t.Errorf("%s: import succeeded", name)
		}
		if entries, _ := os.ReadDir(dest); len(entries) != 0 {
			t.Errorf("%s: files written: %v", name, entries)
		}
	}

	wrong := filepath.Join(t.TempDir(), "store")
	os.MkdirAll(wrong, 0o700)
	db, _ := sql.Open("sqlite3", filepath.Join(wrong, "messages.db"))
	db.Exec("CREATE TABLE unrelated (x INTEGER)")
	db.Close()
	if err := ImportLegacy(wrong, privateDataDir(t), &fakeConsole{interactive: true, confirm: true}); err == nil {
		t.Fatal("imported a database without the expected schema")
	}
}

func TestPairCommandOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("LOCALAPPDATA", t.TempDir())
	var stderr bytes.Buffer
	for _, args := range [][]string{nil, {"bridge"}, {"setup"}, {"setup", "--pair-pi", "extra"}} {
		stderr.Reset()
		if code := run(args, &fakeConsole{}, &stderr); code != 2 || !strings.Contains(stderr.String(), "setup --pair-pi") {
			t.Errorf("run(%q) = %d, stderr %q", args, code, stderr.String())
		}
	}
	pub := base64.StdEncoding.EncodeToString(newPublicKey(t))
	if code := run([]string{"setup", "--pair-pi"}, &fakeConsole{interactive: true, confirm: true, pasted: pub}, &stderr); code != 0 {
		t.Fatalf("setup --pair-pi exit %d: %s", code, stderr.String())
	}
	home, _ := os.UserHomeDir()
	dir, _ := ResolveDataDir(runtime.GOOS, home, os.Getenv("LOCALAPPDATA"), "")
	if _, err := LoadApprover(dir); err != nil {
		t.Fatalf("pairing command did not enroll the approver: %v", err)
	}
}

func TestTokenLoadOnly(t *testing.T) {
	dir := privateDataDir(t)
	if _, err := LoadToken(dir); err == nil {
		t.Fatal("LoadToken succeeded without a token")
	}
	if _, err := os.Stat(filepath.Join(dir, "api-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("LoadToken created a token")
	}
	created, _ := LoadOrCreateToken(dir, rand.Reader)
	if loaded, err := LoadToken(dir); err != nil || !bytes.Equal(created, loaded) {
		t.Fatalf("LoadToken: %v", err)
	}
}

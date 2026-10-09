package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/term"
)

const (
	appName      = "whatsapp-mcp"
	tokenFile    = "api-token"
	approverFile = "approver.json"
)

var (
	errUnsafePath = errors.New("unsafe path: expected a private file or directory owned by the current user")
	errNoTerminal = errors.New("this step needs an interactive terminal")
	errCancelled  = errors.New("cancelled")
)

// TrustedConsole is the local terminal through which the user confirms trust
// decisions. It stops automation without a terminal; it does not stop a process
// running as the same user that drives a pseudo-terminal.
type TrustedConsole interface {
	IsInteractive() bool
	ReadPublicKey() (string, error)
	ConfirmFingerprint(fingerprint string) (bool, error)
	ConfirmImport(description string) (bool, error)
}

// ResolveDataDir returns the per-user directory holding the session, messages and credentials.
func ResolveDataDir(platform, home, localAppData, xdgDataHome string) (string, error) {
	if platform == "windows" {
		if !filepath.IsAbs(localAppData) {
			return "", errors.New("LOCALAPPDATA is not set to an absolute path")
		}
		return filepath.Join(localAppData, appName), nil
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("the home directory is not an absolute path")
	}
	if platform == "darwin" {
		return filepath.Join(home, "Library", "Application Support", appName), nil
	}
	if filepath.IsAbs(xdgDataHome) {
		return filepath.Join(xdgDataHome, appName), nil
	}
	return filepath.Join(home, ".local", "share", appName), nil
}

func defaultDataDir() (string, error) {
	home, _ := os.UserHomeDir()
	return ResolveDataDir(runtime.GOOS, home, os.Getenv("LOCALAPPDATA"), os.Getenv("XDG_DATA_HOME"))
}

// EnsurePrivateDir creates path, or tightens an existing directory, so only the current user can access it.
func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s", errUnsafePath, path)
	}
	return securePath(path, info)
}

// EnsurePrivateFile creates an empty file, or tightens an existing regular file, so only the current user can access it.
func EnsurePrivateFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		f.Close()
	} else if !errors.Is(err, fs.ErrExist) {
		return err
	}
	return checkPrivateFile(path)
}

func checkPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", errUnsafePath, path)
	}
	return securePath(path, info)
}

func readPrivateFile(path string, limit int64) ([]byte, error) {
	if err := checkPrivateFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("%s is larger than expected", path)
	}
	return data, err
}

// writePrivateFile writes data through a temporary file, so readers never see a
// partial file. Without replace it fails with fs.ErrExist instead of overwriting.
func writePrivateFile(path string, data []byte, replace bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	if err := errors.Join(writeErr, tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	if err := checkPrivateFile(tmp.Name()); err != nil {
		return err
	}
	if replace {
		return os.Rename(tmp.Name(), path)
	}
	return os.Link(tmp.Name(), path)
}

// LoadOrCreateToken returns the local API credential, creating it on first use.
func LoadOrCreateToken(dataDir string, random io.Reader) ([]byte, error) {
	path := filepath.Join(dataDir, tokenFile)
	data, err := readPrivateFile(path, 128)
	if errors.Is(err, fs.ErrNotExist) {
		token := make([]byte, 32)
		if _, err := io.ReadFull(random, token); err != nil {
			return nil, fmt.Errorf("generate API token: %w", err)
		}
		if err := writePrivateFile(path, []byte(hex.EncodeToString(token)), false); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		data, err = readPrivateFile(path, 128) // a concurrent setup may have created it first
	}
	if err != nil {
		return nil, err
	}
	return parseToken(path, data)
}

// LoadToken reads the existing API token without ever creating one.
func LoadToken(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, tokenFile)
	data, err := readPrivateFile(path, 128)
	if err != nil {
		return nil, err
	}
	return parseToken(path, data)
}

func parseToken(path string, data []byte) ([]byte, error) {
	token, err := hex.DecodeString(string(data))
	if err != nil || len(token) != 32 {
		return nil, fmt.Errorf("the API token in %s is corrupt: remove it and run setup again", path)
	}
	return token, nil
}

// LoadApprover returns the enrolled approver's public lease, if it is well formed.
func LoadApprover(dataDir string) (ApproverLease, error) {
	data, err := readPrivateFile(filepath.Join(dataDir, approverFile), 1024)
	if err != nil {
		return ApproverLease{}, fmt.Errorf("%w: %v", ErrNotPaired, err)
	}
	var lease ApproverLease
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lease); err != nil || decoder.More() {
		return ApproverLease{}, fmt.Errorf("%w: malformed approver lease", ErrNotPaired)
	}
	key, ok := decodeCanonicalB64(lease.PublicKeyB64)
	if !ok || len(key) != ed25519.PublicKeySize {
		return ApproverLease{}, fmt.Errorf("%w: approver lease is invalid", ErrNotPaired)
	}
	return lease, nil
}

// PairApprover trusts publicKey to approve sends after the user confirms its fingerprint.
// It replaces any earlier pairing.
func PairApprover(dataDir string, publicKey []byte, console TrustedConsole) error {
	if !console.IsInteractive() {
		return errNoTerminal
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return errors.New("an approver public key is 32 bytes")
	}
	confirmed, err := console.ConfirmFingerprint(approverID(publicKey))
	if err != nil {
		return err
	}
	if !confirmed {
		return errCancelled
	}
	lease, err := json.Marshal(ApproverLease{PublicKeyB64: base64.StdEncoding.EncodeToString(publicKey)})
	if err != nil {
		return err
	}
	return writePrivateFile(filepath.Join(dataDir, approverFile), lease, true)
}

// RevokeApprover removes trust in the current approver. It never grants trust.
func RevokeApprover(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, approverFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// RunPairingSetup reads the approver key pasted by the user and enrolls it after confirmation.
func RunPairingSetup(dataDir string, console TrustedConsole) error {
	if !console.IsInteractive() {
		return errNoTerminal
	}
	pasted, err := console.ReadPublicKey()
	if err != nil {
		return err
	}
	key, ok := decodeCanonicalB64(strings.TrimSpace(pasted))
	if !ok || len(key) != ed25519.PublicKeySize {
		return errors.New("that is not the public key shown by /whatsapp-pair")
	}
	if err := EnsurePrivateDir(dataDir); err != nil {
		return err
	}
	return PairApprover(dataDir, key, console)
}

var legacyTables = map[string][]string{
	"messages.db": {"chats", "messages"},
	"whatsapp.db": {"whatsmeow_device"},
}

// ImportLegacy copies the upstream bridge's databases into dataDir. It reads
// consistent SQLite snapshots, never overwrites existing data and leaves the
// originals untouched as the user's backup.
func ImportLegacy(source, dataDir string, console TrustedConsole) error {
	if !console.IsInteractive() {
		return errNoTerminal
	}
	var names []string
	for _, name := range []string{"messages.db", "whatsapp.db"} {
		info, err := os.Lstat(filepath.Join(source, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s", errUnsafePath, filepath.Join(source, name))
		}
		if _, err := os.Lstat(filepath.Join(dataDir, name)); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s already exists in %s: refusing to overwrite existing WhatsApp data", name, dataDir)
		}
		if err := checkLegacySchema(filepath.Join(source, name), legacyTables[name]); err != nil {
			return err
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return fmt.Errorf("no messages.db or whatsapp.db found in %s", source)
	}
	confirmed, err := console.ConfirmImport(fmt.Sprintf(
		"Import %s from %s into %s.\nStop the old bridge first: two bridges on one WhatsApp session conflict.\nThe original files are not modified and remain your backup.",
		strings.Join(names, " and "), source, dataDir))
	if err != nil {
		return err
	}
	if !confirmed {
		return errCancelled
	}
	if err := EnsurePrivateDir(dataDir); err != nil {
		return err
	}
	for _, name := range names {
		if err := snapshotSQLite(filepath.Join(source, name), filepath.Join(dataDir, name)); err != nil {
			return fmt.Errorf("import %s: %w", name, err)
		}
	}
	return nil
}

func openReadOnlySQLite(path string) (*sql.DB, error) {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed // Windows drive paths
	}
	return sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: slashed, RawQuery: "mode=ro"}).String())
}

func checkLegacySchema(path string, tables []string) error {
	db, err := openReadOnlySQLite(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("%s is not a healthy SQLite database", path)
	}
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return err
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		found = append(found, name)
	}
	for _, table := range tables {
		if !slices.Contains(found, table) {
			return fmt.Errorf("%s does not look like a whatsapp-mcp database (missing table %s)", path, table)
		}
	}
	return rows.Err()
}

func snapshotSQLite(source, target string) error {
	db, err := openReadOnlySQLite(source)
	if err != nil {
		return err
	}
	defer db.Close()
	tmp := filepath.Join(filepath.Dir(target), ".import-"+filepath.Base(target))
	os.Remove(tmp) // leftover of an interrupted import
	defer os.Remove(tmp)
	if _, err := db.Exec("VACUUM INTO ?", tmp); err != nil {
		return err
	}
	if err := checkPrivateFile(tmp); err != nil {
		return err
	}
	return os.Link(tmp, target)
}

type terminalConsole struct {
	in *bufio.Reader
}

func newTerminalConsole() *terminalConsole {
	return &terminalConsole{in: bufio.NewReaderSize(os.Stdin, 4096)}
}

func (c *terminalConsole) IsInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func (c *terminalConsole) ReadPublicKey() (string, error) {
	fmt.Print("Paste the public key shown by /whatsapp-pair in Pi: ")
	return c.readLine()
}

func (c *terminalConsole) ConfirmFingerprint(fingerprint string) (bool, error) {
	fmt.Printf("\nApprover fingerprint:\n  %s\n\nCheck that Pi shows the same fingerprint. That Pi session will be able to approve\noutgoing WhatsApp messages until Pi quits or reloads.\nType \"yes\" to trust it: ", fingerprint)
	answer, err := c.readLine()
	return answer == "yes", err
}

func (c *terminalConsole) ConfirmImport(description string) (bool, error) {
	fmt.Printf("%s\nType \"yes\" to import: ", description)
	answer, err := c.readLine()
	return answer == "yes", err
}

func (c *terminalConsole) readLine() (string, error) {
	line, err := c.in.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", errors.New("input line too long")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(string(line)), nil
}

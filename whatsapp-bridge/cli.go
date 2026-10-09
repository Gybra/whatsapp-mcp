package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const usageText = `Usage: whatsapp-mcp <command>

Set up and run:
  setup               Link this computer to WhatsApp and choose autostart (interactive)
  start | stop | restart
  status [--json]     Show whether the bridge runs and WhatsApp is linked
  logs [-n LINES]     Show the end of the bridge log (no message contents are logged)
  autostart enable|disable
                      Start the bridge when you log in, or stop doing so
  uninstall [--purge] Stop the bridge and remove autostart; --purge also deletes all WhatsApp data

Used by MCP clients and the Pi extension:
  mcp                 Serve the WhatsApp tools over stdin/stdout
  link [--json]       Link this computer as a WhatsApp device with a QR code
  pair-pi             Trust the public key read from stdin to approve outgoing messages
  bridge              Run the bridge in the foreground
  import PATH         Import the database of the original whatsapp-mcp bridge
  version
`

// CLI holds everything a command touches, so tests can replace the OS-facing parts.
type CLI struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Console        TrustedConsole
	Command        CommandRunner
	Executable     string
	Service        ServiceConfig
	DataDir        string
	Spawn          func(binary string, args ...string) error
	Status         func(ctx context.Context) (APIStatus, error)
	Shutdown       func(ctx context.Context) error
	Link           func(ctx context.Context, dataDir string, out func(LinkEvent), confirm func(code string) (bool, error)) error
	StartTimeout   time.Duration
	WaitStep       time.Duration
}

// CLIStatus is printed by `status --json` for the Pi extension.
type CLIStatus struct {
	Version    string `json:"version"`
	Running    bool   `json:"running"`
	State      string `json:"state"`
	Connected  bool   `json:"connected"`
	Linked     bool   `json:"linked"`
	ApproverID string `json:"approver_id"`
	Autostart  bool   `json:"autostart"`
	DataDir    string `json:"data_dir"`
	LogFile    string `json:"log_file"`
}

var errUsage = errors.New("usage")

func newCLI() *CLI {
	executable, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	home, _ := os.UserHomeDir()
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(configHome) {
		configHome = filepath.Join(home, ".config")
	}
	dataDir, _ := defaultDataDir()
	c := &CLI{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Console: newTerminalConsole(),
		Command: runCommand, Executable: executable, DataDir: dataDir, Spawn: spawnDetached,
		Service: ServiceConfig{Platform: runtime.GOOS, BinaryPath: executable, Home: home, ConfigHome: configHome, UID: strconv.Itoa(os.Getuid()), TempDir: os.TempDir()},
	}
	c.Status = func(ctx context.Context) (APIStatus, error) {
		client, err := c.client()
		if err != nil {
			return APIStatus{}, err
		}
		return client.Status(ctx)
	}
	c.Shutdown = func(ctx context.Context) error {
		client, err := c.client()
		if err != nil {
			return err
		}
		return client.Shutdown(ctx)
	}
	c.Link = linkDevice
	return c
}

func (c *CLI) client() (*BridgeClient, error) {
	token, err := LoadToken(c.DataDir)
	if err != nil {
		return nil, ErrBridgeUnreachable // never set up, so nothing can be running
	}
	return NewBridgeClient(token), nil
}

// Run executes one command and returns the process exit code.
func (c *CLI) Run(ctx context.Context, args []string) int {
	if c.DataDir == "" {
		fmt.Fprintln(c.Stderr, "Error: cannot determine the data directory (is HOME or LOCALAPPDATA set?)")
		return 1
	}
	err := c.dispatch(ctx, args)
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprint(c.Stderr, usageText)
		return 2
	case err != nil:
		fmt.Fprintln(c.Stderr, "Error:", err)
		return 1
	}
	return 0
}

func (c *CLI) dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	command, rest := args[0], args[1:]
	flag := func(name string) (bool, error) {
		switch {
		case len(rest) == 0:
			return false, nil
		case len(rest) == 1 && rest[0] == name:
			return true, nil
		}
		return false, errUsage
	}
	noArgs := func(run func() error) error {
		if len(rest) != 0 {
			return errUsage
		}
		return run()
	}
	switch command {
	case "version":
		return noArgs(func() error { fmt.Fprintln(c.Stdout, version); return nil })
	case "mcp":
		return noArgs(func() error { return c.mcp(ctx) })
	case "bridge":
		return noArgs(func() error { return runBridge(ctx, c.DataDir, c.Stderr, nil) })
	case "setup":
		return noArgs(func() error { return c.setup(ctx) })
	case "pair-pi":
		return noArgs(func() error { return c.pairPi() })
	case "start":
		return noArgs(func() error { return c.start(ctx) })
	case "stop":
		return noArgs(func() error { return c.stop(ctx) })
	case "restart":
		return noArgs(func() error {
			if err := c.stop(ctx); err != nil {
				return err
			}
			return c.start(ctx)
		})
	case "status":
		asJSON, err := flag("--json")
		if err != nil {
			return err
		}
		return c.printStatus(ctx, asJSON)
	case "link":
		asJSON, err := flag("--json")
		if err != nil {
			return err
		}
		return c.link(ctx, asJSON)
	case "logs":
		lines := 100
		if len(rest) == 2 && rest[0] == "-n" {
			n, err := strconv.Atoi(rest[1])
			if err != nil || n < 1 {
				return errUsage
			}
			lines = n
		} else if len(rest) != 0 {
			return errUsage
		}
		return c.logs(lines)
	case "autostart":
		if len(rest) != 1 || (rest[0] != "enable" && rest[0] != "disable") {
			return errUsage
		}
		return c.autostart(ctx, rest[0] == "enable")
	case "uninstall":
		purge, err := flag("--purge")
		if err != nil {
			return err
		}
		return c.uninstall(ctx, purge)
	case "import":
		if len(rest) != 1 {
			return errUsage
		}
		return c.importLegacy(ctx, rest[0])
	}
	return errUsage
}

func (c *CLI) mcp(ctx context.Context) error {
	token, err := LoadToken(c.DataDir)
	if err != nil {
		return errors.New("the WhatsApp bridge is not set up: run whatsapp-mcp setup first")
	}
	return RunMCP(ctx, NewBridgeClient(token))
}

func (c *CLI) running(ctx context.Context) (APIStatus, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, err := c.Status(ctx)
	return status, err == nil
}

func (c *CLI) start(ctx context.Context) error {
	if _, ok := c.running(ctx); ok {
		fmt.Fprintln(c.Stdout, "The bridge is already running.")
		return nil
	}
	if err := EnsurePrivateDir(c.DataDir); err != nil {
		return err
	}
	var err error
	if AutostartEnabled(ctx, c.Service, c.Command) {
		err = StartService(ctx, c.Service, c.Command)
	} else {
		err = c.Spawn(c.Executable, "bridge")
	}
	if err != nil {
		return err
	}
	if !c.waitFor(ctx, true) {
		return errors.New("the bridge did not start; see whatsapp-mcp logs")
	}
	fmt.Fprintln(c.Stdout, "The bridge is running.")
	return nil
}

func (c *CLI) stop(ctx context.Context) error {
	if _, ok := c.running(ctx); !ok {
		fmt.Fprintln(c.Stdout, "The bridge is not running.")
		return nil
	}
	if err := c.Shutdown(ctx); err != nil {
		return err
	}
	if !c.waitFor(ctx, false) {
		return errors.New("the bridge did not stop; see whatsapp-mcp logs")
	}
	fmt.Fprintln(c.Stdout, "The bridge stopped.")
	return nil
}

// waitFor polls until the bridge is (or is no longer) reachable.
func (c *CLI) waitFor(ctx context.Context, up bool) bool {
	timeout, step := c.StartTimeout, c.WaitStep
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	if step == 0 {
		step = 200 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		if _, ok := c.running(ctx); ok == up {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(step)
	}
}

func (c *CLI) status(ctx context.Context) CLIStatus {
	s := CLIStatus{
		Version: version, DataDir: c.DataDir, LogFile: filepath.Join(c.DataDir, logFileName),
		Autostart: AutostartEnabled(ctx, c.Service, c.Command), State: "stopped",
	}
	if api, ok := c.running(ctx); ok {
		s.Running, s.State, s.Connected, s.ApproverID = true, api.State, api.Connected, api.ApproverID
		s.Linked = api.State != "unpaired"
	} else {
		s.Linked = linkedOnDisk(c.DataDir)
	}
	return s
}

func (c *CLI) printStatus(ctx context.Context, asJSON bool) error {
	s := c.status(ctx)
	if asJSON {
		return json.NewEncoder(c.Stdout).Encode(s)
	}
	bridge := "not running (start it with whatsapp-mcp start)"
	if s.Running {
		bridge = "running, " + s.State
	}
	linked := "not linked (run whatsapp-mcp setup)"
	if s.Linked {
		linked = "linked"
	}
	autostart := "off"
	if s.Autostart {
		autostart = "on"
	}
	fmt.Fprintf(c.Stdout, "whatsapp-mcp %s\nBridge:    %s\nWhatsApp:  %s\nAutostart: %s\nData:      %s\nLog:       %s\n",
		s.Version, bridge, linked, autostart, s.DataDir, s.LogFile)
	return nil
}

// linkedOnDisk reports whether the session database holds a linked device.
func linkedOnDisk(dataDir string) bool {
	path := filepath.Join(dataDir, "whatsapp.db")
	if _, err := os.Stat(path); err != nil {
		return false
	}
	db, err := openReadOnlySQLite(path)
	if err != nil {
		return false
	}
	defer db.Close()
	var devices int
	return db.QueryRow("SELECT COUNT(*) FROM whatsmeow_device").Scan(&devices) == nil && devices > 0
}

func (c *CLI) logs(lines int) error {
	data, err := readPrivateFile(filepath.Join(c.DataDir, logFileName), maxLogBytes*2)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(c.Stdout, "No log yet.")
		return nil
	}
	if err != nil {
		return err
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	fmt.Fprintln(c.Stdout, strings.Join(all[max(0, len(all)-lines):], "\n"))
	return nil
}

func (c *CLI) autostart(ctx context.Context, enable bool) error {
	// Either way the service manager takes over, so stop a bridge started by hand first.
	if _, ok := c.running(ctx); ok {
		if err := c.stop(ctx); err != nil {
			return err
		}
	}
	if !enable {
		if err := DisableAutostart(ctx, c.Service, c.Command); err != nil {
			return err
		}
		fmt.Fprintln(c.Stdout, "Autostart is off and the bridge is stopped. Run whatsapp-mcp start to run it until you log out.")
		return nil
	}
	if err := EnsurePrivateDir(c.DataDir); err != nil {
		return err
	}
	if err := EnableAutostart(ctx, c.Service, c.Command); err != nil {
		return err
	}
	fmt.Fprintln(c.Stdout, "Autostart is on: the bridge starts when you log in, and runs now.")
	return nil
}

func (c *CLI) pairPi() error {
	id, err := pairPiFromInput(c.DataDir, c.Stdin)
	if err != nil {
		return err
	}
	return json.NewEncoder(c.Stdout).Encode(map[string]string{"approver_id": id})
}

func (c *CLI) link(ctx context.Context, asJSON bool) error {
	if _, ok := c.running(ctx); ok {
		return errors.New("the bridge is running; stop it first with whatsapp-mcp stop")
	}
	if err := EnsurePrivateDir(c.DataDir); err != nil {
		return err
	}
	if asJSON {
		encoder := json.NewEncoder(c.Stdout)
		answers := bufio.NewScanner(c.Stdin)
		return c.Link(ctx, c.DataDir, func(e LinkEvent) { encoder.Encode(e) }, func(string) (bool, error) {
			if !answers.Scan() {
				return false, answers.Err()
			}
			return strings.TrimSpace(answers.Text()) == "yes", nil
		})
	}
	return c.Link(ctx, c.DataDir, c.printLinkEvent, func(code string) (bool, error) {
		return c.Console.Confirm("Does your phone show the code " + code + "?")
	})
}

func (c *CLI) printLinkEvent(e LinkEvent) {
	switch e.Event {
	case "qr":
		fmt.Fprintln(c.Stdout, "\nOn your phone open WhatsApp > Settings > Linked devices > Link a device, and scan:")
		fmt.Fprintln(c.Stdout, strings.Join(renderQR(e.Modules), "\n"))
		fmt.Fprintf(c.Stdout, "A new code appears in %d seconds if this one is not scanned.\n", e.ExpiresIn)
	default:
		if e.Message != "" {
			fmt.Fprintln(c.Stdout, e.Message)
		}
	}
}

// linkDevice links WhatsApp while holding the bridge's port, so no bridge starts meanwhile.
func linkDevice(ctx context.Context, dataDir string, out func(LinkEvent), confirm func(string) (bool, error)) error {
	listener, err := ListenLoopback()
	if err != nil {
		return errors.New("the bridge is running; stop it first with whatsapp-mcp stop")
	}
	defer listener.Close()
	logFile, err := openLogFile(dataDir)
	if err != nil {
		return err
	}
	defer logFile.Close()
	store, err := OpenMessageStore(dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	runtime, err := NewWhatsAppRuntime(RuntimeConfig{DataDir: dataDir, Logger: slog.New(slog.NewTextHandler(logFile, nil))}, store)
	if err != nil {
		return err
	}
	defer runtime.Close()
	return runtime.Link(ctx, out, confirm)
}

func (c *CLI) setup(ctx context.Context) error {
	if !c.Console.IsInteractive() {
		return errors.New("setup needs an interactive terminal (in Pi, use /whatsapp-setup)")
	}
	if err := EnsurePrivateDir(c.DataDir); err != nil {
		return err
	}
	if _, err := LoadOrCreateToken(c.DataDir, rand.Reader); err != nil {
		return err
	}
	status := c.status(ctx)
	if status.Linked {
		fmt.Fprintln(c.Stdout, "This computer is already linked to WhatsApp.")
	} else {
		ok, err := c.Console.Confirm("Link this computer to your WhatsApp account now, as a linked device?")
		if err != nil {
			return err
		}
		if ok {
			if status.Running { // an unlinked bridge holds the session database
				if err := c.stop(ctx); err != nil {
					return err
				}
			}
			if err := c.link(ctx, false); err != nil {
				return err
			}
			status.Linked = true
		}
	}
	if status.Linked {
		if !status.Autostart {
			ok, err := c.Console.Confirm("Start the bridge automatically when you log in? It keeps running in the background until you stop it.")
			if err != nil {
				return err
			}
			if ok {
				if err := c.autostart(ctx, true); err != nil {
					return err
				}
			}
		}
		if err := c.start(ctx); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(c.Stdout, "Not linked yet: run whatsapp-mcp setup again when you are ready.")
	}
	fmt.Fprintf(c.Stdout, "\nTo use it from Pi: pi install npm:pi-whatsapp, then run /whatsapp-setup in Pi.\nOther MCP clients: run %q with the argument \"mcp\" over stdio.\n", c.Executable)
	return nil
}

func (c *CLI) uninstall(ctx context.Context, purge bool) error {
	if purge && !c.Console.IsInteractive() {
		return errors.New("--purge needs an interactive terminal to confirm")
	}
	if err := c.stop(ctx); err != nil {
		return err
	}
	if err := DisableAutostart(ctx, c.Service, c.Command); err != nil {
		return err
	}
	fmt.Fprintln(c.Stdout, "Autostart removed.")
	if purge {
		ok, err := c.Console.Confirm("Delete " + c.DataDir + ", including the linked WhatsApp session and the message history?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(c.Stdout, "Data kept.")
			return nil
		}
		if err := os.RemoveAll(c.DataDir); err != nil {
			return err
		}
		fmt.Fprintln(c.Stdout, "Data deleted. Also remove this computer under Linked devices in WhatsApp on your phone.")
	} else {
		fmt.Fprintln(c.Stdout, "Data kept in "+c.DataDir+" (delete it with whatsapp-mcp uninstall --purge).")
	}
	fmt.Fprintln(c.Stdout, "The executable "+c.Executable+" was not removed.")
	return nil
}

func (c *CLI) importLegacy(ctx context.Context, source string) error {
	if _, ok := c.running(ctx); ok {
		return errors.New("stop the bridge first with whatsapp-mcp stop")
	}
	if err := ImportLegacy(source, c.DataDir, c.Console); err != nil {
		return err
	}
	fmt.Fprintln(c.Stdout, "Imported.")
	return nil
}

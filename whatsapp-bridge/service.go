package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

// Autostart runs `whatsapp-mcp bridge` at login through the per-user service
// manager: a LaunchAgent on macOS, a systemd user unit on Linux and a Task
// Scheduler task on Windows. It never needs administrator rights. The bridge
// exits with status 0 on `whatsapp-mcp stop`, which these definitions treat as
// "stay stopped"; they restart it only after a crash, with throttling.
const (
	launchLabel   = "io.github.gybra.whatsapp-mcp"
	systemdUnit   = "whatsapp-mcp.service"
	windowsTask   = "whatsapp-mcp"
	serviceMarker = "Managed by whatsapp-mcp"
)

type ServiceConfig struct {
	Platform   string // runtime.GOOS value
	BinaryPath string // absolute path of the whatsapp-mcp executable
	Home       string
	ConfigHome string // XDG config directory on Linux
	UID        string // numeric user ID on macOS
	TempDir    string
}

// CommandRunner runs a service-manager command. Tests replace it.
type CommandRunner func(ctx context.Context, name string, args ...string) error

func runCommand(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

var windowsAbs = regexp.MustCompile(`^[A-Za-z]:\\`)

// GenerateService returns the service definition for cfg.Platform.
func GenerateService(cfg ServiceConfig) ([]byte, error) {
	absolute := filepath.IsAbs(cfg.BinaryPath)
	if cfg.Platform == "windows" {
		absolute = windowsAbs.MatchString(cfg.BinaryPath)
	}
	if !absolute {
		return nil, errors.New("the whatsapp-mcp path must be absolute")
	}
	switch cfg.Platform {
	case "darwin":
		return []byte(fmt.Sprintf(launchAgentTemplate, serviceMarker, launchLabel, xmlEscape(cfg.BinaryPath))), nil
	case "linux":
		return []byte(fmt.Sprintf(systemdTemplate, serviceMarker, systemdQuote(cfg.BinaryPath))), nil
	case "windows":
		return []byte(fmt.Sprintf(windowsTaskTemplate, serviceMarker, xmlEscape(cfg.BinaryPath))), nil
	}
	return nil, fmt.Errorf("autostart is not supported on %s; run whatsapp-mcp start after login instead", cfg.Platform)
}

// servicePath is where the definition lives; on Windows the task XML is only a temporary import file.
func servicePath(cfg ServiceConfig) string {
	switch cfg.Platform {
	case "darwin":
		return filepath.Join(cfg.Home, "Library", "LaunchAgents", launchLabel+".plist")
	case "linux":
		return filepath.Join(cfg.ConfigHome, "systemd", "user", systemdUnit)
	}
	return filepath.Join(cfg.TempDir, "whatsapp-mcp-task.xml")
}

// EnableAutostart installs and starts the login service. It refuses to replace a definition it did not create.
func EnableAutostart(ctx context.Context, cfg ServiceConfig, run CommandRunner) error {
	definition, err := GenerateService(cfg)
	if err != nil {
		return err
	}
	path := servicePath(cfg)
	if err := checkOwnedService(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if cfg.Platform == "windows" {
		definition = utf16LE(definition) // schtasks /XML expects the encoding the file declares
	}
	if err := writeAtomically(path, definition); err != nil {
		return err
	}
	switch cfg.Platform {
	case "darwin":
		run(ctx, "launchctl", "bootout", "gui/"+cfg.UID+"/"+launchLabel) // reload an older definition; fails harmlessly when not loaded
		return run(ctx, "launchctl", "bootstrap", "gui/"+cfg.UID, path)
	case "linux":
		if err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			os.Remove(path)
			return fmt.Errorf("systemd user services are not available (%v); run whatsapp-mcp start after login instead", err)
		}
		return run(ctx, "systemctl", "--user", "enable", "--now", systemdUnit)
	default:
		defer os.Remove(path)
		return run(ctx, "schtasks", "/Create", "/TN", windowsTask, "/XML", path, "/F")
	}
}

// DisableAutostart stops the login service and removes its definition. It succeeds if none is installed.
func DisableAutostart(ctx context.Context, cfg ServiceConfig, run CommandRunner) error {
	path := servicePath(cfg)
	switch cfg.Platform {
	case "darwin", "linux":
		if err := checkOwnedService(path); err != nil {
			return err
		}
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if cfg.Platform == "darwin" {
			run(ctx, "launchctl", "bootout", "gui/"+cfg.UID+"/"+launchLabel)
		} else {
			run(ctx, "systemctl", "--user", "disable", "--now", systemdUnit)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if cfg.Platform == "linux" {
			run(ctx, "systemctl", "--user", "daemon-reload")
		}
		return nil
	case "windows":
		if !AutostartEnabled(ctx, cfg, run) {
			return nil
		}
		return run(ctx, "schtasks", "/Delete", "/TN", windowsTask, "/F")
	}
	return nil
}

func AutostartEnabled(ctx context.Context, cfg ServiceConfig, run CommandRunner) bool {
	if cfg.Platform == "windows" {
		return run(ctx, "schtasks", "/Query", "/TN", windowsTask) == nil
	}
	data, err := os.ReadFile(servicePath(cfg))
	return err == nil && bytes.Contains(data, []byte(serviceMarker))
}

// StartService asks the service manager to start the bridge now.
func StartService(ctx context.Context, cfg ServiceConfig, run CommandRunner) error {
	switch cfg.Platform {
	case "darwin":
		return run(ctx, "launchctl", "kickstart", "gui/"+cfg.UID+"/"+launchLabel)
	case "linux":
		return run(ctx, "systemctl", "--user", "start", systemdUnit)
	}
	return run(ctx, "schtasks", "/Run", "/TN", windowsTask)
}

func checkOwnedService(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(data, []byte(serviceMarker)) {
		return fmt.Errorf("%s was not created by whatsapp-mcp; left unchanged", path)
	}
	return nil
}

func writeAtomically(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".whatsapp-mcp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func utf16LE(text []byte) []byte {
	units := utf16.Encode([]rune(string(text)))
	out := []byte{0xFF, 0xFE}
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// systemdQuote quotes one ExecStart argument: systemd expands %specifiers and $variables.
func systemdQuote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s)
	return `"` + s + `"`
}

const launchAgentTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- %s. Remove with: whatsapp-mcp autostart disable -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>bridge</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ExitTimeOut</key>
	<integer>15</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>Umask</key>
	<integer>63</integer>
	<key>StandardOutPath</key>
	<string>/dev/null</string>
	<key>StandardErrorPath</key>
	<string>/dev/null</string>
</dict>
</plist>
`

const systemdTemplate = `# %s. Remove with: whatsapp-mcp autostart disable
[Unit]
Description=WhatsApp MCP bridge
After=network-online.target
StartLimitIntervalSec=600
StartLimitBurst=5

[Service]
ExecStart=%s bridge
Restart=on-failure
RestartSec=30
TimeoutStopSec=15
UMask=0077
StandardOutput=null
StandardError=null

[Install]
WantedBy=default.target
`

const windowsTaskTemplate = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>%s. Remove with: whatsapp-mcp autostart disable</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>5</Count>
    </RestartOnFailure>
    <Hidden>true</Hidden>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>bridge</Arguments>
    </Exec>
  </Actions>
</Task>
`

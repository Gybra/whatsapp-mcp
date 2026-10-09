package main

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type commandLog struct {
	calls [][]string
	fail  map[string]error // keyed by the command's first two words
}

func (c *commandLog) run(_ context.Context, name string, args ...string) error {
	call := append([]string{name}, args...)
	c.calls = append(c.calls, call)
	key := strings.Join(call[:min(2, len(call))], " ")
	return c.fail[key]
}

func serviceConfig(t *testing.T, platform string) ServiceConfig {
	t.Helper()
	home := t.TempDir()
	return ServiceConfig{Platform: platform, BinaryPath: filepath.Join(home, `Apps & "Tools" 100%`, "whatsapp-mcp$1"), Home: home, ConfigHome: filepath.Join(home, ".config"), UID: "501", TempDir: t.TempDir()}
}

func TestServiceLaunchAgent(t *testing.T) {
	cfg := serviceConfig(t, "darwin")
	plist, err := GenerateService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		XMLName xml.Name `xml:"plist"`
	}
	if err := xml.Unmarshal(plist, &doc); err != nil {
		t.Fatalf("plist is not valid XML: %v\n%s", err, plist)
	}
	text := string(plist)
	for _, want := range []string{
		"<string>io.github.gybra.whatsapp-mcp</string>",
		"<string>" + xmlEscape(cfg.BinaryPath) + "</string>", "<string>bridge</string>",
		"<key>RunAtLoad</key>\n\t<true/>", "<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ThrottleInterval</key>\n\t<integer>30</integer>", "<key>Umask</key>\n\t<integer>63</integer>",
		"<string>/dev/null</string>", serviceMarker,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plist lacks %q:\n%s", want, text)
		}
	}
	runner := &commandLog{}
	if err := EnableAutostart(context.Background(), cfg, runner.run); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Home, "Library", "LaunchAgents", "io.github.gybra.whatsapp-mcp.plist")
	if data, err := os.ReadFile(path); err != nil || string(data) != text {
		t.Fatalf("plist not written to %s: %v", path, err)
	}
	if lint, err := exec.LookPath("plutil"); err == nil {
		if out, err := exec.Command(lint, "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil rejects the LaunchAgent: %s", out)
		}
	}
	if got := strings.Join(runner.calls[len(runner.calls)-1], " "); got != "launchctl bootstrap gui/501 "+path {
		t.Fatalf("enable ran %q", got)
	}
	if !AutostartEnabled(context.Background(), cfg, runner.run) {
		t.Fatal("autostart not reported as enabled")
	}
	runner.calls = nil
	if err := StartService(context.Background(), cfg, runner.run); err != nil || strings.Join(runner.calls[0], " ") != "launchctl kickstart gui/501/io.github.gybra.whatsapp-mcp" {
		t.Fatalf("start: %v %v", err, runner.calls)
	}
	runner.calls = nil
	if err := DisableAutostart(context.Background(), cfg, runner.run); err != nil {
		t.Fatal(err)
	}
	if strings.Join(runner.calls[0], " ") != "launchctl bootout gui/501/io.github.gybra.whatsapp-mcp" {
		t.Fatalf("disable ran %v", runner.calls)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("plist not removed")
	}
}

func TestServiceSystemdUser(t *testing.T) {
	cfg := serviceConfig(t, "linux")
	unit, err := GenerateService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(unit)
	// Only Windows test hosts produce backslashes in the path; systemd needs them doubled.
	sep := strings.ReplaceAll(string(filepath.Separator), `\`, `\\`)
	wantExec := `ExecStart="` + strings.ReplaceAll(cfg.Home, `\`, `\\`) + sep + `Apps & \"Tools\" 100%%` + sep + `whatsapp-mcp$$1" bridge`
	for _, want := range []string{serviceMarker, wantExec, "Restart=on-failure", "RestartSec=30", "StartLimitBurst=5", "UMask=0077", "WantedBy=default.target"} {
		if !strings.Contains(text, want) {
			t.Errorf("unit lacks %q:\n%s", want, text)
		}
	}
	runner := &commandLog{}
	if err := EnableAutostart(context.Background(), cfg, runner.run); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.ConfigHome, "systemd", "user", "whatsapp-mcp.service")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, c := range runner.calls {
		got = append(got, strings.Join(c, " "))
	}
	if strings.Join(got, "; ") != "systemctl --user daemon-reload; systemctl --user enable --now whatsapp-mcp.service" {
		t.Fatalf("enable ran %v", got)
	}
	for _, c := range runner.calls {
		if strings.Contains(strings.Join(c, " "), "linger") || strings.Contains(strings.Join(c, " "), "sudo") {
			t.Fatal("must not enable lingering or use sudo")
		}
	}

	runner = &commandLog{fail: map[string]error{"systemctl --user": errors.New("Failed to connect to bus")}}
	if err := EnableAutostart(context.Background(), cfg, runner.run); err == nil || !strings.Contains(err.Error(), "whatsapp-mcp start") {
		t.Fatalf("missing systemd must point to manual start: %v", err)
	}
}

func TestServiceWindowsTask(t *testing.T) {
	cfg := serviceConfig(t, "windows")
	cfg.BinaryPath = `C:\Users\Ada Lovelace\AppData\Local\pi-whatsapp\whatsapp-mcp & co.exe`
	task, err := GenerateService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		XMLName xml.Name `xml:"Task"`
		Command string   `xml:"Actions>Exec>Command"`
		Args    string   `xml:"Actions>Exec>Arguments"`
	}
	utf8Task := strings.Replace(string(task), `encoding="UTF-16"`, `encoding="UTF-8"`, 1) // GenerateService returns text; the import file is UTF-16
	if err := xml.Unmarshal([]byte(utf8Task), &doc); err != nil || doc.Command != cfg.BinaryPath || doc.Args != "bridge" {
		t.Fatalf("task XML: %+v %v\n%s", doc, err, task)
	}
	text := string(task)
	for _, want := range []string{serviceMarker, "<LogonTrigger>", "<RunLevel>LeastPrivilege</RunLevel>", "<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "<RestartOnFailure>", "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>"} {
		if !strings.Contains(text, want) {
			t.Errorf("task lacks %q", want)
		}
	}
	if strings.Contains(text, "HighestAvailable") || strings.Contains(text, "<Password>") {
		t.Fatal("task must not request elevation or store a password")
	}
	var imported []byte
	runner := &commandLog{}
	capture := func(ctx context.Context, name string, args ...string) error {
		if len(args) > 4 && args[0] == "/Create" {
			imported, _ = os.ReadFile(args[4])
		}
		return runner.run(ctx, name, args...)
	}
	if err := EnableAutostart(context.Background(), cfg, capture); err != nil {
		t.Fatal(err)
	}
	if c := runner.calls[0]; c[0] != "schtasks" || c[1] != "/Create" || c[3] != "whatsapp-mcp" || c[4] != "/XML" || c[6] != "/F" {
		t.Fatalf("enable ran %v", c)
	}
	if len(imported) < 4 || imported[0] != 0xFF || imported[1] != 0xFE || imported[2] != '<' || imported[3] != 0 {
		t.Fatalf("task file is not UTF-16LE with a BOM: % x", imported[:min(8, len(imported))])
	}
	if _, err := os.Stat(servicePath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary task file left behind")
	}
	runner.calls = nil
	DisableAutostart(context.Background(), cfg, runner.run)
	if strings.Join(runner.calls[len(runner.calls)-1], " ") != "schtasks /Delete /TN whatsapp-mcp /F" {
		t.Fatalf("disable ran %v", runner.calls)
	}
}

func TestServiceRefusesForeignFile(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		cfg := serviceConfig(t, platform)
		path := servicePath(cfg)
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, []byte("someone else's service"), 0o600)
		runner := &commandLog{}
		if err := EnableAutostart(context.Background(), cfg, runner.run); err == nil {
			t.Errorf("%s: overwrote a service file it did not create", platform)
		}
		if err := DisableAutostart(context.Background(), cfg, runner.run); err == nil {
			t.Errorf("%s: removed a service file it did not create", platform)
		}
		if data, _ := os.ReadFile(path); string(data) != "someone else's service" || len(runner.calls) != 0 {
			t.Errorf("%s: foreign file touched", platform)
		}
	}
}

func TestServiceIdempotent(t *testing.T) {
	cfg := serviceConfig(t, "darwin")
	runner := &commandLog{}
	for range 2 {
		if err := EnableAutostart(context.Background(), cfg, runner.run); err != nil {
			t.Fatal(err)
		}
	}
	runner.fail = map[string]error{"launchctl bootout": errors.New("No such process")}
	for range 2 {
		if err := DisableAutostart(context.Background(), cfg, runner.run); err != nil {
			t.Fatalf("disabling twice must succeed: %v", err)
		}
	}
	if AutostartEnabled(context.Background(), cfg, runner.run) {
		t.Fatal("still enabled")
	}
	if _, err := GenerateService(ServiceConfig{Platform: "plan9", BinaryPath: "/x"}); err == nil {
		t.Fatal("unsupported platform accepted")
	}
	if _, err := GenerateService(ServiceConfig{Platform: "linux", BinaryPath: "relative/whatsapp-mcp"}); err == nil {
		t.Fatal("relative binary path accepted")
	}
}

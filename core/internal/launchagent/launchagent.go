// Package launchagent installs the per-user launchd job that keeps the daemon
// (and therefore the host port listeners) running after the GUI quits.
package launchagent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Label is the launchd job label.
const Label = "com.misswell.orbsleep.daemon"

// Path returns the plist location for the current user.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
}

// Installed reports whether the plist exists.
func Installed() bool {
	p, err := Path()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Plist renders the launchd property list.
func Plist(binary, dataDir string) []byte {
	logDir := filepath.Join(dataDir, "logs")
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>--data-dir</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Background</string>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>Umask</key>
	<integer>63</integer>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>%s</string>
	</dict>
	<key>LimitLoadToSessionType</key>
	<string>Aqua</string>
	<key>WorkingDirectory</key>
	<string>%s</string>
</dict>
</plist>
`, Label, binary, dataDir, filepath.Join(logDir, "launchd.out.log"), filepath.Join(logDir, "launchd.err.log"),
		homeDir(), dataDir))
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// Install writes the plist and bootstraps the job.
func Install(binary, dataDir string) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o700); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Join(dataDir, "logs"), 0o700)
	// bootout first so changes to the plist take effect.
	_ = bootout(path)
	if err := os.WriteFile(path, Plist(binary, dataDir), 0o644); err != nil {
		return err
	}
	if err := bootstrap(path); err != nil {
		return err
	}
	return nil
}

// Uninstall unloads the job and removes the plist.
func Uninstall() error {
	path, err := Path()
	if err != nil {
		return err
	}
	_ = bootout(path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Restart asks launchd to restart the job.
func Restart() error {
	uid := os.Getuid()
	return run("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, Label))
}

func bootstrap(path string) error {
	uid := os.Getuid()
	if err := run("launchctl", "bootstrap", fmt.Sprintf("gui/%d", uid), path); err != nil {
		// Older macOS fallback.
		if err2 := run("launchctl", "load", "-w", path); err2 != nil {
			return fmt.Errorf("launchctl bootstrap 失败: %v; load 也失败: %v", err, err2)
		}
	}
	return nil
}

func bootout(path string) error {
	uid := os.Getuid()
	if err := run("launchctl", "bootout", fmt.Sprintf("gui/%d", uid), path); err != nil {
		return run("launchctl", "unload", "-w", path)
	}
	return nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// WritePlistOnly writes the plist without loading it (used by uninstall tools).
func WritePlistOnly(binary, dataDir string) (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, Plist(binary, dataDir), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// CurrentUser returns the current username (used in diagnostics).
func CurrentUser() string {
	u, err := user.Current()
	if err != nil {
		return strconv.Itoa(os.Getuid())
	}
	return u.Username
}

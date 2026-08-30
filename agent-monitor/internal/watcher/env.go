package watcher

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	optionName     = "@agent_watcher_socket"
	listenSuffix   = ".agent-watcher.sock"
	dialTimeout    = 500 * time.Millisecond
	defaultPython  = "python3"
)

// Env abstracts tmux / socket / daemon operations so tests can inject fakes.
type Env struct {
	TmuxAvailable func() bool
	OptionSocket  func() string
	CanonicalSock func() string
	SetOption     func(path string) error
	Dial          func(path string) (net.Conn, error)
	LookupPane    func(session string, window int) (pid int, cwd string, err error)
	StartDaemon   func(python, script, listen string) (*exec.Cmd, error)
	LookPath      func(name string) (string, error)
}

// DefaultEnv talks to the local tmux server and filesystem.
func DefaultEnv() Env {
	return Env{
		TmuxAvailable: tmuxAvailable,
		OptionSocket:  tmuxOptionSocket,
		CanonicalSock: tmuxCanonicalSocket,
		SetOption:     tmuxSetOption,
		Dial:          dialUnix,
		LookupPane:    tmuxLookupPane,
		StartDaemon:   startDaemon,
		LookPath:      exec.LookPath,
	}
}

func tmuxAvailable() bool {
	if _, err := exec.LookPath("tmux"); err != nil {
		return false
	}
	cmd := exec.Command("tmux", "display-message", "-p", "#{socket_path}")
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func tmuxOutput(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func tmuxOptionSocket() string {
	s, err := tmuxOutput("show-option", "-gqv", optionName)
	if err != nil {
		return ""
	}
	return s
}

func tmuxCanonicalSocket() string {
	base, err := tmuxOutput("display-message", "-p", "#{socket_path}")
	if err != nil || base == "" {
		return ""
	}
	return base + listenSuffix
}

func tmuxSetOption(path string) error {
	_, err := tmuxOutput("set-option", "-g", optionName, path)
	return err
}

func tmuxLookupPane(session string, window int) (int, string, error) {
	target := session + ":" + strconv.Itoa(window)
	out, err := tmuxOutput("display-message", "-p", "-t", target, "#{pane_pid}\t#{pane_current_path}")
	if err != nil {
		return 0, "", err
	}
	pid := 0
	cwd := ""
	if tab := strings.IndexByte(out, '\t'); tab >= 0 {
		pid = atoi(out[:tab])
		cwd = out[tab+1:]
	} else {
		pid = atoi(out)
	}
	return pid, cwd, nil
}

func dialUnix(path string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	return d.Dial("unix", path)
}

func startDaemon(python, script, listen string) (*exec.Cmd, error) {
	cmd := exec.Command(python, "-u", script, "--listen", listen)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// SocketLive reports whether path is a connectable Unix socket.
func SocketLive(dial func(string) (net.Conn, error), path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSocket == 0 {
		return false
	}
	conn, err := dial(path)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ResolveScript finds agent_watcher.py next to the installed plugin or in vendor/.
func ResolveScript(pluginRoot, explicit string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
	}
	wd, _ := os.Getwd()
	candidates := []string{
		filepath.Join(pluginRoot, "watcher", "agent_watcher.py"),
		filepath.Join(pluginRoot, "third_party", "agent-watcher", "src", "agent_watcher.py"),
		filepath.Join(wd, "third_party", "agent-watcher", "src", "agent_watcher.py"),
		filepath.Join(wd, "watcher", "agent_watcher.py"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// PluginRootFromExe returns the plugin install root (parent of bin/).
func PluginRootFromExe(exe string) string {
	dir := filepath.Dir(exe)
	if filepath.Base(dir) == "bin" {
		return filepath.Dir(dir)
	}
	return dir
}

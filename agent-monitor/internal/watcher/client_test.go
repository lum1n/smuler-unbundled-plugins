package watcher

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type pipeConn struct {
	net.Conn
}

// listenUnixShort binds a Unix socket under /tmp. t.TempDir() paths exceed
// the ~104-byte sun_path limit on macOS.
func listenUnixShort(t *testing.T, name string) net.Listener {
	t.Helper()
	path := filepath.Join("/tmp", name+".sock")
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ln.Close()
		os.Remove(path)
	})
	return ln
}

func acceptAndClose(ln net.Listener) {
	go func() {
		for {
			c, accErr := ln.Accept()
			if accErr != nil {
				return
			}
			c.Close()
		}
	}()
}

func TestProbeAttachOnlyPrefersLiveOptionSocket(t *testing.T) {
	ln := listenUnixShort(t, "am-opt")
	acceptAndClose(ln)

	spawned := atomic.Bool{}
	env := Env{
		TmuxAvailable: func() bool { return true },
		OptionSocket:  func() string { return ln.Addr().String() },
		CanonicalSock: func() string { return t.TempDir() + "/missing.sock" },
		Dial:          func(path string) (net.Conn, error) { return net.DialTimeout("unix", path, time.Second) },
		StartDaemon: func(python, script, listen string) (*exec.Cmd, error) {
			spawned.Store(true)
			return nil, nil
		},
	}

	path, ok := ProbeAttachOnly(env)
	if !ok {
		t.Fatal("expected attach")
	}
	if path != ln.Addr().String() {
		t.Fatalf("path = %q", path)
	}
	if spawned.Load() {
		t.Fatal("spawn must not run when a socket is live")
	}
}

func TestProbeOrSpawnDoesNotSpawnWhenSocketLive(t *testing.T) {
	ln := listenUnixShort(t, "am-live")
	acceptAndClose(ln)

	spawned := atomic.Bool{}
	c := NewClient(Config{
		Enabled:    true,
		ScriptPath: "/tmp/does-not-matter.py",
		PythonPath: "python3",
		Env: Env{
			TmuxAvailable: func() bool { return true },
			OptionSocket:  func() string { return ln.Addr().String() },
			CanonicalSock: func() string { return "" },
			Dial:          func(path string) (net.Conn, error) { return net.DialTimeout("unix", path, time.Second) },
			LookPath:      func(name string) (string, error) { return name, nil },
			StartDaemon: func(python, script, listen string) (*exec.Cmd, error) {
				spawned.Store(true)
				t.Error("StartDaemon should not be called")
				return nil, nil
			},
		},
	})

	path, mode, err := c.probeOrSpawn()
	if err != nil {
		t.Fatal(err)
	}
	if mode != "attach" {
		t.Fatalf("mode = %q, want attach", mode)
	}
	if path != ln.Addr().String() {
		t.Fatalf("path = %q", path)
	}
	if spawned.Load() {
		t.Fatal("spawned")
	}
}

func TestProbeOrSpawnMarksPythonMissing(t *testing.T) {
	c := NewClient(Config{
		Enabled:    true,
		ScriptPath: "",
		Env: Env{
			TmuxAvailable: func() bool { return true },
			OptionSocket:  func() string { return "" },
			CanonicalSock: func() string { return "/tmp/canonical.agent-watcher.sock" },
			Dial:          func(path string) (net.Conn, error) { return nil, errNeedPython },
			LookPath:      exec.LookPath,
		},
	})
	_, _, err := c.probeOrSpawn()
	if err != errNeedPython {
		t.Fatalf("err = %v, want errNeedPython", err)
	}
	if !c.SpawnFailed() {
		t.Fatal("expected SpawnFailed")
	}
}

func TestDecodeSnapshotFixture(t *testing.T) {
	line := `{"v":1,"type":"snapshot","ts":1,"agents":[{"session":"api","window":0,"kind":"claude","state":"thinking","attached":true,"windows":2}]}`
	var ev Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != "snapshot" || len(ev.Agents) != 1 {
		t.Fatalf("%+v", ev)
	}
	if ev.Agents[0].Kind != "claude" || ev.Agents[0].State != "thinking" {
		t.Fatalf("%+v", ev.Agents[0])
	}
	if MapState(ev.Agents[0].State, ev.Agents[0].Unbound) != "thinking" {
		t.Fatal(MapState(ev.Agents[0].State, false))
	}
}

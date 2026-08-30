package watcher

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

const (
	reconnectMin = 2 * time.Second
	reconnectMax = 30 * time.Second
	spawnSettle  = 300 * time.Millisecond
)

// Handler receives decoded watcher events.
type Handler func(Event)

// Config configures a Client.
type Config struct {
	Enabled    bool
	PythonPath string
	ScriptPath string
	Env        Env
	OnEvent    Handler
	Log        func(string, ...any)
}

// Client attaches to an existing agent-watcher socket or starts the vendored daemon.
type Client struct {
	cfg Config

	mu          sync.Mutex
	connected   bool
	spawnFailed bool
	mode        string // attach | spawn | ""
	listenPath  string
	ownedCmd    *exec.Cmd

	done chan struct{}
	once sync.Once
}

// NewClient builds a stopped client. Call Start to begin the reconnect loop.
func NewClient(cfg Config) *Client {
	if cfg.Env.TmuxAvailable == nil {
		cfg.Env = DefaultEnv()
	}
	if cfg.PythonPath == "" {
		cfg.PythonPath = defaultPython
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &Client{cfg: cfg, done: make(chan struct{})}
}

// Start begins probe / attach / spawn in the background.
func (c *Client) Start() {
	if !c.cfg.Enabled {
		return
	}
	go c.loop()
}

// Stop closes the client connection. A daemon this process spawned is left
// running so Sessh / tmux-agent-state can attach.
func (c *Client) Stop() {
	c.once.Do(func() { close(c.done) })
}

// Connected reports whether a watcher socket is currently being read.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// SpawnFailed is true when tmux is up but python3 / the watcher script is missing.
func (c *Client) SpawnFailed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spawnFailed
}

// Mode is "attach", "spawn", or empty.
func (c *Client) Mode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

func (c *Client) loop() {
	backoff := reconnectMin
	for {
		select {
		case <-c.done:
			return
		default:
		}

		err := c.session()
		if err == nil || errors.Is(err, errStopped) {
			return
		}
		c.cfg.Log("watcher session ended: %v", err)
		c.setConnected(false)

		select {
		case <-c.done:
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > reconnectMax {
			backoff = reconnectMax
		}
	}
}

var errStopped = errors.New("watcher stopped")
var errNoTmux = errors.New("tmux not running")
var errNeedPython = errors.New("python3 or watcher script missing")

func (c *Client) session() error {
	env := c.cfg.Env
	if env.TmuxAvailable == nil || !env.TmuxAvailable() {
		c.mu.Lock()
		c.spawnFailed = false
		c.mu.Unlock()
		return errNoTmux
	}

	path, mode, err := c.probeOrSpawn()
	if err != nil {
		return err
	}

	conn, err := env.Dial(path)
	if err != nil {
		return fmt.Errorf("dial %s: %w", path, err)
	}
	defer conn.Close()

	c.mu.Lock()
	c.connected = true
	c.spawnFailed = false
	c.mode = mode
	c.listenPath = path
	c.mu.Unlock()

	c.cfg.Log("watcher %s %s", mode, path)
	return c.readLoop(conn)
}

func (c *Client) probeOrSpawn() (path, mode string, err error) {
	env := c.cfg.Env
	dial := env.Dial
	if dial == nil {
		return "", "", errors.New("dial not configured")
	}

	if p := env.OptionSocket(); SocketLive(dial, p) {
		return p, "attach", nil
	}
	if p := env.CanonicalSock(); SocketLive(dial, p) {
		return p, "attach", nil
	}

	script := c.cfg.ScriptPath
	if script == "" {
		c.mu.Lock()
		c.spawnFailed = true
		c.mu.Unlock()
		return "", "", errNeedPython
	}
	python := c.cfg.PythonPath
	if env.LookPath != nil {
		if resolved, lookErr := env.LookPath(python); lookErr != nil {
			c.mu.Lock()
			c.spawnFailed = true
			c.mu.Unlock()
			return "", "", errNeedPython
		} else {
			python = resolved
		}
	}

	listen := ""
	if env.CanonicalSock != nil {
		listen = env.CanonicalSock()
	}
	if listen == "" {
		c.mu.Lock()
		c.spawnFailed = true
		c.mu.Unlock()
		return "", "", errors.New("no tmux socket path for --listen")
	}

	if env.StartDaemon == nil {
		return "", "", errors.New("start daemon not configured")
	}
	cmd, err := env.StartDaemon(python, script, listen)
	if err != nil {
		c.mu.Lock()
		c.spawnFailed = true
		c.mu.Unlock()
		return "", "", err
	}
	c.mu.Lock()
	c.ownedCmd = cmd
	c.mu.Unlock()

	if env.SetOption != nil {
		_ = env.SetOption(listen)
	}
	time.Sleep(spawnSettle)
	return listen, "spawn", nil
}

func (c *Client) readLoop(conn net.Conn) error {
	// Request a full snapshot once the socket is up. hello may already be in flight.
	_ = sendControl(conn, Control{Cmd: "snapshot"})

	reader := bufio.NewReader(conn)
	for {
		select {
		case <-c.done:
			_ = sendControl(conn, Control{Cmd: "stop"})
			return errStopped
		default:
		}
		if dc, ok := conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			_ = dc.SetReadDeadline(time.Now().Add(2 * time.Second))
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if errors.Is(err, io.EOF) {
				return err
			}
			return err
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil || ev.Type == "" {
			continue
		}
		if ev.Type == "hello" && c.cfg.OnEvent != nil {
			_ = sendControl(conn, Control{Cmd: "snapshot"})
		}
		if c.cfg.OnEvent != nil {
			c.cfg.OnEvent(ev)
		}
	}
}

func sendControl(w io.Writer, cmd Control) error {
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	c.connected = v
	if !v {
		c.mode = ""
	}
	c.mu.Unlock()
}

// ProbeAttachOnly returns the first live socket without spawning. Used by tests.
func ProbeAttachOnly(env Env) (string, bool) {
	if env.TmuxAvailable == nil || !env.TmuxAvailable() {
		return "", false
	}
	if p := env.OptionSocket(); SocketLive(env.Dial, p) {
		return p, true
	}
	if p := env.CanonicalSock(); SocketLive(env.Dial, p) {
		return p, true
	}
	return "", false
}

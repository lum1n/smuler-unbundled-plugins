package internal

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SocketEvent is emitted by shell hooks or agent native hooks.
type SocketEvent struct {
	Type       string `json:"type"`
	AgentID    string `json:"agentId"`
	AgentName  string `json:"agentName"`
	Command    string `json:"command"`
	PID        int    `json:"pid"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	Message    string `json:"message,omitempty"`
	OutputTail string `json:"outputTail,omitempty"`
	State      string `json:"state,omitempty"`
	Label      string `json:"label,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	Tool         string `json:"tool,omitempty"`
	QuestionText string `json:"questionText,omitempty"`
}

// SocketListener receives lifecycle and status events over a Unix domain socket.
type SocketListener struct {
	path  string
	store *AgentStore
	ln    net.Listener
	done  chan struct{}
}

// NewSocketListener creates a socket listener at the given path.
func NewSocketListener(socketPath string, store *AgentStore) *SocketListener {
	return &SocketListener{
		path:  socketPath,
		store: store,
		done:  make(chan struct{}),
	}
}

// Start binds and begins accepting connections.
func (sl *SocketListener) Start() error {
	dir := filepath.Dir(sl.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	os.Remove(sl.path)
	var err error
	sl.ln, err = net.Listen("unix", sl.path)
	if err != nil {
		return err
	}
	os.Chmod(sl.path, 0600)
	go sl.acceptLoop()
	return nil
}

// Stop closes the listener.
func (sl *SocketListener) Stop() {
	if sl.ln != nil {
		sl.ln.Close()
	}
	close(sl.done)
}

func (sl *SocketListener) acceptLoop() {
	for {
		conn, err := sl.ln.Accept()
		if err != nil {
			select {
			case <-sl.done:
				return
			default:
				continue
			}
		}
		go sl.handleConn(conn)
	}
}

func (sl *SocketListener) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev SocketEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		sl.process(ev)
	}
}

func (sl *SocketListener) process(ev SocketEvent) {
	id := ev.AgentID + "-" + strconv.Itoa(ev.PID)
	switch ev.Type {
	case "agent_start":
		if sl.store.Get(id) == nil {
			sl.store.AddHistory(ev.AgentID, ev.Command, "started", "", time.Now().UnixMilli())
		}
		sl.store.Upsert(AgentSession{
			ID:          id,
			AgentID:     ev.AgentID,
			DisplayName: ev.AgentName,
			Command:     ev.Command,
			State:       "running",
			PID:         ev.PID,
		})

	case "agent_end":
		existing := sl.store.Get(id)
		state := "completed"
		if ev.ExitCode != nil && *ev.ExitCode != 0 {
			state = "error"
		}
		if existing != nil {
			sl.store.AddHistory(existing.AgentID, existing.Command, state, existing.Task, time.Now().UnixMilli())
		}
		sl.store.Upsert(AgentSession{
			ID:       id,
			State:    state,
			PID:      ev.PID,
			ExitCode: ev.ExitCode,
		})
		log.Printf("[agent-monitor] agent %s ended with state=%s exitCode=%v", ev.AgentID, state, ev.ExitCode)

	case "agent_question":
		sl.store.Upsert(AgentSession{
			ID:           id,
			AgentID:      ev.AgentID,
			DisplayName:  ev.AgentName,
			State:        "question",
			Task:         ev.Message,
			QuestionText: ev.QuestionText,
			PID:          ev.PID,
		})
		log.Printf("[agent-monitor] agent %s needs attention: %s", ev.AgentID, ev.Message)

	case "agent_status":
		state := ev.State
		if state == "" {
			state = "running"
		}
		sl.store.Upsert(AgentSession{
			ID:           id,
			AgentID:      ev.AgentID,
			DisplayName:  ev.AgentName,
			Command:      ev.Command,
			State:        state,
			Task:         ev.Message,
			QuestionText: ev.QuestionText,
			CurrentTool:  ev.Tool,
			SessionID:    ev.SessionID,
			PID:          ev.PID,
		})
		log.Printf("[agent-monitor] agent %s status: %s (label: %s)", ev.AgentID, state, ev.Label)
	}
}

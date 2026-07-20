package internal

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// AgentStore holds active and recently completed agent sessions.
type AgentStore struct {
	mu      sync.RWMutex
	agents  map[string]*AgentSession
	history []HistoryEntry
}

// HistoryEntry is a lightweight summary of a completed session.
type HistoryEntry struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	Command   string `json:"command"`
	State     string `json:"state"`
	Task      string `json:"task"`
	Timestamp int64  `json:"timestamp"`
}

// NewAgentStore creates an empty store.
func NewAgentStore() *AgentStore {
	return &AgentStore{agents: make(map[string]*AgentSession)}
}

// Upsert creates or updates a session. Empty fields are ignored so partial
// updates from hooks do not erase richer context from readers.
func (s *AgentStore) Upsert(session AgentSession) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if session.ID == "" {
		return
	}

	existing, ok := s.agents[session.ID]
	if !ok {
		now := time.Now().UnixMilli()
		if session.StartTime == 0 {
			session.StartTime = now
		}
		if session.UpdatedAt == 0 {
			session.UpdatedAt = now
		}
		s.agents[session.ID] = &session
		return
	}

	if session.AgentID != "" {
		existing.AgentID = session.AgentID
	}
	if session.DisplayName != "" {
		existing.DisplayName = session.DisplayName
	}
	if session.PID > 0 {
		existing.PID = session.PID
	}
	if session.Command != "" {
		existing.Command = session.Command
	}
	if session.Task != "" {
		existing.Task = session.Task
	}
	if session.State != "" {
		existing.State = session.State
	}
	if session.OutputTail != "" {
		existing.OutputTail = session.OutputTail
	}
	if session.SessionID != "" {
		existing.SessionID = session.SessionID
	}
	if session.CurrentFile != "" {
		existing.CurrentFile = session.CurrentFile
	}
	if session.CurrentTool != "" {
		existing.CurrentTool = session.CurrentTool
	}
	if session.Model != "" {
		existing.Model = session.Model
	}
	if session.Provider != "" {
		existing.Provider = session.Provider
	}
	if session.Cost != 0 {
		existing.Cost = session.Cost
	}
	if session.TokensInput != 0 {
		existing.TokensInput = session.TokensInput
	}
	if session.TokensOutput != 0 {
		existing.TokensOutput = session.TokensOutput
	}
	if session.CacheRead != 0 {
		existing.CacheRead = session.CacheRead
	}
	if session.CacheWrite != 0 {
		existing.CacheWrite = session.CacheWrite
	}
	if session.FilesChanged != 0 {
		existing.FilesChanged = session.FilesChanged
	}
	if session.Additions != 0 {
		existing.Additions = session.Additions
	}
	if session.Deletions != 0 {
		existing.Deletions = session.Deletions
	}
	if session.CWD != "" {
		existing.CWD = session.CWD
	}
	if session.RepoName != "" {
		existing.RepoName = session.RepoName
	}
	if session.RepoFullName != "" {
		existing.RepoFullName = session.RepoFullName
	}
	if session.Branch != "" {
		existing.Branch = session.Branch
	}
	if session.IsRepoDirty {
		existing.IsRepoDirty = session.IsRepoDirty
	}
	if len(session.Todos) > 0 {
		existing.Todos = session.Todos
	}
	if session.StartTime != 0 {
		existing.StartTime = session.StartTime
	}
	if session.UpdatedAt != 0 {
		existing.UpdatedAt = session.UpdatedAt
	} else {
		existing.UpdatedAt = time.Now().UnixMilli()
	}
	if session.ExitCode != nil {
		existing.ExitCode = session.ExitCode
	}
	if session.Available {
		existing.Available = true
	}
}

// Get returns a copy of the session with the given id.
func (s *AgentStore) Get(id string) *AgentSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.agents[id]
	if !ok {
		return nil
	}
	cp := a.Clone()
	return &cp
}

// List returns a snapshot of all active sessions.
func (s *AgentStore) List() []AgentSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := make([]AgentSession, 0, len(s.agents))
	for _, a := range s.agents {
		r = append(r, a.Clone())
	}
	return r
}

// AddHistory records a completed session summary.
func (s *AgentStore) AddHistory(agentID, command, state, task string, timestamp int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, HistoryEntry{
		ID:        agentID + "-" + strconv.FormatInt(timestamp, 10),
		AgentID:   agentID,
		Command:   command,
		State:     state,
		Task:      task,
		Timestamp: timestamp,
	})
	if len(s.history) > 50 {
		s.history = s.history[len(s.history)-50:]
	}
}

// History returns a copy of recent history.
func (s *AgentStore) History() []HistoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := make([]HistoryEntry, len(s.history))
	copy(r, s.history)
	return r
}

// FindByNamePrefix returns the first running/session entry whose ID starts with prefix.
func (s *AgentStore) FindByNamePrefix(prefix string) *AgentSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, a := range s.agents {
		if strings.HasPrefix(id, prefix) && !a.IsTerminalState() {
			cp := a.Clone()
			return &cp
		}
	}
	for id, a := range s.agents {
		if strings.HasPrefix(id, prefix) {
			cp := a.Clone()
			return &cp
		}
	}
	return nil
}

// Remove deletes a session from the active set.
func (s *AgentStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.agents, id)
}

// RemoveStale deletes sessions not updated since before.
func (s *AgentStore) RemoveStale(before int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, a := range s.agents {
		if a.UpdatedAt < before {
			delete(s.agents, id)
		}
	}
}

// RemoveCompletedOlderThan deletes terminal sessions older than before.
func (s *AgentStore) RemoveCompletedOlderThan(before int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, a := range s.agents {
		if a.IsTerminalState() && a.UpdatedAt < before {
			delete(s.agents, id)
		}
	}
}

// UpdateOutput updates the output tail for a session.
func (s *AgentStore) UpdateOutput(id, outputTail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.agents[id]; ok {
		if a.OutputTail != outputTail || len(outputTail) > 0 {
			a.OutputTail = outputTail
			a.UpdatedAt = time.Now().UnixMilli()
		}
	}
}

// MarkCompleted sets a session's terminal state and records history.
func (s *AgentStore) MarkCompleted(id, state string, timestamp int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.agents[id]; ok {
		a.State = state
		a.UpdatedAt = timestamp
	}
}

// MarkRunning is a convenience for process-detector updates.
func (s *AgentStore) MarkRunning(id, agentID, command string, pid int) {
	s.Upsert(AgentSession{
		ID:      id,
		AgentID: agentID,
		Command: command,
		State:   "running",
		PID:     pid,
	})
}

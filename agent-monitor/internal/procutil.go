package internal

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// commandTimeout bounds every external command (ps, lsof, git, sqlite3) so a
// hung tool can never stall a refresh.
const commandTimeout = 3 * time.Second

// commandOutput runs name with args under commandTimeout and returns stdout.
func commandOutput(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// processAlive reports whether a process with pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ttlCache is a small mutex-guarded string cache with per-entry expiry.
type ttlCache[V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]ttlEntry[V]
}

type ttlEntry[V any] struct {
	value   V
	expires time.Time
}

func newTTLCache[V any](ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, entries: make(map[string]ttlEntry[V])}
}

func (c *ttlCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

func (c *ttlCache[V]) set(key string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// Opportunistically drop expired entries so the map stays bounded.
	if len(c.entries) > 64 {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = ttlEntry[V]{value: v, expires: now.Add(c.ttl)}
}

// jsonlTail incrementally reads complete lines appended to a JSONL file so a
// transcript is parsed once instead of on every refresh. Lines of any length
// are supported, and a partially written trailing line is left for the next
// call.
type jsonlTail struct {
	offset int64
	info   os.FileInfo
	// maxInitial, when > 0, limits the first read of a large file to its last
	// maxInitial bytes (the first, likely partial, line is skipped).
	maxInitial int64
}

// next feeds new complete lines to onLine. If the file was truncated or
// replaced, onReset is called before reading restarts from the beginning.
func (t *jsonlTail) next(path string, onReset func(), onLine func([]byte)) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if t.info != nil && (!os.SameFile(t.info, info) || info.Size() < t.offset) {
		t.offset = 0
		t.info = nil
		onReset()
	}
	if t.info != nil && info.Size() == t.offset {
		t.info = info
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	skipFirst := false
	if t.info == nil && t.offset == 0 && t.maxInitial > 0 && info.Size() > t.maxInitial {
		t.offset = info.Size() - t.maxInitial
		skipFirst = true
	}
	t.info = info
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return err
	}

	r := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// Partial (or empty) trailing line: leave it for the next call.
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		t.offset += int64(len(line))
		if skipFirst {
			skipFirst = false
			continue
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		onLine(line)
	}
}

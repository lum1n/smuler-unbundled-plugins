package internal

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// GenericContextReader extracts repo/branch/cwd context directly via git.
// Results are cached per working directory for a short time: running four git
// commands per agent on every refresh was a major source of latency.
type GenericContextReader struct {
	repoCache *ttlCache[AgentSession]
}

// NewGenericContextReader creates a new generic context reader.
func NewGenericContextReader() *GenericContextReader {
	return &GenericContextReader{repoCache: newTTLCache[AgentSession](10 * time.Second)}
}

// ContextForPID returns generic repository context for the given process.
func (r *GenericContextReader) ContextForPID(pid int) AgentSession {
	if pid <= 0 {
		return AgentSession{}
	}
	cwd := cwdForPID(pid)
	if cwd == "" {
		return AgentSession{}
	}

	if r.repoCache != nil {
		if ctx, ok := r.repoCache.get(cwd); ok {
			return ctx
		}
	}
	ctx := r.repoContext(cwd)
	if r.repoCache != nil {
		r.repoCache.set(cwd, ctx)
	}
	return ctx
}

func (r *GenericContextReader) repoContext(cwd string) AgentSession {
	ctx := AgentSession{CWD: cwd, Available: true}
	repoRoot := runCommand("git", "-C", cwd, "rev-parse", "--show-toplevel")
	if repoRoot == "" {
		return ctx
	}

	ctx.RepoRoot = repoRoot
	ctx.RepoName = filepath.Base(repoRoot)
	ctx.RepoFullName = gitRepoFullName(repoRoot)
	ctx.Branch = runCommand("git", "-C", cwd, "branch", "--show-current")
	status := runCommand("git", "-C", cwd, "status", "--porcelain=v1")
	if status != "" {
		ctx.IsRepoDirty = true
		for _, line := range strings.Split(status, "\n") {
			if strings.TrimSpace(line) != "" {
				ctx.FilesChanged++
			}
		}
	}

	return ctx
}

// cwdCache memoizes process working directories; on macOS each lookup is an
// lsof exec, and several readers ask for the same pid during one refresh.
var cwdCache = newTTLCache[string](15 * time.Second)

func cwdForPID(pid int) string {
	key := strconv.Itoa(pid)
	if cwd, ok := cwdCache.get(key); ok {
		return cwd
	}
	cwd := lookupCWD(pid)
	if cwd != "" {
		cwdCache.set(key, cwd)
	}
	return cwd
}

func lookupCWD(pid int) string {
	if runtime.GOOS == "darwin" {
		out, err := commandOutput("lsof", "-p", strconv.Itoa(pid), "-a", "-d", "cwd", "-Fn")
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "n") {
				return strings.TrimPrefix(line, "n")
			}
		}
		return ""
	}
	if runtime.GOOS == "linux" {
		if dir, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd")); err == nil {
			return dir
		}
	}
	return ""
}

func runCommand(name string, args ...string) string {
	out, err := commandOutput(name, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitRepoFullName(repoRoot string) string {
	remote := runCommand("git", "-C", repoRoot, "remote", "get-url", "origin")
	if remote == "" {
		return ""
	}
	// Support ssh://git@host/owner/repo, https://host/owner/repo, git@host:owner/repo
	remote = strings.TrimSuffix(remote, ".git")
	if idx := strings.LastIndex(remote, "/"); idx >= 0 {
		repo := remote[idx+1:]
		before := remote[:idx]
		if idx2 := strings.LastIndex(before, "/"); idx2 >= 0 {
			return before[idx2+1:] + "/" + repo
		}
		if idx2 := strings.Index(before, ":"); idx2 >= 0 {
			return before[idx2+1:] + "/" + repo
		}
	}
	return ""
}

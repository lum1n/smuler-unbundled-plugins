package internal

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// GenericContextReader extracts repo/branch/cwd context directly via git.
type GenericContextReader struct{}

// NewGenericContextReader creates a new generic context reader.
func NewGenericContextReader() *GenericContextReader {
	return &GenericContextReader{}
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

func cwdForPID(pid int) string {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-a", "-d", "cwd", "-Fn").Output()
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
	out, err := exec.Command(name, args...).Output()
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

package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	imap "github.com/emersion/go-imap"

	email "github.com/lum1n/smuler/plugins/email/internal"
)

// fakeServer is a tiny scripted IMAP server: plain TCP, no STARTTLS, one
// INBOX with a configurable unread UID set.
type fakeServer struct {
	t        *testing.T
	ln       net.Listener
	password string

	mu      sync.Mutex
	unread  []uint32
	fetched []uint32
	logins  int
	conns   []net.Conn
}

func newFakeServer(t *testing.T, unread ...uint32) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, ln: ln, password: "secret", unread: unread}
	go s.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		s.dropConns()
	})
	return s
}

func (s *fakeServer) account() email.Account {
	addr := s.ln.Addr().(*net.TCPAddr)
	return email.Account{ID: "a", Provider: email.ProviderIMAP, Label: "Test", Username: "user", Password: "secret", Server: "127.0.0.1", Port: addr.Port, TLS: false}
}

func (s *fakeServer) dropConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

func (s *fakeServer) setUnread(uids ...uint32) {
	s.mu.Lock()
	s.unread = uids
	s.mu.Unlock()
}

func (s *fakeServer) takeFetched() []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.fetched
	s.fetched = nil
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *fakeServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		go s.handle(c)
	}
}

func (s *fakeServer) handle(c net.Conn) {
	defer c.Close()
	w := bufio.NewWriter(c)
	r := bufio.NewReader(c)
	send := func(lines ...string) {
		for _, l := range lines {
			_, _ = w.WriteString(l + "\r\n")
		}
		_ = w.Flush()
	}
	send("* OK [CAPABILITY IMAP4rev1] ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		tag, cmd := fields[0], strings.ToUpper(fields[1])
		args := fields[2:]
		if cmd == "UID" && len(args) > 0 {
			cmd = "UID " + strings.ToUpper(args[0])
			args = args[1:]
		}
		s.mu.Lock()
		unread := append([]uint32(nil), s.unread...)
		s.mu.Unlock()
		switch cmd {
		case "CAPABILITY":
			send("* CAPABILITY IMAP4rev1", tag+" OK done")
		case "LOGIN":
			pw := ""
			if len(args) > 1 {
				pw = strings.Trim(args[1], `"`)
			}
			if pw != s.password {
				send(tag + " NO [AUTHENTICATIONFAILED] Invalid credentials")
				continue
			}
			s.mu.Lock()
			s.logins++
			s.mu.Unlock()
			send(tag + " OK logged in")
		case "SELECT":
			send(fmt.Sprintf("* %d EXISTS", len(unread)), "* OK [UIDVALIDITY 7] ok", `* FLAGS (\Seen)`, tag+" OK [READ-WRITE] done")
		case "UID SEARCH":
			parts := []string{"* SEARCH"}
			for _, u := range unread {
				parts = append(parts, strconv.FormatUint(uint64(u), 10))
			}
			send(strings.Join(parts, " "), tag+" OK done")
		case "UID FETCH":
			set, err := imap.ParseSeqSet(args[0])
			if err != nil {
				send(tag + " BAD seqset")
				continue
			}
			for i, u := range unread {
				if !set.Contains(u) {
					continue
				}
				s.mu.Lock()
				s.fetched = append(s.fetched, u)
				s.mu.Unlock()
				send(fmt.Sprintf(`* %d FETCH (UID %d FLAGS () ENVELOPE ("Wed, 11 May 2016 14:31:59 +0000" "Subject %d" (("Alice" NIL "alice" "example.com")) NIL NIL NIL NIL NIL NIL "<m%d@x>"))`, i+1, u, u, u))
			}
			send(tag + " OK done")
		case "LOGOUT":
			send("* BYE", tag+" OK done")
			return
		default:
			send(tag + " OK noop")
		}
	}
}

func uidsOf(msgs []email.EmailMessage) []uint32 {
	out := make([]uint32, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.UID)
	}
	return out
}

func TestFetchUnreadCachesAndReportsNewUIDs(t *testing.T) {
	srv := newFakeServer(t, 1, 2, 3, 4, 5)
	p := New(srv.account())
	defer p.Close()

	res, err := p.FetchUnread(context.Background(), 3)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if res.Total != 5 {
		t.Fatalf("total = %d, want 5 (full unread count, not the limit)", res.Total)
	}
	if got := uidsOf(res.Messages); fmt.Sprint(got) != "[5 4 3]" {
		t.Fatalf("messages = %v, want newest-first [5 4 3]", got)
	}
	if len(res.NewUIDs) != 0 {
		t.Fatalf("first poll must not report new mail, got %v", res.NewUIDs)
	}
	if got := srv.takeFetched(); fmt.Sprint(got) != "[3 4 5]" {
		t.Fatalf("fetched = %v", got)
	}

	// Unchanged mailbox: no FETCH at all, nothing new.
	res, err = p.FetchUnread(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.takeFetched(); len(got) != 0 {
		t.Fatalf("expected cached messages, refetched %v", got)
	}
	if len(res.NewUIDs) != 0 {
		t.Fatalf("unexpected new UIDs %v", res.NewUIDs)
	}

	// One new message: only it is fetched and reported.
	srv.setUnread(1, 2, 3, 4, 5, 9)
	res, err = p.FetchUnread(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.takeFetched(); fmt.Sprint(got) != "[9]" {
		t.Fatalf("fetched = %v, want only [9]", got)
	}
	if fmt.Sprint(res.NewUIDs) != "[9]" {
		t.Fatalf("new UIDs = %v, want [9]", res.NewUIDs)
	}
	if got := uidsOf(res.Messages); fmt.Sprint(got) != "[9 5 4]" {
		t.Fatalf("messages = %v", got)
	}
}

func TestFetchUnreadReconnectsAfterDroppedConnection(t *testing.T) {
	srv := newFakeServer(t, 1)
	p := New(srv.account())
	defer p.Close()

	if _, err := p.FetchUnread(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	srv.dropConns()
	time.Sleep(20 * time.Millisecond)

	res, err := p.FetchUnread(context.Background(), 10)
	if err != nil {
		t.Fatalf("expected transparent reconnect, got %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("total = %d", res.Total)
	}
}

func TestConnectWrongPasswordIsAuthError(t *testing.T) {
	srv := newFakeServer(t)
	acc := srv.account()
	acc.Password = "wrong"
	err := New(acc).Connect()
	if !errors.Is(err, email.ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatal("error must not contain the password")
	}
}

func TestConnectMissingPasswordSkipsDial(t *testing.T) {
	err := New(email.Account{Server: "127.0.0.1", Port: 1, Username: "u"}).Connect()
	if !errors.Is(err, email.ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

func TestConnectTimesOutOnSilentServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // never send a greeting
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	}()

	old := dialTimeout
	dialTimeout = 200 * time.Millisecond
	defer func() { dialTimeout = old }()

	acc := email.Account{Username: "u", Password: "p", Server: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	start := time.Now()
	if err := New(acc).Connect(); err == nil {
		t.Fatal("expected error from silent server")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("connect hung for %v", elapsed)
	}
}

func TestArchiveFoldersPreferGmailAllMail(t *testing.T) {
	if got := archiveFolders(email.ProviderGmail)[0]; got != "[Gmail]/All Mail" {
		t.Fatalf("gmail first folder = %q", got)
	}
	if got := archiveFolders(email.ProviderICloud)[0]; got != "Archive" {
		t.Fatalf("icloud first folder = %q", got)
	}
}

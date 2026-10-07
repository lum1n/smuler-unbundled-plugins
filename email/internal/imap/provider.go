package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	imap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	email "github.com/lum1n/smuler/plugins/email/internal"
)

// Network bounds: a stalled server must never block the plugin's handler
// (the SDK serializes calls, so one hung IMAP read would freeze every later
// refresh and action).
var (
	dialTimeout    = 15 * time.Second
	commandTimeout = 30 * time.Second
)

type Provider struct {
	acc         email.Account
	client      *client.Client
	mu          sync.Mutex
	prevUIDs    map[uint32]struct{}
	initialized bool
	uidValidity uint32
	// cache holds already-fetched unread messages by UID so each refresh only
	// FETCHes envelopes for messages it has not seen before.
	cache map[uint32]email.EmailMessage
}

func New(acc email.Account) *Provider {
	return &Provider{
		acc:      acc,
		prevUIDs: make(map[uint32]struct{}),
		cache:    make(map[uint32]email.EmailMessage),
	}
}

func (p *Provider) Account() email.Account { return p.acc }

func (p *Provider) Connect() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connectLocked()
}

func (p *Provider) connectLocked() error {
	if p.client != nil {
		select {
		case <-p.client.LoggedOut():
			// Server dropped the connection; reconnect below.
			p.client = nil
		default:
			return nil
		}
	}
	if p.acc.Username == "" || p.acc.Password == "" {
		return fmt.Errorf("%w: username and password (app password) required", email.ErrAuthFailed)
	}
	addr := fmt.Sprintf("%s:%d", p.acc.Server, p.acc.Port)
	dialer := &net.Dialer{Timeout: dialTimeout}
	var c *client.Client
	var err error
	if p.acc.TLS {
		c, err = client.DialWithDialerTLS(dialer, addr, &tls.Config{ServerName: p.acc.Server})
	} else {
		c, err = client.DialWithDialer(dialer, addr)
	}
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	c.Timeout = commandTimeout
	if !p.acc.TLS {
		if ok, _ := c.SupportStartTLS(); ok {
			if err := c.StartTLS(&tls.Config{ServerName: p.acc.Server}); err != nil {
				_ = c.Close()
				return fmt.Errorf("starttls %s: %w", addr, err)
			}
		}
	}
	if err := c.Login(p.acc.Username, p.acc.Password); err != nil {
		_ = c.Close()
		if isConnError(err) {
			return fmt.Errorf("login %s: %w", p.acc.Username, err)
		}
		return fmt.Errorf("%w: login %s: %v", email.ErrAuthFailed, p.acc.Username, err)
	}
	p.client = c
	return nil
}

// isConnError reports transport failures (timeouts, resets, EOF) as opposed to
// a server NO/BAD reply, which for LOGIN means rejected credentials.
func isConnError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return strings.Contains(err.Error(), "connection closed")
}

// resetLocked drops a connection after any command error so the next call
// reconnects instead of reusing a dead or desynchronized session.
func (p *Provider) resetLocked() {
	if p.client == nil {
		return
	}
	_ = p.client.Close()
	p.client = nil
}

func (p *Provider) FetchUnread(ctx context.Context, limit int) (email.UnreadResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	reused := p.client != nil
	res, err := p.fetchUnreadLocked(limit)
	if err != nil {
		p.resetLocked()
		// A long-lived session may have been dropped by the server (idle
		// timeout); retry once on a fresh connection before reporting.
		if reused && !errors.Is(err, email.ErrAuthFailed) {
			res, err = p.fetchUnreadLocked(limit)
			if err != nil {
				p.resetLocked()
			}
		}
	}
	return res, err
}

func (p *Provider) selectInboxLocked() (*imap.MailboxStatus, error) {
	mbox, err := p.client.Select("INBOX", false)
	if err != nil {
		return nil, fmt.Errorf("select INBOX: %w", err)
	}
	if mbox.UidValidity != p.uidValidity {
		// UIDs from a previous validity epoch are meaningless now.
		p.uidValidity = mbox.UidValidity
		p.cache = make(map[uint32]email.EmailMessage)
		p.prevUIDs = make(map[uint32]struct{})
		p.initialized = false
	}
	return mbox, nil
}

func (p *Provider) fetchUnreadLocked(limit int) (email.UnreadResult, error) {
	if err := p.connectLocked(); err != nil {
		return email.UnreadResult{}, err
	}
	if _, err := p.selectInboxLocked(); err != nil {
		return email.UnreadResult{}, err
	}

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	uids, err := p.client.UidSearch(criteria)
	if err != nil {
		return email.UnreadResult{}, fmt.Errorf("search unread: %w", err)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })

	res := email.UnreadResult{Total: len(uids)}
	res.NewUIDs = p.trackUIDs(uids)

	window := uids
	if limit > 0 && len(window) > limit {
		window = window[len(window)-limit:]
	}

	var missing []uint32
	for _, uid := range window {
		if _, ok := p.cache[uid]; !ok {
			missing = append(missing, uid)
		}
	}
	if len(missing) > 0 {
		fetched, err := p.fetchMessagesLocked(missing)
		if err != nil {
			return email.UnreadResult{}, err
		}
		for _, em := range fetched {
			p.cache[em.UID] = em
		}
	}

	inWindow := make(map[uint32]struct{}, len(window))
	for i := len(window) - 1; i >= 0; i-- {
		uid := window[i]
		inWindow[uid] = struct{}{}
		if em, ok := p.cache[uid]; ok {
			res.Messages = append(res.Messages, em)
		}
	}
	for uid := range p.cache {
		if _, ok := inWindow[uid]; !ok {
			delete(p.cache, uid)
		}
	}
	return res, nil
}

// trackUIDs records the full unread UID set and returns UIDs not present in
// the previous poll. The first poll after (re)initialization reports nothing,
// so existing unread mail does not trigger a notification burst.
func (p *Provider) trackUIDs(uids []uint32) []uint32 {
	var newUIDs []uint32
	if p.initialized {
		for _, uid := range uids {
			if _, seen := p.prevUIDs[uid]; !seen {
				newUIDs = append(newUIDs, uid)
			}
		}
	}
	p.prevUIDs = make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		p.prevUIDs[uid] = struct{}{}
	}
	p.initialized = true
	return newUIDs
}

func (p *Provider) fetchMessagesLocked(uids []uint32) ([]email.EmailMessage, error) {
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uids...)

	messages := make(chan *imap.Message, len(uids))
	fetchItems := []imap.FetchItem{
		imap.FetchEnvelope,
		imap.FetchUid,
		imap.FetchFlags,
		imap.FetchBodyStructure,
		imap.FetchItem("BODY.PEEK[TEXT]<0.512>"),
	}

	done := make(chan error, 1)
	go func() {
		done <- p.client.UidFetch(seqSet, fetchItems, messages)
	}()

	var results []email.EmailMessage
	for msg := range messages {
		results = append(results, envelopeToMessage(p.acc, msg))
	}
	if fetchErr := <-done; fetchErr != nil {
		return nil, fmt.Errorf("fetch: %w", fetchErr)
	}
	return results, nil
}

func envelopeToMessage(acc email.Account, msg *imap.Message) email.EmailMessage {
	em := email.EmailMessage{
		UID:          msg.Uid,
		AccountID:    acc.ID,
		AccountLabel: acc.Label,
		WebLink:      email.WebmailURL(acc.Provider),
	}
	if env := msg.Envelope; env != nil {
		em.Subject = env.Subject
		if len(env.From) > 0 {
			addr := env.From[0]
			if addr.PersonalName != "" {
				em.From = fmt.Sprintf("%s <%s@%s>", addr.PersonalName, addr.MailboxName, addr.HostName)
			} else {
				em.From = fmt.Sprintf("%s@%s", addr.MailboxName, addr.HostName)
			}
		}
		if !env.Date.IsZero() {
			em.ReceivedAt = env.Date
		}
	}
	isUnread := true
	for _, f := range msg.Flags {
		if f == imap.SeenFlag {
			isUnread = false
			break
		}
	}
	em.Unread = isUnread
	em.Snippet = extractSnippet(msg)
	return em
}

func extractSnippet(msg *imap.Message) string {
	for _, lit := range msg.Body {
		buf := make([]byte, 512)
		n, err := lit.Read(buf)
		if err != nil && err != io.EOF {
			continue
		}
		if n == 0 {
			continue
		}
		raw := string(buf[:n])
		clean := cleanSnippet(raw)
		if len(clean) > 0 {
			return clean
		}
	}
	return formatBodyStructure(msg.BodyStructure)
}

func cleanSnippet(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if r == '\r' || r == '\n' || r == '\t' {
			b.WriteByte(' ')
			continue
		}
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
	}
	s := strings.TrimSpace(b.String())
	s = strings.Join(strings.Fields(s), " ")
	const maxLen = 150
	if utf8.RuneCountInString(s) > maxLen {
		runes := []rune(s)
		s = string(runes[:maxLen]) + "..."
	}
	return s
}

func formatBodyStructure(bs *imap.BodyStructure) string {
	if bs == nil {
		return ""
	}
	if strings.EqualFold(bs.MIMEType, "text") && strings.EqualFold(bs.MIMESubType, "plain") {
		return fmt.Sprintf("text/plain, %d bytes", bs.Size)
	}
	for _, part := range bs.Parts {
		if s := formatBodyStructure(part); s != "" {
			return s
		}
	}
	return ""
}

// ensureInboxLocked connects if needed and makes sure INBOX is selected,
// since UID STORE/MOVE require a selected mailbox.
func (p *Provider) ensureInboxLocked() error {
	if err := p.connectLocked(); err != nil {
		return err
	}
	if mbox := p.client.Mailbox(); mbox != nil && strings.EqualFold(mbox.Name, "INBOX") {
		return nil
	}
	_, err := p.selectInboxLocked()
	return err
}

func (p *Provider) MarkRead(uid uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureInboxLocked(); err != nil {
		p.resetLocked()
		return err
	}
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.SeenFlag}
	if err := p.client.UidStore(seqSet, item, flags, nil); err != nil {
		p.resetLocked()
		return err
	}
	delete(p.cache, uid)
	return nil
}

// Archive moves the message out of INBOX into the provider's archive folder.
// It never falls back to deleting: if no archive folder exists the message is
// left untouched and an error is returned.
func (p *Provider) Archive(uid uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureInboxLocked(); err != nil {
		p.resetLocked()
		return err
	}
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)
	var lastErr error
	for _, folder := range archiveFolders(p.acc.Provider) {
		// UidMove uses MOVE when supported, else COPY+STORE+EXPUNGE.
		if lastErr = p.client.UidMove(seqSet, folder); lastErr == nil {
			delete(p.cache, uid)
			return nil
		}
		if isConnError(lastErr) {
			p.resetLocked()
			return lastErr
		}
	}
	return fmt.Errorf("no archive folder found: %w", lastErr)
}

func archiveFolders(provider string) []string {
	if provider == email.ProviderGmail {
		return []string{"[Gmail]/All Mail", "Archive"}
	}
	return []string{"Archive", "[Gmail]/All Mail"}
}

func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		_ = p.client.Logout()
		err := p.client.Close()
		p.client = nil
		return err
	}
	return nil
}

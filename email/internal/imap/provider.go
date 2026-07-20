package imap

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	imap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	email "github.com/lum1n/smuler/plugins/email/internal"
)

type Provider struct {
	acc         email.Account
	client      *client.Client
	mu          sync.Mutex
	prevUIDs    map[uint32]struct{}
	initialized bool
}

func New(acc email.Account) *Provider {
	return &Provider{
		acc:      acc,
		prevUIDs: make(map[uint32]struct{}),
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
		return nil
	}
	addr := fmt.Sprintf("%s:%d", p.acc.Server, p.acc.Port)
	c, err := client.DialTLS(addr, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	if err := c.Login(p.acc.Username, p.acc.Password); err != nil {
		c.Close()
		return fmt.Errorf("login %s: %w", p.acc.Username, err)
	}
	p.client = c
	return nil
}

func (p *Provider) FetchUnread(ctx context.Context, limit int) ([]email.EmailMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.connectLocked(); err != nil {
		return nil, err
	}

	if _, err := p.client.Select("INBOX", false); err != nil {
		return nil, fmt.Errorf("select INBOX: %w", err)
	}

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	uids, err := p.client.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("search unread: %w", err)
	}

	if len(uids) == 0 {
		p.prevUIDs = make(map[uint32]struct{})
		p.initialized = true
		return nil, nil
	}

	if len(uids) > limit {
		uids = uids[len(uids)-limit:]
	}

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

	current := make(map[uint32]struct{}, len(results))
	for _, em := range results {
		current[em.UID] = struct{}{}
	}
	p.prevUIDs = current
	p.initialized = true

	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
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

func (p *Provider) MarkRead(uid uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		return fmt.Errorf("not connected")
	}
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.SeenFlag}
	return p.client.UidStore(seqSet, item, flags, nil)
}

func (p *Provider) Archive(uid uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		return fmt.Errorf("not connected")
	}
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)
	if err := p.archiveToFolder(seqSet); err != nil {
		item := imap.FormatFlagsOp(imap.AddFlags, true)
		flags := []interface{}{imap.SeenFlag, imap.DeletedFlag}
		if storeErr := p.client.UidStore(seqSet, item, flags, nil); storeErr != nil {
			return fmt.Errorf("archive fallback failed: %w", storeErr)
		}
		_ = p.client.Expunge(nil)
	}
	return nil
}

func (p *Provider) archiveToFolder(seqSet *imap.SeqSet) error {
	folders := []string{"Archive", "[Gmail]/All Mail"}
	for _, folder := range folders {
		if err := p.client.UidCopy(seqSet, folder); err == nil {
			item := imap.FormatFlagsOp(imap.AddFlags, true)
			flags := []interface{}{imap.DeletedFlag}
			_ = p.client.UidStore(seqSet, item, flags, nil)
			_ = p.client.Expunge(nil)
			return nil
		}
	}
	return fmt.Errorf("no archive folder found")
}

func (p *Provider) PollNewUIDs(ctx context.Context) ([]uint32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized || p.client == nil {
		return nil, nil
	}
	if _, err := p.client.Select("INBOX", false); err != nil {
		return nil, err
	}
	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	uids, err := p.client.UidSearch(criteria)
	if err != nil {
		return nil, err
	}
	var newUIDs []uint32
	for _, uid := range uids {
		if _, seen := p.prevUIDs[uid]; !seen {
			newUIDs = append(newUIDs, uid)
		}
	}
	p.prevUIDs = make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		p.prevUIDs[uid] = struct{}{}
	}
	return newUIDs, nil
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

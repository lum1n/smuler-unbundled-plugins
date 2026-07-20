package email

import (
	"context"
	"time"
)

const (
	ProviderGmail   = "gmail"
	ProviderOutlook = "outlook"
	ProviderICloud  = "icloud"
	ProviderIMAP    = "imap"
)

var wellKnownServers = map[string]struct {
	Server string
	Port   int
}{
	ProviderGmail:   {Server: "imap.gmail.com", Port: 993},
	ProviderOutlook: {Server: "outlook.office365.com", Port: 993},
	ProviderICloud:  {Server: "imap.mail.me.com", Port: 993},
}

var webmailURLs = map[string]string{
	ProviderGmail:   "https://mail.google.com/mail/u/0/#inbox",
	ProviderOutlook: "https://outlook.live.com/mail/0/inbox",
	ProviderICloud:  "https://www.icloud.com/mail/",
}

type Account struct {
	ID       string
	Provider string
	Label    string
	Username string
	Server   string
	Port     int
	TLS      bool
	Password string
}

type EmailMessage struct {
	UID          uint32
	AccountID    string
	AccountLabel string
	Subject      string
	From         string
	Snippet      string
	ReceivedAt   time.Time
	Unread       bool
	WebLink      string
}

type MailProvider interface {
	Account() Account
	Connect() error
	FetchUnread(ctx context.Context, limit int) ([]EmailMessage, error)
	MarkRead(uid uint32) error
	Archive(uid uint32) error
	Close() error
}

func AccountFromConfig(id, provider, label, username, server, portStr, tlsStr, passwd string) Account {
	acc := Account{
		ID:       id,
		Provider: provider,
		Label:    label,
		Username: username,
		Server:   server,
		TLS:      tlsStr != "false" && tlsStr != "0",
		Password: passwd,
	}
	if wellKnown, ok := wellKnownServers[provider]; ok {
		if acc.Server == "" {
			acc.Server = wellKnown.Server
		}
		if portStr == "" {
			acc.Port = wellKnown.Port
		}
	}
	if acc.Port == 0 {
		if port := ParseInt(portStr); port > 0 {
			acc.Port = port
		} else {
			acc.Port = 993
		}
	}
	if acc.Label == "" {
		switch provider {
		case ProviderGmail:
			acc.Label = "Gmail"
		case ProviderOutlook:
			acc.Label = "Outlook"
		case ProviderICloud:
			acc.Label = "iCloud"
		default:
			acc.Label = username
		}
	}
	return acc
}

func WebmailURL(provider string) string {
	if u, ok := webmailURLs[provider]; ok {
		return u
	}
	return ""
}

func ParseInt(s string) int {
	if s == "" {
		return 0
	}
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

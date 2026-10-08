package main

import (
	"errors"
	"fmt"
	"testing"

	email "github.com/lum1n/smuler/plugins/email/internal"
	"github.com/lum1n/smuler/plugins/sdk-go"
)

func TestApplyDefaultPasswordUsesAuthAccountID(t *testing.T) {
	accounts := parseAccounts(map[string]string{"provider": "gmail", "username": "me@example.com"}, nil)
	applyDefaultPassword(accounts, sdk.InitializeParams{Auth: &sdk.AuthContext{AccountID: " app-pw "}})
	if len(accounts) != 1 || accounts[0].Password != "app-pw" {
		t.Fatalf("password not applied: %+v", accounts)
	}
}

func TestApplyDefaultPasswordKeepsExplicitPassword(t *testing.T) {
	accounts := []email.Account{{ID: "default", Password: "explicit"}}
	applyDefaultPassword(accounts, sdk.InitializeParams{Auth: &sdk.AuthContext{AccountID: "other"}})
	if accounts[0].Password != "explicit" {
		t.Fatalf("explicit password overwritten")
	}
}

func TestBuildSnapshotUsesTotalUnreadAndAuthHealth(t *testing.T) {
	h := &handler{maxItems: 2, refreshSeconds: 60}
	msgs := []email.EmailMessage{{UID: 2, AccountID: "a"}, {UID: 1, AccountID: "a"}}
	snap := h.buildSnapshot([]fetchResult{{messages: msgs, total: 42, account: email.Account{ID: "a"}}})
	if snap.Summary.Value != "42 unread" {
		t.Fatalf("value = %q", snap.Summary.Value)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %d", len(snap.Items))
	}

	authErr := fmt.Errorf("%w: login", email.ErrAuthFailed)
	snap = h.buildSnapshot([]fetchResult{{err: authErr, account: email.Account{ID: "a", Label: "Gmail"}}})
	if snap.Health != sdk.HealthAuthReq {
		t.Fatalf("health = %q, want auth_required", snap.Health)
	}

	snap = h.buildSnapshot([]fetchResult{{err: errors.New("dial: timeout"), account: email.Account{ID: "a"}}})
	if snap.Health != sdk.HealthError {
		t.Fatalf("health = %q, want error", snap.Health)
	}
}

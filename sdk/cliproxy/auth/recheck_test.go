package auth

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestRecheckAuthClearsCooldownAndReading(t *testing.T) {
	now := time.Now()
	manager := NewManager(nil, nil, nil)
	store := NewQuotaSignalStore(filepath.Join(t.TempDir(), QuotaSignalFileName))
	manager.SetQuotaSignalStore(store)
	// The recorded state of Claude 7 before its upgrade: 7d spent, rejected, in cooldown for days.
	spent := claudeAuth("claude-7", 0, now.Add(time.Hour), 1.0, now.Add(65*time.Hour), now.Add(-3*time.Hour))
	spent.Unavailable = true
	spent.NextRetryAfter = now.Add(65 * time.Hour)
	spent.Quota.Exceeded = true
	spent.Quota.NextRecoverAt = now.Add(65 * time.Hour)
	spent.LastError = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate_limit_error"}
	if _, err := manager.Register(context.Background(), spent); err != nil {
		t.Fatal(err)
	}
	store.record("claude-7", spent.Quota)

	updated, err := manager.RecheckAuth(context.Background(), "claude-7")
	if err != nil || updated == nil {
		t.Fatalf("RecheckAuth() = %v, %v", updated, err)
	}
	got, _ := manager.GetByID("claude-7")
	if got.Unavailable || got.Quota.Exceeded || !got.NextRetryAfter.IsZero() {
		t.Fatalf("cooldown still set: unavailable=%v exceeded=%v next=%v", got.Unavailable, got.Quota.Exceeded, got.NextRetryAfter)
	}
	if len(got.Quota.Signals) != 0 || !got.Quota.ObservedAt.IsZero() {
		t.Fatalf("reading still set: %v at %v", got.Quota.Signals, got.Quota.ObservedAt)
	}
	if _, saved := store.get("claude-7"); saved {
		t.Fatal("the saved reading was not dropped")
	}
	if pace := PaceOf(got, "claude-opus-5-5", now); pace.Week != 1 || pace.FiveHour != 1 {
		t.Fatalf("pace after recheck = %+v, want full (not heard from)", pace)
	}
	if blocked, _, _ := isAuthBlockedForModel(got, "claude-opus-5-5", now); blocked {
		t.Fatal("the account is still blocked after a recheck")
	}
	if missing, err := manager.RecheckAuth(context.Background(), "no-such"); missing != nil || err != nil {
		t.Fatalf("RecheckAuth(no-such) = %v, %v", missing, err)
	}
}

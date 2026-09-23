package store

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestCustomAPITokenLifecycle(t *testing.T) {
	s := openTestStore(t)
	if err := s.AddAccount("account-a", "pt-a", "", true, ""); err != nil {
		t.Fatal(err)
	}
	account, err := s.GetAccount("account-a")
	if err != nil {
		t.Fatal(err)
	}
	old := account.APIToken
	custom := "sk-custom.token_123~test"
	for i := 0; i < 2; i++ {
		if err := s.UpdateAPIToken("account-a", custom); err != nil {
			t.Fatal(err)
		}
	}
	account, err = s.GetAccountByToken(custom)
	if err != nil || account == nil || account.UserID != "account-a" {
		t.Fatalf("custom token does not resolve: %v", err)
	}
	account, err = s.GetAccountByToken(old)
	if err != nil || account != nil {
		t.Fatal("old token still associated")
	}
	if err := s.AddAccount("account-a", "new-pt", "", false, ""); err != nil {
		t.Fatal(err)
	}
	account, _ = s.GetAccount("account-a")
	if account.APIToken != custom {
		t.Fatal("credential refresh overwrote custom token")
	}
	fresh, err := s.RenewToken("account-a")
	if err != nil || fresh == custom || fresh == "" {
		t.Fatalf("renew failed: %v", err)
	}
	account, err = s.GetAccountByToken(custom)
	if err != nil || account != nil {
		t.Fatal("custom token still associated after random reset")
	}
	if _, err := s.RenewToken("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing account: %v", err)
	}
}

func TestCustomAPITokenValidationIsAtomic(t *testing.T) {
	s := openTestStore(t)
	for _, id := range []string{"account-a", "account-b"} {
		if err := s.AddAccount(id, "pt-"+id, "", false, ""); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := s.GetAccount("account-a")
	b, _ := s.GetAccount("account-b")
	for _, token := range []string{"", "with space", " token", "token\n", "中文", "$(id)", "\"quote", strings.Repeat("a", 257), "joycode", "default"} {
		if err := s.UpdateAPIToken("account-a", token); !errors.Is(err, ErrInvalidAPIToken) {
			t.Fatalf("invalid token accepted: %q (%v)", token, err)
		}
	}
	for _, token := range []string{"account-b", b.APIToken} {
		if err := s.UpdateAPIToken("account-a", token); !errors.Is(err, ErrAPITokenConflict) {
			t.Fatalf("conflicting token accepted: %v", err)
		}
	}
	unchanged, _ := s.GetAccount("account-a")
	if unchanged.APIToken != a.APIToken {
		t.Fatal("failed update changed token")
	}
	if err := s.UpdateAPIToken("missing", "valid"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing account: %v", err)
	}
	if err := s.UpdateAPIToken("account-a", strings.Repeat("a", 256)); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCustomTokenConflict(t *testing.T) {
	s := openTestStore(t)
	for _, id := range []string{"a", "b"} {
		if err := s.AddAccount(id, "pt-"+id, "", false, ""); err != nil {
			t.Fatal(err)
		}
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); results <- s.UpdateAPIToken(id, "shared-token") }(id)
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrAPITokenConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflict=%d", successes, conflicts)
	}
}

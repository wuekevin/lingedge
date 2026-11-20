package repository

import (
	"errors"
	"testing"

	"github.com/wuekevin/lingedge/internal/tenant/internal/domain"
)

func TestNewRejectsNonAppPool(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("nil 池应被拒绝: %v", err)
	}
}

func TestQuotaKeysAreSortedAndKnown(t *testing.T) {
	prev := ""
	for _, k := range domain.QuotaKeys {
		if k <= prev {
			t.Errorf("QuotaKeys 应升序且无重复: %s <= %s", k, prev)
		}
		prev = k
		if !domain.KnownQuotaKey(k) {
			t.Errorf("%s 应已知", k)
		}
	}
	if domain.KnownQuotaKey("bogus") || domain.KnownQuotaKey("") {
		t.Error("未知键不应通过")
	}
}

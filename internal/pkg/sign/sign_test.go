package sign

import (
	"strings"
	"testing"
	"time"
)

func TestRoundtrip(t *testing.T) {
	s := New("secret-key")
	tok := s.Sign("pan|key|fid", time.Hour)
	if err := s.Verify("pan|key|fid", tok); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
}

func TestExpiredRejected(t *testing.T) {
	s := New("secret-key")
	tok := s.Sign("payload", -time.Minute) // 立即过期
	err := s.Verify("payload", tok)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token must be rejected: %v", err)
	}
}

func TestWrongPayloadRejected(t *testing.T) {
	s := New("secret-key")
	tok := s.Sign("a", time.Hour)
	if err := s.Verify("b", tok); err == nil {
		t.Fatal("signature must be bound to payload")
	}
}

func TestTamperedRejected(t *testing.T) {
	s := New("secret-key")
	tok := s.Sign("a", time.Hour)
	if err := s.Verify("a", tok+"x"); err == nil {
		t.Fatal("tampered token must be rejected")
	}
}

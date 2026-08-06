package localauth

import (
	"strings"
	"testing"
	"time"
)

func TestHashAndVerifyPassword(t *testing.T) {
	phc, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$") {
		t.Fatalf("not PHC format: %s", phc)
	}
	if !VerifyPassword("correct horse battery staple", phc) {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword("wrong password", phc) {
		t.Fatal("wrong password accepted")
	}
	// Two hashes of the same password differ (random salt).
	phc2, _ := HashPassword("correct horse battery staple")
	if phc == phc2 {
		t.Fatal("salt is not random")
	}
}

func TestHashRejectsShortPasswords(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("7-char password should be rejected")
	}
}

func TestVerifyRejectsMalformedPHC(t *testing.T) {
	for _, phc := range []string{"", "plaintext", "$argon2id$broken", "$bcrypt$x$y$z$w"} {
		if VerifyPassword("anything", phc) {
			t.Fatalf("malformed hash %q accepted", phc)
		}
	}
}

func TestSessionsLifecycle(t *testing.T) {
	s := NewSessions(time.Hour)
	token, sess, err := s.Create("sanjin", RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Role != RoleOperator {
		t.Fatalf("role: %s", sess.Role)
	}
	got, ok := s.Lookup(token)
	if !ok || got.Username != "sanjin" {
		t.Fatalf("lookup: %v %+v", ok, got)
	}
	if _, ok := s.Lookup("forged-token"); ok {
		t.Fatal("forged token accepted")
	}
	s.Revoke(token)
	if _, ok := s.Lookup(token); ok {
		t.Fatal("revoked token still valid")
	}
}

func TestSessionsExpire(t *testing.T) {
	s := NewSessions(10 * time.Millisecond)
	token, _, _ := s.Create("sanjin", RoleViewer)
	time.Sleep(20 * time.Millisecond)
	if _, ok := s.Lookup(token); ok {
		t.Fatal("expired session still valid")
	}
}

func TestRevokeUserDropsAllSessions(t *testing.T) {
	s := NewSessions(time.Hour)
	t1, _, _ := s.Create("sanjin", RoleOperator)
	t2, _, _ := s.Create("sanjin", RoleOperator)
	t3, _, _ := s.Create("other", RoleViewer)
	s.RevokeUser("sanjin")
	if _, ok := s.Lookup(t1); ok {
		t.Fatal("t1 survived RevokeUser")
	}
	if _, ok := s.Lookup(t2); ok {
		t.Fatal("t2 survived RevokeUser")
	}
	if _, ok := s.Lookup(t3); !ok {
		t.Fatal("other user's session was dropped")
	}
}

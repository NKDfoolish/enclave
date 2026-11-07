package app

import (
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash := hashPassword("correct horse battery staple")

	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	if hashPassword("correct horse battery staple") == hash {
		t.Fatal("hashing the same password twice must use different salts")
	}

	ok, err := verifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("correct password: ok=%v err=%v", ok, err)
	}
	ok, err = verifyPassword("wrong password", hash)
	if err != nil || ok {
		t.Fatalf("wrong password: ok=%v err=%v", ok, err)
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, hash := range []string{"", "plaintext", "$bcrypt$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA"} {
		if _, err := verifyPassword("password", hash); err == nil {
			t.Errorf("verifyPassword(%q) should fail", hash)
		}
	}
}

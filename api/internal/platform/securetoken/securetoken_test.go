package securetoken

import (
	"bytes"
	"testing"
)

func TestNewReturnsUniqueTokensAndMatchingHash(t *testing.T) {
	first, firstHash := New()
	second, _ := New()

	if first == second {
		t.Fatal("two tokens must not be equal")
	}
	if !bytes.Equal(firstHash, Hash(first)) {
		t.Fatal("hash returned by New must equal Hash(token)")
	}
	if len(firstHash) != 32 {
		t.Fatalf("hash length = %d, want 32", len(firstHash))
	}
}

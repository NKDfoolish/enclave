package email

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		input  string
		want   string
		wantOK bool
	}{
		{"alice@example.com", "alice@example.com", true},
		{"  Alice@Example.COM ", "alice@example.com", true},
		{"", "", false},
		{"not-an-email", "", false},
		{"Alice <alice@example.com>", "", false},
		{"alice@", "", false},
	}
	for _, tc := range tests {
		got, ok := Normalize(tc.input)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("Normalize(%q) = (%q, %v), want (%q, %v)", tc.input, got, ok, tc.want, tc.wantOK)
		}
	}
}

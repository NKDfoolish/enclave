// Package email validates and normalises email addresses entered by users.
package email

import (
	"net/mail"
	"strings"
)

const maxLength = 254

// Normalize trims and lower-cases address and reports whether it is a plain email address
// such as "alice@example.com". Display-name forms like "Alice <alice@example.com>" are rejected.
func Normalize(address string) (string, bool) {
	address = strings.ToLower(strings.TrimSpace(address))
	if address == "" || len(address) > maxLength {
		return "", false
	}
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address {
		return "", false
	}
	return address, true
}

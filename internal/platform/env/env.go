// Package env reads settings from environment variables. A Reader collects every problem it
// meets instead of stopping at the first, so a misconfigured service reports all of them at
// startup.
package env

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Reader struct {
	errs []error
}

// String returns the variable, or fallback when it is unset or empty.
func (r *Reader) String(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Required returns the variable and records an error when it is unset or empty.
func (r *Reader) Required(key string) string {
	v := os.Getenv(key)
	if v == "" {
		r.Fail(fmt.Errorf("%s is required", key))
	}
	return v
}

func (r *Reader) Duration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.Fail(fmt.Errorf("%s: %q is not a duration like 10s or 500ms", key, v))
		return fallback
	}
	return d
}

func (r *Reader) Int(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.Fail(fmt.Errorf("%s: %q is not an integer", key, v))
		return fallback
	}
	return n
}

func (r *Reader) Int64(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		r.Fail(fmt.Errorf("%s: %q is not an integer", key, v))
		return fallback
	}
	return n
}

func (r *Reader) Bool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.Fail(fmt.Errorf("%s: %q is not a boolean", key, v))
		return fallback
	}
	return b
}

// Fail records a problem found while validating settings.
func (r *Reader) Fail(err error) {
	r.errs = append(r.errs, err)
}

// Err returns every recorded problem joined, or nil.
func (r *Reader) Err() error {
	return errors.Join(r.errs...)
}

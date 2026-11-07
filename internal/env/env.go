// Package env reads settings from environment variables.
//
// Every setting has a default, so `go run` works with zero setup. A value
// that is set but can't be parsed is an error, not a silent fallback: a typo
// in your config should stop the server, not quietly change how it behaves.
package env

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Loader reads settings and collects every problem it finds, so a program
// can read all of its settings first and then check them once with Err.
type Loader struct {
	errs []error
}

// String returns the value of key, or def if key is unset or empty.
func (l *Loader) String(key, def string) string {
	if v, ok := lookup(key); ok {
		return v
	}
	return def
}

// Duration reads values like "30ms" or "2s".
func (l *Loader) Duration(key string, def time.Duration) time.Duration {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, v, "a duration like 30ms or 2s")
		return def
	}
	return d
}

// Float reads decimal numbers like "0.25".
func (l *Loader) Float(key string, def float64) float64 {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.fail(key, v, "a number like 0.25")
		return def
	}
	return f
}

// Int reads whole numbers like "24".
func (l *Loader) Int(key string, def int) int {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, v, "a whole number like 24")
		return def
	}
	return n
}

// Bool reads values like "1", "true", "yes" or "on" (and their opposites).
func (l *Loader) Bool(key string, def bool) bool {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	l.fail(key, v, "true or false")
	return def
}

// Err returns every invalid setting found so far, or nil if all were fine.
func (l *Loader) Err() error {
	return errors.Join(l.errs...)
}

func (l *Loader) fail(key, value, want string) {
	l.errs = append(l.errs, fmt.Errorf("%s=%q should be %s", key, value, want))
}

func lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	return v, ok && v != ""
}

package env_test

import (
	"strings"
	"testing"
	"time"

	"github.com/GitDaksh/hardcap/internal/env"
)

func TestUsesDefaultsWhenUnset(t *testing.T) {
	t.Setenv("HC_TEST_DURATION", "") // set but empty counts as unset

	var l env.Loader
	if got := l.String("HC_TEST_NEVER_SET", "fallback"); got != "fallback" {
		t.Errorf("String = %q, want the default", got)
	}
	if got := l.Duration("HC_TEST_DURATION", time.Second); got != time.Second {
		t.Errorf("Duration = %v, want the default", got)
	}
	if err := l.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
}

func TestParsesValues(t *testing.T) {
	t.Setenv("HC_TEST_DURATION", "250ms")
	t.Setenv("HC_TEST_FLOAT", "0.25")
	t.Setenv("HC_TEST_INT", "42")

	var l env.Loader
	if got := l.Duration("HC_TEST_DURATION", 0); got != 250*time.Millisecond {
		t.Errorf("Duration = %v, want 250ms", got)
	}
	if got := l.Float("HC_TEST_FLOAT", 0); got != 0.25 {
		t.Errorf("Float = %v, want 0.25", got)
	}
	if got := l.Int("HC_TEST_INT", 0); got != 42 {
		t.Errorf("Int = %v, want 42", got)
	}
	if err := l.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
}

func TestReportsEveryInvalidValue(t *testing.T) {
	t.Setenv("HC_TEST_DURATION", "fast")
	t.Setenv("HC_TEST_INT", "lots")

	var l env.Loader
	l.Duration("HC_TEST_DURATION", time.Second)
	l.Int("HC_TEST_INT", 1)

	err := l.Err()
	if err == nil || !strings.Contains(err.Error(), "HC_TEST_DURATION") || !strings.Contains(err.Error(), "HC_TEST_INT") {
		t.Fatalf("Err = %v, want both bad settings reported", err)
	}
}
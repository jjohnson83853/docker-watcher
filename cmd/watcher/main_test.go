package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRunReturnsZeroForSuccessfulStart(t *testing.T) {
	var stderr bytes.Buffer
	calls := 0
	code := run(func() error {
		calls++
		return nil
	}, &stderr)
	if code != 0 || calls != 1 || stderr.Len() != 0 {
		t.Fatalf("run success = code %d, calls %d, stderr %q", code, calls, stderr.String())
	}
}

func TestRunReportsSanitizedStartFailure(t *testing.T) {
	var stderr bytes.Buffer
	code := run(func() error { return errors.New("private credential detail") }, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "configuration error:") || strings.Contains(stderr.String(), "private credential detail") {
		t.Fatalf("run failure = code %d, stderr %q", code, stderr.String())
	}
}

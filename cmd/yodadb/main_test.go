package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run --help returned %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "Usage: yodadb") {
		t.Fatalf("help output %q does not contain usage", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run --version returned %d, want 0", code)
	}
	if got, want := stdout.String(), "yodadb dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestRunRejectsDatabaseUntilImplemented(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := run([]string{"example.yoda"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run with database returned %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "not implemented") {
		t.Fatalf("error output %q does not explain unsupported operation", stderr.String())
	}
}

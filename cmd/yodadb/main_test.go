package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run --help returned %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") || !strings.Contains(stdout.String(), "inspect") {
		t.Fatalf("help output %q does not contain usage and inspect command", stdout.String())
	}
}

func TestRunInspect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inspect.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Append(page.TypeCatalog, 1, []byte("catalog fixture")); err != nil {
		t.Fatal(err)
	}
	if err := database.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"inspect", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("inspect returned %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, want := range []string{"format version: 1", "page size: 4096", "active metapage: 0", "page count: 3", "page 2: type=catalog"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("inspect output %q does not contain %q", stdout.String(), want)
		}
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

func TestRunInMemorySQLScenario(t *testing.T) {
	input := strings.NewReader("CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL);\n" +
		"INSERT INTO users VALUES (1, 'Ada');\n" +
		"SELECT name FROM users WHERE id = 1;\n")
	var stdout, stderr bytes.Buffer

	if code := runWithInput(nil, input, &stdout, &stderr); code != 0 {
		t.Fatalf("run shell returned %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Ada") {
		t.Fatalf("shell output %q does not contain Ada", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("shell stderr = %q, want empty", stderr.String())
	}
}

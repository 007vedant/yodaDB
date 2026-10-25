package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"yodadb"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer

	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run --help returned %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") || !strings.Contains(stdout.String(), "inspect") || !strings.Contains(stdout.String(), "pages") || !strings.Contains(stdout.String(), "history") {
		t.Fatalf("help output %q does not contain usage and diagnostic commands", stdout.String())
	}
}

func TestRunHistoryListsRangeRootsAndNamedSnapshots(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.yoda")
	database, err := yodadb.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, "INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSnapshot(ctx, "ada-release", 2); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"history", path, "--from", "2", "--to", "3"}, &stdout, &stderr); code != 0 {
		t.Fatalf("history returned %d; stderr=%q", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		"latest transaction: 3",
		"oldest retained transaction: 0",
		"named snapshots: 1",
		`"ada-release" -> transaction 2`,
		"transaction 2: parent=1",
		"transaction 3: parent=2",
		"table 1 root=",
		"transactions shown: 2",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("history output %q does not contain %q", output, want)
		}
	}
	if strings.Contains(output, "transaction 1: parent=") {
		t.Fatalf("history output ignored --from: %q", output)
	}
}

func TestRunHistoryRejectsPrunedRange(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pruned-history.yoda")
	database, err := yodadb.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, "CREATE TABLE users (id INT64 PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, "INSERT INTO users VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := database.RetainHistoryFrom(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"history", path, "--from", "1"}, &stdout, &stderr); code != 2 {
		t.Fatalf("history returned %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "history_not_retained") {
		t.Fatalf("history stderr=%q", stderr.String())
	}
}

func TestRunPagesTree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tree.yoda")
	database, err := manifest.Create(context.Background(), path, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	left := page.NewLeafBuilder(3)
	_ = left.Insert(0, []byte("a"), []byte("one"))
	leftPayload, _ := left.Encode()
	if id, err := database.Append(page.TypeBTreeLeaf, 1, leftPayload); err != nil || id != 2 {
		t.Fatalf("left page = %d, %v", id, err)
	}
	right := page.NewLeafBuilder(0)
	_ = right.Insert(0, []byte("b"), []byte("two"))
	rightPayload, _ := right.Encode()
	if id, err := database.Append(page.TypeBTreeLeaf, 1, rightPayload); err != nil || id != 3 {
		t.Fatalf("right page = %d, %v", id, err)
	}
	root := page.NewInternalBuilder(2)
	_ = root.Insert(0, []byte("b"), 3)
	rootPayload, _ := root.Encode()
	if id, err := database.Append(page.TypeBTreeInternal, 2, rootPayload); err != nil || id != 4 {
		t.Fatalf("root page = %d, %v", id, err)
	}
	if err := database.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"pages", path, "--tree", "4"}, &stdout, &stderr); code != 0 {
		t.Fatalf("pages returned %d, want 0; stderr: %s", code, stderr.String())
	}
	want := "tree root=4\n" +
		"internal page=4 generation=2 separators=1\n" +
		"├── child[0] range=(-inf,0x62)\n" +
		"│   └── leaf page=2 generation=1 entries=1 live=1 tombstones=0 right=3 keys=0x61..0x61\n" +
		"└── child[1] range=[0x62,+inf)\n" +
		"    └── leaf page=3 generation=1 entries=1 live=1 tombstones=0 right=0 keys=0x62..0x62\n"
	if stdout.String() != want {
		t.Fatalf("pages output:\n%s\nwant:\n%s", stdout.String(), want)
	}
}

func TestRunPagesTreeValidatesRootFlag(t *testing.T) {
	for name, args := range map[string][]string{
		"missing": {"pages", "database.yoda"},
		"invalid": {"pages", "database.yoda", "--tree", "users"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Fatalf("run returned %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "tree") {
				t.Fatalf("stderr %q does not explain tree root error", stderr.String())
			}
		})
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

func TestRunPersistentSQLScenarioAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.yoda")
	firstInput := strings.NewReader("CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL);\n" +
		"INSERT INTO users VALUES (1, 'Ada');\n")
	var stdout, stderr bytes.Buffer
	if code := runWithInput([]string{path}, firstInput, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("first process returned %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runWithInput([]string{path}, strings.NewReader("SELECT name FROM users WHERE id = 1;\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("reopen returned %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Ada") || stderr.Len() != 0 {
		t.Fatalf("reopened output=%q stderr=%q", stdout.String(), stderr.String())
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

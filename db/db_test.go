package db_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"yodadb/db"
	"yodadb/dberrors"
)

func TestM1PublicAPIScenario(t *testing.T) {
	database := db.New()
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()

	if _, err := database.Exec(ctx, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	result, err := database.Exec(ctx, "INSERT INTO users VALUES (?, ?)", int64(1), "Ada")
	if err != nil {
		t.Fatal(err)
	}
	if result.RowsAffected() != 1 {
		t.Fatalf("RowsAffected = %d, want 1", result.RowsAffected())
	}
	rows, err := database.Query(ctx, "SELECT name FROM users WHERE id = ?", int64(1))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if got := rows.Columns(); len(got) != 1 || got[0] != "name" {
		t.Fatalf("Columns = %v, want [name]", got)
	}
	if !rows.Next() {
		t.Fatalf("Next = false, error %v", rows.Err())
	}
	var name string
	if err := rows.Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Ada" {
		t.Fatalf("name = %q, want Ada", name)
	}
	if rows.Next() {
		t.Fatal("unexpected second row")
	}
	if values := rows.Values(); values != nil {
		t.Fatalf("Values after exhaustion = %v, want nil", values)
	}
}

func TestInMemoryM4Statements(t *testing.T) {
	database := db.New()
	defer database.Close()
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, email TEXT NOT NULL)")
	mustExec(t, database, "CREATE UNIQUE INDEX users_email ON users (email)")
	mustExec(t, database, "INSERT INTO users VALUES (1, 'ada@example.com')")
	if result := mustExec(t, database, "UPDATE users SET email = 'grace@example.com' WHERE id = 1"); result.RowsAffected() != 1 {
		t.Fatalf("UPDATE affected %d", result.RowsAffected())
	}
	assertPlan(t, database, "EXPLAIN SELECT id FROM users WHERE email = 'grace@example.com'", "INDEX LOOKUP users_email ON users(email)")
	if result := mustExec(t, database, "DELETE FROM users WHERE id = 1"); result.RowsAffected() != 1 {
		t.Fatalf("DELETE affected %d", result.RowsAffected())
	}
	assertNoRow(t, database, "SELECT id FROM users WHERE id = 1")
}

func TestPersistentM4DMLIndexesExplainAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m4.yoda")
	database, err := db.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, email TEXT NOT NULL, score INT64)")
	mustExec(t, database, "CREATE UNIQUE INDEX users_email ON users (email)")
	mustExec(t, database, "CREATE INDEX users_score ON users (score)")
	for id := int64(1); id <= 40; id++ {
		mustExec(t, database, "INSERT INTO users VALUES (?, ?, ?)", id, fmt.Sprintf("user-%d@example.com", id), id*10)
	}
	for id := int64(1); id <= 40; id++ {
		switch id % 3 {
		case 0:
			result := mustExec(t, database, "DELETE FROM users WHERE id = ?", id)
			if result.RowsAffected() != 1 {
				t.Fatalf("delete %d affected %d rows", id, result.RowsAffected())
			}
		case 1:
			result := mustExec(t, database, "UPDATE users SET score = ? WHERE id = ?", id*100, id)
			if result.RowsAffected() != 1 {
				t.Fatalf("update %d affected %d rows", id, result.RowsAffected())
			}
		}
	}

	if _, err := database.Exec(ctx, "INSERT INTO users VALUES (?, ?, ?)", int64(100), "user-1@example.com", int64(1)); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate unique INSERT error = %v, want constraint", err)
	}
	assertNoRow(t, database, "SELECT id FROM users WHERE id = 100")
	if _, err := database.Exec(ctx, "UPDATE users SET email = 'user-2@example.com' WHERE id = 1"); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("duplicate unique UPDATE error = %v, want constraint", err)
	}
	assertOneInt64(t, database, "SELECT id FROM users WHERE email = 'user-1@example.com'", 1)
	assertPlan(t, database, "EXPLAIN SELECT id FROM users WHERE email = 'user-1@example.com'", "INDEX LOOKUP users_email ON users(email)")
	assertPlan(t, database, "EXPLAIN SELECT id FROM users WHERE score >= 10", "INDEX RANGE SCAN users_score ON users(score)")
	assertPlan(t, database, "EXPLAIN SELECT id FROM users WHERE id = 1", "PRIMARY KEY LOOKUP users(id)")
	assertPlan(t, database, "EXPLAIN SELECT id FROM users WHERE email != 'nobody'", "TABLE SCAN users")

	// These lookups exercise the secondary index and compare it with the model
	// implied by the primary-key mutations above.
	for id := int64(1); id <= 40; id++ {
		rows, err := database.Query(ctx, "SELECT id, score FROM users WHERE email = ?", fmt.Sprintf("user-%d@example.com", id))
		if err != nil {
			t.Fatal(err)
		}
		found := rows.Next()
		if id%3 == 0 {
			if found {
				t.Fatalf("deleted id %d remains in secondary index", id)
			}
		} else {
			if !found {
				t.Fatalf("id %d missing through secondary index: %v", id, rows.Err())
			}
			var gotID, score int64
			if err := rows.Scan(&gotID, &score); err != nil {
				t.Fatal(err)
			}
			wantScore := id * 10
			if id%3 == 1 {
				wantScore = id * 100
			}
			if gotID != id || score != wantScore {
				t.Fatalf("id=%d score=%d, want id=%d score=%d", gotID, score, id, wantScore)
			}
		}
		_ = rows.Close()
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	assertOneInt64(t, database, "SELECT score FROM users WHERE email = 'user-1@example.com'", 100)
}

func TestPersistentIndexesAgreeAfterRandomizedDML(t *testing.T) {
	ctx := context.Background()
	database, err := db.Create(ctx, filepath.Join(t.TempDir(), "random.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE items (id INT64 PRIMARY KEY, code TEXT NOT NULL, value INT64)")
	mustExec(t, database, "CREATE UNIQUE INDEX items_code ON items (code)")
	mustExec(t, database, "CREATE INDEX items_value ON items (value)")

	type item struct {
		code  string
		value int64
	}
	model := make(map[int64]item)
	random := rand.New(rand.NewSource(44))
	for operation := 0; operation < 160; operation++ {
		id := int64(random.Intn(32) + 1)
		switch random.Intn(3) {
		case 0:
			if _, exists := model[id]; exists {
				continue
			}
			entry := item{code: fmt.Sprintf("code-%d", id), value: int64(random.Intn(1000))}
			mustExec(t, database, "INSERT INTO items VALUES (?, ?, ?)", id, entry.code, entry.value)
			model[id] = entry
		case 1:
			entry, exists := model[id]
			if !exists {
				continue
			}
			entry.value = int64(random.Intn(1000))
			mustExec(t, database, "UPDATE items SET value = ? WHERE id = ?", entry.value, id)
			model[id] = entry
		case 2:
			if _, exists := model[id]; !exists {
				continue
			}
			mustExec(t, database, "DELETE FROM items WHERE id = ?", id)
			delete(model, id)
		}
	}
	for id := int64(1); id <= 32; id++ {
		entry, exists := model[id]
		rows, err := database.Query(ctx, "SELECT id, value FROM items WHERE code = ?", fmt.Sprintf("code-%d", id))
		if err != nil {
			t.Fatal(err)
		}
		found := rows.Next()
		if found != exists {
			t.Fatalf("secondary lookup id %d found=%t, model=%t", id, found, exists)
		}
		if found {
			var gotID, gotValue int64
			if err := rows.Scan(&gotID, &gotValue); err != nil {
				t.Fatal(err)
			}
			if gotID != id || gotValue != entry.value {
				t.Fatalf("secondary row=(%d,%d), want=(%d,%d)", gotID, gotValue, id, entry.value)
			}
		}
		_ = rows.Close()
	}
}

func TestPersistentIndexRangeBoundaries(t *testing.T) {
	database, err := db.Create(context.Background(), filepath.Join(t.TempDir(), "ranges.yoda"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mustExec(t, database, "CREATE TABLE values_table (id INT64 PRIMARY KEY, score INT64 NOT NULL)")
	mustExec(t, database, "CREATE INDEX values_score ON values_table (score)")
	for _, pair := range [][2]int64{{1, 10}, {2, 20}, {3, 20}, {4, 30}} {
		mustExec(t, database, "INSERT INTO values_table VALUES (?, ?)", pair[0], pair[1])
	}
	assertIDs(t, database, "SELECT id FROM values_table WHERE score < 20", []int64{1})
	assertIDs(t, database, "SELECT id FROM values_table WHERE score <= 20", []int64{1, 2, 3})
	assertIDs(t, database, "SELECT id FROM values_table WHERE score > 20", []int64{4})
	assertIDs(t, database, "SELECT id FROM values_table WHERE score >= 20", []int64{2, 3, 4})
	assertIDs(t, database, "SELECT id FROM values_table WHERE 20 < score", []int64{4})
	assertIDs(t, database, "SELECT id FROM values_table WHERE id >= 3", []int64{3, 4})
}

func mustExec(t *testing.T, database *db.DB, query string, arguments ...any) db.Result {
	t.Helper()
	result, err := database.Exec(context.Background(), query, arguments...)
	if err != nil {
		t.Fatalf("Exec(%q): %v", query, err)
	}
	return result
}

func assertPlan(t *testing.T, database *db.DB, query, want string) {
	t.Helper()
	rows, err := database.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("EXPLAIN returned no row: %v", rows.Err())
	}
	var got string
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("plan = %q, want %q", got, want)
	}
}

func assertNoRow(t *testing.T, database *db.DB, query string) {
	t.Helper()
	rows, err := database.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatalf("query %q unexpectedly returned %v", query, rows.Values())
	}
}

func assertOneInt64(t *testing.T, database *db.DB, query string, want int64) {
	t.Helper()
	rows, err := database.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("query %q returned no row: %v", query, rows.Err())
	}
	var got int64
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query %q = %d, want %d", query, got, want)
	}
}

func assertIDs(t *testing.T, database *db.DB, query string, want []int64) {
	t.Helper()
	rows, err := database.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if len(got) != len(want) {
		t.Fatalf("query %q returned %v, want %v", query, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("query %q returned %v, want %v", query, got, want)
		}
	}
}

func TestPersistentM1ScenarioSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persistent.yoda")
	database, err := db.Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(context.Background(), "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(context.Background(), "INSERT INTO users VALUES (?, ?)", int64(1), "Ada"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows, err := database.Query(context.Background(), "SELECT name FROM users WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("missing persisted row: %v", rows.Err())
	}
	var name string
	if err := rows.Scan(&name); err != nil || name != "Ada" {
		t.Fatalf("persisted name=%q error=%v", name, err)
	}
}

func TestRowsScanRejectsWrongDestinationType(t *testing.T) {
	database := db.New()
	ctx := context.Background()
	if _, err := database.Exec(ctx, "CREATE TABLE values_table (value INT64)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, "INSERT INTO values_table VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Query(ctx, "SELECT value FROM values_table")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("missing row")
	}
	var wrong string
	if err := rows.Scan(&wrong); !errors.Is(err, dberrors.ErrType) {
		t.Fatalf("Scan error = %v, want type category", err)
	}
}

func TestPublicAPIReturnsStableErrorCategories(t *testing.T) {
	database := db.New()
	ctx := context.Background()
	if _, err := database.Exec(ctx, "CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(ctx, "INSERT INTO users VALUES (1, 'Ada')"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		sql      string
		category error
	}{
		{"malformed", "SELEC name FROM users", dberrors.ErrSyntax},
		{"invalid type", "INSERT INTO users VALUES (2, 99)", dberrors.ErrType},
		{"duplicate key", "INSERT INTO users VALUES (1, 'Grace')", dberrors.ErrConstraint},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := database.Exec(ctx, test.sql)
			if !errors.Is(err, test.category) {
				t.Fatalf("Exec error = %v, want category %v", err, test.category)
			}
		})
	}
}

func TestQueryDoesNotExecuteMutation(t *testing.T) {
	database := db.New()
	_, err := database.Query(context.Background(), "CREATE TABLE users (id INT64)")
	if !errors.Is(err, dberrors.ErrUnsupported) {
		t.Fatalf("Query CREATE error = %v, want unsupported", err)
	}
	if _, err := database.Exec(context.Background(), "CREATE TABLE users (id INT64)"); err != nil {
		t.Fatalf("CREATE was executed by Query: %v", err)
	}
}

func TestClosedDatabaseRejectsOperations(t *testing.T) {
	database := db.New()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := database.Exec(context.Background(), "CREATE TABLE t (id INT64)")
	if !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("Exec after Close error = %v, want closed category", err)
	}
}

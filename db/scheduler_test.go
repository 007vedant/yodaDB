package db_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"yodadb/db"
	"yodadb/dberrors"
)

type writerPhase uint8

const (
	phaseBegin writerPhase = iota
	phaseWrite
	phaseCommit
)

type writerStep struct {
	writer int
	phase  writerPhase
}

func TestDeterministicRowWriterInterleavings(t *testing.T) {
	schedules := twoWriterSchedules()
	if len(schedules) != 20 {
		t.Fatalf("schedule count=%d, want 20", len(schedules))
	}
	scenarios := []struct {
		name      string
		rowByTx   [2]int64
		valueByTx [2]string
	}{
		{name: "disjoint", rowByTx: [2]int64{1, 2}, valueByTx: [2]string{"left", "right"}},
		{name: "same_row", rowByTx: [2]int64{1, 1}, valueByTx: [2]string{"left", "right"}},
	}

	for _, scenario := range scenarios {
		for _, schedule := range schedules {
			t.Run(scenario.name+"/"+scheduleName(schedule), func(t *testing.T) {
				ctx := context.Background()
				database := newScheduledDatabase(t, ctx)
				defer database.Close()
				mustExec(t, database, "CREATE TABLE accounts (id INT64 PRIMARY KEY, value TEXT NOT NULL)")
				mustExec(t, database, "INSERT INTO accounts VALUES (1, 'one')")
				mustExec(t, database, "INSERT INTO accounts VALUES (2, 'two')")

				model := map[int64]string{1: "one", 2: "two"}
				lastWrite := make(map[int64]int)
				revision := 0
				var transactions [2]*db.Tx
				var bases [2]int

				for _, step := range schedule {
					writer := step.writer
					switch step.phase {
					case phaseBegin:
						transaction, err := database.Begin(ctx)
						if err != nil {
							t.Fatal(err)
						}
						transactions[writer] = transaction
						bases[writer] = revision

					case phaseWrite:
						query := fmt.Sprintf("UPDATE accounts SET value = '%s' WHERE id = %d", scenario.valueByTx[writer], scenario.rowByTx[writer])
						if _, err := transactions[writer].Exec(ctx, query); err != nil {
							t.Fatal(err)
						}

					case phaseCommit:
						rowID := scenario.rowByTx[writer]
						wantConflict := lastWrite[rowID] > bases[writer]
						err := transactions[writer].Commit(ctx)
						assertScheduledCommit(t, err, wantConflict)
						if !wantConflict {
							revision++
							lastWrite[rowID] = revision
							model[rowID] = scenario.valueByTx[writer]
						}
					}
					assertAccountModel(t, database, model)
				}
			})
		}
	}
}

func TestDeterministicUniqueKeyInterleavings(t *testing.T) {
	ctx := context.Background()
	for _, schedule := range twoWriterSchedules() {
		t.Run(scheduleName(schedule), func(t *testing.T) {
			database := newScheduledDatabase(t, ctx)
			defer database.Close()
			mustExec(t, database, "CREATE TABLE users (id INT64 PRIMARY KEY, email TEXT)")
			mustExec(t, database, "CREATE UNIQUE INDEX users_email ON users (email)")

			const email = "same@example.com"
			model := make(map[int64]string)
			lastWrite := 0
			revision := 0
			var transactions [2]*db.Tx
			var bases [2]int
			var seesKey [2]bool
			var prepared [2]bool

			for _, step := range schedule {
				writer := step.writer
				switch step.phase {
				case phaseBegin:
					transaction, err := database.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					transactions[writer] = transaction
					bases[writer] = revision
					seesKey[writer] = len(model) != 0

				case phaseWrite:
					query := fmt.Sprintf("INSERT INTO users VALUES (%d, '%s')", writer+1, email)
					_, err := transactions[writer].Exec(ctx, query)
					if seesKey[writer] {
						if !errors.Is(err, dberrors.ErrConstraint) {
							t.Fatalf("visible duplicate error=%v, want constraint", err)
						}
					} else if err != nil {
						t.Fatal(err)
					} else {
						prepared[writer] = true
					}

				case phaseCommit:
					wantConflict := prepared[writer] && lastWrite > bases[writer]
					err := transactions[writer].Commit(ctx)
					assertScheduledCommit(t, err, wantConflict)
					if prepared[writer] && !wantConflict {
						revision++
						lastWrite = revision
						model[int64(writer+1)] = email
					}
				}
				assertUserModel(t, database, model)
			}
		})
	}
}

func TestDeterministicCatalogNameInterleavings(t *testing.T) {
	ctx := context.Background()
	for _, schedule := range twoWriterSchedules() {
		t.Run(scheduleName(schedule), func(t *testing.T) {
			database := newScheduledDatabase(t, ctx)
			defer database.Close()

			created := false
			lastWrite := 0
			revision := 0
			var transactions [2]*db.Tx
			var bases [2]int
			var seesTable [2]bool
			var prepared [2]bool

			for _, step := range schedule {
				writer := step.writer
				switch step.phase {
				case phaseBegin:
					transaction, err := database.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					transactions[writer] = transaction
					bases[writer] = revision
					seesTable[writer] = created

				case phaseWrite:
					_, err := transactions[writer].Exec(ctx, "CREATE TABLE shared (id INT64 PRIMARY KEY)")
					if seesTable[writer] {
						if !errors.Is(err, dberrors.ErrConstraint) {
							t.Fatalf("visible duplicate table error=%v, want constraint", err)
						}
					} else if err != nil {
						t.Fatal(err)
					} else {
						prepared[writer] = true
					}

				case phaseCommit:
					wantConflict := prepared[writer] && lastWrite > bases[writer]
					err := transactions[writer].Commit(ctx)
					assertScheduledCommit(t, err, wantConflict)
					if prepared[writer] && !wantConflict {
						revision++
						lastWrite = revision
						created = true
					}
				}
				assertTableVisibility(t, database, created)
			}
		})
	}
}

func TestSnapshotIsolationAllowsWriteSkew(t *testing.T) {
	ctx := context.Background()
	database := newScheduledDatabase(t, ctx)
	defer database.Close()
	mustExec(t, database, "CREATE TABLE doctors (id INT64 PRIMARY KEY, on_call TEXT NOT NULL)")
	mustExec(t, database, "INSERT INTO doctors VALUES (1, 'on')")
	mustExec(t, database, "INSERT INTO doctors VALUES (2, 'on')")

	left, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	right, err := database.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertTxStrings(t, left, "SELECT on_call FROM doctors WHERE id >= 1", []string{"on", "on"})
	assertTxStrings(t, right, "SELECT on_call FROM doctors WHERE id >= 1", []string{"on", "on"})
	if _, err := left.Exec(ctx, "UPDATE doctors SET on_call = 'off' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := right.Exec(ctx, "UPDATE doctors SET on_call = 'off' WHERE id = 2"); err != nil {
		t.Fatal(err)
	}
	if err := left.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := right.Commit(ctx); err != nil {
		t.Fatalf("disjoint write-skew commit: %v", err)
	}
	assertStrings(t, database, "SELECT on_call FROM doctors WHERE id >= 1", []string{"off", "off"})
}

func twoWriterSchedules() [][]writerStep {
	var schedules [][]writerStep
	var visit func([]writerStep, [2]int)
	visit = func(prefix []writerStep, next [2]int) {
		if len(prefix) == 6 {
			schedules = append(schedules, append([]writerStep(nil), prefix...))
			return
		}
		for writer := range 2 {
			if next[writer] == 3 {
				continue
			}
			step := writerStep{writer: writer, phase: writerPhase(next[writer])}
			nextState := next
			nextState[writer]++
			visit(append(prefix, step), nextState)
		}
	}
	visit(nil, [2]int{})
	return schedules
}

func scheduleName(schedule []writerStep) string {
	parts := make([]string, len(schedule))
	phaseNames := [...]string{"begin", "write", "commit"}
	for index, step := range schedule {
		parts[index] = fmt.Sprintf("%c-%s", 'A'+rune(step.writer), phaseNames[step.phase])
	}
	return strings.Join(parts, "_")
}

func newScheduledDatabase(t *testing.T, ctx context.Context) *db.DB {
	t.Helper()
	database, err := db.CreateWithOptions(ctx, filepath.Join(t.TempDir(), "schedule.yoda"), db.Options{Durability: db.DurabilityProcess})
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func assertScheduledCommit(t *testing.T, err error, wantConflict bool) {
	t.Helper()
	if !wantConflict {
		if err != nil {
			t.Fatalf("commit error=%v, want success", err)
		}
		return
	}
	var conflict *db.ConflictError
	if !errors.As(err, &conflict) || conflict.Kind != db.ConflictWriteWrite {
		t.Fatalf("commit error=%v, want typed write conflict", err)
	}
}

func assertAccountModel(t *testing.T, database *db.DB, model map[int64]string) {
	t.Helper()
	want := []string{model[1], model[2]}
	assertStrings(t, database, "SELECT value FROM accounts WHERE id >= 1", want)
}

func assertUserModel(t *testing.T, database *db.DB, model map[int64]string) {
	t.Helper()
	rows, err := database.Query(context.Background(), "SELECT id FROM users WHERE id >= 1")
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var want []int64
	for id := int64(1); id <= 2; id++ {
		if _, exists := model[id]; exists {
			want = append(want, id)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("committed user IDs=%v, want %v", got, want)
	}
}

func assertTableVisibility(t *testing.T, database *db.DB, created bool) {
	t.Helper()
	rows, err := database.Query(context.Background(), "SELECT id FROM shared")
	if !created {
		if !errors.Is(err, dberrors.ErrBind) {
			t.Fatalf("uncommitted table query error=%v, want bind error", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("new table unexpectedly contains a row")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

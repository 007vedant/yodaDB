package manifest_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"yodadb/dberrors"
	"yodadb/internal/testutil/faultvfs"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

const durabilityDatabasePath = "durability.yoda"

func TestFullDurabilityIsDefaultAndSurvivesPowerCrash(t *testing.T) {
	filesystem, err := faultvfs.New(faultvfs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	database, err := manifest.CreateWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := database.Durability(); got != manifest.DurabilityFull {
		t.Fatalf("Durability = %v, want full", got)
	}
	if err := filesystem.Arm(faultvfs.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Publish(context.Background(), durabilityManifest(1)); err != nil {
		t.Fatal(err)
	}
	if got, want := operationKinds(filesystem.Operations()), []faultvfs.Kind{
		faultvfs.Write, faultvfs.Sync, faultvfs.Write, faultvfs.Sync,
	}; !slices.Equal(got, want) {
		t.Fatalf("full commit operations = %v, want %v", got, want)
	}

	filesystem.Crash()
	reopened, err := manifest.OpenWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{})
	if err != nil {
		t.Fatalf("Open after power crash: %v", err)
	}
	defer reopened.Close()
	assertLatestTxn(t, reopened, 1)
}

func TestProcessDurabilityCreateOmitsBarrierAndSurvivesProcessCrash(t *testing.T) {
	filesystem, err := faultvfs.New(faultvfs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	database, err := manifest.CreateWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{
		Durability: manifest.DurabilityProcess,
	})
	if err != nil {
		t.Fatal(err)
	}
	if database.Durability() != manifest.DurabilityProcess {
		t.Fatalf("created database durability = %v, want process", database.Durability())
	}
	if slices.Contains(operationKinds(filesystem.Operations()), faultvfs.Sync) {
		t.Fatalf("process-durable create unexpectedly synchronized: %+v", filesystem.Operations())
	}
	filesystem.ProcessCrash()

	reopened, err := manifest.OpenWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{
		Durability: manifest.DurabilityProcess,
	})
	if err != nil {
		t.Fatalf("Open after process crash: %v", err)
	}
	defer reopened.Close()
	if _, found, err := reopened.LatestManifest(); err != nil || found {
		t.Fatalf("LatestManifest = (_, %t, %v), want empty database", found, err)
	}
}

func TestProcessDurabilitySurvivesProcessButNotPowerCrash(t *testing.T) {
	filesystem, database := openDurabilityFixture(t, manifest.DurabilityProcess, faultvfs.Config{})
	if got := database.Durability(); got != manifest.DurabilityProcess {
		t.Fatalf("Durability = %v, want process", got)
	}
	if _, err := database.Publish(context.Background(), durabilityManifest(2)); err != nil {
		t.Fatal(err)
	}
	if got, want := operationKinds(filesystem.Operations()), []faultvfs.Kind{faultvfs.Write, faultvfs.Write}; !slices.Equal(got, want) {
		t.Fatalf("process commit operations = %v, want %v", got, want)
	}

	filesystem.ProcessCrash()
	reopened, err := manifest.OpenWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{})
	if err != nil {
		t.Fatalf("Open after process crash: %v", err)
	}
	assertLatestTxn(t, reopened, 2)
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	filesystem.Crash()
	reopened, err = manifest.OpenWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{})
	if err != nil {
		t.Fatalf("Open after later power crash: %v", err)
	}
	defer reopened.Close()
	assertLatestTxn(t, reopened, 1)
}

func TestProcessDurabilityCrashBoundariesRecoverOldOrNew(t *testing.T) {
	for boundary := 1; boundary <= 2; boundary++ {
		t.Run(string(rune('0'+boundary)), func(t *testing.T) {
			filesystem, database := openDurabilityFixture(t, manifest.DurabilityProcess, faultvfs.Config{
				ProcessCrashAfter: uint64(boundary),
			})
			if _, err := database.Publish(context.Background(), durabilityManifest(2)); !errors.Is(err, faultvfs.ErrCrash) {
				t.Fatalf("Publish error = %v, want process crash", err)
			}
			reopened, err := manifest.OpenWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{})
			if err != nil {
				t.Fatalf("Open after process crash: %v", err)
			}
			defer reopened.Close()
			want := uint64(1)
			if boundary == 2 {
				want = 2
			}
			assertLatestTxn(t, reopened, want)
		})
	}
}

func TestProcessModeExplicitSyncStillCreatesBarrier(t *testing.T) {
	filesystem, database := openDurabilityFixture(t, manifest.DurabilityProcess, faultvfs.Config{})
	if err := database.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, want := operationKinds(filesystem.Operations()), []faultvfs.Kind{faultvfs.Sync}; !slices.Equal(got, want) {
		t.Fatalf("explicit Sync operations = %v, want %v", got, want)
	}
}

func TestDurabilityModeValidationPrecedesFileOpen(t *testing.T) {
	filesystem, err := faultvfs.New(faultvfs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	invalid := manifest.Durability(99)
	if _, err := manifest.CreateWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{Durability: invalid}); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("Create invalid durability error = %v, want constraint", err)
	}
	if _, err := manifest.OpenWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{Durability: invalid}); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("Open invalid durability error = %v, want constraint", err)
	}
	if operations := filesystem.Operations(); len(operations) != 0 {
		t.Fatalf("invalid options performed operations: %+v", operations)
	}
}

func openDurabilityFixture(t *testing.T, durability manifest.Durability, config faultvfs.Config) (*faultvfs.FS, *manifest.Database) {
	t.Helper()
	filesystem, err := faultvfs.New(faultvfs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	database, err := manifest.CreateWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Publish(context.Background(), durabilityManifest(1)); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = manifest.OpenWithVFS(context.Background(), filesystem, durabilityDatabasePath, manifest.Options{Durability: durability})
	if err != nil {
		t.Fatal(err)
	}
	if err := filesystem.Arm(config); err != nil {
		t.Fatal(err)
	}
	return filesystem, database
}

func durabilityManifest(txnID uint64) manifest.Manifest {
	return manifest.Manifest{
		TxnID: txnID, ParentTxnID: txnID - 1,
		TableRoots: map[uint64]page.ID{}, IndexRoots: map[uint64]page.ID{},
	}
}

func operationKinds(operations []faultvfs.Operation) []faultvfs.Kind {
	kinds := make([]faultvfs.Kind, len(operations))
	for index, operation := range operations {
		kinds[index] = operation.Kind
	}
	return kinds
}

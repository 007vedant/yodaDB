package bench

import (
	"context"
	"errors"
	"hash/crc64"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yodadb/db"
	"yodadb/dberrors"
)

var (
	benchmarkPage     = make([]byte, 4096)
	benchmarkCRCTable = crc64.MakeTable(crc64.ECMA)
	benchmarkChecksum uint64
	benchmarkError    error
)

func BenchmarkCRC64Page(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchmarkPage)))
	for b.Loop() {
		benchmarkChecksum = crc64.Checksum(benchmarkPage, benchmarkCRCTable)
	}
}

func BenchmarkStructuredErrorWrap(b *testing.B) {
	cause := errors.New("disk failure")
	b.ReportAllocs()
	for b.Loop() {
		benchmarkError = dberrors.Wrap(cause, dberrors.CodeIO, "pager.read", "read page")
	}
}

func BenchmarkWriterContention(b *testing.B) {
	b.Run("disjoint_keys", func(b *testing.B) {
		benchmarkConcurrentWriters(b, false)
	})
	b.Run("hot_key", func(b *testing.B) {
		benchmarkConcurrentWriters(b, true)
	})
}

func benchmarkConcurrentWriters(b *testing.B, hotKey bool) {
	ctx := context.Background()
	database, err := db.CreateWithOptions(ctx, filepath.Join(b.TempDir(), "contention.yoda"), db.Options{Durability: db.DurabilityProcess})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	if _, err := database.Exec(ctx, "CREATE TABLE items (id INT64 PRIMARY KEY, value INT64 NOT NULL)"); err != nil {
		b.Fatal(err)
	}
	if hotKey {
		if _, err := database.Exec(ctx, "INSERT INTO items VALUES (1, 0)"); err != nil {
			b.Fatal(err)
		}
	}

	latencies := make([]int64, b.N)
	var nextID atomic.Uint64
	var completed atomic.Uint64
	var commits atomic.Uint64
	var aborts atomic.Uint64
	var writers atomic.Uint64
	var failureOnce sync.Once
	var failure error

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		writers.Add(1)
		for pb.Next() {
			started := time.Now()
			id := nextID.Add(1)
			transaction, err := database.Begin(ctx)
			if err == nil {
				if hotKey {
					_, err = transaction.Exec(ctx, "UPDATE items SET value = ? WHERE id = 1", int64(id))
				} else {
					_, err = transaction.Exec(ctx, "INSERT INTO items VALUES (?, ?)", int64(id), int64(1))
				}
			}
			if err == nil {
				err = transaction.Commit(ctx)
			} else if transaction != nil {
				_ = transaction.Rollback()
			}

			index := completed.Add(1) - 1
			latencies[index] = time.Since(started).Nanoseconds()
			if err == nil {
				commits.Add(1)
				continue
			}
			var conflict *db.ConflictError
			if errors.As(err, &conflict) {
				aborts.Add(1)
				continue
			}
			failureOnce.Do(func() { failure = err })
		}
	})
	b.StopTimer()

	if failure != nil {
		b.Fatalf("writer workload: %v", failure)
	}
	total := int(completed.Load())
	if total != b.N {
		b.Fatalf("completed attempts=%d, want %d", total, b.N)
	}
	sort.Slice(latencies, func(left, right int) bool { return latencies[left] < latencies[right] })
	b.ReportMetric(percentileLatency(latencies, 50), "p50-ns/op")
	b.ReportMetric(percentileLatency(latencies, 95), "p95-ns/op")
	b.ReportMetric(percentileLatency(latencies, 99), "p99-ns/op")
	b.ReportMetric(100*float64(aborts.Load())/float64(total), "abort_%")
	b.ReportMetric(100*float64(commits.Load())/float64(total), "commit_%")
	b.ReportMetric(float64(writers.Load()), "writers")
}

func percentileLatency(sorted []int64, percentile int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := (len(sorted)*percentile+99)/100 - 1
	return float64(sorted[index])
}

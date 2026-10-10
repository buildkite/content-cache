package metadb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// BenchmarkMetadataDisk includes durable commits, avoiding the misleading
// NoSync/tmpfs speedup. Set TMPDIR to an actual disk for before/after runs.
func BenchmarkMetadataDisk(b *testing.B) {
	for _, workers := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("writers=%d", workers), func(b *testing.B) {
			db := NewBoltDB()
			if err := db.Open(filepath.Join(b.TempDir(), "metadata.db")); err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			configureBenchmarkBatch(b, db.DB())
			keys := make([]string, 128)
			for i := range keys {
				keys[i] = fmt.Sprintf("package-%03d", i)
			}
			env := &MetadataEnvelope{EnvelopeVersion: 1, Payload: make([]byte, 4096), PayloadSize: 4096, ExpiresAtUnixMs: time.Now().Add(time.Hour).UnixMilli()}
			ctx := context.Background()
			for _, key := range keys {
				if err := db.PutEnvelope(ctx, "npm", "metadata", key, env); err != nil {
					b.Fatal(err)
				}
			}
			var txBefore, txAfter int
			_ = db.DB().View(func(tx *bbolt.Tx) error { txBefore = tx.ID(); return nil })
			var next atomic.Int64
			durations := make([]time.Duration, b.N)
			jobs := make(chan int, workers)
			errors := make(chan error, workers)
			b.ResetTimer()
			for range workers {
				go func() {
					var writeErr error
					for i := range jobs {
						start := time.Now()
						err := db.PutEnvelope(ctx, "npm", "metadata", keys[i%len(keys)], env)
						durations[i] = time.Since(start)
						if err != nil {
							writeErr = err
						}
						next.Add(1)
					}
					errors <- writeErr
				}()
			}
			for i := range b.N {
				jobs <- i
			}
			close(jobs)
			for range workers {
				if err := <-errors; err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			_ = db.DB().View(func(tx *bbolt.Tx) error { txAfter = tx.ID(); return nil })
			b.ReportMetric(float64(txAfter-txBefore)/float64(next.Load()), "commits/op")
			slices.Sort(durations)
			b.ReportMetric(float64(durations[len(durations)/2].Microseconds()), "p50-us")
			b.ReportMetric(float64(durations[(len(durations)-1)*95/100].Microseconds()), "p95-us")
		})
	}
}

func configureBenchmarkBatch(b *testing.B, db *bbolt.DB) {
	b.Helper()
	if value := os.Getenv("CONTENT_CACHE_BENCH_BATCH_DELAY"); value != "" {
		delay, err := time.ParseDuration(value)
		if err != nil {
			b.Fatal(err)
		}
		db.MaxBatchDelay = delay
	}
	if value := os.Getenv("CONTENT_CACHE_BENCH_BATCH_SIZE"); value != "" {
		size, err := strconv.Atoi(value)
		if err != nil {
			b.Fatal(err)
		}
		db.MaxBatchSize = size
	}
}

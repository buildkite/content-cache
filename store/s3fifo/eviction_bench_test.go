package s3fifo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/backend"
	"github.com/buildkite/content-cache/store/metadb"
)

type delayedDeleteBackend struct {
	backend.Backend
	started chan struct{}
}

func (d *delayedDeleteBackend) Delete(ctx context.Context, key string) error {
	d.started <- struct{}{}
	time.Sleep(20 * time.Millisecond)
	return d.Backend.Delete(ctx, key)
}

// Isolate admission blocking during slow deletion. NoSync and batches of one
// are deliberate here; durable transaction throughput is measured separately.
func BenchmarkAdmissionDuringEviction(b *testing.B) {
	db := metadb.NewBoltDB(metadb.WithNoSync(true))
	root := b.TempDir()
	if err := db.Open(filepath.Join(root, "metadata.db")); err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	db.DB().MaxBatchSize = 1
	queues, err := NewBoltQueues(db.DB())
	if err != nil {
		b.Fatal(err)
	}
	fs, err := backend.NewFilesystem(filepath.Join(root, "blobs"))
	if err != nil {
		b.Fatal(err)
	}
	slow := &delayedDeleteBackend{Backend: fs, started: make(chan struct{}, 1)}
	m, err := NewManager(queues, db, slow, Config{MaxSize: 5, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	ref, err := contentcache.ParseBlobRef("blake3:" + fmt.Sprintf("%064x", 1))
	if err != nil {
		b.Fatal(err)
	}
	key, victim := contentcache.BlobStorageKey(ref.Hash), ref.String()
	newHash := "blake3:" + fmt.Sprintf("%064x", 2)
	durations := make([]time.Duration, b.N)
	b.ResetTimer()
	b.StopTimer()
	for i := range b.N {
		if err := fs.Write(ctx, key, bytes.NewReader([]byte("0123456789"))); err != nil {
			b.Fatal(err)
		}
		if err := db.PutBlob(ctx, &metadb.BlobEntry{Hash: victim, Size: 10}); err != nil {
			b.Fatal(err)
		}
		m.Admit(ctx, victim, 10)
		done := make(chan struct{})
		go func() { m.maybeEvict(ctx); close(done) }()
		select {
		case <-slow.started:
		case <-time.After(5 * time.Second):
			b.Fatal("eviction did not reach backend deletion")
		}
		b.StartTimer()
		start := time.Now()
		m.Admit(ctx, newHash, 1)
		durations[i] = time.Since(start)
		b.StopTimer()
		<-done
		m.Remove(ctx, newHash, 1)
		m.Remove(ctx, victim, 10)
	}
	slices.Sort(durations)
	b.ReportMetric(float64(durations[len(durations)/2].Microseconds()), "p50-us")
	b.ReportMetric(float64(durations[(len(durations)-1)*95/100].Microseconds()), "p95-us")
}

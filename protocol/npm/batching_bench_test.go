package npm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/content-cache/store/metadb"
)

// Exercise the actual npm index too: a lock around Put can defeat DB batching.
func BenchmarkNPMMetadataDisk(b *testing.B) {
	db := metadb.NewBoltDB()
	if err := db.Open(filepath.Join(b.TempDir(), "metadata.db")); err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if value := os.Getenv("CONTENT_CACHE_BENCH_BATCH_DELAY"); value != "" {
		delay, err := time.ParseDuration(value)
		if err != nil {
			b.Fatal(err)
		}
		db.DB().MaxBatchDelay = delay
	}
	metadataIndex, err := metadb.NewEnvelopeIndex(db, "npm", "metadata", time.Hour)
	if err != nil {
		b.Fatal(err)
	}
	index := NewIndex(metadataIndex, nil)
	data := []byte(`{"name":"pkg","readme":"` + strings.Repeat("metadata readme ", 2048) + `","versions":{}}`)
	keys := make([]string, 128)
	for i := range keys {
		keys[i] = fmt.Sprintf("pkg-%d", i)
		if err := index.PutPackageMetadata(context.Background(), keys[i], data); err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if err := index.PutPackageMetadata(context.Background(), keys[i%len(keys)], data); err != nil {
				b.Error(err)
				return
			}
			i++
		}
	})
}

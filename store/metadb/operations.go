package metadb

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
)

// RefcountDiscrepancy represents a mismatch between stored and computed refcounts.
type RefcountDiscrepancy struct {
	Hash     string `json:"hash"`
	Stored   int    `json:"stored"`
	Computed int    `json:"computed"`
}

// VerifyEnvelopeRefcounts scans all envelope metadata and compares computed
// refcounts to stored blob refcounts. Returns discrepancies without modifying the database.
// This is useful for detecting bugs or corruption.
func (b *BoltDB) VerifyEnvelopeRefcounts(ctx context.Context) ([]RefcountDiscrepancy, error) {
	computed := make(map[string]int)

	err := b.db.View(func(tx *bbolt.Tx) error {
		envBucket := tx.Bucket(bucketEnvelopes)
		if envBucket == nil {
			return nil
		}

		cursor := envBucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var env MetadataEnvelope
			if err := proto.Unmarshal(v, &env); err != nil {
				continue
			}
			for _, ref := range env.BlobRefs {
				computed[ref]++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var discrepancies []RefcountDiscrepancy

	err = b.db.View(func(tx *bbolt.Tx) error {
		blobBucket := tx.Bucket(bucketBlobsByHash)
		if blobBucket == nil {
			for hash, count := range computed {
				discrepancies = append(discrepancies, RefcountDiscrepancy{
					Hash:     hash,
					Stored:   0,
					Computed: count,
				})
			}
			return nil
		}

		cursor := blobBucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			hash := string(k)

			var entry BlobEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				continue
			}

			expected := computed[hash]
			if entry.RefCount != expected {
				discrepancies = append(discrepancies, RefcountDiscrepancy{
					Hash:     hash,
					Stored:   entry.RefCount,
					Computed: expected,
				})
			}
			delete(computed, hash)
		}

		for hash, count := range computed {
			discrepancies = append(discrepancies, RefcountDiscrepancy{
				Hash:     hash,
				Stored:   0,
				Computed: count,
			})
		}

		return nil
	})

	return discrepancies, err
}

// RebuildEnvelopeRefcounts recomputes all blob refcounts from envelope metadata.
// This updates the RefCount field in each BlobEntry based on actual references.
// Use after VerifyEnvelopeRefcounts detects discrepancies.
func (b *BoltDB) RebuildEnvelopeRefcounts(ctx context.Context) (int, error) {
	computed := make(map[string]int)

	err := b.db.View(func(tx *bbolt.Tx) error {
		envBucket := tx.Bucket(bucketEnvelopes)
		if envBucket == nil {
			return nil
		}

		cursor := envBucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var env MetadataEnvelope
			if err := proto.Unmarshal(v, &env); err != nil {
				continue
			}
			for _, ref := range env.BlobRefs {
				computed[ref]++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	updated := 0
	err = b.db.Update(func(tx *bbolt.Tx) error {
		blobBucket := tx.Bucket(bucketBlobsByHash)
		if blobBucket == nil {
			return nil
		}

		cursor := blobBucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			hash := string(k)

			var entry BlobEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				continue
			}

			expected := computed[hash]
			if entry.RefCount != expected {
				entry.RefCount = expected
				data, err := json.Marshal(&entry)
				if err != nil {
					continue
				}
				if err := blobBucket.Put(k, data); err != nil {
					return err
				}
				updated++
			}
			delete(computed, hash)
		}

		return nil
	})

	return updated, err
}

// EnvelopeDBStats contains statistics about the envelope database.
type EnvelopeDBStats struct {
	EnvelopeCount      int64            `json:"envelope_count"`
	BlobCount          int64            `json:"blob_count"`
	ExpiredCount       int64            `json:"expired_count"`
	TotalPayloadSize   int64            `json:"total_payload_size"`
	CompressedCount    int64            `json:"compressed_count"`
	OldestFetchedAt    time.Time        `json:"oldest_fetched_at,omitempty"`
	NewestFetchedAt    time.Time        `json:"newest_fetched_at,omitempty"`
	ByProtocol         map[string]int64 `json:"by_protocol"`
	ByKind             map[string]int64 `json:"by_kind"`
	ByProtocolKind     map[string]int64 `json:"by_protocol_kind"`
	DBFileSize         int64            `json:"db_file_size"`
	NegativeCacheCount int64            `json:"negative_cache_count"`
}

// EnvelopeStats returns statistics about the envelope database.
func (b *BoltDB) EnvelopeStats(ctx context.Context) (*EnvelopeDBStats, error) {
	stats := &EnvelopeDBStats{
		ByProtocol:     make(map[string]int64),
		ByKind:         make(map[string]int64),
		ByProtocolKind: make(map[string]int64),
	}

	now := time.Now()

	err := b.db.View(func(tx *bbolt.Tx) error {
		envBucket := tx.Bucket(bucketEnvelopes)
		if envBucket != nil {
			cursor := envBucket.Cursor()
			for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
				stats.EnvelopeCount++

				protocol, kind, _ := parseEnvelopeKey(k)
				stats.ByProtocol[protocol]++
				stats.ByKind[kind]++
				stats.ByProtocolKind[protocol+"/"+kind]++

				var env MetadataEnvelope
				if err := proto.Unmarshal(v, &env); err != nil {
					continue
				}

				stats.TotalPayloadSize += min(int64(env.PayloadSize), MaxAllowedPayloadSize) //nolint:gosec // bounded by MaxAllowedPayloadSize

				if env.ContentEncoding == ContentEncoding_CONTENT_ENCODING_ZSTD {
					stats.CompressedCount++
				}

				if env.UpstreamStatus >= 400 {
					stats.NegativeCacheCount++
				}

				if env.ExpiresAtUnixMs > 0 && env.ExpiresAtUnixMs < now.UnixMilli() {
					stats.ExpiredCount++
				}

				fetchedAt := time.UnixMilli(env.FetchedAtUnixMs)
				if stats.OldestFetchedAt.IsZero() || fetchedAt.Before(stats.OldestFetchedAt) {
					stats.OldestFetchedAt = fetchedAt
				}
				if fetchedAt.After(stats.NewestFetchedAt) {
					stats.NewestFetchedAt = fetchedAt
				}
			}
		}

		blobBucket := tx.Bucket(bucketBlobsByHash)
		if blobBucket != nil {
			blobStats := blobBucket.Stats()
			stats.BlobCount = int64(blobStats.KeyN)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if b.db != nil {
		dbPath := b.db.Path()
		if fi, err := os.Stat(dbPath); err == nil {
			stats.DBFileSize = fi.Size()
		}
	}

	return stats, nil
}

// EnvelopeInspectResult contains detailed information about a single envelope.
type EnvelopeInspectResult struct {
	Protocol         string    `json:"protocol"`
	Kind             string    `json:"kind"`
	Key              string    `json:"key"`
	Exists           bool      `json:"exists"`
	EnvelopeVersion  uint32    `json:"envelope_version,omitempty"`
	ContentType      string    `json:"content_type,omitempty"`
	ContentEncoding  string    `json:"content_encoding,omitempty"`
	PayloadSize      uint64    `json:"payload_size,omitempty"`
	CompressedSize   int       `json:"compressed_size,omitempty"`
	CompressionRatio float64   `json:"compression_ratio,omitempty"`
	PayloadDigest    string    `json:"payload_digest,omitempty"`
	FetchedAt        time.Time `json:"fetched_at,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	TTLSeconds       int64     `json:"ttl_seconds,omitempty"`
	IsExpired        bool      `json:"is_expired"`
	Etag             string    `json:"etag,omitempty"`
	LastModified     time.Time `json:"last_modified,omitempty"`
	Upstream         string    `json:"upstream,omitempty"`
	UpstreamStatus   uint32    `json:"upstream_status,omitempty"`
	IsNegativeCache  bool      `json:"is_negative_cache"`
	BlobRefs         []string  `json:"blob_refs,omitempty"`
	AttributeKeys    []string  `json:"attribute_keys,omitempty"`
	StoredBytes      int       `json:"stored_bytes,omitempty"`
}

// InspectEnvelope returns detailed information about a specific envelope.
func (b *BoltDB) InspectEnvelope(ctx context.Context, protocol, kind, key string) (*EnvelopeInspectResult, error) {
	result := &EnvelopeInspectResult{
		Protocol: protocol,
		Kind:     kind,
		Key:      key,
		Exists:   false,
	}

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketEnvelopes)
		if bucket == nil {
			return nil
		}

		compoundKey := makeEnvelopeKey(protocol, kind, key)
		val := bucket.Get(compoundKey)
		if val == nil {
			return nil
		}

		result.Exists = true
		result.StoredBytes = len(val)

		var env MetadataEnvelope
		if err := proto.Unmarshal(val, &env); err != nil {
			return fmt.Errorf("unmarshaling envelope: %w", err)
		}

		result.EnvelopeVersion = env.EnvelopeVersion
		result.ContentType = env.ContentType.String()
		result.ContentEncoding = env.ContentEncoding.String()
		result.PayloadSize = env.PayloadSize
		result.CompressedSize = len(env.Payload)
		result.PayloadDigest = env.PayloadDigest
		result.TTLSeconds = env.TtlSeconds
		result.Etag = env.Etag
		result.Upstream = env.Upstream
		result.UpstreamStatus = env.UpstreamStatus
		result.IsNegativeCache = env.UpstreamStatus >= 400
		result.BlobRefs = env.BlobRefs

		if env.PayloadSize > 0 && len(env.Payload) > 0 {
			result.CompressionRatio = float64(env.PayloadSize) / float64(len(env.Payload))
		}

		if env.FetchedAtUnixMs > 0 {
			result.FetchedAt = time.UnixMilli(env.FetchedAtUnixMs)
		}
		if env.ExpiresAtUnixMs > 0 {
			result.ExpiresAt = time.UnixMilli(env.ExpiresAtUnixMs)
			result.IsExpired = time.Now().UnixMilli() > env.ExpiresAtUnixMs
		}
		if env.LastModifiedUnixMs > 0 {
			result.LastModified = time.UnixMilli(env.LastModifiedUnixMs)
		}

		for k := range env.Attributes {
			result.AttributeKeys = append(result.AttributeKeys, k)
		}

		return nil
	})

	return result, err
}

// ListEnvelopesByProtocol returns all protocol/kind combinations in the database.
func (b *BoltDB) ListEnvelopeProtocols(ctx context.Context) ([]string, error) {
	seen := make(map[string]struct{})
	var protocols []string

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketEnvelopes)
		if bucket == nil {
			return nil
		}

		cursor := bucket.Cursor()
		for k, _ := cursor.First(); k != nil; k, _ = cursor.Next() {
			protocol, kind, _ := parseEnvelopeKey(k)
			pk := protocol + "/" + kind
			if _, ok := seen[pk]; !ok {
				seen[pk] = struct{}{}
				protocols = append(protocols, pk)
			}
		}
		return nil
	})

	return protocols, err
}

// CompactDB triggers a database compaction by copying to a new file.
// This reclaims space from deleted entries.
// Note: Requires the database path to be accessible.
func (b *BoltDB) CompactDB(ctx context.Context, destPath string) error {
	destDB, err := bbolt.Open(destPath, 0o600, &bbolt.Options{
		NoSync: b.noSync,
	})
	if err != nil {
		return fmt.Errorf("opening destination database: %w", err)
	}
	defer destDB.Close()

	return b.db.View(func(srcTx *bbolt.Tx) error {
		return destDB.Update(func(destTx *bbolt.Tx) error {
			return srcTx.ForEach(func(name []byte, srcBucket *bbolt.Bucket) error {
				destBucket, err := destTx.CreateBucketIfNotExists(name)
				if err != nil {
					return fmt.Errorf("creating bucket %s: %w", name, err)
				}

				return srcBucket.ForEach(func(k, v []byte) error {
					return destBucket.Put(k, v)
				})
			})
		})
	})
}

// ExportEnvelopesToJSON exports all envelopes to a JSON file for debugging.
func (b *BoltDB) ExportEnvelopesToJSON(ctx context.Context, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	type exportEntry struct {
		Protocol string            `json:"protocol"`
		Kind     string            `json:"kind"`
		Key      string            `json:"key"`
		Envelope *MetadataEnvelope `json:"envelope"`
	}

	var entries []exportEntry

	err = b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bucketEnvelopes)
		if bucket == nil {
			return nil
		}

		cursor := bucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			protocol, kind, key := parseEnvelopeKey(k)

			var env MetadataEnvelope
			if err := proto.Unmarshal(v, &env); err != nil {
				continue
			}

			env.Payload = nil

			entries = append(entries, exportEntry{
				Protocol: protocol,
				Kind:     kind,
				Key:      key,
				Envelope: &env,
			})
		}
		return nil
	})
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	return encoder.Encode(entries)
}

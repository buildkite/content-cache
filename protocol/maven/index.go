package maven

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store/metadb"
)

const (
	// kindMetadata is the metadb kind for maven-metadata.xml cache
	kindMetadata = "metadata"
	// kindArtifact is the metadb kind for cached artifact info with blob refs
	kindArtifact = "artifact"
)

// Index manages the Maven artifact index using metadb envelope storage.
type Index struct {
	metadataIndex *metadb.EnvelopeIndex // protocol="maven", kind="metadata"
	artifactIndex *metadb.EnvelopeIndex // protocol="maven", kind="artifact"
	mu            sync.RWMutex
	now           func() time.Time
}

// NewIndex creates a new Maven artifact index using EnvelopeIndex instances.
// metadataIndex: protocol="maven", kind="metadata" for maven-metadata.xml
// artifactIndex: protocol="maven", kind="artifact" for CachedArtifact with blob refs
func NewIndex(metadataIndex, artifactIndex *metadb.EnvelopeIndex) *Index {
	return &Index{
		metadataIndex: metadataIndex,
		artifactIndex: artifactIndex,
		now:           time.Now,
	}
}

// GetCachedMetadata retrieves cached maven-metadata.xml information.
func (idx *Index) GetCachedMetadata(ctx context.Context, groupID, artifactID string) (*CachedMetadata, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := metadataKey(groupID, artifactID)
	var cached CachedMetadata
	if err := idx.metadataIndex.GetJSON(ctx, key, &cached); err != nil {
		if err == metadb.ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reading metadata: %w", err)
	}

	return &cached, nil
}

// PutCachedMetadata stores cached maven-metadata.xml information.
func (idx *Index) PutCachedMetadata(ctx context.Context, metadata *CachedMetadata) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	metadata.UpdatedAt = idx.now()
	if metadata.CachedAt.IsZero() {
		metadata.CachedAt = metadata.UpdatedAt
	}

	key := metadataKey(metadata.GroupID, metadata.ArtifactID)

	// No blob refs for metadata (raw XML)
	return idx.metadataIndex.PutJSON(ctx, key, metadata, nil)
}

// GetCachedArtifact retrieves cached artifact information.
func (idx *Index) GetCachedArtifact(ctx context.Context, coord ArtifactCoordinate) (*CachedArtifact, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := artifactKey(coord)
	var cached CachedArtifact
	if err := idx.artifactIndex.GetJSON(ctx, key, &cached); err != nil {
		if err == metadb.ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reading artifact: %w", err)
	}

	return &cached, nil
}

// PutCachedArtifact stores cached artifact information.
func (idx *Index) PutCachedArtifact(ctx context.Context, artifact *CachedArtifact) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	artifact.CachedAt = idx.now()

	coord := ArtifactCoordinate{
		GroupID:    artifact.GroupID,
		ArtifactID: artifact.ArtifactID,
		Version:    artifact.Version,
		Classifier: artifact.Classifier,
		Extension:  artifact.Extension,
	}

	key := artifactKey(coord)

	// Collect blob refs from the artifact hash
	refs := collectBlobRefs(artifact)

	return idx.artifactIndex.PutJSON(ctx, key, artifact, refs)
}

// GetArtifactHash retrieves the content hash for a specific artifact.
func (idx *Index) GetArtifactHash(ctx context.Context, coord ArtifactCoordinate) (contentcache.Hash, error) {
	cached, err := idx.GetCachedArtifact(ctx, coord)
	if err != nil {
		return contentcache.Hash{}, err
	}
	return cached.Hash, nil
}

// DeleteMetadata removes cached metadata for an artifact.
func (idx *Index) DeleteMetadata(ctx context.Context, groupID, artifactID string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	key := metadataKey(groupID, artifactID)
	if err := idx.metadataIndex.Delete(ctx, key); err != nil && err != metadb.ErrNotFound {
		return fmt.Errorf("deleting metadata: %w", err)
	}

	return nil
}

// DeleteArtifact removes cached artifact information.
func (idx *Index) DeleteArtifact(ctx context.Context, coord ArtifactCoordinate) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	key := artifactKey(coord)
	if err := idx.artifactIndex.Delete(ctx, key); err != nil && err != metadb.ErrNotFound {
		return fmt.Errorf("deleting artifact: %w", err)
	}

	return nil
}

// IsMetadataExpired checks if cached metadata has exceeded the TTL.
func (idx *Index) IsMetadataExpired(metadata *CachedMetadata, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return idx.now().Sub(metadata.UpdatedAt) > ttl
}

// ListArtifacts returns all cached artifact keys for a given group and artifact.
func (idx *Index) ListArtifacts(ctx context.Context, groupID, artifactID string) ([]string, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	// List all keys and filter by prefix
	prefix := groupIDToPath(groupID) + "/" + artifactID + "/"
	allKeys, err := idx.artifactIndex.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing artifacts: %w", err)
	}

	var keys []string
	for _, key := range allKeys {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}

	return keys, nil
}

// Key generation helpers

// metadataKey generates the storage key for maven-metadata.xml.
// Format: {groupPath}/{artifactId}
func metadataKey(groupID, artifactID string) string {
	return groupIDToPath(groupID) + "/" + artifactID
}

// artifactKey generates the storage key for a cached artifact.
// Format: {groupPath}/{artifactId}/{version}/{classifier}/{extension}
func artifactKey(coord ArtifactCoordinate) string {
	// Use classifier "default" if empty to avoid key collisions
	classifier := coord.Classifier
	if classifier == "" {
		classifier = "_default"
	}
	return fmt.Sprintf("%s/%s/%s/%s/%s",
		groupIDToPath(coord.GroupID),
		coord.ArtifactID,
		coord.Version,
		classifier,
		coord.Extension,
	)
}

// collectBlobRefs extracts blob hash from a CachedArtifact.
// Returns refs in canonical format: "blake3:<hex>"
func collectBlobRefs(artifact *CachedArtifact) []string {
	if artifact == nil || artifact.Hash.IsZero() {
		return nil
	}
	return []string{contentcache.NewBlobRef(artifact.Hash).String()}
}

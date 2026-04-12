package oci

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store/metadb"
)

// ErrNotFound is returned when a resource is not found in the index.
var ErrNotFound = errors.New("not found")

// Index manages the OCI image cache index using metadb envelope storage.
type Index struct {
	imageIndex    *metadb.EnvelopeIndex // protocol="oci", kind="image" for tag->digest mappings
	manifestIndex *metadb.EnvelopeIndex // protocol="oci", kind="manifest" for manifest metadata
	blobIndex     *metadb.EnvelopeIndex // protocol="oci", kind="blob" for blob metadata
	mu            sync.RWMutex
	now           func() time.Time
}

// NewIndex creates a new OCI index using EnvelopeIndex instances.
// imageIndex: protocol="oci", kind="image" for tag-to-digest mappings (CachedImage)
// manifestIndex: protocol="oci", kind="manifest" for manifest metadata (CachedManifest)
// blobIndex: protocol="oci", kind="blob" for blob metadata (CachedBlob)
func NewIndex(imageIndex, manifestIndex, blobIndex *metadb.EnvelopeIndex) *Index {
	return &Index{
		imageIndex:    imageIndex,
		manifestIndex: manifestIndex,
		blobIndex:     blobIndex,
		now:           time.Now,
	}
}

// GetTagDigest returns the cached digest for a tag and when it was last refreshed.
func (idx *Index) GetTagDigest(ctx context.Context, name, tag string) (string, time.Time, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := encodeImageName(name)
	var image CachedImage
	if err := idx.imageIndex.GetJSON(ctx, key, &image); err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return "", time.Time{}, ErrNotFound
		}
		return "", time.Time{}, err
	}

	cachedTag, ok := image.Tags[tag]
	if !ok {
		return "", time.Time{}, ErrNotFound
	}

	return cachedTag.Digest, cachedTag.RefreshedAt, nil
}

// SetTagDigest updates the tag->digest mapping for an image.
func (idx *Index) SetTagDigest(ctx context.Context, name, tag, digest string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	key := encodeImageName(name)

	// Get or create cached image
	var image CachedImage
	err := idx.imageIndex.GetJSON(ctx, key, &image)
	if errors.Is(err, metadb.ErrNotFound) {
		image = CachedImage{
			Name:      name,
			Tags:      make(map[string]*CachedTag),
			CachedAt:  idx.now(),
			UpdatedAt: idx.now(),
		}
	} else if err != nil {
		return err
	}

	now := idx.now()
	image.Tags[tag] = &CachedTag{
		Tag:         tag,
		Digest:      digest,
		CachedAt:    now,
		RefreshedAt: now,
	}
	image.UpdatedAt = now

	// Collect blob refs from all tags (digests of manifests)
	refs := collectImageBlobRefs(&image)

	return idx.imageIndex.PutJSON(ctx, key, &image, refs)
}

// RefreshTag updates the RefreshedAt timestamp for a tag without changing the digest.
func (idx *Index) RefreshTag(ctx context.Context, name, tag string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	key := encodeImageName(name)
	var image CachedImage
	if err := idx.imageIndex.GetJSON(ctx, key, &image); err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}

	cachedTag, ok := image.Tags[tag]
	if !ok {
		return ErrNotFound
	}

	cachedTag.RefreshedAt = idx.now()
	image.UpdatedAt = idx.now()

	refs := collectImageBlobRefs(&image)
	return idx.imageIndex.PutJSON(ctx, key, &image, refs)
}

// GetManifest returns the cached manifest metadata by digest.
func (idx *Index) GetManifest(ctx context.Context, digest string) (*CachedManifest, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := encodeDigest(digest)
	var manifest CachedManifest
	if err := idx.manifestIndex.GetJSON(ctx, key, &manifest); err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	return &manifest, nil
}

// PutManifest stores manifest metadata in the index.
func (idx *Index) PutManifest(ctx context.Context, digest, mediaType string, hash contentcache.Hash, size int64) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	manifest := &CachedManifest{
		Digest:      digest,
		MediaType:   mediaType,
		ContentHash: hash,
		Size:        size,
		CachedAt:    idx.now(),
	}

	key := encodeDigest(digest)
	// Manifest content hash is a blob ref
	refs := []string{contentcache.NewBlobRef(hash).String()}

	return idx.manifestIndex.PutJSON(ctx, key, manifest, refs)
}

// GetBlob returns the cached blob metadata by digest.
func (idx *Index) GetBlob(ctx context.Context, digest string) (*CachedBlob, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := encodeDigest(digest)
	var blob CachedBlob
	if err := idx.blobIndex.GetJSON(ctx, key, &blob); err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	return &blob, nil
}

// PutBlob stores blob metadata in the index.
func (idx *Index) PutBlob(ctx context.Context, digest string, hash contentcache.Hash, size int64) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	blob := &CachedBlob{
		Digest:      digest,
		ContentHash: hash,
		Size:        size,
		CachedAt:    idx.now(),
	}

	key := encodeDigest(digest)
	// Blob content hash is a blob ref
	refs := []string{contentcache.NewBlobRef(hash).String()}

	return idx.blobIndex.PutJSON(ctx, key, blob, refs)
}

// ListImages returns a list of all cached image names.
func (idx *Index) ListImages(ctx context.Context) ([]string, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	keys, err := idx.imageIndex.List(ctx)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, key := range keys {
		decoded, err := decodeImageName(key)
		if err != nil {
			continue
		}
		names = append(names, decoded)
	}

	return names, nil
}

// encodeImageName encodes an image name for use in storage keys.
// Replaces forward slashes with %2F to create flat key structure.
func encodeImageName(name string) string {
	return url.PathEscape(name)
}

// decodeImageName decodes a storage key back to an image name.
func decodeImageName(encoded string) (string, error) {
	return url.PathUnescape(encoded)
}

// encodeDigest encodes a digest for use as a storage key.
// Uses the digest hex portion (after algorithm:) for uniqueness.
func encodeDigest(digest string) string {
	d, err := ParseDigest(digest)
	if err != nil {
		return url.PathEscape(digest)
	}
	return d.Algorithm + "_" + d.Hex
}

// collectImageBlobRefs extracts manifest digest references from a CachedImage.
// Note: These are not CAFS blob refs, but OCI manifest digests stored for reference tracking.
// The actual CAFS blob refs are stored when manifests/blobs are cached.
func collectImageBlobRefs(_ *CachedImage) []string {
	// Image metadata doesn't directly reference CAFS blobs - the manifest and blob
	// indexes track those references. Return nil to avoid double-counting.
	return nil
}

package rubygems

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store"
	"github.com/buildkite/content-cache/store/metadb"
)

// Index provides storage for RubyGems metadata using metadb envelope storage.
type Index struct {
	versionsIndex *metadb.EnvelopeIndex // protocol="rubygems", kind="versions"
	infoIndex     *metadb.EnvelopeIndex // protocol="rubygems", kind="info"
	specsIndex    *metadb.EnvelopeIndex // protocol="rubygems", kind="specs"
	gemIndex      *metadb.EnvelopeIndex // protocol="rubygems", kind="gem"
	gemspecIndex  *metadb.EnvelopeIndex // protocol="rubygems", kind="gemspec"
	store         store.Store
	mu            sync.RWMutex
	now           func() time.Time
}

// NewIndex creates a new Index using EnvelopeIndex instances.
func NewIndex(
	versionsIndex,
	infoIndex,
	specsIndex,
	gemIndex,
	gemspecIndex *metadb.EnvelopeIndex,
	st store.Store,
) *Index {
	return &Index{
		versionsIndex: versionsIndex,
		infoIndex:     infoIndex,
		specsIndex:    specsIndex,
		gemIndex:      gemIndex,
		gemspecIndex:  gemspecIndex,
		store:         st,
		now:           time.Now,
	}
}

// Storage key patterns:
// versions: "versions" (single global entry)
// info:     "{gem}" (gem name)
// specs:    "{type}" (specs, latest_specs, prerelease_specs)
// gem:      "{filename}" (e.g., rails-7.0.0.gem)
// gemspec:  "{name}-{version}[-{platform}]" (e.g., rails-7.0.0, nokogiri-1.13.0-x86_64-linux)

const versionsKey = "versions"

// GetVersions retrieves the cached /versions file and its metadata.
func (idx *Index) GetVersions(ctx context.Context) (*CachedVersions, []byte, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	content, env, err := idx.versionsIndex.GetWithEnvelope(ctx, versionsKey)
	if err != nil {
		if err == metadb.ErrNotFound {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}

	if env.ContentType == metadb.ContentType_CONTENT_TYPE_JSON {
		var meta CachedVersions
		if err := json.Unmarshal(content, &meta); err != nil {
			return nil, nil, err
		}
		blobContent, err := idx.readContent(ctx, meta.ContentHash)
		if err != nil {
			return nil, nil, err
		}
		return &meta, blobContent, nil
	}

	meta := &CachedVersions{
		ETag:       env.Etag,
		ReprDigest: extractReprDigest(env),
		Size:       min(int64(env.PayloadSize), metadb.MaxPayloadSize), //nolint:gosec // bounded by MaxPayloadSize
		CachedAt:   time.UnixMilli(env.FetchedAtUnixMs),
		UpdatedAt:  time.UnixMilli(env.FetchedAtUnixMs),
	}

	return meta, content, nil
}

// PutVersions stores the /versions file and its metadata.
func (idx *Index) PutVersions(ctx context.Context, meta *CachedVersions, content []byte) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	hash, err := idx.putContent(ctx, content)
	if err != nil {
		return err
	}

	storedMeta := *meta
	storedMeta.ContentHash = hash
	storedMeta.Size = int64(len(content))

	return idx.versionsIndex.PutJSONWithOptions(
		ctx,
		versionsKey,
		&storedMeta,
		blobRefs(hash),
		metadb.PutOptions{Etag: storedMeta.ETag},
	)
}

// AppendVersions appends content to the /versions file and updates metadata.
func (idx *Index) AppendVersions(ctx context.Context, meta *CachedVersions, appendContent []byte) error {
	// Read existing content
	_, existingContent, err := idx.GetVersions(ctx)
	if err != nil && err != ErrNotFound {
		return err
	}

	// Append new content
	newContent := make([]byte, len(existingContent)+len(appendContent))
	copy(newContent, existingContent)
	copy(newContent[len(existingContent):], appendContent)
	meta.Size = int64(len(newContent))

	return idx.PutVersions(ctx, meta, newContent)
}

// GetInfo retrieves the cached /info/{gem} file and its metadata.
func (idx *Index) GetInfo(ctx context.Context, gem string) (*CachedGemInfo, []byte, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	content, env, err := idx.infoIndex.GetWithEnvelope(ctx, gem)
	if err != nil {
		if err == metadb.ErrNotFound {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}

	if env.ContentType == metadb.ContentType_CONTENT_TYPE_JSON {
		var meta CachedGemInfo
		if err := json.Unmarshal(content, &meta); err != nil {
			return nil, nil, err
		}
		blobContent, err := idx.readContent(ctx, meta.ContentHash)
		if err != nil {
			return nil, nil, err
		}
		return &meta, blobContent, nil
	}

	meta := &CachedGemInfo{
		Name:       gem,
		ETag:       env.Etag,
		ReprDigest: extractReprDigest(env),
		Size:       min(int64(env.PayloadSize), metadb.MaxPayloadSize), //nolint:gosec // bounded by MaxPayloadSize
		CachedAt:   time.UnixMilli(env.FetchedAtUnixMs),
		UpdatedAt:  time.UnixMilli(env.FetchedAtUnixMs),
	}

	// Checksums are stored as JSON in attributes if present
	if checksumData, ok := env.Attributes["checksums"]; ok {
		meta.Checksums = decodeChecksums(checksumData)
	}
	if len(meta.Checksums) == 0 {
		meta.Checksums = ParseInfoChecksums(content)
	}

	// MD5 from versions file
	if md5Data, ok := env.Attributes["md5"]; ok {
		meta.MD5 = string(md5Data)
	}

	return meta, content, nil
}

// PutInfo stores the /info/{gem} file and its metadata.
func (idx *Index) PutInfo(ctx context.Context, gem string, meta *CachedGemInfo, content []byte) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	hash, err := idx.putContent(ctx, content)
	if err != nil {
		return err
	}

	storedMeta := *meta
	storedMeta.Name = gem
	storedMeta.ContentHash = hash
	storedMeta.Size = int64(len(content))

	return idx.infoIndex.PutJSONWithOptions(
		ctx,
		gem,
		&storedMeta,
		blobRefs(hash),
		metadb.PutOptions{Etag: storedMeta.ETag},
	)
}

// GetSpecs retrieves the cached specs file (specs, latest_specs, or prerelease_specs).
func (idx *Index) GetSpecs(ctx context.Context, specsType string) (*CachedSpecs, []byte, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	content, env, err := idx.specsIndex.GetWithEnvelope(ctx, specsType)
	if err != nil {
		if err == metadb.ErrNotFound {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}

	meta := &CachedSpecs{
		Type:       specsType,
		ETag:       env.Etag,
		ReprDigest: extractReprDigest(env),
		Size:       min(int64(env.PayloadSize), metadb.MaxPayloadSize), //nolint:gosec // bounded by MaxPayloadSize
		CachedAt:   time.UnixMilli(env.FetchedAtUnixMs),
		UpdatedAt:  time.UnixMilli(env.FetchedAtUnixMs),
	}

	return meta, content, nil
}

// PutSpecs stores a specs file and its metadata.
func (idx *Index) PutSpecs(ctx context.Context, specsType string, meta *CachedSpecs, content []byte) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	opts := metadb.PutOptions{
		Etag: meta.ETag,
	}

	// Specs files are binary (gzipped Marshal data)
	return idx.specsIndex.PutWithOptions(ctx, specsType, content, metadb.ContentType_CONTENT_TYPE_OCTET_STREAM, nil, opts)
}

// GetGem retrieves cached gem metadata.
func (idx *Index) GetGem(ctx context.Context, filename string) (*CachedGem, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var cached CachedGem
	if err := idx.gemIndex.GetJSON(ctx, filename, &cached); err != nil {
		if err == metadb.ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}

	return &cached, nil
}

// PutGem stores gem metadata with blob reference.
func (idx *Index) PutGem(ctx context.Context, gem *CachedGem) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Collect blob ref for the gem content
	var refs []string
	if !gem.ContentHash.IsZero() {
		refs = []string{contentcache.NewBlobRef(gem.ContentHash).String()}
	}

	return idx.gemIndex.PutJSON(ctx, gem.Filename, gem, refs)
}

// GetGemspec retrieves cached gemspec metadata.
func (idx *Index) GetGemspec(ctx context.Context, name, version, platform string) (*CachedGemspec, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	filename := gemspecFilename(name, version, platform)
	var cached CachedGemspec
	if err := idx.gemspecIndex.GetJSON(ctx, filename, &cached); err != nil {
		if err == metadb.ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}

	return &cached, nil
}

// PutGemspec stores gemspec metadata with blob reference.
func (idx *Index) PutGemspec(ctx context.Context, spec *CachedGemspec) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	filename := gemspecFilename(spec.Name, spec.Version, spec.Platform)

	// Collect blob ref for the gemspec content
	var refs []string
	if !spec.ContentHash.IsZero() {
		refs = []string{contentcache.NewBlobRef(spec.ContentHash).String()}
	}

	return idx.gemspecIndex.PutJSON(ctx, filename, spec, refs)
}

// IsExpired checks if the cached content is expired based on TTL.
func (idx *Index) IsExpired(cachedAt time.Time, ttl time.Duration) bool {
	return idx.now().Sub(cachedAt) > ttl
}

// gemspecFilename builds the filename for a gemspec.
func gemspecFilename(name, version, platform string) string {
	if platform == "" || platform == "ruby" {
		return name + "-" + version
	}
	return name + "-" + version + "-" + platform
}

// extractReprDigest extracts the ReprDigest from envelope attributes if present.
// This is used for the Repr-Digest header in Compact Index responses.
func extractReprDigest(env *metadb.MetadataEnvelope) string {
	if env.Attributes == nil {
		return ""
	}
	if data, ok := env.Attributes["repr_digest"]; ok {
		return string(data)
	}
	return ""
}

// encodeChecksums encodes checksums map to JSON bytes for envelope attributes.
func encodeChecksums(checksums map[string]string) []byte {
	if len(checksums) == 0 {
		return nil
	}
	// Simple JSON encoding: {"key":"value",...}
	// Using manual encoding to avoid import cycle and keep it simple
	var buf []byte
	buf = append(buf, '{')
	first := true
	for k, v := range checksums {
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = append(buf, '"')
		buf = append(buf, escapeJSON(k)...)
		buf = append(buf, `":"`...)
		buf = append(buf, escapeJSON(v)...)
		buf = append(buf, '"')
	}
	buf = append(buf, '}')
	return buf
}

// decodeChecksums decodes JSON bytes to checksums map.
func decodeChecksums(data []byte) map[string]string {
	if len(data) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m
}

// escapeJSON escapes a string for JSON encoding.
func escapeJSON(s string) string {
	var buf []byte
	for _, r := range s {
		switch r {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\t':
			buf = append(buf, '\\', 't')
		default:
			buf = append(buf, string(r)...)
		}
	}
	return string(buf)
}

func (idx *Index) putContent(ctx context.Context, content []byte) (contentcache.Hash, error) {
	if idx.store == nil {
		return contentcache.Hash{}, errors.New("store is not configured")
	}
	return idx.store.Put(ctx, bytes.NewReader(content))
}

func (idx *Index) readContent(ctx context.Context, hash contentcache.Hash) ([]byte, error) {
	if idx.store == nil {
		return nil, errors.New("store is not configured")
	}
	if hash.IsZero() {
		return nil, errors.New("content hash is not set")
	}
	rc, err := idx.store.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func blobRefs(hash contentcache.Hash) []string {
	if hash.IsZero() {
		return nil
	}
	return []string{contentcache.NewBlobRef(hash).String()}
}

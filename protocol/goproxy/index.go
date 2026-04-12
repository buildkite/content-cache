package goproxy

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store/metadb"
)

// Index manages the module version index using metadb envelope storage.
type Index struct {
	modIndex  *metadb.EnvelopeIndex // protocol="goproxy", kind="mod"
	infoIndex *metadb.EnvelopeIndex // protocol="goproxy", kind="info"
	listIndex *metadb.EnvelopeIndex // protocol="goproxy", kind="list"
	mu        sync.RWMutex
}

// NewIndex creates a new module index using EnvelopeIndex instances.
// modIndex: protocol="goproxy", kind="mod" for .mod files
// infoIndex: protocol="goproxy", kind="info" for .info files
// listIndex: protocol="goproxy", kind="list" for version lists
func NewIndex(modIndex, infoIndex, listIndex *metadb.EnvelopeIndex) *Index {
	return &Index{
		modIndex:  modIndex,
		infoIndex: infoIndex,
		listIndex: listIndex,
	}
}

// ListVersions returns all cached versions for a module.
func (idx *Index) ListVersions(ctx context.Context, modulePath string) ([]string, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := encodeModuleKey(modulePath)
	data, err := idx.listIndex.Get(ctx, key)
	if err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return nil, nil // No versions cached yet
		}
		return nil, err
	}

	if len(data) == 0 {
		return nil, nil
	}

	versions := strings.Split(strings.TrimSpace(string(data)), "\n")
	return versions, nil
}

// GetVersionInfo returns the info for a specific module version.
func (idx *Index) GetVersionInfo(ctx context.Context, modulePath, version string) (*VersionInfo, error) {
	mv, err := idx.GetModuleVersion(ctx, modulePath, version)
	if err != nil {
		return nil, err
	}
	return &mv.Info, nil
}

// GetModuleVersion returns all cached data for a module version.
func (idx *Index) GetModuleVersion(ctx context.Context, modulePath, version string) (*ModuleVersion, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := encodeVersionKey(modulePath, version)

	// Get the info (which contains ZipHash)
	var mv ModuleVersion
	if err := idx.infoIndex.GetJSON(ctx, key, &mv); err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	return &mv, nil
}

// GetMod returns the go.mod content for a module version.
func (idx *Index) GetMod(ctx context.Context, modulePath, version string) ([]byte, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := encodeVersionKey(modulePath, version)
	data, err := idx.modIndex.Get(ctx, key)
	if err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

// PutModuleVersion stores a module version in the index.
func (idx *Index) PutModuleVersion(ctx context.Context, modulePath, version string, mv *ModuleVersion, modFile []byte) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	versionKey := encodeVersionKey(modulePath, version)

	// Collect blob refs from the zip hash
	refs := collectBlobRefs(mv)

	// Store version info (includes ZipHash)
	if err := idx.infoIndex.PutJSON(ctx, versionKey, mv, refs); err != nil {
		return err
	}

	// Store go.mod content
	if err := idx.modIndex.Put(ctx, versionKey, modFile, metadb.ContentType_CONTENT_TYPE_TEXT, nil); err != nil {
		return err
	}

	// Update version list
	if err := idx.addVersion(ctx, modulePath, version); err != nil {
		return err
	}

	return nil
}

// DeleteModuleVersion removes a module version from the index.
func (idx *Index) DeleteModuleVersion(ctx context.Context, modulePath, version string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	versionKey := encodeVersionKey(modulePath, version)

	// Delete version info (with blob refs cleanup)
	if err := idx.infoIndex.Delete(ctx, versionKey); err != nil && !errors.Is(err, metadb.ErrNotFound) {
		return err
	}

	// Delete go.mod
	if err := idx.modIndex.Delete(ctx, versionKey); err != nil && !errors.Is(err, metadb.ErrNotFound) {
		return err
	}

	// Update version list
	if err := idx.removeVersion(ctx, modulePath, version); err != nil {
		return err
	}

	return nil
}

// addVersion adds a version to the module's version list.
func (idx *Index) addVersion(ctx context.Context, modulePath, version string) error {
	versions, err := idx.listVersionsInternal(ctx, modulePath)
	if err != nil {
		return err
	}

	// Check if version already exists
	for _, v := range versions {
		if v == version {
			return nil
		}
	}

	versions = append(versions, version)
	sort.Strings(versions)

	return idx.writeVersionList(ctx, modulePath, versions)
}

// removeVersion removes a version from the module's version list.
func (idx *Index) removeVersion(ctx context.Context, modulePath, version string) error {
	versions, err := idx.listVersionsInternal(ctx, modulePath)
	if err != nil {
		return err
	}

	// Filter out the version
	filtered := make([]string, 0, len(versions))
	for _, v := range versions {
		if v != version {
			filtered = append(filtered, v)
		}
	}

	if len(filtered) == 0 {
		// Delete the list entry if no versions remain
		key := encodeModuleKey(modulePath)
		return idx.listIndex.Delete(ctx, key)
	}

	return idx.writeVersionList(ctx, modulePath, filtered)
}

// listVersionsInternal reads version list without locking (for internal use when already locked).
func (idx *Index) listVersionsInternal(ctx context.Context, modulePath string) ([]string, error) {
	key := encodeModuleKey(modulePath)
	data, err := idx.listIndex.Get(ctx, key)
	if err != nil {
		if errors.Is(err, metadb.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}

	if len(data) == 0 {
		return nil, nil
	}

	return strings.Split(strings.TrimSpace(string(data)), "\n"), nil
}

// writeVersionList writes the version list for a module.
func (idx *Index) writeVersionList(ctx context.Context, modulePath string, versions []string) error {
	key := encodeModuleKey(modulePath)
	content := strings.Join(versions, "\n") + "\n"
	return idx.listIndex.Put(ctx, key, []byte(content), metadb.ContentType_CONTENT_TYPE_TEXT, nil)
}

// encodeModuleKey creates a storage key for module-level data.
func encodeModuleKey(modulePath string) string {
	return encodePath(modulePath)
}

// encodeVersionKey creates a storage key for version-specific data.
func encodeVersionKey(modulePath, version string) string {
	return encodePath(modulePath) + "@" + version
}

// collectBlobRefs extracts blob hashes from a ModuleVersion.
// Returns refs in canonical format: "blake3:<hex>"
func collectBlobRefs(mv *ModuleVersion) []string {
	if mv == nil || mv.ZipHash.IsZero() {
		return nil
	}
	return []string{contentcache.NewBlobRef(mv.ZipHash).String()}
}

// ModuleVersionJSON is the JSON representation stored in the info index.
// This includes all fields from ModuleVersion that should be persisted.
type ModuleVersionJSON struct {
	Info    VersionInfo       `json:"info"`
	ModHash contentcache.Hash `json:"mod_hash,omitempty"`
	ZipHash contentcache.Hash `json:"zip_hash"`
}

// MarshalJSON implements json.Marshaler for ModuleVersion.
func (mv ModuleVersion) MarshalJSON() ([]byte, error) {
	return json.Marshal(ModuleVersionJSON{
		Info:    mv.Info,
		ModHash: mv.ModHash,
		ZipHash: mv.ZipHash,
	})
}

// UnmarshalJSON implements json.Unmarshaler for ModuleVersion.
func (mv *ModuleVersion) UnmarshalJSON(data []byte) error {
	var j ModuleVersionJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	mv.Info = j.Info
	mv.ModHash = j.ModHash
	mv.ZipHash = j.ZipHash
	return nil
}

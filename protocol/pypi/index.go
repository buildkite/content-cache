package pypi

import (
	"context"
	"sync"
	"time"

	contentcache "github.com/buildkite/content-cache"
	"github.com/buildkite/content-cache/store/metadb"
)

// Index manages the PyPI package index using metadb envelope storage.
type Index struct {
	projectIndex *metadb.EnvelopeIndex // protocol="pypi", kind="project"
	mu           sync.RWMutex
	now          func() time.Time
}

// NewIndex creates a new PyPI package index using EnvelopeIndex.
// projectIndex: protocol="pypi", kind="project" for CachedProject with file hashes
func NewIndex(projectIndex *metadb.EnvelopeIndex) *Index {
	return &Index{
		projectIndex: projectIndex,
		now:          time.Now,
	}
}

// GetCachedProject retrieves cached project info including file hashes.
func (idx *Index) GetCachedProject(ctx context.Context, name string) (*CachedProject, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	key := NormalizeProjectName(name)
	var cached CachedProject
	if err := idx.projectIndex.GetJSON(ctx, key, &cached); err != nil {
		if err == metadb.ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &cached, nil
}

// PutCachedProject stores cached project info with blob references.
func (idx *Index) PutCachedProject(ctx context.Context, project *CachedProject) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	project.UpdatedAt = idx.now()
	if project.CachedAt.IsZero() {
		project.CachedAt = project.UpdatedAt
	}

	key := NormalizeProjectName(project.Name)

	// Collect all blob refs from cached files
	refs := collectBlobRefs(project)

	return idx.projectIndex.PutJSON(ctx, key, project, refs)
}

// GetFileHash retrieves the content hash for a specific file.
func (idx *Index) GetFileHash(ctx context.Context, project, filename string) (contentcache.Hash, error) {
	cached, err := idx.GetCachedProject(ctx, project)
	if err != nil {
		return contentcache.Hash{}, err
	}

	file, ok := cached.Files[filename]
	if !ok {
		return contentcache.Hash{}, ErrNotFound
	}

	return file.ContentHash, nil
}

// SetFileHash stores the content hash for a specific file.
func (idx *Index) SetFileHash(ctx context.Context, project, filename string, hash contentcache.Hash, size int64, hashes map[string]string, upstreamURL, requiresPython string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	key := NormalizeProjectName(project)

	// Get or create cached project: fetch existing project if present, otherwise
	// start with an empty struct. We ignore ErrNotFound because this is a
	// get-or-create pattern - if the project doesn't exist, we'll create it below.
	var cached CachedProject
	_ = idx.projectIndex.GetJSON(ctx, key, &cached)

	if cached.Name == "" {
		cached.Name = project
		cached.CachedAt = idx.now()
	}
	if cached.Files == nil {
		cached.Files = make(map[string]*CachedFile)
	}

	cached.Files[filename] = &CachedFile{
		Filename:       filename,
		ContentHash:    hash,
		Size:           size,
		Hashes:         hashes,
		RequiresPython: requiresPython,
		UpstreamURL:    upstreamURL,
		CachedAt:       idx.now(),
	}
	cached.UpdatedAt = idx.now()

	// Collect all blob refs
	refs := collectBlobRefs(&cached)

	return idx.projectIndex.PutJSON(ctx, key, &cached, refs)
}

// GetCachedFile retrieves cached file info for a specific file.
func (idx *Index) GetCachedFile(ctx context.Context, project, filename string) (*CachedFile, error) {
	cached, err := idx.GetCachedProject(ctx, project)
	if err != nil {
		return nil, err
	}

	file, ok := cached.Files[filename]
	if !ok {
		return nil, ErrNotFound
	}

	return file, nil
}

// DeleteProject removes all cached data for a project.
func (idx *Index) DeleteProject(ctx context.Context, name string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	key := NormalizeProjectName(name)
	if err := idx.projectIndex.Delete(ctx, key); err != nil && err != metadb.ErrNotFound {
		return err
	}

	return nil
}

// ListProjects returns all cached project names.
func (idx *Index) ListProjects(ctx context.Context) ([]string, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	keys, err := idx.projectIndex.List(ctx)
	if err != nil {
		return nil, err
	}

	// Keys are already normalized project names
	return keys, nil
}

// IsExpired checks if a cached project has exceeded the TTL.
func (idx *Index) IsExpired(project *CachedProject, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return idx.now().Sub(project.UpdatedAt) > ttl
}

// collectBlobRefs extracts all blob hashes from a CachedProject.
// Returns refs in canonical format: "blake3:<hex>"
func collectBlobRefs(project *CachedProject) []string {
	if project == nil || project.Files == nil {
		return nil
	}

	refs := make([]string, 0, len(project.Files))
	for _, file := range project.Files {
		if file != nil && !file.ContentHash.IsZero() {
			refs = append(refs, contentcache.NewBlobRef(file.ContentHash).String())
		}
	}
	return refs
}

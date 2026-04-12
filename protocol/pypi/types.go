// Package pypi implements the PyPI Simple Repository API for caching Python packages.
package pypi

import (
	"time"

	contentcache "github.com/buildkite/content-cache"
)

// APIMeta contains API version information (PEP 691).
type APIMeta struct {
	APIVersion string `json:"api-version"`
}

// ProjectList represents the root index response (PEP 691).
type ProjectList struct {
	Meta     APIMeta          `json:"meta"`
	Projects []ProjectSummary `json:"projects"`
}

// ProjectSummary represents a project in the root index.
type ProjectSummary struct {
	Name string `json:"name"`
}

// ProjectPage represents the project detail response (PEP 691).
type ProjectPage struct {
	Meta  APIMeta       `json:"meta"`
	Name  string        `json:"name"`
	Files []ProjectFile `json:"files"`
}

// ProjectFile represents a downloadable file for a project.
type ProjectFile struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes,omitempty"`
	RequiresPython string            `json:"requires-python,omitempty"`
	Yanked         any               `json:"yanked,omitempty"` // bool or string reason
	GPGSig         bool              `json:"gpg-sig,omitempty"`
	DistInfoMeta   any               `json:"dist-info-metadata,omitempty"` // bool or hash dict
}

// CachedProject stores cached project information.
type CachedProject struct {
	Name      string                 `json:"name"`
	Files     map[string]*CachedFile `json:"files"` // filename -> CachedFile
	CachedAt  time.Time              `json:"cached_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// CachedFile stores cached file information.
type CachedFile struct {
	Filename       string            `json:"filename"`
	ContentHash    contentcache.Hash `json:"content_hash"` // BLAKE3 hash in CAFS
	Size           int64             `json:"size"`
	Hashes         map[string]string `json:"hashes"` // Original hashes (sha256, etc.)
	RequiresPython string            `json:"requires-python,omitempty"`
	UpstreamURL    string            `json:"upstream_url"`
	CachedAt       time.Time         `json:"cached_at"`
}

// CurrentAPIVersion is the current Simple API version.
const CurrentAPIVersion = "1.0"

// Content types for the Simple API (PEP 691).
const (
	ContentTypeJSON = "application/vnd.pypi.simple.v1+json"
	ContentTypeHTML = "text/html"
)

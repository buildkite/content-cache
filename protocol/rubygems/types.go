// Package rubygems implements the RubyGems registry API for caching Ruby gems.
package rubygems

import (
	"errors"
	"regexp"
	"strings"
	"time"

	contentcache "github.com/buildkite/content-cache"
)

// DefaultUpstreamURL is the default RubyGems.org URL.
const DefaultUpstreamURL = "https://rubygems.org"

// ErrNotFound indicates the requested resource was not found.
var ErrNotFound = errors.New("not found")

// CachedVersions stores the cached /versions file metadata.
type CachedVersions struct {
	ETag        string            `json:"etag"`
	ReprDigest  string            `json:"repr_digest"` // SHA256 from Repr-Digest header
	ContentHash contentcache.Hash `json:"content_hash"`
	Size        int64             `json:"size"` // For Range request support
	CachedAt    time.Time         `json:"cached_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// CachedGemInfo stores cached /info/{gem} metadata.
type CachedGemInfo struct {
	Name        string            `json:"name"`
	ETag        string            `json:"etag"`
	ReprDigest  string            `json:"repr_digest"` // SHA256 from Repr-Digest header
	MD5         string            `json:"md5"`         // From /versions file (for invalidation)
	ContentHash contentcache.Hash `json:"content_hash"`
	Size        int64             `json:"size"`
	Checksums   map[string]string `json:"checksums"` // version[-platform] -> SHA256 checksum
	CachedAt    time.Time         `json:"cached_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// CachedGem stores cached gem file metadata.
type CachedGem struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Platform    string            `json:"platform,omitempty"` // "ruby" if omitted
	Filename    string            `json:"filename"`
	ContentHash contentcache.Hash `json:"content_hash"` // BLAKE3 hash in CAFS
	Size        int64             `json:"size"`
	SHA256      string            `json:"sha256"` // From /info/{gem} checksum: field
	CachedAt    time.Time         `json:"cached_at"`
}

// CachedGemspec stores cached quick/Marshal.4.8/*.gemspec.rz metadata.
type CachedGemspec struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Platform    string            `json:"platform,omitempty"`
	ContentHash contentcache.Hash `json:"content_hash"`
	Size        int64             `json:"size"`
	CachedAt    time.Time         `json:"cached_at"`
}

// CachedSpecs stores cached specs.4.8.gz file metadata.
type CachedSpecs struct {
	Type       string    `json:"type"` // "specs", "latest_specs", "prerelease_specs"
	ETag       string    `json:"etag"`
	ReprDigest string    `json:"repr_digest"`
	Size       int64     `json:"size"`
	CachedAt   time.Time `json:"cached_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ParsedGemFilename contains the parsed components of a gem filename.
type ParsedGemFilename struct {
	Name     string
	Version  string
	Platform string // "ruby" if no platform specified
}

// versionStartRegex matches the start of a version number (digit, optionally preceded by hyphen).
var versionStartRegex = regexp.MustCompile(`-(\d)`)

// gemNameRegex matches valid RubyGems gem names.
// Gem names can contain: a-z, A-Z, 0-9, -, _
var gemNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// IsValidGemName validates a gem name to prevent path traversal and invalid characters.
// Gem names must contain only alphanumeric characters, hyphens, and underscores.
func IsValidGemName(name string) bool {
	if name == "" {
		return false
	}
	// Check for path traversal attempts
	if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	// Check for valid gem name characters
	return gemNameRegex.MatchString(name)
}

// ParseGemFilename parses a gem filename into name, version, and platform.
// Gem filenames have the format: {name}-{version}[-{platform}].gem
// This is non-trivial because gem names can contain hyphens.
//
// Algorithm:
// 1. Strip .gem suffix
// 2. Find the last hyphen where the suffix starts with a digit (version start)
// 3. Split version[-platform] at first hyphen after version (platforms don't start with digits)
func ParseGemFilename(filename string) (*ParsedGemFilename, error) {
	// Strip .gem suffix
	if !strings.HasSuffix(filename, ".gem") {
		return nil, errors.New("filename must end with .gem")
	}
	base := strings.TrimSuffix(filename, ".gem")

	// Find all positions where -digit occurs (potential version starts)
	matches := versionStartRegex.FindAllStringIndex(base, -1)
	if len(matches) == 0 {
		return nil, errors.New("cannot find version in filename")
	}

	// Use the last match as the version start (gem names can have version-like parts)
	lastMatch := matches[len(matches)-1]
	splitPos := lastMatch[0]

	name := base[:splitPos]
	versionPlatform := base[splitPos+1:] // Skip the hyphen

	if name == "" {
		return nil, errors.New("empty gem name")
	}

	// Now split version from platform
	// Version is everything up to the first hyphen where suffix doesn't start with digit
	// (platforms like x86_64-linux don't start with digits)
	version := versionPlatform
	platform := "ruby"

	// Find first hyphen in versionPlatform where suffix doesn't start with digit
	for i := 0; i < len(versionPlatform); i++ {
		if versionPlatform[i] == '-' && i+1 < len(versionPlatform) {
			nextChar := versionPlatform[i+1]
			if nextChar < '0' || nextChar > '9' {
				// This is the platform separator
				version = versionPlatform[:i]
				platform = versionPlatform[i+1:]
				break
			}
		}
	}

	return &ParsedGemFilename{
		Name:     name,
		Version:  version,
		Platform: platform,
	}, nil
}

// VersionPlatformKey returns the key used for checksum lookups (version or version-platform).
func (p *ParsedGemFilename) VersionPlatformKey() string {
	if p.Platform == "ruby" || p.Platform == "" {
		return p.Version
	}
	return p.Version + "-" + p.Platform
}

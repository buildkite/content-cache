// Package npm implements the NPM registry protocol for caching NPM packages.
package npm

import (
	"encoding/json"
	"time"

	contentcache "github.com/buildkite/content-cache"
)

// PackageMetadata represents the full metadata for an NPM package.
// This is returned by GET /{package} endpoint.
type PackageMetadata struct {
	ID             string                      `json:"_id"`
	Name           string                      `json:"name"`
	Description    string                      `json:"description,omitempty"`
	DistTags       map[string]string           `json:"dist-tags,omitempty"`
	Versions       map[string]*VersionMetadata `json:"versions,omitempty"`
	Time           map[string]time.Time        `json:"time,omitempty"`
	Author         *Person                     `json:"author,omitempty"`
	Maintainers    []*Person                   `json:"maintainers,omitempty"`
	Repository     *Repository                 `json:"repository,omitempty"`
	Keywords       []string                    `json:"keywords,omitempty"`
	License        any                         `json:"license,omitempty"` // Can be string or object
	Readme         string                      `json:"readme,omitempty"`
	ReadmeFilename string                      `json:"readmeFilename,omitempty"`
	Homepage       string                      `json:"homepage,omitempty"`
	Bugs           *Bugs                       `json:"bugs,omitempty"`
}

// VersionMetadata represents metadata for a specific package version.
type VersionMetadata struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Description          string            `json:"description,omitempty"`
	Main                 string            `json:"main,omitempty"`
	Types                string            `json:"types,omitempty"`
	Scripts              map[string]string `json:"scripts,omitempty"`
	Dependencies         map[string]string `json:"dependencies,omitempty"`
	DevDependencies      map[string]string `json:"devDependencies,omitempty"`
	PeerDependencies     map[string]string `json:"peerDependencies,omitempty"`
	OptionalDependencies map[string]string `json:"optionalDependencies,omitempty"`
	Engines              map[string]string `json:"engines,omitempty"`
	Author               *Person           `json:"author,omitempty"`
	Maintainers          []*Person         `json:"maintainers,omitempty"`
	Repository           *Repository       `json:"repository,omitempty"`
	Keywords             []string          `json:"keywords,omitempty"`
	License              any               `json:"license,omitempty"`
	Bugs                 *Bugs             `json:"bugs,omitempty"`
	Homepage             string            `json:"homepage,omitempty"`
	Dist                 *Dist             `json:"dist"`
	GitHead              string            `json:"gitHead,omitempty"`
	ID                   string            `json:"_id,omitempty"`
	NodeVersion          string            `json:"_nodeVersion,omitempty"`
	NPMVersion           string            `json:"_npmVersion,omitempty"`
	NPMUser              *Person           `json:"_npmUser,omitempty"`
	HasShrinkwrap        bool              `json:"_hasShrinkwrap,omitempty"`
}

// Dist contains distribution information for a package version.
type Dist struct {
	Integrity    string `json:"integrity,omitempty"` // SRI hash (e.g., "sha512-...")
	Shasum       string `json:"shasum"`              // SHA-1 hash
	Tarball      string `json:"tarball"`             // URL to download tarball
	FileCount    int    `json:"fileCount,omitempty"`
	UnpackedSize int64  `json:"unpackedSize,omitempty"`
	NPMSignature string `json:"npm-signature,omitempty"`
}

// Person represents a person (author, maintainer, etc.).
type Person struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Repository contains repository information.
type Repository struct {
	Type      string `json:"type,omitempty"`
	URL       string `json:"url,omitempty"`
	Directory string `json:"directory,omitempty"`
}

// Bugs contains bug reporting information.
type Bugs struct {
	URL   string `json:"url,omitempty"`
	Email string `json:"email,omitempty"`
}

// CachedPackage stores cached package information including content hashes.
type CachedPackage struct {
	Name      string                    `json:"name"`
	Versions  map[string]*CachedVersion `json:"versions"`
	Metadata  json.RawMessage           `json:"metadata"` // Full metadata JSON
	CachedAt  time.Time                 `json:"cached_at"`
	UpdatedAt time.Time                 `json:"updated_at"`
}

// CachedVersion stores cached version information.
type CachedVersion struct {
	Version     string            `json:"version"`
	TarballHash contentcache.Hash `json:"tarball_hash"` // Hash in CAFS
	TarballSize int64             `json:"tarball_size"`
	Shasum      string            `json:"shasum"`    // Original SHA-1
	Integrity   string            `json:"integrity"` // Original SRI hash
	CachedAt    time.Time         `json:"cached_at"`
}

// AbbreviatedMetadata is a minimal version of package metadata.
// Used for faster responses when full metadata isn't needed.
type AbbreviatedMetadata struct {
	Name     string                         `json:"name"`
	DistTags map[string]string              `json:"dist-tags,omitempty"`
	Versions map[string]*AbbreviatedVersion `json:"versions,omitempty"`
	Modified time.Time                      `json:"modified,omitempty"`
}

// AbbreviatedVersion is minimal version metadata.
type AbbreviatedVersion struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Dependencies         map[string]string `json:"dependencies,omitempty"`
	DevDependencies      map[string]string `json:"devDependencies,omitempty"`
	PeerDependencies     map[string]string `json:"peerDependencies,omitempty"`
	OptionalDependencies map[string]string `json:"optionalDependencies,omitempty"`
	Dist                 *Dist             `json:"dist"`
	Engines              map[string]string `json:"engines,omitempty"`
	HasShrinkwrap        bool              `json:"_hasShrinkwrap,omitempty"`
}

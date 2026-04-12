// Package oci implements the OCI Distribution Specification v2 protocol
// as a read-through cache for container registries.
package oci

import (
	"time"

	contentcache "github.com/buildkite/content-cache"
)

// Media types for OCI and Docker manifests.
const (
	MediaTypeDockerManifestV2      = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList    = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeOCIManifest           = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex              = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerContainerConfig = "application/vnd.docker.container.image.v1+json"
	MediaTypeOCIConfig             = "application/vnd.oci.image.config.v1+json"
)

// Descriptor describes a blob in an OCI manifest.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"` // sha256:...
	Size        int64             `json:"size"`
	URLs        []string          `json:"urls,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Platform describes the OS and architecture of a manifest.
type Platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
	Features     []string `json:"features,omitempty"`
}

// ManifestDescriptor extends Descriptor with platform information
// for manifest lists/indexes.
type ManifestDescriptor struct {
	Descriptor
	Platform *Platform `json:"platform,omitempty"`
}

// Manifest represents an OCI image manifest (v2).
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ManifestList represents an OCI image index (for multi-arch images).
type ManifestList struct {
	SchemaVersion int                  `json:"schemaVersion"`
	MediaType     string               `json:"mediaType,omitempty"`
	Manifests     []ManifestDescriptor `json:"manifests"`
	Annotations   map[string]string    `json:"annotations,omitempty"`
}

// CachedImage stores cached image metadata including tag mappings.
type CachedImage struct {
	Name      string                `json:"name"`
	Tags      map[string]*CachedTag `json:"tags"`
	CachedAt  time.Time             `json:"cached_at"`
	UpdatedAt time.Time             `json:"updated_at"`
}

// CachedTag tracks a tag->digest mapping with TTL info.
type CachedTag struct {
	Tag         string    `json:"tag"`
	Digest      string    `json:"digest"` // Current digest for this tag
	CachedAt    time.Time `json:"cached_at"`
	RefreshedAt time.Time `json:"refreshed_at"` // Last upstream check
}

// CachedManifest stores a cached manifest's metadata.
type CachedManifest struct {
	Digest      string            `json:"digest"`
	MediaType   string            `json:"media_type"`
	ContentHash contentcache.Hash `json:"content_hash"` // Hash in CAFS
	Size        int64             `json:"size"`
	CachedAt    time.Time         `json:"cached_at"`
}

// CachedBlob stores cached blob metadata.
type CachedBlob struct {
	Digest      string            `json:"digest"`       // sha256:...
	ContentHash contentcache.Hash `json:"content_hash"` // BLAKE3 hash in CAFS
	Size        int64             `json:"size"`
	CachedAt    time.Time         `json:"cached_at"`
}

// AuthChallenge represents a parsed WWW-Authenticate Bearer challenge.
type AuthChallenge struct {
	Realm   string
	Service string
	Scope   string
}

// TokenResponse represents an authentication token response.
type TokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"` // Some registries use this field
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// DefaultAcceptHeader is the Accept header sent when fetching manifests.
var DefaultAcceptHeader = []string{
	MediaTypeDockerManifestV2,
	MediaTypeDockerManifestList,
	MediaTypeOCIManifest,
	MediaTypeOCIIndex,
}

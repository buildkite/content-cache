// Package maven implements the Maven repository protocol for caching Maven artifacts.
package maven

import (
	"encoding/xml"
	"time"

	contentcache "github.com/buildkite/content-cache"
)

// MavenMetadata represents the content of a maven-metadata.xml file.
type MavenMetadata struct {
	XMLName    xml.Name   `xml:"metadata" json:"-"`
	GroupID    string     `xml:"groupId" json:"group_id"`
	ArtifactID string     `xml:"artifactId" json:"artifact_id"`
	Version    string     `xml:"version,omitempty" json:"version,omitempty"`
	Versioning Versioning `xml:"versioning" json:"versioning"`
}

// Versioning contains version information within maven-metadata.xml.
type Versioning struct {
	Latest      string   `xml:"latest,omitempty" json:"latest,omitempty"`
	Release     string   `xml:"release,omitempty" json:"release,omitempty"`
	Versions    Versions `xml:"versions" json:"versions"`
	LastUpdated string   `xml:"lastUpdated,omitempty" json:"last_updated,omitempty"`
}

// Versions is a wrapper for the list of versions in maven-metadata.xml.
type Versions struct {
	Version []string `xml:"version" json:"version"`
}

// CachedMetadata stores cached maven-metadata.xml information.
type CachedMetadata struct {
	GroupID    string    `json:"group_id"`
	ArtifactID string    `json:"artifact_id"`
	Metadata   []byte    `json:"metadata"` // Raw XML content
	CachedAt   time.Time `json:"cached_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CachedArtifact stores cached artifact information.
type CachedArtifact struct {
	GroupID    string            `json:"group_id"`
	ArtifactID string            `json:"artifact_id"`
	Version    string            `json:"version"`
	Classifier string            `json:"classifier,omitempty"` // e.g., "sources", "javadoc"
	Extension  string            `json:"extension"`            // e.g., "jar", "pom", "war"
	Hash       contentcache.Hash `json:"hash"`                 // BLAKE3 hash in CAFS
	Size       int64             `json:"size"`
	Checksums  Checksums         `json:"checksums"`
	CachedAt   time.Time         `json:"cached_at"`
}

// Checksums holds various checksum values for an artifact.
type Checksums struct {
	MD5    string `json:"md5,omitempty"`
	SHA1   string `json:"sha1,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	SHA512 string `json:"sha512,omitempty"`
}

// ArtifactCoordinate identifies a Maven artifact.
type ArtifactCoordinate struct {
	GroupID    string
	ArtifactID string
	Version    string
	Classifier string
	Extension  string
}

// Filename returns the standard Maven filename for this artifact.
func (c ArtifactCoordinate) Filename() string {
	name := c.ArtifactID + "-" + c.Version
	if c.Classifier != "" {
		name += "-" + c.Classifier
	}
	return name + "." + c.Extension
}

// GroupPath returns the group ID as a path (dots replaced with slashes).
func (c ArtifactCoordinate) GroupPath() string {
	return groupIDToPath(c.GroupID)
}

// FullPath returns the full repository path for this artifact.
func (c ArtifactCoordinate) FullPath() string {
	return c.GroupPath() + "/" + c.ArtifactID + "/" + c.Version + "/" + c.Filename()
}

// DefaultRepositoryURL is the default Maven Central repository URL.
const DefaultRepositoryURL = "https://repo.maven.apache.org/maven2"

// Common file extensions for Maven artifacts.
const (
	ExtensionJAR = "jar"
	ExtensionPOM = "pom"
	ExtensionWAR = "war"
	ExtensionEAR = "ear"
	ExtensionAAR = "aar"
	ExtensionZIP = "zip"
)

// Checksum file extensions.
const (
	ChecksumMD5    = "md5"
	ChecksumSHA1   = "sha1"
	ChecksumSHA256 = "sha256"
	ChecksumSHA512 = "sha512"
)

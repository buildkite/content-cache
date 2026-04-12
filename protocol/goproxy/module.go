// Package goproxy implements the GOPROXY protocol for caching Go modules.
package goproxy

import (
	"errors"
	"fmt"
	"strings"
	"time"

	contentcache "github.com/buildkite/content-cache"
)

// ErrNotFound is returned when a module or version is not found.
var ErrNotFound = errors.New("module not found")

// VersionInfo contains metadata about a module version.
// This is the JSON format returned by the GOPROXY /@v/{version}.info endpoint.
type VersionInfo struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time,omitempty"`
}

// ModuleVersion contains all cached data for a specific module version.
type ModuleVersion struct {
	Info    VersionInfo       `json:"info"`
	ModHash contentcache.Hash `json:"mod_hash,omitempty"` // Hash of go.mod in CAFS (if stored separately)
	ZipHash contentcache.Hash `json:"zip_hash"`           // Hash of module zip in CAFS
	ModFile []byte            `json:"-"`                  // go.mod content (loaded on demand)
}

// encodePath encodes a module path for use in URLs/filenames.
// Uppercase letters are replaced with '!' followed by the lowercase letter.
// This matches the Go module proxy protocol encoding.
func encodePath(modulePath string) string {
	var b strings.Builder
	for _, r := range modulePath {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// decodePath decodes a module path from URL/filename encoding.
func decodePath(encoded string) (string, error) {
	var b strings.Builder
	escape := false
	for _, r := range encoded {
		switch {
		case r == '!':
			escape = true
		case escape:
			if r >= 'a' && r <= 'z' {
				b.WriteRune(r - ('a' - 'A'))
			} else {
				return "", fmt.Errorf("invalid escape sequence: !%c", r)
			}
			escape = false
		default:
			b.WriteRune(r)
		}
	}
	if escape {
		return "", errors.New("trailing escape character")
	}
	return b.String(), nil
}

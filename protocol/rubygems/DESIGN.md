# RubyGems Protocol Handler Design

## Overview

This document describes the design for adding RubyGems caching proxy support to the content-cache project. The implementation will follow the established protocol handler patterns (goproxy, npm, pypi, maven) and support both modern Bundler (Compact Index API) and legacy `gem` command workflows.

## Customer Problem

Ruby developers using Bundler or the `gem` command need reliable, fast access to gems from rubygems.org. Network issues, rate limiting, or rubygems.org outages can disrupt CI/CD pipelines and local development. A caching proxy provides:

1. **Reliability**: Cached gems remain available during upstream outages
2. **Speed**: Local cache eliminates network round-trips for previously-fetched gems
3. **Bandwidth**: Reduces repeated downloads across builds/developers
4. **Auditability**: Visibility into which gems are being used

## Critical Design Constraints

### Compact Index Byte-Exactness

The Compact Index API (`/versions`, `/info/{gem}`, `/names`) has strict byte-level semantics that Bundler relies on:

1. **Never transform bytes**: No newline normalization, compression rewriting, or templating
2. **Request identity encoding**: Use `Accept-Encoding: identity` to upstream to avoid gzip complications with Range offsets
3. **Preserve headers**: Forward `ETag`, `Accept-Ranges`, `Content-Range`, and `Repr-Digest` headers
4. **Opaque content**: Store and serve raw bytes exactly as received from upstream

### Repr-Digest Header (Critical for Bundler)

RubyGems Compact Index responses include the `Repr-Digest` header containing SHA256 checksums. Bundler uses this to validate file completeness after Range appends:

```
Repr-Digest: sha-256=:base64encodedchecksum:
```

- **Store** the `Repr-Digest` value in cached metadata
- **Forward** it to clients in responses
- **Validate** after Range appends: compute SHA256 of full file, compare to `Repr-Digest`
- The MD5 in `/versions` is for `/info` invalidation only, not integrity

### Range Request Handling

The `/versions` file is append-only within a month but **rebuilt monthly** (can shrink/rewrite):

- Support `Range: bytes=N-` requests for incremental updates
- Handle `416 Range Not Satisfiable` gracefully (pass through to client; monthly rebuild invalidates cached sizes)
- On Range response (206), append to cached content and validate against `Repr-Digest`

## Route Registration

The handler will be mounted at `/rubygems/` to avoid conflicts with existing handlers:

| Existing Route | Protocol |
|----------------|----------|
| `/npm/`        | NPM      |
| `/pypi/`       | PyPI     |
| `/maven/`      | Maven    |
| `/goproxy/`    | Go Proxy |
| `/v2/`         | OCI      |
| `/{module...}` | Go Proxy (root fallback) |
| **`/rubygems/`** | **RubyGems (new)** |

## API Endpoints

### Compact Index API (Modern Bundler)

Used by Bundler 1.12+ for efficient dependency resolution.

| Endpoint | Purpose | Caching Strategy |
|----------|---------|------------------|
| `GET /rubygems/versions` | Master index: all gem names, versions, MD5 checksums | TTL for revalidation + ETag + Range + Repr-Digest |
| `GET /rubygems/info/{gem}` | Per-gem dependency/version info | TTL for revalidation + ETag + Repr-Digest |
| `GET /rubygems/names` | List of all gem names | TTL for revalidation (optional, rarely used) |

**Key Features**:
- HTTP `Range` requests for incremental updates (append new bytes)
- `ETag`/`If-None-Match` for cache revalidation
- `Repr-Digest` header with SHA256 for integrity verification
- `/versions` is append-only within a month, rebuilt monthly
- `/info/{gem}` is append-only for new versions, recalculated on yanks

### Legacy Specs API (gem command)

Used by the `gem` command and older Bundler versions.

| Endpoint | Purpose | Caching Strategy |
|----------|---------|------------------|
| `GET /rubygems/specs.4.8.gz` | All [name, version, platform] tuples | TTL-based (changes with new releases) |
| `GET /rubygems/latest_specs.4.8.gz` | Latest versions only | TTL-based |
| `GET /rubygems/prerelease_specs.4.8.gz` | Prerelease versions | TTL-based |
| `GET /rubygems/quick/Marshal.4.8/{gem}-{version}.gemspec.rz` | Marshalled gemspec per version | Immutable (content-addressed) |

### Gem Downloads

| Endpoint | Purpose | Caching Strategy |
|----------|---------|------------------|
| `GET /rubygems/gems/{gem}-{version}.gem` | The actual gem file | Immutable (content-addressed in CAFS) |
| `GET /rubygems/gems/{gem}-{version}-{platform}.gem` | Platform-specific gem | Immutable (content-addressed in CAFS) |

#### Gem Filename Parsing

Parsing `{gem}-{version}[-{platform}].gem` is non-trivial because gem names can contain hyphens. Algorithm:

1. Strip `.gem` suffix
2. Find the **last hyphen** where the suffix **starts with a digit** → this separates `name` from `version[-platform]`
3. Split `version[-platform]` at the first hyphen after the version (platforms usually don't start with digits)

Examples:
- `rails-7.1.0.gem` → name=`rails`, version=`7.1.0`, platform=`ruby`
- `nokogiri-1.15.4-x86_64-linux.gem` → name=`nokogiri`, version=`1.15.4`, platform=`x86_64-linux`
- `aws-sdk-s3-1.140.0.gem` → name=`aws-sdk-s3`, version=`1.140.0`, platform=`ruby`

**Fallback**: If parsing fails, log a warning and proceed without integrity verification (rather than failing the request).

## Package Structure

```
protocol/rubygems/
├── handler.go        # HTTP handler with ServeHTTP, routing logic
├── handler_test.go   # Handler tests
├── upstream.go       # Client for fetching from rubygems.org
├── upstream_test.go  # Upstream client tests
├── index.go          # Metadata storage (versions, info files, gemspecs)
├── index_test.go     # Index tests
├── types.go          # Data structures
└── compact.go        # Compact Index parsing utilities
```

## Data Types

```go
// types.go

package rubygems

import (
    "time"
    contentcache "github.com/buildkite/content-cache"
)

// DefaultUpstreamURL is the default RubyGems.org URL.
const DefaultUpstreamURL = "https://rubygems.org"

// ErrNotFound indicates the requested resource was not found.
var ErrNotFound = errors.New("not found")

// CachedVersions stores the cached /versions file metadata.
type CachedVersions struct {
    ETag       string    `json:"etag"`
    ReprDigest string    `json:"repr_digest"`  // SHA256 from Repr-Digest header (critical for Bundler)
    Size       int64     `json:"size"`         // For Range request support
    CachedAt   time.Time `json:"cached_at"`
    UpdatedAt  time.Time `json:"updated_at"`
}

// CachedGemInfo stores cached /info/{gem} metadata.
type CachedGemInfo struct {
    Name       string             `json:"name"`
    ETag       string             `json:"etag"`
    ReprDigest string             `json:"repr_digest"`  // SHA256 from Repr-Digest header
    MD5        string             `json:"md5"`          // From /versions file (for invalidation, not integrity)
    Size       int64              `json:"size"`
    Checksums  map[string]string  `json:"checksums"`    // version -> SHA256 checksum (from checksum: field)
    CachedAt   time.Time          `json:"cached_at"`
    UpdatedAt  time.Time          `json:"updated_at"`
}

// CachedGem stores cached gem file metadata.
type CachedGem struct {
    Name        string            `json:"name"`
    Version     string            `json:"version"`
    Platform    string            `json:"platform,omitempty"` // "ruby" if omitted
    Filename    string            `json:"filename"`
    ContentHash contentcache.Hash `json:"content_hash"`       // BLAKE3 hash in CAFS
    Size        int64             `json:"size"`
    SHA256      string            `json:"sha256"`             // From /info/{gem} checksum: field
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
    Type      string    `json:"type"`       // "specs", "latest_specs", "prerelease_specs"
    ETag      string    `json:"etag"`
    Size      int64     `json:"size"`
    CachedAt  time.Time `json:"cached_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
```

## Handler Implementation

```go
// handler.go (outline)

package rubygems

import (
    "net/http"
    "strings"
)

type Handler struct {
    index       *Index
    store       store.Store
    upstream    *Upstream
    logger      *slog.Logger
    metadataTTL time.Duration  // TTL for versions/info/specs files
    
    wg     sync.WaitGroup
    ctx    context.Context
    cancel context.CancelFunc
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet && r.Method != http.MethodHead {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }

    path := r.URL.Path

    switch {
    // Compact Index API
    case path == "/versions":
        h.handleVersions(w, r)
    case path == "/names":
        h.handleNames(w, r)
    case strings.HasPrefix(path, "/info/"):
        gemName := strings.TrimPrefix(path, "/info/")
        h.handleInfo(w, r, gemName)
    
    // Legacy Specs API
    case path == "/specs.4.8.gz":
        h.handleSpecs(w, r, "specs")
    case path == "/latest_specs.4.8.gz":
        h.handleSpecs(w, r, "latest_specs")
    case path == "/prerelease_specs.4.8.gz":
        h.handleSpecs(w, r, "prerelease_specs")
    case strings.HasPrefix(path, "/quick/Marshal.4.8/"):
        h.handleGemspec(w, r)
    
    // Gem downloads
    case strings.HasPrefix(path, "/gems/"):
        h.handleGem(w, r)
    
    default:
        http.NotFound(w, r)
    }
}
```

## Upstream Client

```go
// upstream.go (outline)

package rubygems

type Upstream struct {
    baseURL    string
    httpClient *http.Client
}

// VersionsResponse contains the response from fetching /versions.
type VersionsResponse struct {
    Body       io.ReadCloser
    StatusCode int               // 200, 206 (partial), 304 (not modified), 416 (range not satisfiable)
    ETag       string
    ReprDigest string            // SHA256 from Repr-Digest header
    Size       int64             // Content-Length or full size
}

// InfoResponse contains the response from fetching /info/{gem}.
type InfoResponse struct {
    Body       io.ReadCloser
    StatusCode int
    ETag       string
    ReprDigest string
    Size       int64
}

// Compact Index methods
// CRITICAL: Always request with Accept-Encoding: identity to avoid gzip complications with Range
func (u *Upstream) FetchVersions(ctx context.Context, etag string, rangeStart int64) (*VersionsResponse, error)
func (u *Upstream) FetchInfo(ctx context.Context, gem string, etag string, rangeStart int64) (*InfoResponse, error)
func (u *Upstream) FetchNames(ctx context.Context) (io.ReadCloser, error)

// Legacy Specs methods
func (u *Upstream) FetchSpecs(ctx context.Context, specsType string, etag string) (*SpecsResponse, error)
func (u *Upstream) FetchGemspec(ctx context.Context, gem, version, platform string) (io.ReadCloser, error)

// Gem download
func (u *Upstream) FetchGem(ctx context.Context, filename string) (io.ReadCloser, int64, error)
```

### Upstream Request Headers

For Compact Index endpoints, always set:

```go
req.Header.Set("Accept-Encoding", "identity")  // Critical: avoid gzip for Range compatibility
req.Header.Set("Accept", "text/plain")
if etag != "" {
    req.Header.Set("If-None-Match", etag)
}
if rangeStart > 0 {
    req.Header.Set("Range", fmt.Sprintf("bytes=%d-", rangeStart))
}
```

## Index Storage

```go
// index.go (outline)

package rubygems

type Index struct {
    backend backend.Backend
}

// Storage paths:
// rubygems/versions.json           - CachedVersions metadata
// rubygems/versions                 - Raw /versions file content
// rubygems/names                    - Raw /names file content
// rubygems/info/{gem}.json         - CachedGemInfo metadata
// rubygems/info/{gem}              - Raw /info/{gem} content
// rubygems/specs/{type}.json       - CachedSpecs metadata (type = specs|latest_specs|prerelease_specs)
// rubygems/specs/{type}.4.8.gz     - Raw specs file content
// rubygems/gemspecs/{gem}-{ver}.json  - CachedGemspec metadata
// rubygems/gems/{filename}.json    - CachedGem metadata

func (i *Index) GetVersions(ctx context.Context) (*CachedVersions, []byte, error)
func (i *Index) PutVersions(ctx context.Context, meta *CachedVersions, content []byte) error

func (i *Index) GetInfo(ctx context.Context, gem string) (*CachedGemInfo, []byte, error)
func (i *Index) PutInfo(ctx context.Context, gem string, meta *CachedGemInfo, content []byte) error

func (i *Index) GetSpecs(ctx context.Context, specsType string) (*CachedSpecs, []byte, error)
func (i *Index) PutSpecs(ctx context.Context, specsType string, meta *CachedSpecs, content []byte) error

func (i *Index) GetGem(ctx context.Context, filename string) (*CachedGem, error)
func (i *Index) PutGem(ctx context.Context, gem *CachedGem) error

func (i *Index) GetGemspec(ctx context.Context, gem, version, platform string) (*CachedGemspec, error)
func (i *Index) PutGemspec(ctx context.Context, spec *CachedGemspec) error

func (i *Index) IsExpired(cachedAt time.Time, ttl time.Duration) bool
```

## Server Integration

### Configuration

Add to `server.Config`:

```go
// UpstreamRubyGems is the upstream RubyGems registry URL
UpstreamRubyGems string

// RubyGemsMetadataTTL is how long to cache versions/info/specs files.
// Default: 5 minutes (new versions may be published)
RubyGemsMetadataTTL time.Duration
```

### Server Struct Fields

Add to `Server` struct in `server/http.go`:

```go
rubygemsIndex *rubygems.Index
rubygems      *rubygems.Handler
```

### Handler Initialization

Add to `New()` in `server/http.go` (following PyPI/Maven pattern):

```go
// Initialize RubyGems components
rubygemsIndex := rubygems.NewIndex(fsBackend)
rubygemsUpstreamOpts := []rubygems.UpstreamOption{}
if cfg.UpstreamRubyGems != "" {
    rubygemsUpstreamOpts = append(rubygemsUpstreamOpts, rubygems.WithRegistryURL(cfg.UpstreamRubyGems))
}
rubygemsUpstream := rubygems.NewUpstream(rubygemsUpstreamOpts...)
rubygemsHandlerOpts := []rubygems.HandlerOption{
    rubygems.WithUpstream(rubygemsUpstream),
    rubygems.WithLogger(cfg.Logger.With("component", "rubygems")),
}
if cfg.RubyGemsMetadataTTL > 0 {
    rubygemsHandlerOpts = append(rubygemsHandlerOpts, rubygems.WithMetadataTTL(cfg.RubyGemsMetadataTTL))
}
rubygemsHandler := rubygems.NewHandler(rubygemsIndex, cafsStore, rubygemsHandlerOpts...)
```

### Route Registration

Add to `registerRoutes()` in `server/http.go`:

```go
// RubyGems registry endpoints
// The rubygems handler handles all paths under /rubygems/
mux.Handle("GET /rubygems/", http.StripPrefix("/rubygems", s.rubygems))
mux.Handle("HEAD /rubygems/", http.StripPrefix("/rubygems", s.rubygems))
```

Add to `deriveProtocol()`:

```go
case len(path) >= 10 && path[:10] == "/rubygems/":
    return "rubygems"
```

### Command-Line Flags

Add to `cmd/content-cache/main.go`:

```go
upstreamRubyGems    = flag.String("upstream-rubygems", "", "Upstream RubyGems registry URL (default: https://rubygems.org)")
rubygemsMetadataTTL = flag.Duration("rubygems-metadata-ttl", 5*time.Minute, "TTL for RubyGems metadata (versions, info, specs)")
```

## Client Configuration

### Bundler (Gemfile)

```ruby
# Use the caching proxy (trailing slash recommended)
source "http://localhost:8080/rubygems/"

gem "rails"
gem "puma"
```

Or via environment variable:

```bash
export BUNDLE_MIRROR__HTTPS://RUBYGEMS__ORG="http://localhost:8080/rubygems/"
bundle install
```

### gem command

```bash
gem sources --add http://localhost:8080/rubygems/
gem sources --remove https://rubygems.org/

gem install rails
```

**Note**: Both `/rubygems` and `/rubygems/` should work. The handler accepts requests without trailing slash.

## Caching Strategy

### TTL Semantics

TTL is used to decide **when to revalidate**, not "serve stale until TTL expires":

1. On cache hit with valid TTL: serve from cache, honor client `If-None-Match` (return 304 if matches)
2. On TTL expiry: fetch upstream using `If-None-Match` and/or `Range`; update cache
3. On 304 response: update TTL, serve cached content
4. On 206 response: append to cached content, validate with `Repr-Digest`

| Content Type | Strategy | Rationale |
|--------------|----------|-----------|
| `/versions` | TTL + ETag + Range + Repr-Digest | Append-only within month, rebuilt monthly |
| `/info/{gem}` | TTL + ETag + Repr-Digest | Append-only for new versions, recalculated on yanks |
| `/names` | TTL + ETag | Rarely changes, simple list |
| `specs.4.8.gz` | TTL + ETag | Changes with new releases |
| `*.gemspec.rz` | Immutable (CAFS) | Version-specific, never changes |
| `*.gem` | Immutable (CAFS) | Content-addressed, integrity verified by SHA256 |

### Integrity Verification

- **Gem files**: SHA256 checksum from `/info/{gem}` `checksum:` field verified before serving
- **Compact Index files**: `Repr-Digest` SHA256 validates completeness after Range appends
- **Gemspec files**: Content is immutable once published
- **MD5 in /versions**: Used only for `/info` invalidation, not integrity verification

### Gem Checksum Lookup

To verify a `.gem` download:

1. Parse filename to extract `name`, `version`, `platform`
2. Look up `/info/{name}` (fetch if not cached)
3. Parse the `/info` file to find the line for `version[-platform]`
4. Extract `checksum:sha256hex` from the requirements section
5. Compare computed SHA256 of downloaded gem against expected

If lookup fails (gem info not cached, parse error), log warning and serve without verification.

## Implementation Phases

### Phase 1: Core Infrastructure
1. Create `protocol/rubygems/` package structure
2. Implement `types.go` with data structures
3. Implement `index.go` for metadata storage
4. Add basic tests

### Phase 2: Compact Index API
1. Implement `upstream.go` for Compact Index endpoints
   - Use `Accept-Encoding: identity` for all requests
   - Parse `Repr-Digest` header from responses
   - Support Range requests and 206/304/416 handling
2. Implement handler methods: `handleVersions`, `handleInfo`, `handleNames`
   - Treat content as opaque bytes (no transformation)
   - Forward `Repr-Digest`, `ETag`, `Accept-Ranges` headers
3. Implement `compact.go` for parsing `/info/{gem}` to extract checksums
4. Add Range request support with `Repr-Digest` validation
5. Integration tests with Bundler (byte-exact roundtrip tests)

### Phase 3: Gem Downloads
1. Implement gem filename parsing with comprehensive test cases
   - Test hyphenated gem names: `aws-sdk-s3`, `net-http-persistent`
   - Test platform gems: `nokogiri-1.15.4-x86_64-linux.gem`
2. Implement `handleGem` for gem file downloads
3. Look up expected SHA256 from `/info/{gem}` checksums
4. Store gems in CAFS with SHA256 verification
5. Stream while caching (following PyPI pattern)

### Phase 4: Legacy Specs API
1. Implement `handleSpecs` for specs.4.8.gz endpoints
2. Implement `handleGemspec` for quick/Marshal.4.8 endpoint
3. Integration tests with `gem` command

### Phase 5: Server Integration
1. Add configuration options
2. Register routes in server
3. Add command-line flags
4. Update README with usage examples

## Testing Strategy

1. **Unit tests**: Mock upstream, test each handler method
2. **Byte-exact roundtrip tests**: Verify Compact Index content is unchanged
   - Fetch from upstream, cache, serve to client
   - Compare bytes exactly (no normalization)
3. **Gem filename parsing tests**: Comprehensive edge cases
   - Simple: `rails-7.1.0.gem`
   - Hyphenated: `aws-sdk-s3-1.140.0.gem`
   - Platform: `nokogiri-1.15.4-x86_64-linux.gem`
   - Prerelease: `rails-7.2.0.beta1.gem`
4. **Range request tests**: 
   - 206 Partial Content handling
   - 416 Range Not Satisfiable passthrough
   - `Repr-Digest` validation after append
5. **Integration tests**: Run against real rubygems.org (rate-limited)
6. **End-to-end tests**: 
   - `bundle install` with proxy
   - `gem install` with proxy
   - Cache hit/miss scenarios

## Risks and Guardrails

| Risk | Impact | Guardrail |
|------|--------|-----------|
| Corrupting Compact Index data (newline changes, gzip, partial merges) | Bundler may fail or fall back to full fetch | Treat bodies as opaque bytes; request identity encoding; byte-exact roundtrip tests |
| Incorrect gem checksum verification due to filename parsing | False negatives break installs | Robust parsing with test cases; bypass verification on parse failure (log warning) |
| Range edge cases on monthly rebuild (upstream 416) | Clients get stale data or errors | Pass through 416; don't "fix up" with cached sizes |
| Repr-Digest mismatch after Range append | Corrupted index data | Validate SHA256 after append; refetch full file on mismatch |

## Future Enhancements

1. **Proxy-side incremental refresh**: Background `Range` requests on TTL expiry to keep cache warm
2. **API v1 JSON endpoints**: `/api/v1/gems/{name}.json` for metadata queries
3. **Dependencies endpoint**: `/api/v1/dependencies?gems=...` for legacy dependency resolution (older Bundler)
4. **Private gem support**: Authentication for private gem sources
5. **Gem push support**: `POST /api/v1/gems` for publishing (requires auth)

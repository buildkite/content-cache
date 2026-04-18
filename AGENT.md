# content-cache Development Guide

## Quick Commands

```bash
# Install the pinned toolchain
mise install

# Build
mise run build

# Run tests
mise run test

# Run tests with coverage
mise run coverage

# Lint (matches CI)
mise run lint

# Format code (auto-fix imports and formatting)
mise run fix

# Run server locally
./content-cache -address :8080 -storage ./cache -log-level debug
```

## Project Structure

```
cmd/content-cache/     # Main entrypoint
server/                # HTTP server, routing, middleware
protocol/              # Protocol handlers (one package per protocol)
  ├── goproxy/         # Go module proxy (GOPROXY)
  ├── npm/             # NPM registry
  ├── oci/             # OCI/Docker registry
  ├── pypi/            # Python Package Index
  ├── maven/           # Maven repository
  └── git/             # Git Smart HTTP proxy
download/              # Singleflight-based download deduplication
store/                 # Content-addressable storage (CAFS)
  ├── gc/              # Garbage collection (TTL expiry, orphan cleanup)
  ├── metadb/          # BoltDB metadata index (blob entries, meta entries)
  └── s3fifo/          # S3-FIFO eviction algorithm (queues + manager)
backend/               # Storage backends (filesystem, future: S3)
cache/                 # Local cache directory (gitignored)
```

## Protocol Handler Pattern

Each protocol package follows a consistent structure:
- `handler.go` - HTTP handler with ServeHTTP method
- `upstream.go` - Client for fetching from upstream registry
- `index.go` - Metadata storage and lookup
- `types.go` - Data structures and constants
- `*_test.go` - Tests for each component

When adding a new protocol:
1. Create a new package under `protocol/`
2. Implement Handler, Upstream, and Index types
3. Register routes in `server/http.go` (`registerRoutes`)
4. Add configuration flags in `cmd/content-cache/main.go`
5. Update README.md with usage examples
6. Add OpenTelemetry metrics via `telemetry/metrics.go` (counter + histogram as appropriate)

## Code Style

- **Logging**: ALWAYS use `"log/slog"` for all logging operations
- **Testing**: Use `testify/require` for assertions
- **Error handling**: Return errors up the stack, log at top level only
- **Package names**: Lowercase, descriptive (goproxy, npm, oci, pypi, maven, git)
- **Context scopes**: Three distinct scopes exist — do not mix them up:
  - `r.Context()` — request-scoped; cancelled when the HTTP request ends. Use for all synchronous work inside a handler.
  - `h.ctx` — handler-scoped; cancelled when the server shuts down (`h.cancel()` in `Close()`). Use for background goroutines spawned by a handler so they are cancelled cleanly on shutdown.
  - `context.Background()` — unbounded; use only when neither of the above applies. Do NOT use this for handler background goroutines.
  - Background goroutines must always use `context.WithTimeout(h.ctx, cacheTimeout)`, not `context.WithTimeout(context.Background(), cacheTimeout)`.
- **Options pattern**: Use functional options for configurable types (see `WithLogger`, `WithUpstream`)
- **Metrics**: Every feature must ship with metrics. Request-lifecycle attributes (protocol, outcome, endpoint) go on `RequestTags` via setters in `telemetry/tags.go`; recording happens in `RecordHTTP` / `RecordBackendOp`. Do not call metrics APIs directly from handlers or middleware — set tags instead.

## Documentation Style

When creating documentation (README, code comments, design docs):
- Start with the customer problem and work backwards
- Use clear, concise, and data-driven language
- Include specific examples and concrete details
- Structure with clear headings and bullet points
- Focus on operational excellence, security, and scalability
- Include implementation details and edge cases

## Commit Message Style

Use conventional commits format:
```
feat: add npm registry support and TTL cache expiration

- Add npm protocol handler with tarball caching and integrity verification
- Implement expiry system with TTL expiration and S3-FIFO size eviction
- Fix golangci-lint errors across codebase
```

Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `test`

## CI/CD (Buildkite)

Pipeline runs on every push:
1. **base_image** - Rebuilds Docker image when `.buildkite/Dockerfile.build` changes (main only)
2. **QA group** - Runs in parallel:
   - `golangci-lint run --verbose --timeout 3m` via the repo `mise.toml`
   - `go test -coverprofile coverage.out -coverpkg=./... ./...` via the repo `mise.toml`

The base image is cached at `${BUILDKITE_HOSTED_REGISTRY_URL}/content_cache_base:latest`.

## Key Dependencies

- `github.com/zeebo/blake3` - BLAKE3 hashing for content addressing
- `github.com/google/uuid` - Request ID generation
- `github.com/stretchr/testify` - Test assertions
- `golang.org/x/net` - Extended networking utilities
- `golang.org/x/sync` - Singleflight for download deduplication

## Planned Features

- S3 storage backend
- Compression (zstd)

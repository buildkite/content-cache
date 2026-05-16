// Command content-cache is a content-addressable cache server for Go modules and NPM packages.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	httppprof "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/buildkite/content-cache/auth"
	"github.com/buildkite/content-cache/credentials"
	"github.com/buildkite/content-cache/credentials/opprovider"
	"github.com/buildkite/content-cache/server"
	"github.com/buildkite/content-cache/telemetry"
	"github.com/lmittmann/tint"
)

var version = "dev"

type CLI struct {
	Version kong.VersionFlag `kong:"short='v',help='Print version and exit'"`

	Serve     ServeCmd     `kong:"cmd,default='1',help='Start the cache server'"`
	Cacheprog CacheprogCmd `kong:"cmd,help='Run as a GOCACHEPROG subprocess'"`
}

type ServeCmd struct {
	ListenAddress string `kong:"name='listen',default=':8080',env='LISTEN_ADDRESS',help='Address to listen on',group='Server'"`
	Storage       string `kong:"name='storage',default='./cache',env='CACHE_STORAGE',help='Storage directory path',group='Server'"`
	TLSCertFile   string `kong:"name='tls-cert',env='TLS_CERT_FILE',type='existingfile',help='Path to TLS certificate file (enables HTTPS)',group='Server'"`
	TLSKeyFile    string `kong:"name='tls-key',env='TLS_KEY_FILE',type='existingfile',help='Path to TLS private key file (enables HTTPS)',group='Server'"`

	AuthToken        string `kong:"name='auth-token',env='AUTH_TOKEN',help='Bearer token for inbound authentication (mutually exclusive with --oidc-policies)',group='Auth'"`
	AuthTokenFile    string `kong:"name='auth-token-file',env='AUTH_TOKEN_FILE',type='existingfile',help='Path to file containing auth token (for k8s secret mounts)',group='Auth'"`
	CredentialsFile  string `kong:"name='credentials-file',env='CREDENTIALS_FILE',type='existingfile',help='Path to credentials template file for upstream auth',group='Auth'"`
	OIDCPoliciesFile string `kong:"name='oidc-policies',env='OIDC_POLICIES_FILE',type='existingfile',help='Path to OIDC trust policies JSON file (mutually exclusive with --auth-token)',group='Auth'"`

	GoUpstream       string `kong:"name='go-upstream',env='GO_UPSTREAM',help='Upstream Go module proxy URL (default: proxy.golang.org)',group='Upstream'"`
	NPMUpstream      string `kong:"name='npm-upstream',env='NPM_UPSTREAM',help='Upstream NPM registry URL (default: registry.npmjs.org)',group='Upstream'"`
	OCIUpstream      string `kong:"name='oci-upstream',env='OCI_UPSTREAM',help='Upstream OCI registry URL (default: registry-1.docker.io)',group='Upstream'"`
	PyPIUpstream     string `kong:"name='pypi-upstream',env='PYPI_UPSTREAM',help='Upstream PyPI Simple API URL (default: pypi.org/simple/)',group='Upstream'"`
	MavenUpstream    string `kong:"name='maven-upstream',env='MAVEN_UPSTREAM',help='Upstream Maven repository URL (default: repo.maven.apache.org/maven2)',group='Upstream'"`
	RubyGemsUpstream string `kong:"name='rubygems-upstream',env='RUBYGEMS_UPSTREAM',help='Upstream RubyGems registry URL (default: rubygems.org)',group='Upstream'"`

	GitAllowedHosts       []string `kong:"name='git-allowed-hosts',env='GIT_ALLOWED_HOSTS',help='Comma-separated list of allowed Git upstream hosts (e.g. github.com,gitlab.com)',group='Git'"`
	GitMaxRequestBodySize int64    `kong:"name='git-max-request-body',default='104857600',env='GIT_MAX_REQUEST_BODY',help='Maximum git-upload-pack request body size in bytes (default: 100MB)',group='Git'"`

	FetchAllowedHosts []string `kong:"name='fetch-allowed-hosts',env='FETCH_ALLOWED_HOSTS',help='Comma-separated list of allowed upstream hosts for the generic /fetch cache (e.g. raw.githubusercontent.com,releases.hashicorp.com)',group='Fetch'"`

	OCIPrefix string        `kong:"name='oci-prefix',default='docker-hub',env='OCI_PREFIX',help='Routing prefix for the OCI registry',group='OCI'"`
	OCITagTTL time.Duration `kong:"name='oci-tag-ttl',default='5m',env='OCI_TAG_TTL',help='TTL for OCI tag->digest cache mappings',group='OCI'"`

	GoProxyMetadataTTL  time.Duration `kong:"name='goproxy-metadata-ttl',default='24h',env='GOPROXY_METADATA_TTL',help='TTL for Go module list cache (immutable mod/info/zip use blob retention)',group='TTL'"`
	NPMMetadataTTL      time.Duration `kong:"name='npm-metadata-ttl',default='24h',env='NPM_METADATA_TTL',help='TTL for NPM package metadata cache',group='TTL'"`
	FetchMetadataTTL    time.Duration `kong:"name='fetch-metadata-ttl',default='24h',env='FETCH_METADATA_TTL',help='TTL for direct download cache metadata under /fetch and /github-release',group='TTL'"`
	PyPIMetadataTTL     time.Duration `kong:"name='pypi-metadata-ttl',default='5m',env='PYPI_METADATA_TTL',help='TTL for PyPI project metadata cache',group='TTL'"`
	MavenMetadataTTL    time.Duration `kong:"name='maven-metadata-ttl',default='5m',env='MAVEN_METADATA_TTL',help='TTL for maven-metadata.xml cache',group='TTL'"`
	RubyGemsMetadataTTL time.Duration `kong:"name='rubygems-metadata-ttl',default='5m',env='RUBYGEMS_METADATA_TTL',help='TTL for RubyGems metadata cache',group='TTL'"`
	HTTPCacheTTL        time.Duration `kong:"name='httpcache-ttl',default='24h',env='HTTPCACHE_TTL',help='TTL for sccache/Gradle HTTP build cache entries',group='TTL'"`

	HTTPCachePrefix string `kong:"name='httpcache-prefix',default='httpcache',env='HTTPCACHE_PREFIX',help='URL path prefix for the HTTP cache endpoint (sccache/Gradle)',group='Cache'"`

	BlobRetention       time.Duration `kong:"name='blob-retention',default='24h',env='BLOB_RETENTION',help='Minimum time to retain blobs after last access before GC may delete them (0 to disable)',group='Cache'"`
	CacheMaxSize        int64         `kong:"name='cache-max-size',default='10737418240',env='CACHE_MAX_SIZE',help='Maximum cache size in bytes (default: 10GB, 0 to disable)',group='Cache'"`
	ExpiryCheckInterval time.Duration `kong:"name='expiry-check-interval',default='1h',env='EXPIRY_CHECK_INTERVAL',help='How often to check for expired content',group='Cache'"`
	GCInterval          time.Duration `kong:"name='gc-interval',default='1h',env='GC_INTERVAL',help='How often to run garbage collection',group='Cache'"`
	GCStartupDelay      time.Duration `kong:"name='gc-startup-delay',default='5m',env='GC_STARTUP_DELAY',help='Delay before first GC run after startup',group='Cache'"`

	MetadataDSN string `kong:"name='metadata-dsn',env='METADATA_DSN',help='Metadata database path (default: <storage>/metadata.db)',group='Storage'"`

	LogLevel  string `kong:"name='log-level',default='info',env='LOG_LEVEL',enum='debug,info,warn,error',help='Log level',group='Logging'"`
	LogFormat string `kong:"name='log-format',default='text',env='LOG_FORMAT',enum='text,json',help='Log format',group='Logging'"`

	PprofAddress string `kong:"name='pprof-address',env='PPROF_ADDRESS',help='Address for pprof HTTP server (e.g., localhost:6060); disabled if empty',group='Debug'"`

	MetricsOTLPEndpoint string        `kong:"name='metrics-otlp-endpoint',env='METRICS_OTLP_ENDPOINT',help='OTLP gRPC endpoint for metrics (e.g., localhost:4317)',group='Metrics'"`
	MetricsPrometheus   bool          `kong:"name='metrics-prometheus',env='METRICS_PROMETHEUS',help='Enable Prometheus /metrics endpoint',group='Metrics'"`
	MetricsInterval     time.Duration `kong:"name='metrics-interval',default='10s',env='METRICS_INTERVAL',help='Metrics export interval',group='Metrics'"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("content-cache"),
		kong.Description("A content-addressable cache server for Go modules, NPM packages, OCI images, PyPI, Maven, RubyGems, Git repositories, and direct download artefacts."),
		kong.Vars{"version": version},
		kong.UsageOnError(),
	)

	switch ctx.Command() {
	case "serve":
		return cli.Serve.Run()
	case "cacheprog":
		return cli.Cacheprog.Run()
	default:
		return fmt.Errorf("unknown command: %s", ctx.Command())
	}
}

func (cmd *ServeCmd) Run() error {
	// Resolve auth token from file if specified
	authToken := cmd.AuthToken
	if cmd.AuthTokenFile != "" {
		data, err := os.ReadFile(cmd.AuthTokenFile)
		if err != nil {
			return fmt.Errorf("reading auth token file: %w", err)
		}
		authToken = strings.TrimSpace(string(data))
	}

	// Setup logger
	var level slog.Level
	switch cmd.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	switch cmd.LogFormat {
	case "text":
		handler = tint.NewHandler(os.Stderr, &tint.Options{
			Level: level,
		})
	case "json":
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)

	// Initialize metrics
	metricsCfg := telemetry.MetricsConfig{
		ServiceName:      "content-cache",
		ServiceVersion:   "0.1.0",
		OTLPEndpoint:     cmd.MetricsOTLPEndpoint,
		EnablePrometheus: cmd.MetricsPrometheus,
		FlushInterval:    cmd.MetricsInterval,
	}
	metricsShutdown, err := telemetry.InitMetrics(context.Background(), metricsCfg)
	if err != nil {
		return fmt.Errorf("initializing metrics: %w", err)
	}
	if cmd.MetricsOTLPEndpoint != "" {
		logger.Info("metrics OTLP export enabled", "endpoint", cmd.MetricsOTLPEndpoint)
	}
	if cmd.MetricsPrometheus {
		logger.Info("metrics Prometheus endpoint enabled", "path", "/metrics")
	}

	// Resolve credentials file if specified
	var creds *credentials.Credentials
	if cmd.CredentialsFile != "" {
		resolver := credentials.NewResolver(
			credentials.WithLogger(logger),
			opprovider.WithOnePassword(),
		)

		credCtx, credCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer credCancel()

		creds, err = resolver.ResolveFile(credCtx, cmd.CredentialsFile)
		if err != nil {
			return fmt.Errorf("resolving credentials: %w", err)
		}
		logger.Info("credentials file resolved", "path", cmd.CredentialsFile)
	}

	// Auth token: CLI flag takes precedence over credentials file
	if authToken == "" && creds != nil {
		authToken = creds.AuthToken
	}

	// OIDC policies and static bearer token are mutually exclusive.
	if authToken != "" && cmd.OIDCPoliciesFile != "" {
		return fmt.Errorf("--auth-token and --oidc-policies are mutually exclusive")
	}

	// Initialize OIDC validator if a policies file is provided.
	var oidcValidator *auth.OIDCValidator
	if cmd.OIDCPoliciesFile != "" {
		policies, err := auth.LoadPoliciesFromFile(cmd.OIDCPoliciesFile)
		if err != nil {
			return fmt.Errorf("loading OIDC policies: %w", err)
		}
		oidcCtx, oidcCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer oidcCancel()
		oidcValidator, err = auth.NewOIDCValidator(oidcCtx, policies, logger)
		if err != nil {
			return fmt.Errorf("initializing OIDC validator: %w", err)
		}
		logger.Info("OIDC auth enabled", "policies", len(policies))
		if cmd.TLSCertFile == "" {
			logger.Warn("OIDC auth configured without TLS — tokens will be transmitted in plaintext")
		}
	}

	logger.Info("inbound auth", "enabled", authToken != "" || oidcValidator != nil)

	// Warn if static token auth enabled without TLS
	if authToken != "" && cmd.TLSCertFile == "" {
		logger.Warn("auth token configured without TLS — bearer tokens will be transmitted in plaintext")
	}

	// Create server
	cfg := server.Config{
		Address:               cmd.ListenAddress,
		StoragePath:           cmd.Storage,
		TLSCertFile:           cmd.TLSCertFile,
		TLSKeyFile:            cmd.TLSKeyFile,
		AuthToken:             authToken,
		OIDCValidator:         oidcValidator,
		Credentials:           creds,
		UpstreamGoProxy:       cmd.GoUpstream,
		GoProxyMetadataTTL:    cmd.GoProxyMetadataTTL,
		UpstreamNPMRegistry:   cmd.NPMUpstream,
		NPMMetadataTTL:        cmd.NPMMetadataTTL,
		UpstreamOCIRegistry:   cmd.OCIUpstream,
		OCIPrefix:             cmd.OCIPrefix,
		OCITagTTL:             cmd.OCITagTTL,
		UpstreamPyPI:          cmd.PyPIUpstream,
		PyPIMetadataTTL:       cmd.PyPIMetadataTTL,
		UpstreamMaven:         cmd.MavenUpstream,
		MavenMetadataTTL:      cmd.MavenMetadataTTL,
		UpstreamRubyGems:      cmd.RubyGemsUpstream,
		RubyGemsMetadataTTL:   cmd.RubyGemsMetadataTTL,
		GitAllowedHosts:       cmd.GitAllowedHosts,
		FetchAllowedHosts:     cmd.FetchAllowedHosts,
		FetchMetadataTTL:      cmd.FetchMetadataTTL,
		HTTPCacheTTL:          cmd.HTTPCacheTTL,
		HTTPCachePrefix:       cmd.HTTPCachePrefix,
		GitMaxRequestBodySize: cmd.GitMaxRequestBodySize,
		BlobRetention:         cmd.BlobRetention,
		CacheMaxSize:          cmd.CacheMaxSize,
		ExpiryCheckInterval:   cmd.ExpiryCheckInterval,
		GCInterval:            cmd.GCInterval,
		GCStartupDelay:        cmd.GCStartupDelay,
		MetadataDSN:           cmd.MetadataDSN,
		Logger:                logger,
	}

	srv, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("creating server: %w", err)
	}

	// Start pprof server if configured
	if cmd.PprofAddress != "" {
		pprofMux := http.NewServeMux()
		pprofMux.HandleFunc("/debug/pprof/", httppprof.Index)
		pprofMux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
		pprofMux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
		pprofMux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
		pprofMux.HandleFunc("/debug/pprof/trace", httppprof.Trace)
		pprofServer := &http.Server{
			Addr:        cmd.PprofAddress,
			Handler:     pprofMux,
			ReadTimeout: 30 * time.Second,
			// WriteTimeout is intentionally long to allow pprof profile collection
			WriteTimeout: 5 * time.Minute,
		}
		go func() {
			logger.Info("pprof server started", "address", cmd.PprofAddress)
			if err := pprofServer.ListenAndServe(); err != nil {
				logger.Error("pprof server error", "error", err)
			}
		}()
	}

	// Handle shutdown signals
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Start server in goroutine
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Start(); err != nil {
			errCh <- err
		}
	}()

	logger.Info("server started", "address", srv.Address())

	// Wait for shutdown or error
	select {
	case <-ctx.Done():
		// Graceful shutdown
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		// Shutdown server first
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown error", "error", err)
		}

		// Flush and shutdown metrics (use shorter timeout for CI environments)
		metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer metricsCancel()
		if err := metricsShutdown(metricsCtx); err != nil {
			logger.Error("metrics shutdown error", "error", err)
		}
		logger.Info("metrics flushed")

		return nil
	case err := <-errCh:
		// Server error - still try to flush metrics
		metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer metricsCancel()
		_ = metricsShutdown(metricsCtx)
		return err
	}
}

// Package telemetry provides request tagging for structured logging and metrics.
package telemetry

import (
	"context"
	"net/http"
)

type contextKey string

const (
	// requestTagsKey is the context key for request tags holder.
	requestTagsKey contextKey = "request_tags"
	// protocolKey is the context key for propagating protocol to background goroutines.
	protocolKey contextKey = "protocol"
)

// CacheResult represents the outcome of a cache lookup.
type CacheResult string

const (
	// CacheCoalesced identifies requests waiting on an existing metadata fetch.
	CacheCoalesced CacheResult = "coalesced"
	CacheHit       CacheResult = "hit"
	CacheMiss      CacheResult = "miss"
	CacheBypass    CacheResult = "bypass"
	CacheNA        CacheResult = "na"
)

// AuthOutcome represents the result of an authentication check.
type AuthOutcome string

const (
	AuthOutcomeAllowed      AuthOutcome = "allowed"
	AuthOutcomeUnauthorized AuthOutcome = "unauthorized"
	AuthOutcomeForbidden    AuthOutcome = "forbidden"
)

// RequestTags holds mutable request metadata that handlers can set for logging.
type RequestTags struct {
	Protocol    string
	CacheResult CacheResult
	Endpoint    string
	AuthOutcome AuthOutcome // empty string means the route is not auth-protected
}

// InjectTags creates a new request with an empty RequestTags in context.
// Call this in middleware before handlers run.
func InjectTags(r *http.Request) *http.Request {
	tags := &RequestTags{CacheResult: CacheBypass}
	return r.WithContext(context.WithValue(r.Context(), requestTagsKey, tags))
}

// GetTags retrieves the request tags from context.
// Returns nil if not in a request context with logging middleware.
func GetTags(r *http.Request) *RequestTags {
	if tags, ok := r.Context().Value(requestTagsKey).(*RequestTags); ok {
		return tags
	}
	return nil
}

// SetCacheResult sets the cache result for logging.
func SetCacheResult(r *http.Request, result CacheResult) {
	if tags := GetTags(r); tags != nil {
		tags.CacheResult = result
	}
}

// SetProtocol sets the protocol tag for metrics and logging.
func SetProtocol(r *http.Request, protocol string) {
	if tags := GetTags(r); tags != nil {
		tags.Protocol = protocol
	}
}

// SetEndpoint sets the endpoint type for logging.
func SetEndpoint(r *http.Request, endpoint string) {
	if tags := GetTags(r); tags != nil {
		tags.Endpoint = endpoint
	}
}

// SetAuthOutcome sets the auth outcome tag for metrics and logging.
func SetAuthOutcome(r *http.Request, outcome AuthOutcome) {
	if tags := GetTags(r); tags != nil {
		tags.AuthOutcome = outcome
	}
}

// ProtocolFromContext retrieves the protocol from a context.
// It checks both background contexts (set by WithProtocolContext) and
// request contexts (set by SetProtocol middleware via InjectTags).
func ProtocolFromContext(ctx context.Context) string {
	if p, ok := ctx.Value(protocolKey).(string); ok && p != "" {
		return p
	}
	if tags, ok := ctx.Value(requestTagsKey).(*RequestTags); ok && tags != nil {
		return tags.Protocol
	}
	return ""
}

// WithProtocolContext returns a context with the protocol stored.
// Use this to propagate the protocol into goroutines that outlive the request context.
func WithProtocolContext(ctx context.Context, protocol string) context.Context {
	return context.WithValue(ctx, protocolKey, protocol)
}

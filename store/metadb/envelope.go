package metadb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	// CompressionThreshold is the minimum payload size before compression is considered.
	// 2KB threshold - zstd overhead not worth it for smaller payloads.
	CompressionThreshold = 2048

	// MaxPayloadSize is the default uncompressed metadata limit.
	MaxPayloadSize = 64 * 1024 * 1024 // 64MiB

	// MaxDecompressedSize is the default decompression limit.
	MaxDecompressedSize = MaxPayloadSize

	// MaxAllowedPayloadSize bounds the configurable limit.
	MaxAllowedPayloadSize = 256 * 1024 * 1024

	// MaxAttributeKeys is the maximum number of attribute keys allowed.
	MaxAttributeKeys = 10

	// MaxAttributesSize is the maximum total size of all attributes.
	MaxAttributesSize = 1024 // 1KB

	// CurrentEnvelopeVersion is the current envelope schema version.
	CurrentEnvelopeVersion = 1
)

var (
	// ErrPayloadTooLarge is returned when payload exceeds the configured limit.
	ErrPayloadTooLarge = errors.New("payload exceeds maximum size")

	// ErrDecompressionBomb is returned when decompressed size exceeds limit.
	ErrDecompressionBomb = errors.New("decompressed payload exceeds maximum size")

	// ErrCorrupted is returned when payload digest verification fails.
	ErrCorrupted = errors.New("payload digest mismatch")

	// ErrTooManyAttributes is returned when attributes exceed MaxAttributeKeys.
	ErrTooManyAttributes = errors.New("too many attributes")

	// ErrAttributesTooLarge is returned when attributes exceed MaxAttributesSize.
	ErrAttributesTooLarge = errors.New("attributes exceed maximum size")

	// ErrInvalidDigestFormat is returned when a blob ref has invalid format.
	ErrInvalidDigestFormat = errors.New("invalid digest format")
)

// EnvelopeCodec handles envelope encoding/decoding with optional compression.
// encoderPool holds reusable zstd encoders; decoder is goroutine-safe.
type EnvelopeCodec struct {
	maxPayloadSize int
	encoderPool    sync.Pool
	decoder        *zstd.Decoder
	mu             sync.RWMutex
}

// EnvelopeCodecOption configures metadata encoding and decoding.
type EnvelopeCodecOption func(*EnvelopeCodec) error

// ValidateMaxPayloadSize checks a metadata limit before opening storage.
func ValidateMaxPayloadSize(size int) error {
	if size < 1 || size > MaxAllowedPayloadSize {
		return fmt.Errorf("metadata maximum size must be between 1 and %d bytes", MaxAllowedPayloadSize)
	}
	return nil
}

// WithMaxPayloadSize applies the same limit to writes and decompression.
func WithMaxPayloadSize(size int) EnvelopeCodecOption {
	return func(c *EnvelopeCodec) error {
		if err := ValidateMaxPayloadSize(size); err != nil {
			return err
		}
		c.maxPayloadSize = size
		return nil
	}
}

// NewEnvelopeCodec creates a new codec with a pooled zstd encoder and shared decoder.
func NewEnvelopeCodec(opts ...EnvelopeCodecOption) (*EnvelopeCodec, error) {
	c := &EnvelopeCodec{maxPayloadSize: MaxPayloadSize}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(uint64(c.maxPayloadSize)))
	if err != nil {
		return nil, fmt.Errorf("creating zstd decoder: %w", err)
	}

	c.decoder = dec
	c.encoderPool = sync.Pool{
		New: func() any {
			enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
			if err != nil {
				return nil
			}
			return enc
		},
	}

	return c, nil
}

// Close releases decoder resources. Pooled encoders are released by the GC.
func (c *EnvelopeCodec) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.decoder != nil {
		c.decoder.Close()
		c.decoder = nil
	}
}

// EncodePayload compresses payload if beneficial and returns encoded bytes with encoding type.
// Also computes and returns the digest of the original (uncompressed) payload.
func (c *EnvelopeCodec) EncodePayload(data []byte) (payload []byte, encoding ContentEncoding, digest string, err error) {
	if len(data) > c.maxPayloadSize {
		return nil, ContentEncoding_CONTENT_ENCODING_IDENTITY, "", ErrPayloadTooLarge
	}

	digest = computeDigest(data)

	if len(data) < CompressionThreshold {
		return data, ContentEncoding_CONTENT_ENCODING_IDENTITY, digest, nil
	}

	enc, _ := c.encoderPool.Get().(*zstd.Encoder)
	if enc == nil {
		return data, ContentEncoding_CONTENT_ENCODING_IDENTITY, digest, nil
	}

	var buf bytes.Buffer
	enc.Reset(&buf)
	if _, werr := enc.Write(data); werr != nil {
		enc.Close() //nolint:errcheck // discarding broken encoder
		return data, ContentEncoding_CONTENT_ENCODING_IDENTITY, digest, nil
	}
	if werr := enc.Close(); werr != nil {
		return data, ContentEncoding_CONTENT_ENCODING_IDENTITY, digest, nil
	}
	c.encoderPool.Put(enc)

	compressed := buf.Bytes()
	if len(compressed) >= len(data) {
		return data, ContentEncoding_CONTENT_ENCODING_IDENTITY, digest, nil
	}

	return compressed, ContentEncoding_CONTENT_ENCODING_ZSTD, digest, nil
}

// DecodePayload decompresses payload if needed and verifies digest.
func (c *EnvelopeCodec) DecodePayload(payload []byte, encoding ContentEncoding, expectedDigest string, expectedSize uint64) ([]byte, error) {
	if expectedSize > uint64(c.maxPayloadSize) {
		return nil, ErrDecompressionBomb
	}
	if encoding == ContentEncoding_CONTENT_ENCODING_IDENTITY {
		if len(payload) > c.maxPayloadSize {
			return nil, ErrDecompressionBomb
		}
		if expectedDigest != "" && computeDigest(payload) != expectedDigest {
			return nil, ErrCorrupted
		}
		return payload, nil
	}

	if encoding != ContentEncoding_CONTENT_ENCODING_ZSTD {
		return nil, fmt.Errorf("unsupported encoding: %v", encoding)
	}

	c.mu.RLock()
	dec := c.decoder
	c.mu.RUnlock()

	if dec == nil {
		return nil, errors.New("decoder not initialized")
	}

	decompressed, err := dec.DecodeAll(payload, nil)
	if err != nil {
		return nil, fmt.Errorf("decompressing payload: %w", err)
	}

	if len(decompressed) > c.maxPayloadSize {
		return nil, ErrDecompressionBomb
	}

	if expectedDigest != "" && computeDigest(decompressed) != expectedDigest {
		return nil, ErrCorrupted
	}

	return decompressed, nil
}

// computeDigest computes sha256 digest in canonical format.
func computeDigest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ValidateEnvelope validates an envelope before storage.
func ValidateEnvelope(env *MetadataEnvelope) error {
	if env == nil {
		return errors.New("envelope is nil")
	}

	if len(env.Attributes) > MaxAttributeKeys {
		return ErrTooManyAttributes
	}

	var totalSize int
	for _, v := range env.Attributes {
		totalSize += len(v)
	}
	if totalSize > MaxAttributesSize {
		return ErrAttributesTooLarge
	}

	for _, ref := range env.BlobRefs {
		if err := ValidateDigestFormat(ref); err != nil {
			return err
		}
	}

	return nil
}

// ValidateDigestFormat validates that a digest string is in canonical format.
// Accepted formats: "sha256:<64 hex chars>" or "blake3:<64 hex chars>"
func ValidateDigestFormat(digest string) error {
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("%w: missing algorithm prefix in %q", ErrInvalidDigestFormat, digest)
	}

	algo, hash := parts[0], parts[1]
	algo = strings.ToLower(algo)

	switch algo {
	case "sha256", "blake3":
		if len(hash) != 64 {
			return fmt.Errorf("%w: expected 64 hex chars, got %d in %q", ErrInvalidDigestFormat, len(hash), digest)
		}
		for _, c := range hash {
			isDigit := c >= '0' && c <= '9'
			isLowerHex := c >= 'a' && c <= 'f'
			isUpperHex := c >= 'A' && c <= 'F'
			if !isDigit && !isLowerHex && !isUpperHex {
				return fmt.Errorf("%w: invalid hex character in %q", ErrInvalidDigestFormat, digest)
			}
		}
	default:
		return fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidDigestFormat, algo)
	}

	return nil
}

// CanonicalizeRefs deduplicates, lowercases, and sorts blob refs.
func CanonicalizeRefs(refs []string) []string {
	if len(refs) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(refs))
	result := make([]string, 0, len(refs))

	for _, ref := range refs {
		lower := strings.ToLower(ref)
		if _, ok := seen[lower]; !ok {
			seen[lower] = struct{}{}
			result = append(result, lower)
		}
	}

	sort.Strings(result)
	return result
}

// DiffRefs computes added and removed refs between old and new sets.
// Both inputs should already be canonicalized.
func DiffRefs(oldRefs, newRefs []string) (added, removed []string) {
	oldSet := make(map[string]struct{}, len(oldRefs))
	newSet := make(map[string]struct{}, len(newRefs))

	for _, r := range oldRefs {
		oldSet[r] = struct{}{}
	}
	for _, r := range newRefs {
		newSet[r] = struct{}{}
	}

	for _, r := range newRefs {
		if _, ok := oldSet[r]; !ok {
			added = append(added, r)
		}
	}
	for _, r := range oldRefs {
		if _, ok := newSet[r]; !ok {
			removed = append(removed, r)
		}
	}

	return added, removed
}

// RefsEqual returns true if two ref slices contain the same refs (order-independent).
func RefsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aCopy := slices.Clone(a)
	bCopy := slices.Clone(b)
	sort.Strings(aCopy)
	sort.Strings(bCopy)
	return slices.Equal(aCopy, bCopy)
}

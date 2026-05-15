package backend

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var (
	// MagicBytes is the 4-byte prefix for framed blob files.
	MagicBytes = []byte("CCB1")

	// ErrInvalidMagic is returned when a file doesn't start with the expected magic bytes.
	ErrInvalidMagic = errors.New("invalid magic bytes: expected CCB1")

	// ErrHeaderTooLarge is returned when the header exceeds MaxHeaderSize.
	ErrHeaderTooLarge = errors.New("header exceeds maximum size")
)

// MaxHeaderSize is the maximum allowed size for the JSON header (64 KiB).
const MaxHeaderSize = 64 * 1024

// BlobHeader contains metadata for a cached blob.
type BlobHeader struct {
	ContentType   string `json:"content_type"`
	ContentLength int64  `json:"content_length"`
	CachedAt      string `json:"cached_at"`
	UpstreamETag  string `json:"upstream_etag,omitempty"`
	ContentHash   string `json:"content_hash"`
}

// WriteFramed writes a framed blob to the writer.
// Format: MAGIC (4 bytes) | HDRLEN (uint32 big-endian) | HDRBYTES (JSON) | BODYBYTES
func WriteFramed(w io.Writer, header *BlobHeader, body io.Reader) error {
	// Serialize header to JSON
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshaling header: %w", err)
	}

	headerLen := len(headerBytes)
	if headerLen > MaxHeaderSize {
		return ErrHeaderTooLarge
	}

	// Write magic bytes
	if _, err := w.Write(MagicBytes); err != nil {
		return fmt.Errorf("writing magic bytes: %w", err)
	}

	// Write header length as big-endian uint32
	if err := binary.Write(w, binary.BigEndian, uint32(headerLen)); err != nil { //nolint:gosec // headerLen is bounds-checked above
		return fmt.Errorf("writing header length: %w", err)
	}

	// Write header JSON
	if _, err := w.Write(headerBytes); err != nil {
		return fmt.Errorf("writing header: %w", err)
	}

	// Write body
	if _, err := io.Copy(w, body); err != nil {
		return fmt.Errorf("writing body: %w", err)
	}

	return nil
}

// ReadFramed reads a framed blob from the reader.
// Returns the parsed header and a reader for the body.
func ReadFramed(r io.Reader) (*BlobHeader, io.Reader, error) {
	// Read and verify magic bytes
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, nil, fmt.Errorf("reading magic bytes: %w", err)
	}
	if !bytes.Equal(magic, MagicBytes) {
		return nil, nil, ErrInvalidMagic
	}

	// Read header length
	var headerLen uint32
	if err := binary.Read(r, binary.BigEndian, &headerLen); err != nil {
		return nil, nil, fmt.Errorf("reading header length: %w", err)
	}

	if headerLen > MaxHeaderSize {
		return nil, nil, ErrHeaderTooLarge
	}

	// Read header JSON
	headerBytes := make([]byte, headerLen)
	if _, err := io.ReadFull(r, headerBytes); err != nil {
		return nil, nil, fmt.Errorf("reading header: %w", err)
	}

	// Parse header
	var header BlobHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, nil, fmt.Errorf("parsing header: %w", err)
	}

	// Return header and remaining reader for body
	return &header, r, nil
}

// IsFramed checks if the reader contains a framed blob by looking for magic bytes.
// The reader is seeked back to the start after checking.
func IsFramed(r io.ReadSeeker) (bool, error) {
	magic := make([]byte, 4)
	n, err := r.Read(magic)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading magic bytes: %w", err)
	}

	// Seek back to start
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false, fmt.Errorf("seeking to start: %w", err)
	}

	// Check if we read enough bytes and they match
	if n < 4 {
		return false, nil
	}

	return bytes.Equal(magic, MagicBytes), nil
}

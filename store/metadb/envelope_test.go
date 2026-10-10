package metadb

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvelopeCodec_EncodeDecodeRoundTrip(t *testing.T) {
	codec, err := NewEnvelopeCodec()
	require.NoError(t, err)
	defer codec.Close()

	tests := []struct {
		name    string
		data    []byte
		wantEnc ContentEncoding
	}{
		{
			name:    "small payload stays uncompressed",
			data:    []byte("hello world"),
			wantEnc: ContentEncoding_CONTENT_ENCODING_IDENTITY,
		},
		{
			name:    "empty payload",
			data:    []byte{},
			wantEnc: ContentEncoding_CONTENT_ENCODING_IDENTITY,
		},
		{
			name:    "large compressible payload gets compressed",
			data:    []byte(strings.Repeat("hello world ", 500)),
			wantEnc: ContentEncoding_CONTENT_ENCODING_ZSTD,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, encoding, digest, err := codec.EncodePayload(tt.data)
			require.NoError(t, err)
			require.Equal(t, tt.wantEnc, encoding)
			require.NotEmpty(t, digest)
			require.True(t, strings.HasPrefix(digest, "sha256:"))

			decoded, err := codec.DecodePayload(payload, encoding, digest, uint64(len(tt.data)))
			require.NoError(t, err)
			require.Equal(t, tt.data, decoded)
		})
	}
}

func TestEnvelopeCodec_IncompressibleData(t *testing.T) {
	codec, err := NewEnvelopeCodec()
	require.NoError(t, err)
	defer codec.Close()

	randomish := make([]byte, 3000)
	for i := range randomish {
		randomish[i] = byte(i * 7 % 256)
	}

	payload, encoding, digest, err := codec.EncodePayload(randomish)
	require.NoError(t, err)
	require.NotEmpty(t, digest)

	decoded, err := codec.DecodePayload(payload, encoding, digest, uint64(len(randomish)))
	require.NoError(t, err)
	require.Equal(t, randomish, decoded)
}

func TestEnvelopeCodec_PayloadTooLarge(t *testing.T) {
	codec, err := NewEnvelopeCodec()
	require.NoError(t, err)
	defer codec.Close()

	largeData := make([]byte, MaxPayloadSize+1)
	_, _, _, err = codec.EncodePayload(largeData)
	require.ErrorIs(t, err, ErrPayloadTooLarge)
}

func TestEnvelopeCodecConfiguredLimit(t *testing.T) {
	codec, err := NewEnvelopeCodec(WithMaxPayloadSize(4096))
	require.NoError(t, err)
	defer codec.Close()
	data := []byte(strings.Repeat("x", 4096))
	payload, encoding, digest, err := codec.EncodePayload(data)
	require.NoError(t, err)
	decoded, err := codec.DecodePayload(payload, encoding, digest, uint64(len(data)))
	require.NoError(t, err)
	require.Equal(t, data, decoded)
	_, _, _, err = codec.EncodePayload(make([]byte, 4097))
	require.ErrorIs(t, err, ErrPayloadTooLarge)
	for _, encoding := range []ContentEncoding{ContentEncoding_CONTENT_ENCODING_IDENTITY, ContentEncoding_CONTENT_ENCODING_ZSTD} {
		_, err = codec.DecodePayload(nil, encoding, "", 4097)
		require.ErrorIs(t, err, ErrDecompressionBomb)
	}
	_, err = codec.DecodePayload(make([]byte, 4097), ContentEncoding_CONTENT_ENCODING_IDENTITY, "", 0)
	require.ErrorIs(t, err, ErrDecompressionBomb)
	for _, limit := range []int{0, -1, MaxAllowedPayloadSize + 1} {
		_, err = NewEnvelopeCodec(WithMaxPayloadSize(limit))
		require.Error(t, err)
	}
}

func TestEnvelopeCodec_DigestVerification(t *testing.T) {
	codec, err := NewEnvelopeCodec()
	require.NoError(t, err)
	defer codec.Close()

	data := []byte("test data")
	payload, encoding, _, err := codec.EncodePayload(data)
	require.NoError(t, err)

	_, err = codec.DecodePayload(payload, encoding, "sha256:0000000000000000000000000000000000000000000000000000000000000000", uint64(len(data)))
	require.ErrorIs(t, err, ErrCorrupted)
}

func TestEnvelopeCodec_DecompressionBomb(t *testing.T) {
	codec, err := NewEnvelopeCodec()
	require.NoError(t, err)
	defer codec.Close()

	data := []byte(strings.Repeat("x", 5000))
	payload, encoding, digest, err := codec.EncodePayload(data)
	require.NoError(t, err)
	require.Equal(t, ContentEncoding_CONTENT_ENCODING_ZSTD, encoding)

	_, err = codec.DecodePayload(payload, encoding, digest, MaxDecompressedSize+1)
	require.ErrorIs(t, err, ErrDecompressionBomb)
}

func TestValidateDigestFormat(t *testing.T) {
	tests := []struct {
		name    string
		digest  string
		wantErr bool
	}{
		{"valid sha256", "sha256:a948904f2f0f479b8f8564cbf12dac6b5c4a0f7e0c94f5b5d6a2d3e8f1b2c3d4", false},
		{"valid blake3", "blake3:a948904f2f0f479b8f8564cbf12dac6b5c4a0f7e0c94f5b5d6a2d3e8f1b2c3d4", false},
		{"valid uppercase", "SHA256:A948904F2F0F479B8F8564CBF12DAC6B5C4A0F7E0C94F5B5D6A2D3E8F1B2C3D4", false},
		{"missing prefix", "a948904f2f0f479b8f8564cbf12dac6b5c4a0f7e0c94f5b5d6a2d3e8f1b2c3d4", true},
		{"wrong length", "sha256:abc123", true},
		{"invalid hex", "sha256:gggg904f2f0f479b8f8564cbf12dac6b5c4a0f7e0c94f5b5d6a2d3e8f1b2c3d4", true},
		{"unsupported algo", "md5:a948904f2f0f479b8f8564cbf12dac6b5c4a0f7e0c94f5b5d6a2d3e8f1b2c3d4", true},
		{"empty", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDigestFormat(tt.digest)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCanonicalizeRefs(t *testing.T) {
	tests := []struct {
		name   string
		input  []string
		expect []string
	}{
		{
			name:   "empty",
			input:  nil,
			expect: nil,
		},
		{
			name:   "single",
			input:  []string{"sha256:abc123"},
			expect: []string{"sha256:abc123"},
		},
		{
			name:   "deduplicates",
			input:  []string{"sha256:abc", "sha256:abc", "sha256:def"},
			expect: []string{"sha256:abc", "sha256:def"},
		},
		{
			name:   "lowercases",
			input:  []string{"SHA256:ABC", "sha256:def"},
			expect: []string{"sha256:abc", "sha256:def"},
		},
		{
			name:   "sorts",
			input:  []string{"sha256:zzz", "sha256:aaa", "blake3:mmm"},
			expect: []string{"blake3:mmm", "sha256:aaa", "sha256:zzz"},
		},
		{
			name:   "case-insensitive dedup",
			input:  []string{"SHA256:ABC", "sha256:abc"},
			expect: []string{"sha256:abc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CanonicalizeRefs(tt.input)
			require.Equal(t, tt.expect, result)
		})
	}
}

func TestEnvelopeDiffRefs(t *testing.T) {
	tests := []struct {
		name        string
		oldRefs     []string
		newRefs     []string
		wantAdded   []string
		wantRemoved []string
	}{
		{
			name:        "no change",
			oldRefs:     []string{"a", "b"},
			newRefs:     []string{"a", "b"},
			wantAdded:   nil,
			wantRemoved: nil,
		},
		{
			name:        "all new",
			oldRefs:     nil,
			newRefs:     []string{"a", "b"},
			wantAdded:   []string{"a", "b"},
			wantRemoved: nil,
		},
		{
			name:        "all removed",
			oldRefs:     []string{"a", "b"},
			newRefs:     nil,
			wantAdded:   nil,
			wantRemoved: []string{"a", "b"},
		},
		{
			name:        "mixed",
			oldRefs:     []string{"a", "b", "c"},
			newRefs:     []string{"b", "c", "d"},
			wantAdded:   []string{"d"},
			wantRemoved: []string{"a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			added, removed := DiffRefs(tt.oldRefs, tt.newRefs)
			require.Equal(t, tt.wantAdded, added)
			require.Equal(t, tt.wantRemoved, removed)
		})
	}
}

func TestRefsEqual(t *testing.T) {
	tests := []struct {
		name   string
		a      []string
		b      []string
		expect bool
	}{
		{"both nil", nil, nil, true},
		{"both empty", []string{}, []string{}, true},
		{"same order", []string{"a", "b"}, []string{"a", "b"}, true},
		{"different order", []string{"b", "a"}, []string{"a", "b"}, true},
		{"different length", []string{"a"}, []string{"a", "b"}, false},
		{"different content", []string{"a", "b"}, []string{"a", "c"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expect, RefsEqual(tt.a, tt.b))
		})
	}
}

func TestValidateEnvelope(t *testing.T) {
	t.Run("nil envelope", func(t *testing.T) {
		err := ValidateEnvelope(nil)
		require.Error(t, err)
	})

	t.Run("valid envelope", func(t *testing.T) {
		env := &MetadataEnvelope{
			EnvelopeVersion: 1,
			BlobRefs:        []string{"sha256:a948904f2f0f479b8f8564cbf12dac6b5c4a0f7e0c94f5b5d6a2d3e8f1b2c3d4"},
		}
		err := ValidateEnvelope(env)
		require.NoError(t, err)
	})

	t.Run("too many attributes", func(t *testing.T) {
		env := &MetadataEnvelope{
			EnvelopeVersion: 1,
			Attributes:      make(map[string][]byte),
		}
		for i := 0; i <= MaxAttributeKeys; i++ {
			env.Attributes[string(rune('a'+i))] = []byte("val")
		}
		err := ValidateEnvelope(env)
		require.ErrorIs(t, err, ErrTooManyAttributes)
	})

	t.Run("attributes too large", func(t *testing.T) {
		env := &MetadataEnvelope{
			EnvelopeVersion: 1,
			Attributes: map[string][]byte{
				"big": make([]byte, MaxAttributesSize+1),
			},
		}
		err := ValidateEnvelope(env)
		require.ErrorIs(t, err, ErrAttributesTooLarge)
	})

	t.Run("invalid blob ref", func(t *testing.T) {
		env := &MetadataEnvelope{
			EnvelopeVersion: 1,
			BlobRefs:        []string{"invalid-ref"},
		}
		err := ValidateEnvelope(env)
		require.ErrorIs(t, err, ErrInvalidDigestFormat)
	})
}

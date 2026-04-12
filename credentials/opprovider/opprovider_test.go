package opprovider

import (
	"testing"

	"github.com/buildkite/content-cache/credentials"
	"github.com/stretchr/testify/require"
)

func TestWithOnePassword_RegistersProvider(t *testing.T) {
	// We can only verify the option doesn't panic during construction.
	// Actually calling `op read` requires the 1Password CLI installed.
	opt := WithOnePassword()
	r := credentials.NewResolver(opt)
	require.NotNil(t, r)
}

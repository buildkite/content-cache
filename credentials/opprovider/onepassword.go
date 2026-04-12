package opprovider

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/buildkite/content-cache/credentials"
)

// WithOnePassword registers an "op" template function that resolves secrets
// using the 1Password CLI (`op read`).
func WithOnePassword() credentials.ResolverOption {
	return credentials.WithProvider("op", func(ctx context.Context, ref string) (string, error) {
		cmd := exec.CommandContext(ctx, "op", "read", ref)

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("op read %q: %s: %w", ref, strings.TrimSpace(stderr.String()), err)
		}

		return strings.TrimSpace(stdout.String()), nil
	})
}

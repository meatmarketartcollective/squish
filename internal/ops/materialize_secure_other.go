//go:build !linux

package ops

import (
	"fmt"

	"github.com/aloysiusvonfck/squish/internal/store"
)

// Secure materialization currently relies on Linux openat/O_NOFOLLOW semantics.
// Fail closed elsewhere rather than silently using path-based writes that can
// follow an attacker-controlled symlink.
func materializeSecure(_ *store.Store, _ *store.Manifest, _ string, _, _ bool) (int, error) {
	return 0, fmt.Errorf("secure materialization currently requires Linux")
}

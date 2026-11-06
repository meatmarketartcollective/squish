//go:build !linux

package ops

import (
	"github.com/aloysiusvonfck/squish/internal/store"
)

// materializeSecure on non-Linux platforms delegates to the path-based
// fallback (see materialize_fallback_other.go). The Linux build uses the
// openat/O_NOFOLLOW backend instead.
func materializeSecure(st *store.Store, m *store.Manifest, out string, useHardlinks, verifyEach bool) (int, error) {
	return materializeFallback(st, m, out, useHardlinks, verifyEach)
}

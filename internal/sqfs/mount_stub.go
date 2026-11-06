//go:build !linux

package sqfs

import (
	"errors"

	"github.com/aloysiusvonfck/squish/internal/store"
)

// Mount is only implemented on Linux builds of squish.
func Mount(st *store.Store, m *store.Manifest, mnt string) error {
	return errors.New("mount: only supported on Linux builds of squish")
}

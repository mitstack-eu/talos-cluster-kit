//go:build !linux

package packagecache

import (
	"io/fs"
	"time"
)

// lastUsed is info's modification time where the kit reads no access time.
func lastUsed(info fs.FileInfo) time.Time { return info.ModTime() }

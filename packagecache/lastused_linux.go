package packagecache

import (
	"io/fs"
	"syscall"
	"time"
)

// lastUsed is the later of info's access and modification times, since a registry reading a blob moves only its access
// time, at most once a day on a filesystem mounted relatime.
func lastUsed(info fs.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime()
	}
	if accessed := time.Unix(st.Atim.Unix()); accessed.After(info.ModTime()) {
		return accessed
	}
	return info.ModTime()
}

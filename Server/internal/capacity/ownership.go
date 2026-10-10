package capacity

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("capacity_random_failed")
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func privateRegular(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrUnsafeTarget
	}
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeTarget
		}
		if p == path {
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || st.Uid != uint32(os.Getuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				return ErrUnsafeTarget
			}
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return nil
}

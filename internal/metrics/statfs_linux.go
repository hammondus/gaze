package metrics

import (
	"fmt"
	"os"
	"syscall"
)

// statfsUsage returns total, used, and unprivileged-available bytes for a
// mounted filesystem, plus inode counts.
//
// Used is derived from blocks minus free rather than from blocks minus
// available, because the difference between the two is the reserve the kernel
// holds for root: it is genuinely occupied by nobody. Percent is then computed
// against used plus available, which is what df reports and what a user can
// act on.
func statfsUsage(path string) (total, used, avail, inodesUsed, inodesFree uint64, err error) {
	// A bind mount can attach a single file, and /proc/self/mounts lists it
	// alongside real filesystems. Docker does this for /etc/resolv.conf and
	// /etc/hosts. statfs on such a path succeeds and reports the filesystem
	// underneath, so the same disk appears two or three times over. Only a
	// directory is a filesystem worth reporting.
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if !fi.IsDir() {
		err = fmt.Errorf("%s is a bind-mounted file, not a filesystem", path)
		return
	}

	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return
	}
	bs := uint64(st.Bsize)
	total = st.Blocks * bs
	avail = st.Bavail * bs
	used = (st.Blocks - st.Bfree) * bs
	inodesFree = st.Ffree
	inodesUsed = st.Files - st.Ffree
	return
}

package metrics

import (
	"io"
	"strconv"
	"strings"
)

// hiddenMountPrefixes are paths whose mounts are noise on a normal screen:
// kernel interfaces, container overlays, and snap loopbacks. Trim this list if
// you want to see them.
var hiddenMountPrefixes = []string{
	"/proc", "/sys", "/dev", "/run",
	"/var/lib/docker", "/var/lib/containers", "/var/lib/kubelet",
	"/snap",
}

// keepPseudoFS lists filesystems that /proc/filesystems marks "nodev" but that
// still hold real data you want to see. Everything else marked nodev is a
// kernel interface and is dropped.
var keepPseudoFS = map[string]bool{
	"tmpfs": true, "zfs": true, "btrfs": true, "overlay": true,
	"nfs": true, "nfs4": true, "cifs": true, "fuse.sshfs": true,
}

// mountEntry is one line of /proc/self/mounts.
type mountEntry struct {
	device, path, fstype string
}

// parseMounts reads /proc/self/mounts.
//
// The kernel escapes space, tab, newline, and backslash in the device and path
// fields as octal, so both are unescaped before use. Without that, a mount
// under a path containing a space silently becomes two fields.
func parseMounts(r io.Reader) ([]mountEntry, error) {
	var out []mountEntry
	err := scanLines(r, func(line string) error {
		f := strings.Fields(line)
		if len(f) < 3 {
			return nil
		}
		out = append(out, mountEntry{
			device: unescapeMount(f[0]),
			path:   unescapeMount(f[1]),
			fstype: f[2],
		})
		return nil
	})
	return out, err
}

// unescapeMount decodes the three-digit octal escapes the kernel writes into
// the mount table.
func unescapeMount(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseFilesystems reads /proc/filesystems and returns the set of types the
// kernel marks "nodev", meaning they are not backed by a block device.
//
// Asking the kernel which filesystems are virtual beats hard-coding a list
// that goes stale every release.
func parseFilesystems(r io.Reader) (map[string]bool, error) {
	nodev := make(map[string]bool)
	err := scanLines(r, func(line string) error {
		if name, ok := strings.CutPrefix(line, "nodev"); ok {
			nodev[strings.TrimSpace(name)] = true
		}
		return nil
	})
	return nodev, err
}

// keepMount reports whether a mount belongs on screen.
func keepMount(m mountEntry, nodev map[string]bool, seen map[string]bool) bool {
	if nodev[m.fstype] && !keepPseudoFS[m.fstype] {
		return false
	}
	for _, p := range hiddenMountPrefixes {
		if m.path == p || strings.HasPrefix(m.path, p+"/") {
			return false
		}
	}
	// Bind mounts and btrfs subvolumes repeat one device across many paths.
	// Showing each would report the same disk several times over, so only the
	// first path for a device is kept.
	key := m.device + "\x00" + m.fstype
	if seen[key] {
		return false
	}
	seen[key] = true
	return true
}

// Package devices holds the naming rule that separates the hardware on a host
// from the interfaces and block devices a container runtime builds around it.
//
// One container is one veth, one user-defined Docker network is one br-
// bridge, and one image layer or snap is one loop device. A host running
// thirty containers lists thirty interfaces of no interest above the one NIC
// that carries the traffic, and the bridge and veth figures are the same bytes
// counted again on their way past it. Both front ends leave them out by
// default and report how many they dropped.
//
// The rule is a list of names known to be virtual, not a list of names known
// to be hardware. Both are lists that go stale, but they fail in opposite
// directions: a runtime that invents a new prefix leaves one unfamiliar
// interface on screen, which is what happens today anyway, whereas an
// unfamiliar NIC name matched against a list of real ones disappears along
// with the traffic on it.
//
// A tunnel is not on the list. wg0, tun0, and tap0 are software, but what
// moves over them is a real link to somewhere else, and on a host that reaches
// the world through a VPN it is the figure you came to read.
//
// The rule is presentation policy, not collection: the collector reports every
// device and the store keeps every one, so a stored report can still be asked
// what a bridge was doing. The terminal dashboard and the server's web pages
// each decide whether to draw them, and this is the one place that says what
// "virtual" means, so the two can never disagree.
//
// Standard library only.
package devices

import "strings"

// netPrefixes are the interface names container and VM runtimes build.
// A bridge is br0 by hand or br-<id> from a user-defined Docker network, and
// docker0 and podman0 are the default bridges; cni, flannel, and cali come from
// Kubernetes networking; virbr and vnet come from libvirt.
var netPrefixes = []string{
	"veth", "br", "docker", "podman", "cni", "flannel", "cali", "virbr", "vnet",
}

// VirtualNet reports whether an interface is a software construct rather than a
// link to hardware or to another machine.
func VirtualNet(name string) bool {
	if name == "lo" {
		return true
	}
	return hasAnyPrefix(name, netPrefixes)
}

// diskPrefixes are the block devices the kernel makes without hardware
// behind them: loop for images and snaps, ram for the fixed ramdisk set.
//
// zram is not on the list. A host has one of them, so it is never noise, and
// its I/O is real page traffic that a machine under memory pressure is doing.
// Nor is dm-, which is where the writes of an LVM or LUKS system actually
// appear, or md, which is the array you built.
var diskPrefixes = []string{"loop", "ram"}

// VirtualDisk reports whether a block device is backed by a file or by memory
// rather than by hardware.
func VirtualDisk(name string) bool {
	return hasAnyPrefix(name, diskPrefixes)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

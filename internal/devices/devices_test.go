package devices

import "testing"

// TestVirtualNames pins the classifier. A name matched wrongly in either
// direction is a device on screen that should not be, or traffic that has
// silently vanished.
func TestVirtualNames(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"lo", true}, {"docker0", true}, {"br-4f1a2c9e8d31", true}, {"br0", true},
		{"veth3a91c07", true}, {"virbr0", true}, {"vnet3", true}, {"cali7d2b", true},
		{"eth0", false}, {"enp3s0", false}, {"wlp2s0", false}, {"eno1", false},
		{"bond0", false}, {"eth0.42", false},
		// A tunnel is software, but the traffic on it goes somewhere real.
		{"wg0", false}, {"tun0", false}, {"tap0", false},
	} {
		if got := VirtualNet(c.name); got != c.want {
			t.Errorf("VirtualNet(%q) = %v, want %v", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		name string
		want bool
	}{
		{"loop0", true}, {"loop12", true}, {"ram0", true},
		{"nvme0n1", false}, {"sda", false}, {"mmcblk0", false}, {"vda", false},
		// These are where the writes of a real system land, however they are
		// implemented underneath.
		{"dm-0", false}, {"md0", false}, {"zram0", false}, {"nbd0", false},
	} {
		if got := VirtualDisk(c.name); got != c.want {
			t.Errorf("VirtualDisk(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

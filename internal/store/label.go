package store

import (
	"context"
	"fmt"
)

// The kinds of thing a label can name. The kind is part of the key because
// a mount path and a device name live in different namespaces: nothing
// stops a block device and an interface from sharing a name.
const (
	LabelMount = "mount" // keyed by mount path
	LabelNet   = "net"   // keyed by interface name
	LabelDisk  = "disk"  // keyed by block device name
)

// SetLabel records the operator's name for one mount, interface, or block
// device on a host, replacing any earlier one. An empty label removes the
// row, so the page shows the reported name again. The label is stored as
// given: callers trim and bound it.
func (s *Store) SetLabel(ctx context.Context, hostID int64, kind, name, label string) error {
	switch kind {
	case LabelMount, LabelNet, LabelDisk:
	default:
		return fmt.Errorf("set label: unknown kind %q", kind)
	}
	if name == "" {
		return fmt.Errorf("set label: empty %s name", kind)
	}
	var err error
	if label == "" {
		_, err = s.write.ExecContext(ctx,
			`DELETE FROM labels WHERE host_id = ? AND kind = ? AND name = ?`,
			hostID, kind, name)
	} else {
		_, err = s.write.ExecContext(ctx, `
			INSERT INTO labels (host_id, kind, name, label) VALUES (?, ?, ?, ?)
			ON CONFLICT (host_id, kind, name) DO UPDATE SET label = excluded.label`,
			hostID, kind, name, label)
	}
	if err != nil {
		return fmt.Errorf("set label for host %d: %w", hostID, err)
	}
	return nil
}

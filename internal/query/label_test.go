package query

import (
	"testing"
	"time"

	"github.com/hammondus/gaze/internal/report"
	"github.com/hammondus/gaze/internal/store"
)

func TestLabels(t *testing.T) {
	var none Labels
	if got := none.Of(store.LabelMount, "/"); got != "/" {
		t.Errorf("nil Labels.Of = %q, want the name back", got)
	}
	if none.Has(store.LabelMount, "/") {
		t.Error("nil Labels claims a label")
	}

	s, q, id := seed(t)
	ctx := t.Context()
	if _, err := s.InsertReports(ctx, id, []report.Report{sampleReport(time.Now().Add(-time.Minute))}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLabel(ctx, id, store.LabelMount, "/", "Root"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLabel(ctx, id, store.LabelNet, "eth0", "LAN"); err != nil {
		t.Fatal(err)
	}
	// Same name, different kind: the two namespaces stay apart.
	if err := s.SetLabel(ctx, id, store.LabelDisk, "eth0", "odd"); err != nil {
		t.Fatal(err)
	}

	fleet, err := q.Fleet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 1 {
		t.Fatalf("fleet has %d hosts, want 1", len(fleet))
	}
	l := fleet[0].Labels
	for _, c := range []struct{ kind, name, want string }{
		{store.LabelMount, "/", "Root"},
		{store.LabelNet, "eth0", "LAN"},
		{store.LabelDisk, "eth0", "odd"},
		{store.LabelDisk, "sda", "sda"},
		{store.LabelNet, "wg0", "wg0"},
	} {
		if got := l.Of(c.kind, c.name); got != c.want {
			t.Errorf("Of(%s, %s) = %q, want %q", c.kind, c.name, got, c.want)
		}
	}
	if l.Has(store.LabelDisk, "sda") {
		t.Error("Has reports a label sda never got")
	}
}

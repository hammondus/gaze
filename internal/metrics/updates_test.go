package metrics

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestParseUpdates(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Updates
		wantErr bool
	}{
		{name: "both", in: "upgradable=12\nsecurity=3\n", want: Updates{Upgradable: 12, Security: 3}},
		{name: "none pending", in: "upgradable=0\nsecurity=0\n", want: Updates{}},
		{name: "unknown key ignored", in: "upgradable=1\nheld=4\nsecurity=0\n", want: Updates{Upgradable: 1}},
		{name: "missing security", in: "upgradable=12\n", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "not a number", in: "upgradable=lots\nsecurity=0\n", wantErr: true},
		{name: "negative", in: "upgradable=-1\nsecurity=0\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUpdates(strings.NewReader(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestCollectUpdates covers the three standings the host list draws
// differently: counted, not counted, and a file the hook garbled. Only the
// last is an error; a host without the hook is the normal case.
func TestCollectUpdates(t *testing.T) {
	proc := loadMapFS(t, "testdata/proc")
	sys := loadMapFS(t, "testdata/sys")
	counted := time.Date(2026, 10, 9, 6, 25, 0, 0, time.UTC)

	state := fstest.MapFS{UpdatesFile: {Data: []byte("upgradable=12\nsecurity=3\n"), ModTime: counted}}
	s := NewWithSource(Source{Proc: proc, Sys: sys, State: state}, Options{}).Collect(context.Background())
	want := Updates{Upgradable: 12, Security: 3, Counted: counted}
	if s.Host.Updates == nil || *s.Host.Updates != want {
		t.Errorf("Updates = %+v, want %+v", s.Host.Updates, want)
	}

	for name, src := range map[string]Source{
		"no state filesystem": {Proc: proc, Sys: sys},
		"no file":             {Proc: proc, Sys: sys, State: fstest.MapFS{}},
	} {
		s := NewWithSource(src, Options{}).Collect(context.Background())
		if s.Host.Updates != nil {
			t.Errorf("%s: Updates = %+v, want nil", name, s.Host.Updates)
		}
		for _, err := range s.Errs {
			if strings.Contains(err.Error(), UpdatesFile) {
				t.Errorf("%s: recorded error %v", name, err)
			}
		}
	}

	garbled := fstest.MapFS{UpdatesFile: {Data: []byte("12 updates\n")}}
	s = NewWithSource(Source{Proc: proc, Sys: sys, State: garbled}, Options{}).Collect(context.Background())
	if s.Host.Updates != nil {
		t.Errorf("garbled file: Updates = %+v, want nil", s.Host.Updates)
	}
	found := false
	for _, err := range s.Errs {
		found = found || strings.Contains(err.Error(), UpdatesFile)
	}
	if !found {
		t.Errorf("garbled file: no error recorded, Errs = %v", s.Errs)
	}
}

package store

import "testing"

func TestSetLabel(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	token, err := s.Enroll(ctx, "web-01")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		var label string
		err := s.Read().QueryRowContext(ctx,
			`SELECT label FROM labels WHERE host_id = ? AND kind = ? AND name = ?`,
			id, LabelMount, "/opt/FileMaker/Backups").Scan(&label)
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return label
	}

	if err := s.SetLabel(ctx, id, "container", "web", "x"); err == nil {
		t.Error("unknown kind accepted")
	}
	if err := s.SetLabel(ctx, id, LabelMount, "", "x"); err == nil {
		t.Error("empty name accepted")
	}
	if err := s.SetLabel(ctx, id+1, LabelMount, "/", "x"); err == nil {
		t.Error("label for a host that does not exist accepted")
	}

	if err := s.SetLabel(ctx, id, LabelMount, "/opt/FileMaker/Backups", "Backups"); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "Backups" {
		t.Fatalf("label = %q, want Backups", got)
	}
	if err := s.SetLabel(ctx, id, LabelMount, "/opt/FileMaker/Backups", "FM backups"); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "FM backups" {
		t.Fatalf("second set: label = %q, want FM backups", got)
	}
	if err := s.SetLabel(ctx, id, LabelMount, "/opt/FileMaker/Backups", ""); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "<sql: no rows in result set>" {
		t.Fatalf("blank label did not remove the row: %q", got)
	}
}

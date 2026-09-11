package runsreg

import "testing"

// TestRenameGroupsMovesRunsWithTheProject: a service started before its
// project was renamed follows the rename instead of keeping a group of the old
// name to itself.
func TestRenameGroupsMovesRunsWithTheProject(t *testing.T) {
	r := New()
	r.Mirror = false
	r.Register(Record{PID: 101, Group: "shop", Name: "api"})
	r.Register(Record{PID: 102, Group: "shop@feat", Name: "api"})
	r.Register(Record{PID: 103, Group: "other", Name: "job"})

	if n := r.RenameGroups(map[string]string{"shop": "market", "shop@feat": "market@feat"}); n != 2 {
		t.Fatalf("RenameGroups moved %d runs, want 2", n)
	}
	for pid, want := range map[int]string{101: "market", 102: "market@feat", 103: "other"} {
		rec, ok := r.Lookup(pid)
		if !ok || rec.Group != want {
			t.Errorf("pid %d group = %q (%v), want %q", pid, rec.Group, ok, want)
		}
	}
	if n := r.RenameGroups(nil); n != 0 {
		t.Errorf("an empty rename moved %d runs", n)
	}
}

package state

import "testing"

// TestDiffPublishesACheckoutChange: a checkout switching branches changes
// nothing but its group's branch, and that still has to reach subscribers —
// the group page shows it.
func TestDiffPublishesACheckoutChange(t *testing.T) {
	group := func(branch, worktree string) Group {
		return Group{Name: "shop@feat", Repo: "shop", Worktree: worktree, Branch: branch,
			Members: []int{}, Services: []Service{}}
	}
	prev := Snapshot{Groups: []Group{group("main", "feat")}}

	for _, next := range []Group{group("fix", "feat"), group("main", "")} {
		d := diff(prev, Snapshot{Groups: []Group{next}}, false)
		if len(d.Groups.Updated) != 1 {
			t.Errorf("changing %+v published %+v, want one updated group", next, d.Groups)
		}
	}
	if d := diff(prev, prev, false); len(d.Groups.Updated) != 0 {
		t.Errorf("an unchanged group published %+v", d.Groups)
	}
}

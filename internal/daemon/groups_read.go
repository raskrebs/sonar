package daemon

import (
	"context"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/scanner"
	"github.com/raskrebs/sonar/internal/state"
)

// The read half of the groups namespace. A group is derived state — the
// scanner resolves every port to a project and joins the `sonar.yaml`
// services onto it — so listing groups is reading the same snapshot the ports
// read uses, not a second computation that could disagree with it.
func init() {
	RegisterHandler("groups.list", handleGroupsList)
}

func handleGroupsList(_ context.Context, req *Request) (any, error) {
	groups, err := readGroups(req.Runtime)
	if err != nil {
		return nil, err
	}
	return rpc.GroupsListResult{Groups: groups}, nil
}

// readGroups is readPorts' twin: served from the cache while a subscriber
// keeps it fresh, and from a scan otherwise (contract §20).
func readGroups(rt *Runtime) ([]state.Group, error) {
	if rt.Subscribers() > 0 {
		if snap := rt.Scanner.Cached(); snap.Seq > 0 {
			return orEmptyGroups(snap.Groups), nil
		}
	}
	snap, err := rt.Scanner.Snapshot(scanner.Include{})
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInternal, "scan failed: "+err.Error(),
			"check `sonar daemon log` for the scanner error")
	}
	return orEmptyGroups(snap.Groups), nil
}

// orEmptyGroups keeps the collection an array on the wire, never null
// (contract §6).
func orEmptyGroups(groups []state.Group) []state.Group {
	if groups == nil {
		return []state.Group{}
	}
	return groups
}

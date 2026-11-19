package client

// The few places where the simulation touches the write-control API of the code under test, so that the rest of
// the simulation (sim_wc_test.go) only talks JSON and file systems.

import (
	"context"
	"net/url"
	"strconv"

	"kaze-sync/internal/proto"
)

// simRule is one write-control setting of a folder: Owner (the index of a client) matters for exclusive only.
type simRule struct {
	Path, Mode string
	Owner      int
}

// simConfig is the folder configuration a client gets for the rules.
func simConfig(id int64, rules []simRule) proto.FolderConfig {
	fc := proto.FolderConfig{ID: id, Adopt: true}
	for _, r := range rules {
		fc.Writectl = append(fc.Writectl, proto.WCRule{Path: r.Path, Mode: r.Mode})
	}
	return fc
}

// setRule makes the server apply the rule; devs are the device ids by client index.
func (e *cenv) setRule(fid int64, r simRule, devs []int64) int {
	form := url.Values{"path": {r.Path}, "mode": {r.Mode}}
	if r.Mode == proto.ModeExclusive {
		form.Set("device", strconv.FormatInt(devs[r.Owner], 10))
	}
	st, _ := e.post("/folders/"+strconv.FormatInt(fid, 10)+"/writectl", form)
	return st
}

// simHeld is a held change as the simulation sees it.
type simHeld struct{ Path, Reason, Kind string }

func (sf *simFolder) heldList() []simHeld {
	var out []simHeld
	rows, _ := sf.f.DB.held()
	for _, h := range rows {
		out = append(out, simHeld{h.Path, h.Reason, h.Kind})
	}
	return out
}

func (sf *simFolder) frozenIDs() map[string]bool {
	out := map[string]bool{}
	for id := range sf.f.frozen {
		out[id] = true
	}
	return out
}

func (sf *simFolder) resolve(ctx context.Context, path, action string, save bool) error {
	return sf.f.Resolve(ctx, proto.Action{Path: path, Action: action, Save: save})
}

// appliedCursor is the cursor the client has applied for a scope.
func (sf *simFolder) appliedCursor(scope string) int64 {
	if v, ok := sf.f.frozen[scope]; ok {
		return v
	}
	sf.hw = max(sf.hw, sf.f.DB.metaInt("cursor"))
	return sf.hw
}

var simActions = []string{ActUpload, ActOverwrite, ActDiscard}

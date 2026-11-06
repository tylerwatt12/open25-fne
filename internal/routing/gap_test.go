package routing

import (
	"context"
	"errors"
	"testing"
	"open25.local/open25-fne/internal/dispatch"
)

type gapTarget struct {
	testTarget
	gaps []dispatch.Grant
}

func (g *gapTarget) Gap(_ context.Context, grant dispatch.Grant) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gaps = append(g.gaps, grant)
	return nil
}

func TestGapPreservesAdmittedLegAndNeverCreatesMedia(t *testing.T) {
	target := &gapTarget{testTarget: testTarget{id: "site"}}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{"site", target, q.Talkgroups}}}
	}})
	g, err := r.Begin(t.Context(), "owner", request("gap", 1001))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Gap(t.Context(), "foreign", g); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatal("foreign owner marked media", err)
	}
	if err = r.Gap(t.Context(), "owner", g); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	if len(target.gaps) != 1 || target.gaps[0].ID != target.requests[0].ID+"@site" || len(target.frames) != 0 || target.renews != 0 || len(target.ends) != 0 {
		t.Error("gap changed lifecycle or produced media", target.gaps)
	}
	target.mu.Unlock()
}

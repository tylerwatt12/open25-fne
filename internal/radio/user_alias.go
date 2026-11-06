package radio

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
)

var ErrUserAliasRefresh = errors.New("on-air name update failed")

// IsValidUserAlias describes the Phase 1 on-air representation, not permission
// to save configuration. Existing names are never truncated or corrected.
func IsValidUserAlias(name string) bool {
	if len(name) == 0 || len(name) > 14 {
		return false
	}
	for i := range name {
		if name[i] < 0x20 || name[i] > 0x7e {
			return false
		}
	}
	return true
}

type userAlias struct {
	RID   int    `json:"rid"`
	Alias string `json:"alias"`
}

func radioNames(system System) map[int]string {
	names := make(map[int]string, len(system.Radios))
	for _, radio := range system.Radios {
		names[radio.RID] = radio.Alias
	}
	return names
}

func (c *Controller) reportUserAliasOmissions(p Profile, previous map[int]string) {
	for _, radio := range p.System.Radios {
		if previous != nil && previous[radio.RID] == radio.Alias {
			continue
		}
		if radio.Alias != "" && !IsValidUserAlias(radio.Alias) {
			c.emit(Event{Kind: "user_alias_omitted", RID: radio.RID, Message: "On-air name omitted: Phase 1 User Alias requires 1–14 printable ASCII characters. Voice operation continues."})
		}
	}
}

// applyUserAliases runs under opMu. It changes display metadata only; the
// native endpoint preserves its existing admission and protection state.
func (c *Controller) applyUserAliases(ctx context.Context, old, next Profile) error {
	names := radioNames(next.System)
	previous := radioNames(old.System)
	if maps.Equal(previous, names) {
		return nil
	}
	aliases := make([]userAlias, 0, len(names))
	for rid, name := range names {
		if IsValidUserAlias(name) {
			aliases = append(aliases, userAlias{rid, name})
		}
	}
	slices.SortFunc(aliases, func(a, b userAlias) int {
		if a.RID < b.RID {
			return -1
		}
		if a.RID > b.RID {
			return 1
		}
		return 0
	})
	c.mu.Lock()
	processes := make([]*child, 0, len(c.children))
	for _, process := range c.children {
		if process.alive() {
			processes = append(processes, process)
		}
	}
	c.mu.Unlock()
	for _, process := range processes {
		if _, err := process.client.Put(ctx, "/p25/aliases", struct {
			Aliases []userAlias `json:"aliases"`
		}{aliases}); err != nil {
			return fmt.Errorf("%w on %s: %w", ErrUserAliasRefresh, process.id, err)
		}
	}
	c.reportUserAliasOmissions(next, previous)
	return nil
}

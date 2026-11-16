package radio

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ApplyNeighborSites uses the receiving Site's running programming. A Save
// does not change its selected neighbors until the next explicit Site start.
func (c *Controller) ApplyNeighborSites(ctx context.Context, sites []NeighborSite) error {
	c.mu.Lock()
	child := c.children[c.status.ActiveControlChannelID]
	if c.status.State != "running" || c.status.Mode != "hardware" ||
		child == nil || !child.readyNow() || !child.acceptsObservations() || c.runContext == nil {
		c.mu.Unlock()
		return nil
	}
	p := c.configuredProfile
	run := c.runContext
	c.mu.Unlock()

	local, err := siteIdentity(p)
	if err != nil {
		return err
	}
	type announcement struct {
		AdjacentSite
		TTLMillis int64   `json:"ttl_ms"`
		RXChannel *uint16 `json:"rx_channel,omitempty"`
	}
	rows := make([]announcement, 0, len(sites))
	seen := map[int]bool{}
	var omitted []error
	now := time.Now()
	for _, site := range sites {
		control := site.Control
		ttl := site.ValidUntil.Sub(now).Milliseconds()
		if control == nil || ttl <= 0 || site.ID == p.Site.ID ||
			control.SiteID == local.SiteID || control.WACN != p.System.WACN || control.SystemID != p.System.SystemID || control.RFSSID != p.Zone.RFSSID {
			continue
		}
		if p.Site.NeighborMode == "selected" && !slices.Contains(p.Site.NeighborSiteIDs, site.ID) {
			continue
		}
		if seen[control.SiteID] {
			continue
		}
		receive, encodable := neighborChannelReferences(p.Zone.ChannelIdentifiers, *control)
		if !encodable {
			omitted = append(omitted, fmt.Errorf("%s control channel cannot be represented by this Site's running Band Plan", site.Name))
			continue
		}
		seen[control.SiteID] = true
		rows = append(rows, announcement{*control, ttl, receive})
	}

	request, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(run, cancel)
	defer stop()
	if run.Err() != nil {
		return nil
	}
	_, err = child.client.Put(request, "/p25/adjacent-sites", map[string]any{"sites": rows})
	if run.Err() != nil {
		return nil
	}
	return errors.Join(err, errors.Join(omitted...))
}

// For an explicit FDMA reference, AABC-E F.5 uses base + channel × spacing,
// with no implicit duplex offset. Prefer the transmitting identifier, then
// the lowest programmed identifier that exactly represents the receive Hz.
func neighborChannelReferences(plans []Identifier, control AdjacentSite) (*uint16, bool) {
	var tx Identifier
	for _, plan := range plans {
		if plan.Identifier == control.Identifier {
			tx = plan
			break
		}
	}
	if tx.SpacingHz <= 0 || control.ChannelNo < 1 || control.ChannelNo > 4095 ||
		control.TXHz != tx.BaseHz+int64(control.ChannelNo)*tx.SpacingHz {
		return nil, false
	}
	if control.TXHz >= 762000000 && control.RXHz >= 762000000 {
		return nil, control.RXHz == control.TXHz+tx.OffsetHz
	}

	var best uint16
	found := false
	for _, plan := range plans {
		delta := control.RXHz - plan.BaseHz
		if delta < 0 || plan.SpacingHz <= 0 || delta%plan.SpacingHz != 0 || delta/plan.SpacingHz > 4095 {
			continue
		}
		ref := uint16(plan.Identifier<<12) | uint16(delta/plan.SpacingHz)
		if plan.Identifier == tx.Identifier {
			return &ref, true
		}
		if !found || ref < best {
			best, found = ref, true
		}
	}
	return &best, found
}

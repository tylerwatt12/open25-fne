package siteaudio

import (
	"context"
	"open25.local/open25-fne/internal/dispatch"
)

// Gap never writes media or renews a native lease. The next complete encoded
// LDU boundary resets the native batch; partial encrypted LDUs are not padded.
func (r *Router) Gap(ctx context.Context, grant dispatch.Grant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := r.lookup(grant)
	if err != nil {
		return err
	}
	if !c.media.TryRLock() {
		return dispatch.ErrUnavailable
	}
	defer c.media.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != "active" {
		return dispatch.ErrUnavailable
	}
	c.waitBoundary = true
	return nil
}

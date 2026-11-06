package siteaudio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
	"open25.local/open25-fne/internal/dispatch"
)

func voiceMediaID(allocation string, epoch uint64) string {
	sum := sha256.Sum256([]byte("p25-media:" + allocation + ":" + strconv.FormatUint(epoch, 10)))
	return hex.EncodeToString(sum[:16])
}

// EndSpurt terminates only this actual native voice stream. Its allocation and
// floor remain held; ordered native End/next-boundary packets need no new grant.
func (r *Router) EndSpurt(ctx context.Context, grant dispatch.Grant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := r.lookup(grant)
	if err != nil {
		return err
	}
	if !c.media.TryLock() {
		return dispatch.ErrUnavailable
	}
	defer c.media.Unlock()
	c.mu.Lock()
	if c.state != "active" {
		c.mu.Unlock()
		return dispatch.ErrUnavailable
	}
	frame := c.lastFrame
	leases := append([]*nativeLease{}, c.leases...)
	c.waitBoundary = true
	c.mu.Unlock()
	if frame.CallID == "" {
		return nil
	}
	frame.End, frame.PCM, frame.Encoded, frame.P25 = true, nil, nil, nil
	frame.CapturedAt = time.Now()
	for _, lease := range leases {
		if lease.disabled.Load() {
			continue
		}
		lease.media.RLock()
		if !lease.disabled.Load() {
			err = r.options.Codec.Write(ctx, frame.WithGroup(lease.Group).WithEmergency(c.request.Emergency))
		}
		lease.media.RUnlock()
		if err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.lastFrame = dispatch.Frame{}
	c.mu.Unlock()
	return nil
}

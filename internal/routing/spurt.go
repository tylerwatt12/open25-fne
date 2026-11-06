package routing

import (
	"context"
	"open25.local/open25-fne/internal/dispatch"
)

// EndSpurt sends each segment's own End without releasing its allocation.
func (r *Router) EndSpurt(ctx context.Context, owner string, grant dispatch.Grant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := r.find(owner, grant)
	if err != nil {
		return err
	}
	c.media.Lock()
	defer c.media.Unlock()
	if c.ended {
		return dispatch.ErrUnavailable
	}
	r.retainAvailableLegs(ctx, c, func(leg leg) error {
		if endpoint, ok := leg.target.Router.(dispatch.MediaSpurts); ok {
			return endpoint.EndSpurt(ctx, leg.grant)
		}
		return nil
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	c.keyClosed = true
	if r.options.EndSpurt != nil {
		r.options.EndSpurt(c.request)
	}
	return nil
}

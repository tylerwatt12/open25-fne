package routing

import (
	"context"
	"open25.local/open25-fne/internal/dispatch"
)

func (r *Router) Gap(ctx context.Context, owner string, grant dispatch.Grant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := r.find(owner, grant)
	if err != nil {
		return err
	}
	if err := r.validateCall(c, nil); err != nil {
		_ = r.End(context.Background(), owner, grant)
		return err
	}
	c.media.Lock()
	defer c.media.Unlock()
	if c.ended {
		return dispatch.ErrUnavailable
	}
	r.retainAvailableLegs(ctx, c, func(leg leg) error {
		if endpoint, ok := leg.target.Router.(dispatch.MediaGaps); ok {
			return endpoint.Gap(ctx, leg.grant)
		}
		return nil
	})
	return ctx.Err()
}

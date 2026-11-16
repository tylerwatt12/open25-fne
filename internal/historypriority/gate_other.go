//go:build !linux

package historypriority

import "context"

// Deployment uses Linux's cross-controller gate. Other development hosts leave
// scheduling unchanged; isolated qualification explicitly uses AcquirePath.
func Acquire(ctx context.Context) (*Turn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &Turn{}, nil
}

func AcquireForeground() (*Turn, error) { return &Turn{}, nil }

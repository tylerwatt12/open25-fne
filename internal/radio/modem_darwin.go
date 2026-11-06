//go:build darwin

package radio

import (
	"context"
	"errors"
	"io"
)

func acquireDevice(context.Context, string) (io.Closer, error) {
	return nil, errors.New("hardware radio operation requires Linux; use null mode for local software testing")
}
func verifyFirmware(context.Context, io.Closer, string, bool) error {
	return errors.New("hardware modem verification requires Linux")
}
func deviceSame(io.Closer, string) bool { return false }

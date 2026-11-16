package radio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestRestrictedWACNStartDoesNotLaunchNativeOrTouchModems(t *testing.T) {
	for _, mode := range []string{"null", "hardware"} {
		t.Run(mode, func(t *testing.T) {
			c := testController(t, nil)
			c.opts.AllowRF = true
			c.modem = modemOperations{
				resolve: func(string) (string, error) {
					t.Error("restricted startup attempted modem discovery")
					return "", errors.New("unexpected modem discovery")
				},
				acquire: func(context.Context, string) (io.Closer, error) {
					t.Error("restricted startup attempted modem acquisition")
					return nil, errors.New("unexpected modem acquisition")
				},
			}
			for wacn := 0xBEE00; wacn <= 0xBEEFF; wacn++ {
				p := fixtureProfile()
				p.System.WACN = wacn
				before, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				err = c.Start(context.Background(), p, mode)
				want := fmt.Sprintf("WACN %05X is blocked for operation: BEE00-BEEFF is restricted to reduce unintended subscriber roaming risk", wacn)
				if err == nil || err.Error() != want {
					t.Fatalf("startup did not return the policy error: got %v, want %q", err, want)
				}
				after, marshalErr := json.Marshal(p)
				if marshalErr != nil || !bytes.Equal(after, before) {
					t.Fatal("refused startup changed the caller's programming")
				}
				c.mu.Lock()
				started := c.rootDir != "" || c.fne != nil || len(c.children) != 0 || c.siteWanted
				c.mu.Unlock()
				if started {
					t.Fatal("restricted identity acquired a native run or retry")
				}
			}
		})
	}
}

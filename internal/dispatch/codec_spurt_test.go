package dispatch

import (
	"errors"
	"testing"
	"time"
)

func TestExplicitNativeEndKeepsTailFenceAndPermitsFreshMediaOwner(t *testing.T) {
	c := shutdownCodec(t)
	c.options.Opaque = true
	r := shutdownResource(t)
	c.resources[r.tg] = r
	voice := packetFixture()
	voice.CapturedAt = time.Now()
	if err := c.Write(t.Context(), voice); err != nil {
		t.Fatal(err)
	}
	ending := voice
	ending.End, ending.Encoded, ending.P25 = true, nil, nil
	if err := c.Write(t.Context(), ending); err != nil {
		t.Fatal(err)
	}
	if !r.ended || r.lastCall != voice.CallID || time.Since(r.lastWrite) >= 220*time.Millisecond {
		t.Fatal("explicit End lost the pending native tail owner")
	}
	if err := c.Write(t.Context(), voice); !errors.Is(err, ErrUnavailable) {
		t.Fatal("retired media identity was reused", err)
	}
	if err := c.Write(t.Context(), ending); err != nil {
		t.Fatal("exact End repeat was not idempotent", err)
	}
	voice.CallID = "fresh-next-spurt"
	if err := c.Write(t.Context(), voice); err != nil {
		t.Fatal("fresh spurt unnecessarily waited for allocation release", err)
	}
	if err := c.Write(t.Context(), ending); !errors.Is(err, ErrPermission) {
		t.Fatal("old End affected the new source", err)
	}
}

package siteaudio

import (
	"context"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
)

func TestGrantObservationMeasuresNativeAcquisitionIncludingFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "unconfirmed"}[failed], func(t *testing.T) {
			release := make(chan struct{})
			native := &fakeNative{holdGrant: release, ambiguous: failed, statusFailure: failed}
			observations := make(chan Event, 8)
			router := New(Options{Control: func() (radio.Native, error) { return native, nil }, Codec: &fakeCodec{}, HasRFInterest: func(int) bool { return true }, AllowTX: allowed,
				Event: func(e Event) {
					select {
					case observations <- e:
					default:
					}
				}})
			before := time.Now()
			result := make(chan error, 1)
			var grant dispatch.Grant
			go func() {
				var err error
				grant, err = router.Begin(context.Background(), request("grant-timing", 1001))
				result <- err
			}()
			deadline := time.Now().Add(time.Second)
			for len(native.paths()) == 0 {
				if time.Now().After(deadline) {
					close(release)
					t.Fatal("native grant was not entered")
				}
				time.Sleep(time.Millisecond)
			}
			held := time.Now()
			// The gate creates a real measured native wait; it is not a mocked clock.
			time.Sleep(25 * time.Millisecond)
			close(release)
			select {
			case err := <-result:
				if (err != nil) != failed {
					t.Fatalf("unexpected grant outcome: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("grant did not complete after native response")
			}
			var event Event
			for len(observations) > 0 {
				e := <-observations
				if e.Kind == "rf.grant" {
					event = e
				}
			}
			if event.RequestStartedAt.Before(before) || event.RequestStartedAt.After(held) || event.DurationMS < 25 || event.At.Before(event.RequestStartedAt) {
				t.Fatalf("native acquisition timing is missing or excludes the wait: %+v", event)
			}
			wallDuration := event.At.Sub(event.RequestStartedAt)
			measuredDuration := time.Duration(event.DurationMS * float64(time.Millisecond))
			if difference := measuredDuration - wallDuration; difference < -5*time.Millisecond || difference > 5*time.Millisecond {
				t.Fatalf("duration and observation timestamps disagree: %v", difference)
			}
			if !failed {
				if err := router.End(context.Background(), grant); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestFullObservationQueueDoesNotDelayGrantOrRelease(t *testing.T) {
	observations := make(chan Event, 1)
	observations <- Event{}
	native := &fakeNative{}
	router := New(Options{Control: func() (radio.Native, error) { return native, nil }, Codec: &fakeCodec{}, HasRFInterest: func(int) bool { return true }, AllowTX: allowed,
		Event: func(e Event) {
			select {
			case observations <- e:
			default:
			}
		}})
	completed := make(chan error, 1)
	go func() {
		grant, err := router.Begin(context.Background(), request("full-observation-queue", 1001))
		if err == nil {
			err = router.End(context.Background(), grant)
		}
		completed <- err
	}()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("full observation queue delayed the native grant/release")
	}
	if len(observations) != 1 || !native.released {
		t.Fatal("observation overflow changed native lease handling")
	}
}

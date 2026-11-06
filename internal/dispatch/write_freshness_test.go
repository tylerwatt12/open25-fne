package dispatch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type freshnessRouter struct {
	testRouter
	writeDelay time.Duration
	blockWrite bool
	started    chan struct{}
	once       sync.Once
	budgetMu   sync.Mutex
	deadlines  []time.Time
	captured   []time.Time
	entered    []time.Time
}

func (r *freshnessRouter) Write(ctx context.Context, grant Grant, frame Frame) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("audio write has no freshness deadline")
	}
	r.budgetMu.Lock()
	r.deadlines = append(r.deadlines, deadline)
	r.captured = append(r.captured, frame.CapturedAt)
	r.entered = append(r.entered, time.Now())
	r.budgetMu.Unlock()
	r.once.Do(func() {
		if r.started != nil {
			close(r.started)
		}
	})
	if r.blockWrite {
		<-ctx.Done()
		return ctx.Err()
	}
	if frame.Sequence == 2 && r.writeDelay > 0 {
		timer := time.NewTimer(r.writeDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return r.testRouter.Write(ctx, grant, frame)
}

func TestFreshAudioWriteMayExceedOneCadenceWithoutLosingCall(t *testing.T) {
	router := &freshnessRouter{writeDelay: 40 * time.Millisecond}
	h := New(Options{Router: router, MaxAudioAge: 250 * time.Millisecond})
	defer h.Close()
	pcm := make([]byte, FrameBytes*6)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	frames, err := h.PlayPCM(context.Background(), principal(10001, 1001), "fresh-audio", []int{1001}, pcm, false, 0, 0)
	if err != nil || frames != 6 {
		t.Fatalf("one 40 ms processing delay aborted still-fresh audio: frames=%d err=%v", frames, err)
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.writes) != 6 || len(router.ends) != 1 {
		t.Fatalf("write delay lost frames or final release: writes=%d ends=%d", len(router.writes), len(router.ends))
	}
	for i, frame := range router.writes {
		if frame.Sequence != uint64(i+1) || !bytes.Equal(frame.PCM, pcm[i*FrameBytes:(i+1)*FrameBytes]) {
			t.Fatal("freshness budget reordered or changed audio")
		}
	}
	router.budgetMu.Lock()
	defer router.budgetMu.Unlock()
	for i, deadline := range router.deadlines {
		if !deadline.Equal(router.captured[i].Add(h.options.MaxAudioAge)) {
			t.Fatal("processing deadline was not tied to the original capture time")
		}
	}
}

func TestExpiredQueuedAudioNeverReachesRouter(t *testing.T) {
	router := &freshnessRouter{}
	h := New(Options{Router: router, MaxAudioAge: 250 * time.Millisecond})
	defer h.Close()
	client := readyClient(t, h, principal(10001, 1001))
	tx, err := h.start(client, []int{1001}, "expired-queued", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitGrant(t, h, tx)
	if err = h.acceptPCM(client, make([]byte, FrameBytes), time.Now().Add(2*time.Second)); err == nil {
		t.Fatal("future-dated microphone input was accepted")
	}
	// Model a frame that became stale while queued, independently of the
	// existing admission check that rejects already-stale microphone input.
	tx.Queue <- queuedFrame{PCM: bytes.Repeat([]byte{0x11}, FrameBytes), At: time.Now().Add(-time.Second), Sequence: 1}
	tx.Queue <- queuedFrame{PCM: bytes.Repeat([]byte{0x33}, FrameBytes), At: time.Now().Add(2 * time.Second), Sequence: 2}
	fresh := bytes.Repeat([]byte{0x22}, FrameBytes)
	if err = h.acceptPCM(client, fresh, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, writes, _ := router.counts(); return writes == 1 })
	h.stop(tx, "")
	waitEnd(t, tx)
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.writes) != 1 || !bytes.Equal(router.writes[0].PCM, fresh) {
		t.Fatal("expired audio was delivered or displaced fresh audio")
	}
}

func TestAudioWriteCancellationDoesNotWaitForFreshnessDeadline(t *testing.T) {
	router := &freshnessRouter{blockWrite: true, started: make(chan struct{})}
	h := New(Options{Router: router, MaxAudioAge: 250 * time.Millisecond})
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.PlayPCM(ctx, principal(10001, 1001), "cancel-fresh", []int{1001}, make([]byte, FrameBytes*10), false, 0, 0)
		done <- err
	}()
	select {
	case <-router.started:
	case <-time.After(time.Second):
		t.Fatal("write never started")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled playback was reported complete")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("cancellation waited for the 250 ms freshness budget")
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("cancellation was not bounded")
	}
	waitFor(t, func() bool { _, _, ended := router.counts(); return ended == 1 })
	if _, writes, _ := router.counts(); writes != 0 {
		t.Fatal("cancelled write delivered audio")
	}
}

func TestBlockedAudioWriteStillExpiresWithinOriginalFreshness(t *testing.T) {
	router := &freshnessRouter{blockWrite: true}
	h := New(Options{Router: router, MaxAudioAge: 250 * time.Millisecond})
	defer h.Close()
	start := time.Now()
	_, err := h.PlayPCM(context.Background(), principal(10001, 1001), "freshness-expiry", []int{1001}, make([]byte, FrameBytes), false, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || time.Since(start) > 600*time.Millisecond {
		t.Fatalf("blocked processing escaped its existing freshness bound: %v elapsed=%v", err, time.Since(start))
	}
	if _, writes, _ := router.counts(); writes != 0 {
		t.Fatal("expired processing delivered audio")
	}
}

func TestFutureAudioWriteCannotExtendProcessingFreshness(t *testing.T) {
	router := &freshnessRouter{blockWrite: true, started: make(chan struct{})}
	h := New(Options{Router: router, MaxAudioAge: 250 * time.Millisecond})
	defer h.Close()
	client := readyClient(t, h, principal(10001, 1001))
	tx, err := h.start(client, []int{1001}, "future-budget", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitGrant(t, h, tx)
	// The existing clock-tolerance check permits this modest lead, but it
	// must not extend the processing budget by another half second.
	if err = h.acceptPCM(client, make([]byte, FrameBytes), time.Now().Add(500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-router.started:
	case <-time.After(time.Second):
		t.Fatal("future-dated valid frame did not reach the write boundary")
	}
	router.budgetMu.Lock()
	deadline, captured, entered := router.deadlines[0], router.captured[0], router.entered[0]
	router.budgetMu.Unlock()
	h.stop(tx, "")
	waitEnd(t, tx)
	if deadline.Sub(entered) > h.options.MaxAudioAge || !deadline.Before(captured) {
		t.Fatalf("clock tolerance extended processing freshness: remaining=%v captured=%v deadline=%v", deadline.Sub(entered), captured, deadline)
	}
}

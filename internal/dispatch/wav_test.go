package dispatch

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWAVCarriesOperatorSelectedKey(t *testing.T) {
	router := &testRouter{}
	h := New(Options{Router: router})
	defer h.Close()
	p := principal(10001, 1001)
	p.CanSecure = true
	r := httptest.NewRequest("POST", "http://dispatch.test/api/dispatch/wav?talkgroup_id=1001&secure=true&algorithm_id=129&key_id=0", bytes.NewReader(wavFixture(make([]byte, FrameBytes))))
	r.Header.Set("Origin", "http://dispatch.test")
	r.Header.Set("Content-Type", "audio/wav")
	w := httptest.NewRecorder()
	h.ServeWAV(w, r, p, "selected-key")
	if w.Code != 200 {
		t.Fatalf("selected-key WAV: %d %s", w.Code, w.Body.String())
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.begin) != 1 || !router.begin[0].Secure || router.begin[0].AlgorithmID != 129 || router.begin[0].KeyID != 0 {
		t.Fatal("WAV lost operator-selected key")
	}
}

func TestWAVReportsRoutingFailureInsteadOfTheCancelledProducerSymptom(t *testing.T) {
	router := &wavFailingRouter{cause: errors.New("receiving Site rejected the call")}
	h := New(Options{Router: router})
	defer h.Close()
	_, err := h.PlayPCM(context.Background(), principal(10001, 1001), "file", []int{1001}, make([]byte, 20*FrameBytes), false, 0, 0)
	if err == nil || !strings.Contains(err.Error(), router.cause.Error()) || strings.Contains(err.Error(), "no granted transmission") || strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("lost the actual transmission failure: %v", err)
	}
}

type wavFailingRouter struct {
	testRouter
	cause error
}

func (r *wavFailingRouter) Write(context.Context, Grant, Frame) error { return r.cause }

func TestWAVWaitsForTheRoutingWorkersTerminalReasonAfterCancellation(t *testing.T) {
	h := New(Options{Router: &testRouter{}})
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx := &transmission{Context: ctx, Done: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- h.awaitTransmissionError(context.Background(), tx) }()
	select {
	case err := <-result:
		t.Fatalf("returned before worker recorded its reason: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	h.mu.Lock()
	tx.Error = "voice channel renewal refused"
	h.mu.Unlock()
	close(tx.Done)
	if err := <-result; err == nil || err.Error() != tx.Error {
		t.Fatal("cancellation masked terminal reason", err)
	}
}

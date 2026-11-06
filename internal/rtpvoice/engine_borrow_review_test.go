package rtpvoice

import (
	"context"
	"errors"
	"testing"
	"time"

	"open25.local/open25-fne/internal/p25rtp"
)

func TestIndependentCanceledEngineReleasesReaderButKeepsCallSession(t *testing.T) {
	home, serving := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	engine, err := NewEngine(home, spurt)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- engine.Run(t.Context()) }()
	engineReady(t, engine)
	if err = engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled actor result %v", err)
	}
	select {
	case <-home.done:
		t.Fatal("canceled borrowed Engine closed call Session")
	default:
	}
	// Reading the same socket after Done proves the old reader and its deadline
	// callback have both relinquished ownership. This control carries no voice.
	packet := p25rtp.Packet{Role: p25rtp.GroupServing, Timestamp: 1234, Control: p25rtp.PacketTypeControl{Type: p25rtp.Heartbeat, ServiceOptions: 4, Interval: 10}}
	if err = serving.Send(t.Context(), packet); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	got, err := home.Receive(ctx)
	if err != nil || got.Control.Type != p25rtp.Heartbeat || got.Timestamp != 1234 {
		t.Fatalf("borrowed socket not reusable: %#v %v", got, err)
	}
	spurt.TSN += 2
	next, err := NewEngine(home, spurt)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-home.done:
		t.Fatal("unstarted replacement Engine closed call Session")
	default:
	}
	if home.Counters().ReadErrors != 0 {
		t.Fatal("reader cancellation was mistaken for transport failure")
	}
}

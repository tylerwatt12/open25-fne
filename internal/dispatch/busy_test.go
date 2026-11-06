package dispatch

import (
	"testing"
	"time"
)

func TestBusyProtectsOnlyEligibleLiveReceiveAudioAndExpiresAfterQuiet(t *testing.T) {
	hub := New(Options{Router: &testRouter{}})
	defer hub.Close()
	client := readyClient(t, hub, principal(10002, 1001))
	if err := hub.subscribe(client, []int{1001}); err != nil {
		t.Fatal(err)
	}
	client.peerMu.Lock()
	client.tracks[1001] = &rxTrack{queue: make(chan Frame, 8)}
	client.peerMu.Unlock()
	frame := Frame{CallID: "radio-call", TalkgroupID: 1001, SourceID: 10001, PCM: make([]byte, FrameBytes), CapturedAt: time.Now()}
	for _, change := range []func(*Frame){func(f *Frame) { f.SourceID = 10002 }, func(f *Frame) { f.TalkgroupID = 1002 }, func(f *Frame) { f.Encrypted = true }, func(f *Frame) { f.CapturedAt = time.Now().Add(-time.Second) }} {
		bad := frame
		change(&bad)
		hub.Publish(bad)
		if hub.Busy() {
			t.Fatal("ineligible incoming audio blocked controller recovery")
		}
	}
	if err := hub.subscribe(client, []int{}); err != nil {
		t.Fatal(err)
	}
	hub.Publish(frame)
	if hub.Busy() {
		t.Fatal("an unsubscribed resource blocked recovery")
	}
	if err := hub.subscribe(client, []int{1001}); err != nil {
		t.Fatal(err)
	}
	frame.CapturedAt = time.Now()
	hub.Publish(frame)
	if !hub.Busy() {
		t.Fatal("live receive audio did not protect the ongoing call")
	}
	time.Sleep(300 * time.Millisecond)
	own := frame
	own.SourceID = 10002
	own.CapturedAt = time.Now()
	hub.Publish(own)
	end := frame
	end.End = true
	end.PCM = nil
	end.CapturedAt = time.Now()
	hub.Publish(end)
	time.Sleep(250 * time.Millisecond)
	if hub.Busy() {
		t.Fatal("receive end or own-source audio extended the quiet guard")
	}
}

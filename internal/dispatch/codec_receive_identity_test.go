package dispatch

import (
	"bytes"
	"testing"
	"time"
)

// Real loopback UDP and the production receiver, without a native process,
// FNE, radio or modem. Read the bounded output queue directly.
func opaqueReceiveFixture(t *testing.T) (*Codec, func(Frame), func() Frame) {
	t.Helper()
	codec := shutdownCodec(t)
	resource := shutdownResource(t)
	codec.frames = make(chan codecReceiveUpdate, 16)
	codec.options.Opaque = true
	codec.wg.Add(1)
	go codec.receive(resource)
	t.Cleanup(func() { codec.cancel(); resource.read.Close(); codec.wg.Wait() })
	send := func(frame Frame) {
		t.Helper()
		packet, err := EncodeOpaqueCodecPacket(frame)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = resource.write.Write(packet); err != nil {
			t.Fatal(err)
		}
	}
	next := func() Frame {
		t.Helper()
		select {
		case frame := <-codec.frames:
			return frame.frame
		case <-time.After(time.Second):
			t.Fatal("native receive did not publish the expected frame")
			return Frame{}
		}
	}
	return codec, send, next
}

func TestCodecOpaqueIdleResumeOwnsANewApplicationStream(t *testing.T) {
	_, send, next := opaqueReceiveFixture(t)
	frame := packetFixture()
	frame.CallID = "same-native-call-through-silence"
	send(frame)
	first := next()
	frame.Sequence++
	send(frame)
	continuous := next()
	if continuous.CallID != first.CallID {
		t.Fatal("continuous native media changed its application stream")
	}
	idle := next()
	if !idle.End || idle.CallID != first.CallID {
		t.Fatal("idle end did not retire its matching application stream")
	}
	frame.Sequence++
	send(frame)
	resumed := next()
	if resumed.CallID == first.CallID {
		t.Error("resumed native token reused the application stream whose HTTP cleanup may still be in flight")
	}
	if resumed.Sequence != frame.Sequence || !resumed.Encrypted || resumed.AlgorithmID != frame.AlgorithmID || resumed.KeyID != frame.KeyID || !bytes.Equal(resumed.Encoded, frame.Encoded) || len(resumed.MessageIndicator) != 0 {
		t.Fatal("stream identity handling changed native media, sequence or crypto metadata")
	}
	if _, err := EncodeOpaqueCodecPacket(resumed); err != nil {
		t.Fatal("fresh application stream is not wire-compatible:", err)
	}
	frame.Sequence++
	frame.End = true
	frame.Encoded = nil
	frame.P25 = nil
	frame.MessageIndicator = nil
	send(frame)
	end := next()
	if !end.End || end.CallID != resumed.CallID {
		t.Fatal("native End was not mapped to the resumed application stream")
	}
}

func TestCodecOpaqueNativeChangeAndLateEndMatchOnlyTheirOwnStream(t *testing.T) {
	codec, send, next := opaqueReceiveFixture(t)
	frame := packetFixture()
	frame.CallID = "native-first"
	send(frame)
	first := next()
	oldEnd := frame
	oldEnd.Sequence++
	oldEnd.End = true
	oldEnd.Encoded = nil
	oldEnd.P25 = nil
	oldEnd.MessageIndicator = nil
	frame.CallID = "native-second"
	send(frame)
	ended, nextCall := next(), next()
	if !ended.End || ended.CallID != first.CallID || nextCall.End || nextCall.CallID == first.CallID {
		t.Fatal("native token change did not end the old application stream exactly once")
	}
	send(oldEnd)
	select {
	case f := <-codec.frames:
		t.Fatalf("late End for the old native token affected the newer stream: %+v", f)
	case <-time.After(70 * time.Millisecond):
	}
	frame.Sequence++
	send(frame)
	continuous := next()
	if continuous.CallID != nextCall.CallID {
		t.Fatal("ignored old End changed the newer application identity")
	}
	frame.Sequence++
	frame.End = true
	frame.Encoded = nil
	frame.P25 = nil
	frame.MessageIndicator = nil
	send(frame)
	end := next()
	if !end.End || end.CallID != nextCall.CallID {
		t.Fatal("matching explicit native End did not close the active application stream")
	}
	send(frame)
	select {
	case <-codec.frames:
		t.Fatal("duplicate native End emitted an extra application end")
	case <-time.After(70 * time.Millisecond):
	}
}

package siteaudio

import (
	"context"
	"testing"

	"open25.local/open25-fne/internal/radio"
)

func TestNativeObservationsUseConversationIdentityWithoutChangingGrantIdentity(t *testing.T) {
	native := &fakeNative{}
	codec := &fakeCodec{}
	observations := []Event{}
	r := New(Options{Control: func() (radio.Native, error) { return native, nil }, Codec: codec, HasRFInterest: func(int) bool { return true }, AllowTX: allowed, Event: func(e Event) { observations = append(observations, e) }})
	defer r.Close(context.Background())
	q := request("one-use-allocation", 1001)
	q.LogicalCallID = "one-conversation"
	grant, err := r.Begin(context.Background(), q)
	if err != nil || grant.ID != q.ID {
		t.Fatal("logical identity changed the native route grant", grant, err)
	}
	if err = r.Write(context.Background(), grant, frame(q.ID, 1)); err != nil {
		t.Fatal(err)
	}
	if len(codec.frames) != 1 || codec.frames[0].CallID != voiceMediaID(q.ID, 0) {
		t.Fatal("media was no longer fenced by its allocation and source epoch")
	}
	changed := q
	changed.LogicalCallID = "another-conversation"
	if _, err = r.Begin(context.Background(), changed); err == nil {
		t.Fatal("an existing allocation was relabeled as another conversation")
	}
	if err = r.End(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	if len(observations) < 2 {
		t.Fatal("native grant/release observations were not produced")
	}
	for _, event := range observations {
		if event.CallID != q.LogicalCallID || event.AllocationID != q.ID {
			t.Fatalf("native observation lost conversation/allocation distinction: %#v", event)
		}
	}
}

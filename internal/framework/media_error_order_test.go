package framework

import (
	"context"
	"errors"
	"strings"
	"testing"

	"open25.local/open25-fne/internal/dispatch"
)

func TestBrowserReleaseCannotLeaveItsQueuedFailureForTheNextCall(t *testing.T) {
	for range 1000 {
		browser := &mediaBrowser{state: make(chan dispatch.State, 1), errors: make(chan error, 1)}
		failure := &browserMediaFailure{Action: "ptt_end", RequestID: "first-call", Message: "independent renewal deadline"}
		browser.errors <- failure
		browser.state <- dispatch.State{TX: dispatch.TXState{State: "idle"}, EndedRequestID: "first-call"}
		_, err := browser.waitStateResult(context.Background(), func(state dispatch.State) bool { return state.TX.State == "idle" })
		var actual *browserMediaFailure
		if !errors.As(err, &actual) || actual.RequestID != "first-call" || actual.Action != "ptt_end" || !strings.Contains(err.Error(), "first-call") {
			t.Fatal("release lost or misattributed its failure:", err)
		}
		if len(browser.errors) != 0 {
			t.Fatal("old-call error remained queued")
		}
	}
}

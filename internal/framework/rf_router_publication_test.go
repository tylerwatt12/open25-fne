package framework

import (
	"bytes"
	"context"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
)

func TestBlockedRadioCommandDoesNotBlockSIPRFSelectionOrMedia(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	site := c.otherSite.service
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	native := &commandLifecycleRadio{operationRadio: c.radios["site-2"]}
	native.complete = func(ctx context.Context, command radio.Command) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Background operation loops are already joined by newMediaCluster. Install
	// the existing command fixture before the test starts either operation.
	site.operationMu.Lock()
	site.radio = native
	site.operationMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type commandResult struct {
		receipt radioCommandReceipt
		err     error
	}
	commandDone := make(chan commandResult, 1)
	go func() {
		defer close(commandDone)
		receipt, err := site.sendSiteRadioCommand(ctx, radioCommandInput{ID: "paused-check", Action: "radio_check", RadioID: 10003}, OperatorActor{Administrator: true}, 0)
		commandDone <- commandResult{receipt, err}
	}()
	defer func() {
		cancel()
		select {
		case <-commandDone:
		case <-time.After(time.Second):
			t.Error("native command did not join after cancellation")
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("radio command did not reach its blocked native operation")
	}
	if site.operationMu.TryLock() {
		site.operationMu.Unlock()
		t.Fatal("command fixture did not retain its operation owner")
	}
	selection := make(chan routing.Selection, 1)
	started := time.Now()
	go func() {
		selection <- site.broker.targets(routing.Request{CallRequest: dispatch.CallRequest{RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"})
	}()
	select {
	case got := <-selection:
		if len(got.Targets) != 1 || got.Targets[0].ID != "site-2" || got.Targets[0].Router == nil {
			t.Fatalf("current Site router was not selected: %+v", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("blocked radio command blocked RF target selection")
	}
	selectionTime := time.Since(started)
	call, request := sipRFCall(t, c, "media-during-radio-command", false, false)
	for epoch := uint64(1); epoch <= 2; epoch++ {
		sipWriteBurst(t, call, request, epoch, 18)
		frames := sipActualFrames(t, c, "site-2", 18)
		for ordinal, frame := range frames {
			want := sipRFFrame(call, request, epoch, uint64(ordinal))
			if frame.SourceID != 10001 || frame.TalkgroupID != 1001 || frame.SourceEpoch != epoch || !bytes.Equal(frame.Encoded, want.Encoded) {
				t.Fatalf("epoch%d word%d changed during blocked command", epoch, ordinal)
			}
		}
		if epoch == 1 {
			time.Sleep(1100 * time.Millisecond)
			if err := call.Renew(); err != nil {
				t.Fatal("blocked command interrupted SIP allocation renewal", err)
			}
		}
	}
	select {
	case result := <-commandDone:
		t.Fatalf("native command was not still blocked: %+v", result)
	default:
	}
	lease := c.natives["site-2"]
	lease.mu.Lock()
	renewals := 0
	for _, path := range lease.paths {
		if path == "/p25/dispatch/renew" {
			renewals++
		}
	}
	alive := time.Now().Before(lease.expires)
	lease.mu.Unlock()
	if renewals < 2 || !alive {
		t.Fatalf("one-second native lease did not remain renewed: renewals%d alive%v", renewals, alive)
	}
	end, endCancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := call.End(end)
	endCancel()
	if err != nil {
		t.Fatal("call could not end while radio command remained blocked", err)
	}
	mediaWait(t, time.Second, func() bool {
		return len(c.zone.service.broker.router.Active()) == 0 && len(site.broker.router.Active()) == 0
	}, "SIP allocation cleanup during blocked command")
	close(release)
	released = true
	select {
	case result := <-commandDone:
		if result.err != nil || result.receipt.State != "sent" || !result.receipt.AwaitingAcknowledgment {
			t.Fatalf("command ownership or send-only result changed: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("released command did not finish")
	}
	native.mu.Lock()
	commands := len(native.commands)
	native.mu.Unlock()
	if commands != 1 {
		t.Fatalf("native command attempted%d times", commands)
	}
	t.Logf("RF selection%s;36 SIP/UDP words, native renewals%d and normal release completed while one native command stayed blocked", selectionTime, renewals)
}

package radio

import (
	"context"
	"errors"
	"net/http"
	"syscall"
	"testing"
	"time"
)

func TestUnchangedInhibitPolicyDefersToPendingControlFallback(t *testing.T) {
	endpoint := inhibitEndpoint(t, func(http.ResponseWriter, *http.Request) {
		t.Error("unchanged policy contacted the retiring native owner")
	})
	c := inhibitController(t, endpoint, endpoint, InhibitPolicy{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempt := &controlSwitchAttempt{ctx: ctx, cancel: cancel}
	attempt.outage.Store(true)
	c.controlSwitch = attempt
	c.siteWanted = true
	c.status.InhibitionReady = false
	if err := c.ApplyInhibitionPolicy(context.Background(), []int{}); err != nil {
		t.Fatal(err)
	}
	if !c.siteWanted || c.controlSwitch != attempt || c.status.InhibitionReady {
		t.Fatal("unchanged policy cancelled fallback or reopened admission")
	}
	if _, err := c.ControlClient(); err == nil {
		t.Fatal("fallback exposed an unprotected control client")
	}
	if err := c.synchronizeInhibition(context.Background(), false); !errors.Is(err, context.Canceled) {
		t.Fatal("old-owner observation was not retired", err)
	}
	close(c.children["control-1"].done)
	if err := c.synchronizeInhibition(context.Background(), true); err == nil {
		t.Fatal("replacement startup bypassed actual inhibition enforcement")
	}
}

func TestChangedInhibitPolicyCancelsPendingControlSwitch(t *testing.T) {
	c := testController(t, nil)
	p := selectionProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	selectionFlag(t, selectionWorker(c, "control-1"), "quiesce-busy")
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	waitRadio(t, "pending control switch", func() bool { return c.Status().ControlSwitchTarget != "" })
	if err := c.ApplyInhibitionPolicy(context.Background(), []int{10001}); err != nil {
		t.Fatal(err)
	}
	waitRadio(t, "changed policy left control switching active", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.controlSwitch == nil })
	if c.Status().State != "stopped" || c.Status().InhibitionReady {
		t.Fatal("new desired policy left stale RF active", c.Status())
	}
}

func TestControlFallbackSurvivesRepeatedUnchangedInhibitReconciliation(t *testing.T) {
	c := testController(t, nil)
	p := selectionProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		waitRadio(t, "independent voice recovery did not finish", func() bool {
			for _, channel := range c.Status().Channels {
				if !channel.Ready {
					return false
				}
			}
			return true
		})
		old := c.Status().ActiveControlChannelID
		if err := syscall.Kill(selectionWorker(c, old).cmd.Process.Pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		recovered := false
		for time.Now().Before(deadline) {
			if err := c.ApplyInhibitionPolicy(context.Background(), []int{}); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			status := c.Status()
			if status.State == "running" && status.ActiveControlChannelID != old && status.InhibitionReady && status.ControlSwitchTarget == "" {
				recovered = true
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !recovered {
			t.Fatal("unchanged policy withdrew repeated fallback", c.Status())
		}
	}
}

func TestLostNativePolicyContactFencesAdmissionAndRetainsRecovery(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), selectionProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	old := selectionWorker(c, "control-1")
	selectionFlag(t, old, "inhibit-contact-lost")
	waitSelected(t, c, "voice-1")
	if !c.Status().InhibitionReady || !selectedChannel(c, "voice-1").Ready {
		t.Fatal("replacement admitted operation without verified inhibition")
	}
	c.mu.Lock()
	wanted := c.siteWanted
	c.mu.Unlock()
	if !wanted || old.alive() {
		t.Fatal("unconfirmed policy owner was retained or desired recovery was withdrawn")
	}
}

func TestNativePolicyRejectionIsNotContactLoss(t *testing.T) {
	for _, err := range []error{&NativeError{HTTPStatus: 503, Status: 503, Message: "rejected"}, &NativeError{Message: "Native response was invalid."}, errors.New("bad inhibit snapshot")} {
		if nativeContactLost(err) {
			t.Fatal("actual rejection or invalid data treated as a retryable contact loss")
		}
	}
	if !nativeContactLost(&NativeError{ContactLost: true}) {
		t.Fatal("transport loss lost its classification")
	}
}

func TestControlCandidateSnapshotDoesNotAliasCaller(t *testing.T) {
	p := selectionProfileThree()
	p.Site.ControlPolicy.CandidateIDs = []string{"voice-2", "voice-1", "control-1"}
	before := append([]string(nil), p.Site.ControlPolicy.CandidateIDs...)
	cloned := cloneProfile(p)
	cloned.Site.ControlPolicy.CandidateIDs[0] = "changed"
	for i, id := range before {
		if p.Site.ControlPolicy.CandidateIDs[i] != id {
			t.Fatal("snapshot changed caller control pool")
		}
	}
	_ = RFConfigurationSignature(p)
	for i, id := range before {
		if p.Site.ControlPolicy.CandidateIDs[i] != id {
			t.Fatal("fingerprinting changed caller control pool order")
		}
	}
}

func TestUnchangedPolicyDuringControlRetirementDoesNotWithdrawRecovery(t *testing.T) {
	endpoint := inhibitEndpoint(t, func(http.ResponseWriter, *http.Request) { t.Error("retired control owner was contacted") })
	c := inhibitController(t, endpoint, endpoint, InhibitPolicy{})
	c.siteWanted = true
	c.status.InhibitionReady = false
	close(c.children["control-1"].done)
	if err := c.ApplyInhibitionPolicy(context.Background(), []int{}); !errors.Is(err, context.Canceled) {
		t.Fatal("retired-owner observation was not cancelled", err)
	}
	if !c.siteWanted || c.status.InhibitionReady || c.status.State != "running" {
		t.Fatal("retirement window withdrew desired recovery or reopened admission")
	}
}

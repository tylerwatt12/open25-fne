package framework

import (
	"context"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/siteaudio"
	"sync/atomic"
	"testing"
)

type drainedPolicyRadio struct {
	*operationRadio
	router             *siteaudio.Router
	native             *mediaNative
	appliedBeforeDrain atomic.Bool
}

func (r *drainedPolicyRadio) ApplyPolicy(ctx context.Context, profile radio.Profile) error {
	if r.router.Status().ActiveCalls != 0 || r.router.Status().RFGrants != 0 {
		r.appliedBeforeDrain.Store(true)
	}
	r.native.mu.Lock()
	released := false
	for _, path := range r.native.paths {
		released = released || path == "/p25/dispatch/release"
	}
	r.native.mu.Unlock()
	if !released {
		r.appliedBeforeDrain.Store(true)
	}
	return r.operationRadio.ApplyPolicy(ctx, profile)
}
func TestChangedCallPolicyReleasesRFGrantBeforeNativeReplacement(t *testing.T) {
	for _, cause := range []string{"mode", "start-policy", "priority", "affiliation-access", "affiliation-list", "transmit-access", "transmit-list"} {
		t.Run(cause, func(t *testing.T) {
			var router *siteaudio.Router
			var checker *drainedPolicyRadio
			service, runtime := operationConfiguredFixture(t, func(s *Service) {
				system := storeFixtureSystem()
				group := &system.Talkgroups[0]
				group.StartPolicy = "fast_start"
				group.Priority = 10
				group.AdmissionMode = "both"
				group.TransmitMode = "selectable"
				group.AlgorithmID = 132
				group.KeyID = 7
				if cause == "affiliation-list" {
					group.AffiliationAccess = "selected"
					group.AffiliationRadioIDs = []int{10001}
				}
				if cause == "transmit-list" {
					group.TransmitAccess = "selected"
					group.TransmitRadioIDs = []int{10001}
				}
				if err := s.Store.WriteCache("system", storeDoc(t, system)); err != nil {
					t.Fatal(err)
				}
				s.refreshDirectory()
				old, err := s.profileRadio()
				if err != nil {
					t.Fatal(err)
				}
				s.radioAppliedProfile = old
				s.radioCandidateSignature = profileSignature(old)
				native := &mediaNative{}
				router = siteaudio.New(siteaudio.Options{AllowTX: s.allowedRadioGroup, HasRFInterest: func(int) bool { return true }, Control: func() (radio.Native, error) { return native, nil }, Codec: &mediaCodec{frames: make(chan dispatch.Frame, 16)}})
				if _, err = router.Begin(context.Background(), dispatch.CallRequest{ID: "active-before-policy", RadioID: 10001, Talkgroups: []int{1001}}); err != nil {
					t.Fatal(err)
				}
				if router.Status().RFGrants != 1 {
					t.Fatal("fixture had no RF grant to retire")
				}
				s.rfRouter.Store(router)
				checker = &drainedPolicyRadio{operationRadio: s.radio.(*operationRadio), router: router, native: native}
				s.radio = checker
			})
			var system SystemConfig
			if !service.readCache("system", &system) {
				t.Fatal("System policy unavailable")
			}
			switch cause {
			case "affiliation-access":
				system.Talkgroups[0].AffiliationAccess = "selected"
			case "affiliation-list":
				system.Talkgroups[0].AffiliationRadioIDs = []int{}
			case "transmit-access":
				system.Talkgroups[0].TransmitAccess = "selected"
			case "transmit-list":
				system.Talkgroups[0].TransmitRadioIDs = []int{}
			case "mode":
				system.Talkgroups[0].AdmissionMode = "secure"
				system.Talkgroups[0].TransmitMode = "secure"
			case "start-policy":
				system.Talkgroups[0].StartPolicy = "all_start"
			case "priority":
				system.Talkgroups[0].Priority = 2
			}
			if err := service.Store.WriteCache("system", storeDoc(t, system)); err != nil {
				t.Fatal(err)
			}
			service.refreshDirectory()
			service.reconcileRadio()
			// refreshDirectory also wakes the background worker, which may
			// already own the replacement when reconcileRadio returns.
			frameworkWait(t, "changed rule never reached native policy replacement", func() bool {
				select {
				case <-runtime.applications:
					return true
				default:
					return false
				}
			})
			if checker.appliedBeforeDrain.Load() {
				t.Fatal("native policy was applied before old RF grant release")
			}
			if _, err := router.Begin(context.Background(), dispatch.CallRequest{ID: "after-policy", RadioID: 10001, Talkgroups: []int{1001}}); err == nil {
				t.Fatal("old router admitted work after policy withdrawal")
			}
		})
	}
}

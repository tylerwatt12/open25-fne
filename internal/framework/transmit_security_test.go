package framework

import (
	"context"
	"errors"
	"open25.local/open25-fne/internal/dispatch"
	"slices"
	"testing"
	"time"
)

func TestNetworkRouterResolvesStrappingBeforeIdentityPermission(t *testing.T) {
	c := newMediaCluster(t)
	for _, test := range []struct {
		mode      string
		requested bool
		want      error
	}{
		{"clear", true, dispatch.ErrUnavailable},
		{"secure", false, dispatch.ErrPermission},
	} {
		t.Run(test.mode, func(t *testing.T) {
			router := identityRouter(t, c, "strapping-"+test.mode, c.zone.server.URL)
			view := *router.service.view()
			view.System.Talkgroups = slices.Clone(view.System.Talkgroups)
			for i := range view.System.Talkgroups {
				view.System.Talkgroups[i].AdmissionMode = "both"
				view.System.Talkgroups[i].TransmitMode = test.mode
				view.System.Talkgroups[i].AlgorithmID = 132
				view.System.Talkgroups[i].KeyID = 7
			}
			router.service.directory.value.Store(&view)
			p := identityPrincipal()
			p.TransmitGroups = []int{1001}
			if err := router.ClaimAudio(context.Background(), p, "strapping-session"); err != nil {
				t.Fatal(err)
			}
			defer router.ReleaseAudio(context.Background(), p, "strapping-session")
			_, err := router.Begin(context.Background(), dispatch.CallRequest{ID: "strapping-call", UserID: p.UserID, SessionID: "strapping-session", RadioID: p.RadioID, Talkgroups: []int{1001}, Secure: test.requested})
			// The permitted clear call reaches this fixture's unavailable packet codec;
			// forced secure is rejected according to its actual encryption permission.
			if !errors.Is(err, test.want) {
				t.Fatalf("permission checked before strapping: %v; want %v", err, test.want)
			}
		})
	}
}

type clearAutomationRouter struct{ speechTestRouter }

func (r *clearAutomationRouter) Begin(ctx context.Context, q dispatch.CallRequest) (dispatch.Grant, error) {
	g, err := r.speechTestRouter.Begin(ctx, q)
	g.Secure, g.AlgorithmID, g.KeyID = false, 0, 0
	return g, err
}
func TestAutomationEncryptedIntentCanResolveToClearWithoutSecurePermission(t *testing.T) {
	c := newMediaCluster(t)
	a, _ := seedMediaAutomation(t, c)
	s := c.dispatch.service
	if s.hub != nil {
		s.hub.Close()
	}
	router := &clearAutomationRouter{}
	s.hub = dispatch.New(dispatch.Options{Router: router})
	defer s.hub.Close()
	runtime, err := s.automationRuntimeForRequest()
	if err != nil {
		t.Fatal(err)
	}
	input := automationJobInput{Talkgroups: []int{1001}, Secure: true, RequestID: "forced-clear", ExpiresAt: time.Now().Add(time.Minute), pcm: make([]byte, dispatch.FrameBytes), action: "wav"}
	if _, status, err := runtime.start(s.view().Automations[a.ID], input); err != nil || status != 202 {
		t.Fatalf("requested encryption rejected before actual strapping: %d %v", status, err)
	}
	runtime.wg.Wait()
	job, ok := runtime.get(a.ID, input.RequestID, false)
	if !ok || job.State != "completed" || job.Frames != 1 || !router.secure.Load() {
		t.Fatalf("forced-clear job failed or request was rewritten: %+v", job)
	}
}

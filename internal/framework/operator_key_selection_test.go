package framework

import (
	"open25.local/open25-fne/internal/dispatch"
	"testing"
)

func TestOperatorTransmitKeySelectionFollowsResolvedMode(t *testing.T) {
	for _, test := range []struct {
		name, mode             string
		secure                 bool
		algorithm, key         int
		wantSecure             bool
		wantAlgorithm, wantKey int
		wantError              bool
	}{
		{"assigned default", "selectable", true, 0, 0, true, 132, 7, false},
		{"ALGID zero means assigned", "selectable", true, 0, 99, true, 132, 7, false},
		{"operator alternate KID zero", "selectable", true, 129, 0, true, 129, 0, false},
		{"strapped encrypted keeps alternate key", "secure", false, 129, 0, true, 129, 0, false},
		{"strapped clear ignores selection", "clear", true, 999, -1, false, 0, 0, false},
		{"operator clear ignores selection", "selectable", false, 129, 29, false, 0, 0, false},
		{"invalid explicit key fails instead of defaulting", "secure", true, 999, 7, false, 0, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := securityPolicyService(Talkgroup{TGID: 1001, Enabled: true, AdmissionMode: "both", TransmitMode: test.mode, AlgorithmID: 132, KeyID: 7})
			q, err := s.prepareTransmit(dispatch.CallRequest{Talkgroups: []int{1001}, Secure: test.secure, AlgorithmID: test.algorithm, KeyID: test.key})
			if (err != nil) != test.wantError {
				t.Fatalf("transmit resolution: %+v, %v", q, err)
			}
			if err == nil && (q.Secure != test.wantSecure || q.AlgorithmID != test.wantAlgorithm || q.KeyID != test.wantKey) {
				t.Fatalf("resolved identity: %+v", q)
			}
		})
	}
}

func TestOperatorKeyCanUnifyDifferentAssignedMultiselectKeys(t *testing.T) {
	s := securityPolicyService(
		Talkgroup{TGID: 1001, Enabled: true, AdmissionMode: "both", TransmitMode: "secure", AlgorithmID: 132, KeyID: 7},
		Talkgroup{TGID: 1002, Enabled: true, AdmissionMode: "both", TransmitMode: "secure", AlgorithmID: 170, KeyID: 8},
	)
	request := dispatch.CallRequest{Talkgroups: []int{1001, 1002}, Secure: true}
	if _, err := s.prepareTransmit(request); err == nil {
		t.Fatal("different assigned keys were silently replaced")
	}
	request.AlgorithmID = 129
	q, err := s.prepareTransmit(request)
	if err != nil || q.AlgorithmID != 129 || q.KeyID != 0 {
		t.Fatalf("explicit shared operator key lost: %+v, %v", q, err)
	}
}

package framework

import (
	"open25.local/open25-fne/internal/dispatch"
	"testing"
)

func securityPolicyService(groups ...Talkgroup) *Service {
	s := &Service{}
	s.directory.value.Store(&directorySnapshot{System: SystemConfig{Talkgroups: groups}})
	return s
}

func TestTransmitStrappingAndNetworkAdmissionAreIndependent(t *testing.T) {
	for _, test := range []struct {
		name, admission, transmit   string
		requested, actual, accepted bool
	}{
		{"strapped encrypted on mixed group", "both", "secure", false, true, true},
		{"strapped clear on mixed group", "both", "clear", true, false, true},
		{"operator chooses clear", "both", "selectable", false, false, true},
		{"operator chooses encrypted", "both", "selectable", true, true, true},
		{"clear strapping on encrypted group fails when used", "secure", "clear", true, false, false},
		{"encrypted strapping on clear group fails when used", "clear", "secure", false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := securityPolicyService(Talkgroup{TGID: 1001, Enabled: true, AdmissionMode: test.admission, TransmitMode: test.transmit, AlgorithmID: 132, KeyID: 7})
			request := dispatch.CallRequest{RadioID: 10001, Talkgroups: []int{1001}, Secure: test.requested}
			prepared, err := s.prepareTransmit(request)
			if err != nil || prepared.Secure != test.actual {
				t.Fatalf("transmit result: %+v, %v", prepared, err)
			}
			if test.actual && (prepared.AlgorithmID != 132 || prepared.KeyID != 7) {
				t.Fatal("assigned transmit identity was not selected")
			}
			if !test.actual && (prepared.AlgorithmID != 0 || prepared.KeyID != 0) {
				t.Fatal("clear transmit retained an encrypted key identity")
			}
			_, err = s.prepareCall(prepared, "rf", false)
			if (err == nil) != test.accepted {
				t.Fatalf("call admission: %v", err)
			}
		})
	}
}

func TestIngressNeverAppliesConsoleStrappingOrAssignedKey(t *testing.T) {
	for _, mode := range []string{"clear", "secure", "selectable"} {
		for _, secure := range []bool{false, true} {
			s := securityPolicyService(Talkgroup{TGID: 1001, Enabled: true, AdmissionMode: "both", TransmitMode: mode, AlgorithmID: 132, KeyID: 7})
			request := dispatch.CallRequest{RadioID: 10001, Talkgroups: []int{1001}, Secure: secure}
			if secure {
				request.AlgorithmID, request.KeyID = 129, 29
			}
			got, err := s.prepareCall(request, "rf", false)
			if err != nil || got.Secure != request.Secure || got.AlgorithmID != request.AlgorithmID || got.KeyID != request.KeyID {
				t.Fatalf("%s ingress was changed: %+v, %v", mode, got, err)
			}
		}
	}
}

func TestMultiselectResolvesEveryGroupFromOriginalOperatorChoice(t *testing.T) {
	s := securityPolicyService(
		Talkgroup{TGID: 1001, Enabled: true, AdmissionMode: "both", TransmitMode: "secure", AlgorithmID: 132, KeyID: 7},
		Talkgroup{TGID: 1002, Enabled: true, AdmissionMode: "both", TransmitMode: "selectable", AlgorithmID: 132, KeyID: 7},
	)
	if _, err := s.prepareTransmit(dispatch.CallRequest{Talkgroups: []int{1001, 1002}, Secure: false}); err == nil {
		t.Fatal("the first group's strapping changed the operator's choice for the next group")
	}
	if _, err := s.prepareTransmit(dispatch.CallRequest{Talkgroups: []int{1001, 1002}, Secure: true}); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownModesAreSavedValuesThatFailAtUse(t *testing.T) {
	s := securityPolicyService(Talkgroup{TGID: 1001, Enabled: true, AdmissionMode: "future", TransmitMode: "future"})
	request := dispatch.CallRequest{Talkgroups: []int{1001}}
	if _, err := s.prepareTransmit(request); err == nil {
		t.Fatal("unknown transmit mode pretended to work")
	}
	if _, err := s.prepareCall(request, "rf", false); err == nil {
		t.Fatal("unknown admission mode pretended to work")
	}
}

func TestPendingEncryptionIdentityIsRFAllocationOnly(t *testing.T) {
	s := securityPolicyService(Talkgroup{TGID: 1001, Enabled: true, AdmissionMode: "both", TransmitMode: "secure"})
	q := dispatch.CallRequest{Talkgroups: []int{1001}, Secure: true, EncryptionPending: true}
	if _, err := s.prepareCall(q, "rf", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareCall(q, "application", false); err == nil {
		t.Fatal("application acquired an RF reservation exception")
	}
	if _, err := s.prepareTransmit(q); err == nil {
		t.Fatal("local transmitter acquired an RF reservation exception")
	}
	q.AlgorithmID = 132
	if _, err := s.prepareCall(q, "rf", false); err == nil {
		t.Fatal("pending reservation invented a partial identity")
	}
}

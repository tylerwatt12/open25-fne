package framework

import (
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/routing"
)

func TestRFReceiveActivityAllStartRequiresItsCurrentCommittedAlias(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "clear", true: "encrypted"}[secure], func(t *testing.T) {
			c := newMediaCluster(t)
			m := &rfAdmissionManager{s: c.site.service, entries: map[string]*admittedRF{}, aliases: map[string]string{}, seen: map[string]time.Time{}}
			now := time.Now()
			q := routing.Request{CallRequest: dispatch.CallRequest{ID: "committed-allocation", RadioID: 10001, Talkgroups: []int{1001}, Secure: secure}}
			if secure {
				q.EncryptionPending = true
			}
			entry := &admittedRF{request: nativeRFAdmission{ID: "native-record", State: "committed", Source: 10001, Group: 1001, Secure: secure}, stream: &rfStream{request: q}, approvedAt: now, lastObserved: now, boundStream: "bound-application", inputSequence: 9}
			if secure {
				entry.boundAlgorithm, entry.boundKey = 0x84, 7
			}
			m.mu.Lock()
			m.entries["native-record"] = entry
			m.aliases["bound-application"] = "native-record"
			m.mu.Unlock()
			a := dispatch.CodecReceiveActivity{CallID: "bound-application", SourceID: 10001, TalkgroupID: 1001, Encrypted: secure, AlgorithmID: 0x80, LastVoiceSequence: 9, ControlSequence: 1, LastMediaAt: now, ValidUntil: now.Add(100 * time.Millisecond)}
			if secure {
				a.AlgorithmID = 0x84
				a.KeyID = 7
			}
			if id, ok := m.activityOwner(a); !ok || id != q.ID {
				t.Fatal("matching committed alias not found")
			}
			m.mu.Lock()
			if entry.inputSequence != 9 || entry.boundStream != a.CallID || entry.lastObserved != now {
				m.mu.Unlock()
				t.Fatal("activity advanced audio sequence or invented native admission observations")
			}
			entry.boundStream = "replacement-application"
			m.mu.Unlock()
			if _, ok := m.activityOwner(a); ok {
				t.Fatal("retired application alias refreshed its replacement")
			}
			m.mu.Lock()
			entry.boundStream = a.CallID
			entry.lastObserved = now.Add(-2 * time.Second)
			m.mu.Unlock()
			if _, ok := m.activityOwner(a); ok {
				t.Fatal("media activity overrode stale native admission authority")
			}
			m.mu.Lock()
			entry.lastObserved = time.Now()
			m.mu.Unlock()
			terminal := dispatch.Frame{CallID: a.CallID, SourceID: a.SourceID, TalkgroupID: a.TalkgroupID, Sequence: a.LastVoiceSequence + 1, End: true, Encrypted: secure, AlgorithmID: a.AlgorithmID, KeyID: a.KeyID, CapturedAt: time.Now()}
			if _, accepted := m.bind(terminal); accepted {
				t.Fatal("AllStart burst End was published as fresh audio")
			}
			m.mu.Lock()
			bound := entry.boundStream
			m.mu.Unlock()
			if bound != "" {
				t.Fatal("matching clear/secure End did not release its AllStart media alias")
			}

		})
	}
}

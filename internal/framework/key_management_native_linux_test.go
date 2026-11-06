//go:build linux

package framework

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25crypto"
)

// Real local codec only: no FNE, Site engine, modem, or RF transmitter runs.
func TestNativeCryptoRuntimeLoadsThenRemovesKeysAndRestarts(t *testing.T) {
	binary := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	if binary == "" {
		t.Skip("set reviewed native packet codec executable")
	}
	system := keyManagementSystem(t)
	profile := storeFixtureSystem()
	zone := storeFixtureZone()
	endpoint := startIntegrationService(t, t.TempDir(), RoleApplication, "application-1", ApplicationConfig{Name: "Application Server", ConnectionURL: "http://127.0.0.1:5001"}, ServiceCredentials{Peers: map[string]PeerCredential{}}, profile, zone)
	private, err := endpointKey(endpoint.service.Config().StateDir)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := readCredentials(system.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	creds.CryptoRecipients = map[string]string{"application-1": hex.EncodeToString(private.PublicKey().Bytes())}
	if err = WritePrivateJSON(system.service.Config().CredentialFile, creds); err != nil {
		t.Fatal(err)
	}
	if err = system.service.writeTrafficKeys([]trafficKey{publicFixtureKey()}); err != nil {
		t.Fatal(err)
	}
	pinFixtureSystem(t, system.service, endpoint.service)
	envelopes, err := system.service.keyEnvelopes()
	if err != nil {
		t.Fatal(err)
	}
	if err = endpoint.service.writeCache("key_envelopes", envelopes); err != nil {
		t.Fatal(err)
	}
	endpoint.service.mu.Lock()
	endpoint.service.config.Media.CodecBinary = binary
	endpoint.service.mu.Unlock()
	if err = endpoint.service.reconcileCrypto(); err != nil {
		t.Fatal(err)
	}
	codec, keys := endpoint.service.packetCodec()
	if codec == nil || !codec.Alive() || !keys[p25crypto.Pair(132, 7)] {
		t.Fatal("enrolled key not available at trusted endpoint")
	}
	if _, err = os.Stat(filepath.Join(endpoint.service.Config().StateDir, "crypto", "codec.keys")); !os.IsNotExist(err) {
		t.Fatal("plaintext runtime key file survived native loading", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame := dispatch.Frame{CallID: "runtime-key-check", SourceID: 10003, TalkgroupID: 1001, Sequence: 1, PCM: make([]byte, dispatch.FrameBytes), Encrypted: true, AlgorithmID: 132, KeyID: 7}
	encoded, err := codec.Encode(ctx, frame)
	if err != nil || !encoded.Encrypted || len(encoded.PCM) != 0 {
		t.Fatal("endpoint did not encrypt with enrolled key", err)
	}
	if _, err = codec.Decode(ctx, encoded); err != nil {
		t.Fatal("endpoint did not decrypt enrolled key", err)
	}
	if err = codec.Close(); err != nil {
		t.Fatal(err)
	}
	if err = endpoint.service.reconcileCrypto(); err != nil {
		t.Fatal(err)
	}
	restarted, keys := endpoint.service.packetCodec()
	if restarted == codec || restarted == nil || !restarted.Alive() || !keys[p25crypto.Pair(132, 7)] {
		t.Fatal("dead helper with unchanged key digest was not restarted")
	}
	if err = endpoint.service.Store.WriteCache("key_envelopes", []byte(`{"application-1":{}}`)); err != nil {
		t.Fatal(err)
	}
	if err = endpoint.service.reconcileCrypto(); err == nil {
		t.Fatal("corrupt inventory accepted")
	}
	stopped, keys := endpoint.service.packetCodec()
	if stopped != nil || len(keys) != 0 || restarted.Alive() {
		t.Fatal("corrupt inventory left an authorized crypto helper running")
	}
}

// A changed inventory or restarted native process must prevent an existing
// receive stream from silently adopting another key/MI. Actual SIP transmit
// retirement is covered by TestCodecReplacementRetiresTransmitGrantOnWriteAndRenew.
// All processes here are vocoders on private pipes; no RF hardware is opened.
func TestNativeCryptoReplacementDoesNotResumeReceiveCalls(t *testing.T) {
	binary := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	if binary == "" {
		t.Skip("set reviewed native packet codec executable")
	}
	for _, cause := range []string{"material-change", "process-restart", "key-delete", "invalid-inventory"} {
		t.Run(cause, func(t *testing.T) {
			system := keyManagementSystem(t)
			profile, zone := storeFixtureSystem(), storeFixtureZone()
			endpoint := startIntegrationService(t, t.TempDir(), RoleApplication, "application-1", ApplicationConfig{Name: "Application Server", ConnectionURL: "http://127.0.0.1:5001"}, ServiceCredentials{OutboundID: "application-1", OutboundToken: strings.Repeat("fixture-", 8), Peers: map[string]PeerCredential{}}, profile, zone)
			endpoint.service.operationCancel()
			endpoint.service.operationWG.Wait()
			endpoint.service.operationCtx, endpoint.service.operationCancel = context.WithCancel(context.Background())
			endpoint.service.broker.cancel()
			endpoint.service.broker.ctx, endpoint.service.broker.cancel = context.WithCancel(endpoint.service.operationCtx)
			endpoint.service.mediaClient.Close()
			private, err := endpointKey(endpoint.service.Config().StateDir)
			if err != nil {
				t.Fatal(err)
			}
			credentials, err := readCredentials(system.service.Config().CredentialFile)
			if err != nil {
				t.Fatal(err)
			}
			credentials.CryptoRecipients = map[string]string{"application-1": hex.EncodeToString(private.PublicKey().Bytes())}
			if err = WritePrivateJSON(system.service.Config().CredentialFile, credentials); err != nil {
				t.Fatal(err)
			}
			pinFixtureSystem(t, system.service, endpoint.service)
			endpoint.service.mu.Lock()
			endpoint.service.config.Media.CodecBinary = binary
			endpoint.service.mu.Unlock()
			update := func(keys []trafficKey) {
				t.Helper()
				if err := system.service.writeTrafficKeys(keys); err != nil {
					t.Fatal(err)
				}
				envelopes, err := system.service.keyEnvelopes()
				if err != nil {
					t.Fatal(err)
				}
				if err = endpoint.service.writeCache("key_envelopes", envelopes); err != nil {
					t.Fatal(err)
				}
				if err = endpoint.service.reconcileCrypto(); err != nil {
					t.Fatal(err)
				}
			}
			key := publicFixtureKey()
			update([]trafficKey{key})
			old, _ := endpoint.service.packetCodec()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			input := dispatch.Frame{CallID: "active-old-process", SourceID: 10003, TalkgroupID: 1001, Sequence: 1, PCM: make([]byte, dispatch.FrameBytes), AlgorithmID: 132, KeyID: 7, Encrypted: true, CapturedAt: time.Now()}
			first, err := old.Encode(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = endpoint.service.decodeDispatchFrame(first); err != nil {
				t.Fatal("initial receive", err)
			}
			input.Sequence = 2
			second, err := old.Encode(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			switch cause {
			case "material-change":
				key.Material = strings.Repeat("cd", 32)
				update([]trafficKey{key})
			case "process-restart":
				if err = old.Close(); err != nil {
					t.Fatal(err)
				}
				if err = endpoint.service.reconcileCrypto(); err != nil {
					t.Fatal(err)
				}
			case "key-delete":
				update([]trafficKey{})
			case "invalid-inventory":
				if err = endpoint.service.Store.WriteCache("key_envelopes", []byte(`{"application-1":{}}`)); err != nil {
					t.Fatal(err)
				}
				if err = endpoint.service.reconcileCrypto(); err == nil {
					t.Fatal("invalid inventory accepted")
				}
			}
			current, _ := endpoint.service.packetCodec()
			if current == old || old.Alive() {
				t.Fatal("old native process retained ownership")
			}
			if _, err = endpoint.service.decodeDispatchFrame(second); !errors.Is(err, dispatch.ErrUnavailable) {
				t.Fatal("old receive stream adopted replacement process", err)
			}
			// Keep an independent new-process probe with the same call token. Old End
			// operations must not clear or pad that replacement process's state.
			probe := input
			probe.Sequence = 1
			hasProbe := cause == "material-change" || cause == "process-restart"
			if hasProbe {
				if _, err = current.Encode(ctx, probe); err != nil {
					t.Fatal(err)
				}
			}
			end := first
			end.End = true
			end.PCM = nil
			end.Encoded = nil
			end.P25 = nil
			endpoint.service.endDispatchFrame(end)
			if endpoint.service.crypto.activeReceiveOwner(first) != nil {
				t.Fatal("receive End left process binding")
			}
			if hasProbe {
				if _, err = current.Encode(ctx, probe); err == nil {
					t.Fatal("old End reset the replacement process")
				}
			}
			update([]trafficKey{key})
			fresh, _ := endpoint.service.packetCodec()
			input.CallID = "new-distinct-call"
			input.Sequence = 1
			encoded, err := fresh.Encode(ctx, input)
			if err != nil {
				t.Fatal("new call encode", err)
			}
			if _, err = endpoint.service.decodeDispatchFrame(encoded); err != nil {
				t.Fatal("new distinct call remained blocked", err)
			}
		})
	}
}

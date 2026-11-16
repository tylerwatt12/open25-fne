package framework

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func envelopeCacheFixture(t *testing.T) (*Service, map[string]*ecdh.PrivateKey, []trafficKey) {
	t.Helper()
	s := keyManagementSystem(t).service
	keys := []trafficKey{publicFixtureKey()}
	if err := s.writeTrafficKeys(keys); err != nil {
		t.Fatal(err)
	}
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := map[string]*ecdh.PrivateKey{"application-1": private}
	setEnvelopeCacheRecipients(t, s, endpoints)
	return s, endpoints, keys
}

func setEnvelopeCacheRecipients(t *testing.T, s *Service, endpoints map[string]*ecdh.PrivateKey) {
	t.Helper()
	creds, err := readCredentials(s.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	creds.CryptoRecipients = make(map[string]string, len(endpoints))
	for id, private := range endpoints {
		creds.CryptoRecipients[id] = hex.EncodeToString(private.PublicKey().Bytes())
	}
	if err = WritePrivateJSON(s.Config().CredentialFile, creds); err != nil {
		t.Fatal(err)
	}
}

func readEnvelopeCache(t *testing.T, s *Service) map[string]keyEnvelope {
	t.Helper()
	value, err := s.keyEnvelopes()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func checkEnvelopeCachePlaintext(t *testing.T, s *Service, endpoints map[string]*ecdh.PrivateKey, envelopes map[string]keyEnvelope, want []trafficKey) {
	t.Helper()
	if len(envelopes) != len(endpoints) {
		t.Fatal("envelope recipient set does not match current enrollment")
	}
	public := pinFixtureSystem(t, s, nil)
	for recipient, private := range endpoints {
		env, present := envelopes[recipient]
		if !present {
			t.Fatal("current recipient has no envelope")
		}
		if !ed25519.Verify(public, envelopeMessage(env), env.Signature) {
			t.Fatal("generated envelope signature is invalid")
		}
		got, err := openKeyEnvelope(private, s.view().System.ID, recipient, env, public)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("current recipient cannot open the current inventory", err)
		}
	}
}

func TestKeyEnvelopeCacheReusesExactInputsAndReturnsOwnedBytes(t *testing.T) {
	s, endpoints, keys := envelopeCacheFixture(t)
	want := readEnvelopeCache(t, s)
	checkEnvelopeCachePlaintext(t, s, endpoints, want, keys)
	mutated := readEnvelopeCache(t, s)
	if !reflect.DeepEqual(mutated, want) {
		t.Fatal("unchanged inventory and enrollment regenerated envelopes")
	}
	env := mutated["application-1"]
	for _, raw := range [][]byte{env.Signature, env.Public, env.Nonce, env.Ciphertext} {
		raw[0] ^= 1
	}
	env.SystemID = "caller-mutated"
	mutated["application-1"] = env
	delete(mutated, "application-1")
	mutated["caller-added"] = keyEnvelope{}
	if got := readEnvelopeCache(t, s); !reflect.DeepEqual(got, want) {
		t.Fatal("caller mutations changed the cached envelope")
	}
	// Returned copies can be used and changed concurrently without sharing the
	// authority's backing map or any cryptographic byte slice.
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			got, err := s.keyEnvelopes()
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Error("concurrent read did not receive the unchanged envelope", err)
				return
			}
			got["application-1"].Ciphertext[0] ^= 1
			delete(got, "application-1")
		})
	}
	workers.Wait()
}

func TestKeyEnvelopeCacheRotatesForEveryAuthorityInput(t *testing.T) {
	for _, change := range []string{"material", "name", "enabled", "algorithm and Key ID", "protected document", "recipient key", "recipient added", "recipient removed", "signing identity", "System identity"} {
		t.Run(change, func(t *testing.T) {
			s, endpoints, keys := envelopeCacheFixture(t)
			before := readEnvelopeCache(t, s)
			switch change {
			case "material":
				keys[0].Material = strings.Repeat("34", 32)
			case "name":
				keys[0].Name = "Changed inventory name"
			case "enabled":
				keys[0].Enabled = false
			case "algorithm and Key ID":
				keys[0].AlgorithmID, keys[0].KeyID = 129, 99
			case "recipient key", "recipient added":
				private, err := ecdh.X25519().GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				id := "application-1"
				if change == "recipient added" {
					id = "application-2"
				}
				endpoints[id] = private
				setEnvelopeCacheRecipients(t, s, endpoints)
			case "recipient removed":
				delete(endpoints, "application-1")
				setEnvelopeCacheRecipients(t, s, endpoints)
			case "signing identity":
				seed := make([]byte, ed25519.SeedSize)
				if _, err := rand.Read(seed); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(s.Config().StateDir, "crypto", "system-signing.key"), seed, 0600); err != nil {
					t.Fatal(err)
				}
				clear(seed)
			case "System identity":
				var system SystemConfig
				if err := s.owned(&system); err != nil {
					t.Fatal(err)
				}
				system.ID = "system-rotated"
				if err := s.Store.SaveConfig(storeDoc(t, system)); err != nil {
					t.Fatal(err)
				}
				s.refreshDirectory()
			}
			if change == "material" || change == "name" || change == "enabled" || change == "algorithm and Key ID" || change == "protected document" {
				if err := s.writeTrafficKeys(keys); err != nil {
					t.Fatal(err)
				}
			}
			after := readEnvelopeCache(t, s)
			if reflect.DeepEqual(after, before) {
				t.Fatal("changed authority input reused the old encrypted inventory")
			}
			for recipient, env := range after {
				if old, present := before[recipient]; present && (bytes.Equal(env.Public, old.Public) || bytes.Equal(env.Nonce, old.Nonce)) {
					t.Fatal("changed authority input reused an ephemeral key or nonce")
				}
			}
			checkEnvelopeCachePlaintext(t, s, endpoints, after, keys)
			if repeated := readEnvelopeCache(t, s); !reflect.DeepEqual(repeated, after) {
				t.Fatal("new unchanged authority input was not retained")
			}
		})
	}
}

func TestKeyEnvelopeCacheValidatesSourcesBeforeReuse(t *testing.T) {
	for _, failure := range []string{"protected ciphertext", "master permissions", "signing permissions", "recipient enrollment"} {
		t.Run(failure, func(t *testing.T) {
			s, _, _ := envelopeCacheFixture(t)
			_ = readEnvelopeCache(t, s)
			switch failure {
			case "protected ciphertext":
				raw, _, err := s.Store.ReadCache("traffic_keys")
				if err != nil {
					t.Fatal(err)
				}
				var protected protectedKeys
				if err = json.Unmarshal(raw, &protected); err != nil {
					t.Fatal(err)
				}
				protected.Ciphertext[0] ^= 1
				if err = s.writeCache("traffic_keys", protected); err != nil {
					t.Fatal(err)
				}
			case "master permissions", "signing permissions":
				file := "master.key"
				if failure == "signing permissions" {
					file = "system-signing.key"
				}
				if err := os.Chmod(filepath.Join(s.Config().StateDir, "crypto", file), 0644); err != nil {
					t.Fatal(err)
				}
			case "recipient enrollment":
				creds, err := readCredentials(s.Config().CredentialFile)
				if err != nil {
					t.Fatal(err)
				}
				creds.CryptoRecipients["application-2"] = strings.Repeat("00", 32)
				if err = WritePrivateJSON(s.Config().CredentialFile, creds); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := s.keyEnvelopes(); err == nil || out != nil {
				t.Fatal("cached envelope hid an invalid current authority input")
			}
		})
	}
}

func TestKeyEnvelopeCacheEmptyInventoryDoesNotRequireMasterKey(t *testing.T) {
	s := keyManagementSystem(t).service
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := map[string]*ecdh.PrivateKey{"application-1": private}
	setEnvelopeCacheRecipients(t, s, endpoints)
	first := readEnvelopeCache(t, s)
	checkEnvelopeCachePlaintext(t, s, endpoints, first, []trafficKey{})
	if second := readEnvelopeCache(t, s); !reflect.DeepEqual(first, second) {
		t.Fatal("unchanged empty inventory regenerated envelopes")
	}
	if _, err := os.Stat(filepath.Join(s.Config().StateDir, "crypto", "master.key")); !os.IsNotExist(err) {
		t.Fatal("reading an absent inventory created or required a master key", err)
	}
}

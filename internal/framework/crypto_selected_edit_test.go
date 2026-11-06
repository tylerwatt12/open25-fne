package framework

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

func TestSSRKeyEditReplacesSelectedPairAndRetainsMaterial(t *testing.T) {
	pair := keyManagementSystem(t)
	old := publicFixtureKey()
	if err := pair.service.writeTrafficKeys([]trafficKey{old}); err != nil {
		t.Fatal(err)
	}
	config, _ := pair.service.Store.Config()
	client := earlyWebLogin(t, pair)
	form := earlyWebForm(t, earlyWebGet(t, client, pair.web.URL+"/keys/edit?row=0", 200), "key_id")
	form.Set("algorithm_id", "129")
	form.Set("key_id", "8")
	form.Set("name", "renamed key")
	form.Set("material", "")
	earlyWebPost(t, client, pair.web.URL+"/keys/edit?row=0", form, pair.web.URL, 303)
	saved, err := pair.service.readTrafficKeys()
	if err != nil {
		t.Fatal(err)
	}
	oldStillPresent, newMaterialRetained := false, false
	for _, key := range saved {
		if key.AlgorithmID == old.AlgorithmID && key.KeyID == old.KeyID {
			oldStillPresent = true
		}
		if key.AlgorithmID == 129 && key.KeyID == 8 {
			newMaterialRetained = key.Material == old.Material
		}
	}
	if len(saved) != 1 || oldStillPresent || !newMaterialRetained {
		t.Fatalf("selected key edit: records=%d oldPairStillPresent=%v newMaterialRetained=%v", len(saved), oldStillPresent, newMaterialRetained)
	}
	after, _ := pair.service.Store.Config()
	if !bytes.Equal(config, after) {
		t.Fatal("key edit rewrote talkgroup references")
	}
}
func TestSSRRecipientEditRevokesSelectedOldIdentity(t *testing.T) {
	pair := keyManagementSystem(t)
	creds, err := readCredentials(pair.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	// RFC7748 public X25519 test value, not private key material.
	public := "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a"
	creds.CryptoRecipients = map[string]string{"dispatch-old": public}
	if err = WritePrivateJSON(pair.service.Config().CredentialFile, creds); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, pair)
	form := earlyWebForm(t, earlyWebGet(t, client, pair.web.URL+"/recipients/edit?row=0", 200), "recipient")
	form.Set("recipient", "dispatch-new")
	earlyWebPost(t, client, pair.web.URL+"/recipients/edit?row=0", form, pair.web.URL, 303)
	saved, err := readCredentials(pair.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	_, oldStillAuthorized := saved.CryptoRecipients["dispatch-old"]
	if len(saved.CryptoRecipients) != 1 || oldStillAuthorized || saved.CryptoRecipients["dispatch-new"] != public {
		t.Fatalf("selected recipient edit: recipients=%d oldStillAuthorized=%v", len(saved.CryptoRecipients), oldStillAuthorized)
	}
}

func TestSSRKeyEditAndDeleteKeepOriginalSelectionAfterReordering(t *testing.T) {
	pair := keyManagementSystem(t)
	old := publicFixtureKey()
	other := old
	other.KeyID = 99
	other.Name = "untouched"
	other.Material = strings.Repeat("AB", 32)
	if err := pair.service.writeTrafficKeys([]trafficKey{old, other}); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, pair)
	form := earlyWebForm(t, earlyWebGet(t, client, pair.web.URL+"/keys/edit?row=0", 200), "key_id")
	if form.Get("resource") != "132/7" {
		t.Fatal("form lost its original resource identity")
	}
	if err := pair.service.writeTrafficKeys([]trafficKey{other, old}); err != nil {
		t.Fatal(err)
	}
	form.Set("key_id", "8")
	form.Set("material", "")
	earlyWebPost(t, client, pair.web.URL+"/keys/edit?row=0", form, pair.web.URL, 303)
	saved, err := pair.service.readTrafficKeys()
	if err != nil || len(saved) != 2 || saved[0] != other || saved[1].KeyID != 8 || saved[1].Material != old.Material {
		t.Fatal("edit retargeted the reordered row or changed another key", err)
	}
	// The full editor's Delete action carries the selected identity too.
	remove := earlyWebForm(t, earlyWebGet(t, client, pair.web.URL+"/keys/edit?row=0", 200), "resource")
	if remove.Get("resource") != "132/99" {
		t.Fatal("delete form lost its original resource identity")
	}
	if err = pair.service.writeTrafficKeys([]trafficKey{saved[1], saved[0]}); err != nil {
		t.Fatal(err)
	}
	remove.Set("action", "delete")
	earlyWebPost(t, client, pair.web.URL+"/keys?row=0", remove, pair.web.URL, 303)
	remaining, err := pair.service.readTrafficKeys()
	if err != nil || len(remaining) != 1 || remaining[0] != saved[1] {
		t.Fatal("delete retargeted the reordered row", err)
	}
}

func TestSSRRecipientEditKeepsOriginalSelectionAndEnteredHexCase(t *testing.T) {
	pair := keyManagementSystem(t)
	creds, err := readCredentials(pair.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	public := strings.ToUpper("8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a")
	creds.CryptoRecipients = map[string]string{"dispatch-old": public}
	if err = WritePrivateJSON(pair.service.Config().CredentialFile, creds); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, pair)
	form := earlyWebForm(t, earlyWebGet(t, client, pair.web.URL+"/recipients/edit?row=0", 200), "recipient")
	if form.Get("resource") != "dispatch-old" {
		t.Fatal("form lost its original recipient")
	}
	creds.CryptoRecipients["dispatch-a"] = public
	if err = WritePrivateJSON(pair.service.Config().CredentialFile, creds); err != nil {
		t.Fatal(err)
	}
	form.Set("recipient", "dispatch-new")
	earlyWebPost(t, client, pair.web.URL+"/recipients/edit?row=0", form, pair.web.URL, 303)
	saved, err := readCredentials(pair.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.CryptoRecipients) != 2 || saved.CryptoRecipients["dispatch-a"] != public || saved.CryptoRecipients["dispatch-new"] != public {
		t.Fatal("recipient edit retargeted a row or normalized entered public-key case")
	}
	if _, old := saved.CryptoRecipients["dispatch-old"]; old {
		t.Fatal("edited old recipient remained authorized")
	}
	api := integrationLogin(t, pair, integrationPassword)
	if status, _ := api.request(t, http.MethodDelete, "/api/crypto/recipients/dispatch-new", nil); status != 200 {
		t.Fatal("selected recipient deletion failed", status)
	}
	remaining, err := readCredentials(pair.service.Config().CredentialFile)
	if err != nil || len(remaining.CryptoRecipients) != 1 || remaining.CryptoRecipients["dispatch-a"] != public {
		t.Fatal("recipient deletion changed another enrollment", err)
	}
}

func TestCryptoMutationsAddressSelectedResources(t *testing.T) {
	pair := keyManagementSystem(t)
	key := publicFixtureKey()
	if err := pair.service.writeTrafficKeys([]trafficKey{key}); err != nil {
		t.Fatal(err)
	}
	client := integrationLogin(t, pair, integrationPassword)
	for _, path := range []string{"/api/crypto/keys", "/api/crypto/recipients"} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			if status, _ := client.request(t, method, path, map[string]any{}); status != 405 {
				t.Fatalf("collection mutation still accepted: %s %s returned %d", method, path, status)
			}
		}
	}
	for path, want := range map[string]int{"/api/crypto/keys/missing/7": 400, "/api/crypto/keys/132/999": 200, "/api/crypto/recipients/missing": 200} {
		if status, _ := client.request(t, http.MethodDelete, path, nil); status != want {
			t.Fatalf("selected resource %s returned %d, want %d", path, status, want)
		}
	}
	saved, err := pair.service.readTrafficKeys()
	if err != nil || len(saved) != 1 || saved[0] != key {
		t.Fatal("rejected resource address changed stored key", err)
	}
}

func TestDispatchEnrollmentStatusShowsOnlyPublicIdentity(t *testing.T) {
	c := integrationNewCluster(t)
	pair := c.dispatch
	client := earlyWebLogin(t, pair)
	private, err := endpointKey(pair.service.Config().StateDir)
	if err != nil {
		t.Fatal(err)
	}
	home := earlyWebGet(t, client, pair.web.URL+"/", 200)
	if bytes.Contains(home, []byte(hex.EncodeToString(private.PublicKey().Bytes()))) {
		t.Fatal("interface directory mixed in enrollment status")
	}
	status := earlyWebGet(t, client, pair.web.URL+"/enrollment-status", 200)
	if !bytes.Contains(status, []byte(hex.EncodeToString(private.PublicKey().Bytes()))) || !bytes.Contains(status, []byte(pair.service.Config().InstanceID)) {
		t.Fatal("Dispatch enrollment status omitted its public identity")
	}
	if bytes.Contains(status, []byte(hex.EncodeToString(private.Bytes()))) || bytes.Contains(home, []byte(hex.EncodeToString(private.Bytes()))) {
		t.Fatal("Dispatch administration exposed private enrollment material")
	}
	if bytes.Contains(status, []byte("http-equiv=refresh")) || bytes.Contains(home, []byte("http-equiv=refresh")) {
		t.Fatal("manual enrollment/directory page became a polling page")
	}
}

func TestSSRKeyEditSavesAfterOriginalIdentityDisappears(t *testing.T) {
	pair := keyManagementSystem(t)
	old := publicFixtureKey()
	other := old
	other.KeyID = 99
	if err := pair.service.writeTrafficKeys([]trafficKey{old}); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, pair)
	form := earlyWebForm(t, earlyWebGet(t, client, pair.web.URL+"/keys/edit?row=0", 200), "key_id")
	if err := pair.service.writeTrafficKeys([]trafficKey{other}); err != nil {
		t.Fatal(err)
	}
	form.Set("name", "submitted after deletion")
	earlyWebPost(t, client, pair.web.URL+"/keys/edit?row=0", form, pair.web.URL, 303)
	saved, err := pair.service.readTrafficKeys()
	if err != nil || len(saved) != 2 || saved[0] != other || saved[1].AlgorithmID != old.AlgorithmID || saved[1].KeyID != old.KeyID || saved[1].Name != "submitted after deletion" || saved[1].Material != "" {
		t.Fatal("submitted key was blocked, retargeted, or given invented material", err)
	}
}

func TestSSRKeyIdentityPreservesExactEnteredInteger(t *testing.T) {
	large := int64(9007199254740993)
	key := publicFixtureKey()
	key.KeyID = int(large)
	if int64(key.KeyID) != large {
		t.Skip("wide integer fixture requires a 64-bit target")
	}
	pair := keyManagementSystem(t)
	if err := pair.service.writeTrafficKeys([]trafficKey{key}); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, pair)
	form := earlyWebForm(t, earlyWebGet(t, client, pair.web.URL+"/keys/edit?row=0", 200), "key_id")
	if form.Get("resource") != "132/9007199254740993" || form.Get("key_id") != "9007199254740993" {
		t.Fatal("entered identity lost integer precision")
	}
	form.Set("name", "wide identity")
	earlyWebPost(t, client, pair.web.URL+"/keys/edit?row=0", form, pair.web.URL, 303)
	saved, err := pair.service.readTrafficKeys()
	if err != nil || len(saved) != 1 || saved[0].KeyID != key.KeyID || saved[0].Material != key.Material {
		t.Fatal("wide identity or key material changed", err)
	}
}

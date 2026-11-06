package framework

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func adminFormResponse(t *testing.T, client *http.Client, address, origin string, form url.Values, status int) []byte {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, address, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("form response HTTP %d; want %d", response.StatusCode, status)
	}
	return body
}

func TestEarlyWebRejectedPasswordStaysOutOfRenderedSnapshot(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	address := c.system.web.URL + "/users/edit"
	form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "username")
	form.Set("id", "pending-user")
	form.Set("username", "pending-username")
	password := "RejectedCredential-" + strings.Repeat("x", 73)
	form.Set("password", password)
	body := adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusOK)
	if !bytes.Contains(body, []byte(bcrypt.ErrPasswordTooLong.Error())) {
		t.Fatal("the credential rejection was not rendered")
	}
	if bytes.Contains(body, []byte(password)) {
		t.Fatal("the rejected password was copied into the response HTML")
	}
	retry := earlyWebForm(t, body, "username")
	snapshot, err := webConfig([]byte(retry.Get("record")))
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := snapshot["password"]; exists || retry.Get("password") != "" || retry.Get("username") != "pending-username" {
		t.Fatal("the retry form retained a secret or lost the entered ordinary fields")
	}
	if _, _, err = c.system.service.Store.User("pending-user"); err == nil {
		t.Fatal("the rejected credential was saved")
	}
}

func TestEarlyWebUserPasswordHasNoAdministratorMinimumLength(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	address := c.system.web.URL + "/users/edit"
	form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "username")
	form.Set("id", "entered-password")
	form.Set("username", "entered-password-user")
	form.Set("password", "x")
	adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusSeeOther)
	_, verifier, err := c.system.service.Store.User("entered-password")
	if err != nil || bcrypt.CompareHashAndPassword(verifier, []byte("x")) != nil {
		t.Fatal("the entered password was refused or changed", err)
	}
}

func TestEarlyWebAdministratorPasswordUsesEnteredValueAndCurrentAuthentication(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	address := c.system.web.URL + "/account"
	form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "new_password")
	_, original, err := c.system.service.Store.Account()
	if err != nil {
		t.Fatal(err)
	}
	form.Set("current_password", "incorrect-password")
	form.Set("new_password", "x")
	body := adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusOK)
	_, unchanged, err := c.system.service.Store.Account()
	if err != nil || !bytes.Equal(original, unchanged) || !bytes.Contains(body, []byte("current password is incorrect")) {
		t.Fatal("incorrect current credentials changed the administrator password", err)
	}
	form.Set("current_password", integrationPassword)
	adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusSeeOther)
	_, saved, err := c.system.service.Store.Account()
	if err != nil || bcrypt.CompareHashAndPassword(saved, []byte("x")) != nil || bcrypt.CompareHashAndPassword(saved, []byte(integrationPassword)) == nil {
		t.Fatal("the entered administrator password was refused or changed", err)
	}
	response, err := client.PostForm(c.system.web.URL+"/login", url.Values{"username": {"administrator"}, "password": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatal("the saved administrator password could not authenticate")
	}
}

func TestEarlyWebLargeEncodedSnapshotPreservesUnknownField(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	raw, err := c.system.service.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	config, err := webConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("%", (2<<20)+4096)
	config["future_large_setting"] = large
	raw = storeDoc(t, config)
	if len(raw) <= 2<<20 || len(raw) >= 4<<20 {
		t.Fatal("the fixture did not exercise a valid document above the old form limit")
	}
	if err = c.system.service.Store.SaveConfig(raw); err != nil {
		t.Fatal(err)
	}
	address := c.system.web.URL + "/system"
	form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "callsign")
	form.Set("callsign", "Submitted large configuration")
	if len(form.Encode()) > 32<<10 {
		t.Fatal("the settings form embedded unrelated configuration")
	}
	adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusSeeOther)
	raw, err = c.system.service.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := webConfig(raw)
	if err != nil || saved["future_large_setting"] != large || saved["callsign"] != "Submitted large configuration" {
		t.Fatal("the large form save failed or changed the unknown field", err)
	}
}

func TestEarlyWebNullEditableSnapshotsSaveWithoutPanic(t *testing.T) {
	t.Run("owned configuration", func(t *testing.T) {
		c := integrationNewCluster(t)
		client := earlyWebLogin(t, c.system)
		address := c.system.web.URL + "/system"
		form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "callsign")
		form.Set("record", "null")
		form.Set("callsign", "Submitted null snapshot")
		adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusSeeOther)
		raw, _ := c.system.service.Store.Config()
		saved, err := webConfig(raw)
		if err != nil || saved["callsign"] != "Submitted null snapshot" {
			t.Fatal("null configuration was not editable", err)
		}
	})
	t.Run("directory record", func(t *testing.T) {
		c := integrationNewCluster(t)
		client := earlyWebLogin(t, c.system)
		address := c.system.web.URL + "/users/edit"
		form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "username")
		form.Set("record", "null")
		form.Set("id", "from-null")
		form.Set("username", "null-record-user")
		form.Set("password", "null-record-password")
		adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusSeeOther)
		saved, _, err := c.system.service.Store.User("from-null")
		if err != nil || saved.Username != "null-record-user" {
			t.Fatal("null directory record was not editable", err)
		}
	})
	t.Run("null collection entry", func(t *testing.T) {
		c := integrationNewCluster(t)
		client := earlyWebLogin(t, c.system)
		raw, _ := c.system.service.Store.Config()
		config, err := webConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		config["radios"] = []any{nil}
		if err = c.system.service.Store.SaveConfig(storeDoc(t, config)); err != nil {
			t.Fatal(err)
		}
		address := c.system.web.URL + "/radios/edit?row=0"
		form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "rid")
		form.Set("rid", "123")
		form.Set("alias", "Entered radio")
		adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusSeeOther)
		raw, _ = c.system.service.Store.Config()
		saved, err := webConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		rows, ok := saved["radios"].([]any)
		if !ok || len(rows) != 1 {
			t.Fatal("the collection was not preserved")
		}
		record, ok := rows[0].(map[string]any)
		if !ok || record["alias"] != "Entered radio" || webText(record["rid"]) != "123" {
			t.Fatal("the null collection entry was not replaced with the entered record")
		}
	})
}

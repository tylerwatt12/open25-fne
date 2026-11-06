package dispatch

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionKeysExposeOnlyPublicIdentities(t *testing.T) {
	h := New(Options{Router: &testRouter{}})
	defer h.Close()
	r := httptest.NewRequest("POST", "http://dispatch.test/api/dispatch/session", strings.NewReader("{}"))
	r.Header.Set("Origin", "http://dispatch.test")
	w := httptest.NewRecorder()
	h.ServeSession(w, r, principal(10001, 1001), "public-key-list", []KeyIdentity{{AlgorithmID: 129, KeyID: 0}})
	var value struct {
		Keys []map[string]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil || w.Code != 201 || len(value.Keys) != 1 {
		t.Fatalf("session keys: %d %s", w.Code, w.Body.String())
	}
	if len(value.Keys[0]) != 2 || string(value.Keys[0]["algorithm_id"]) != "129" || string(value.Keys[0]["key_id"]) != "0" {
		t.Fatal("session exposed fields beyond the loaded public pair")
	}
}

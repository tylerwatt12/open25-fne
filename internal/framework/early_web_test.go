package framework

import (
	"bytes"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestEarlyWebFieldReaderPreservesInconsistentSettings(t *testing.T) {
	fields := []webField{{Name: "mode", Label: "Mode", Kind: "text"}, {Name: "key", Label: "Key ID", Kind: "number"}, {Name: "groups", Label: "Groups", Kind: "numbers"}, {Name: "enabled", Label: "Enabled", Kind: "bool"}}
	record := map[string]any{"mode": "secure", "key": 7, "keep": "untouched"}
	values := url.Values{"mode": {"clear"}, "key": {"999999"}, "groups": {"100, 100, -7"}, "enabled": {"true"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := webReadFields(req, fields, record); err != nil {
		t.Fatal(err)
	}
	if webText(record["key"]) != "999999" || webText(record["groups"]) != "100, 100, -7" || record["keep"] != "untouched" {
		t.Fatalf("values rewritten: %+v", record)
	}
	var form bytes.Buffer
	webFields(&form, fields, record)
	body := form.String()
	for _, token := range []string{"required", "readonly", "disabled", "pattern=", " min=", " max="} {
		if strings.Contains(body, token) {
			t.Fatalf("field gate %s", token)
		}
	}
	if !strings.Contains(body, "type=number") {
		t.Fatal("native numeric input missing")
	}
}
func TestEarlyWebFieldsEscapeSavedText(t *testing.T) {
	var form bytes.Buffer
	webFields(&form, []webField{{Name: "name", Label: "Name", Kind: "text"}}, map[string]any{"name": "\"><script>bad()</script>"})
	if strings.Contains(form.String(), "<script>") || !strings.Contains(form.String(), "&lt;script&gt;") {
		t.Fatal("saved text escaped incorrectly")
	}
}

func TestEarlyWebBlankNumericFieldRemainsBlank(t *testing.T) {
	record := map[string]any{"frequency": 100}
	req := httptest.NewRequest("POST", "/", strings.NewReader("frequency="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := webReadFields(req, []webField{{Name: "frequency", Kind: "number"}}, record); err != nil {
		t.Fatal(err)
	}
	if record["frequency"] != nil {
		t.Fatal("empty entry was replaced with a number")
	}
}

package framework

import (
	"net/http/httptest"
	"open25.local/open25-fne/internal/frameworkui"
	"strings"
	"testing"
)

func TestEarlyWebAssetsRetireThemedApplications(t *testing.T) {
	service := &Service{config: FileConfig{Role: RoleApplication}, assets: frameworkui.Assets()}
	for _, path := range []string{"/style.css", "/forms.js", "/testing.js", "/admin.css", "/admin.js", "/admin-chooser.js", "/login"} {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 || response.Body.Len() == 0 {
			t.Fatalf("current page %s: %d", path, response.Code)
		}
	}
	for _, path := range []string{"/index.html", "/app.js", "/desktop.js", "/desktop.css", "/radio.js", "/radio.css", "/sw.js", "/manifest.webmanifest", "/relay_icons.js", "/network.js"} {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 404 {
			t.Fatalf("retired asset %s: %d", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/login", nil))
	body := response.Body.String()
	if strings.Contains(body, "app.js") || strings.Contains(body, "<dialog") || !strings.Contains(body, "<form method=post action=/login>") {
		t.Fatal("login is not an ordinary HTML form")
	}
}

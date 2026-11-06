package framework

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func smtpFixture(t *testing.T) (smtpProfile, <-chan string, *atomic.Bool) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 20)
	failed := new(atomic.Bool)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				fmt.Fprint(conn, "220 relay\r\n")
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"):
						fmt.Fprint(conn, "250-relay\r\n250 AUTH PLAIN\r\n")
					case strings.HasPrefix(line, "AUTH"):
						fmt.Fprint(conn, "235 authenticated\r\n")
					case strings.HasPrefix(line, "DATA"):
						fmt.Fprint(conn, "354 send\r\n")
						var body strings.Builder
						for {
							line, err = reader.ReadString('\n')
							if err != nil {
								return
							}
							if line == ".\r\n" {
								break
							}
							body.WriteString(line)
						}
						if failed.Load() {
							fmt.Fprint(conn, "550 private-error-do-not-expose\r\n")
						} else {
							received <- body.String()
							fmt.Fprint(conn, "250 accepted\r\n")
						}
					case strings.HasPrefix(line, "QUIT"):
						fmt.Fprint(conn, "221 bye\r\n")
						return
					default:
						fmt.Fprint(conn, "250 ok\r\n")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(port)
	return smtpProfile{Enabled: true, Host: host, Port: number, Authentication: "password", Username: "operator", Password: "parent-password-private", SenderAddress: "fne@example.test", Recipients: "admin@example.test"}, received, failed
}
func TestSMTPInheritedDeliveryAndExplicitRetry(t *testing.T) {
	c := integrationNewCluster(t)
	profile, received, failed := smtpFixture(t)
	if err := c.system.service.Store.smtpDocument("smtp-profile", profile); err != nil {
		t.Fatal(err)
	}
	id, err := c.site.service.queueSMTP()
	if err != nil {
		t.Fatal(err)
	}
	c.site.service.processSMTP(context.Background())
	var d smtpDelivery
	if err = c.site.service.Store.smtpRead("smtp-delivery:"+id, &d); err != nil || d.State != "smtp_accepted" || !strings.HasPrefix(d.Route, "parent → ") {
		t.Fatalf("inherited delivery: %+v %v", d, err)
	}
	select {
	case body := <-received:
		if !strings.Contains(body, "Subject: Open25 SMTP test") || strings.Contains(body, profile.Password) {
			t.Fatal("invalid message or secret disclosure")
		}
	case <-time.After(time.Second):
		t.Fatal("relay received no message")
	}
	if err = c.site.service.retrySMTP(id); err == nil {
		t.Fatal("accepted delivery was requeued")
	}
	failed.Store(true)
	id, err = c.site.service.queueSMTP()
	if err != nil {
		t.Fatal(err)
	}
	c.site.service.processSMTP(context.Background())
	_ = c.site.service.Store.smtpRead("smtp-delivery:"+id, &d)
	if d.State != "failed" || strings.Contains(d.Result, "private-error") {
		t.Fatalf("failure receipt: %+v", d)
	}
	failed.Store(false)
	c.site.service.processSMTP(context.Background())
	select {
	case <-received:
		t.Fatal("failed delivery retried automatically")
	default:
	}
	if err = c.site.service.retrySMTP(id); err != nil {
		t.Fatal(err)
	}
	c.site.service.processSMTP(context.Background())
	_ = c.site.service.Store.smtpRead("smtp-delivery:"+id, &d)
	if d.State != "smtp_accepted" || d.Attempts != 2 {
		t.Fatalf("explicit retry: %+v", d)
	}
	for _, service := range []*Service{c.site.service, c.zone.service} {
		rows, err := service.Store.db.Query("SELECT document FROM framework_documents")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var raw string
			if err = rows.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(raw, profile.Password) {
				t.Fatal("parent secret copied to child")
			}
		}
		rows.Close()
	}
}
func TestSMTPPersistenceAndRuntimeErrors(t *testing.T) {
	profile, received, _ := smtpFixture(t)
	path := filepath.Join(t.TempDir(), "config.db")
	store, err := OpenStore(path, RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.smtpDocument("smtp-profile", profile); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = OpenStore(path, RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := &Service{Store: store, config: FileConfig{Role: RoleSystem}}
	saved, err := service.smtpProfile()
	if err != nil || saved != profile {
		t.Fatal("saved profile did not survive reopen", err)
	}
	profile.Host = "invalid host"
	if err = store.smtpDocument("smtp-profile", profile); err != nil {
		t.Fatal("Save enforced compatibility", err)
	}
	id, err := service.queueSMTP()
	if err != nil {
		t.Fatal(err)
	}
	service.processSMTP(context.Background())
	var d smtpDelivery
	_ = store.smtpRead("smtp-delivery:"+id, &d)
	if d.State != "failed" {
		t.Fatal("invalid runtime falsely succeeded", d)
	}
	select {
	case <-received:
		t.Fatal("unexpected delivery")
	default:
	}
}
func TestSMTPPeerAuthorization(t *testing.T) {
	c := integrationNewCluster(t)
	for _, peer := range []PeerCredential{{Role: RoleSite, InstanceID: "foreign-site"}, {Role: RoleApplication, InstanceID: "foreign-app"}} {
		w := httptest.NewRecorder()
		c.system.service.smtpInternal(w, httptest.NewRequest(http.MethodPost, "/internal/smtp", strings.NewReader(`{}`)), peer)
		if w.Code != 403 {
			t.Fatal("foreign peer accepted", w.Code)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, c.system.server.URL+"/internal/smtp", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 401 {
		t.Fatal("missing credentials accepted")
	}
}

func TestSMTPWebOperationsAndReadOnlyDeliveryPage(t *testing.T) {
	c := integrationNewCluster(t)
	profile, received, _ := smtpFixture(t)
	if err := c.system.service.Store.smtpDocument("smtp-profile", profile); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, c.site)
	page := earlyWebGet(t, client, c.site.web.URL+"/notification-actions", http.StatusOK)
	form := earlyWebForm(t, page, "operation")
	form.Set("operation", "test")
	body := adminFormResponse(t, client, c.site.web.URL+"/notification-actions", c.site.web.URL, form, http.StatusOK)
	if !strings.Contains(string(body), "Test queued:") {
		t.Fatal("explicit operation did not queue")
	}
	deliveries, err := c.site.service.Store.smtpDeliveries()
	if err != nil || len(deliveries) != 1 || deliveries[0].State != "queued" {
		t.Fatal("missing persistent queue", err)
	}
	// Reading the delivery page is not an operation.
	_ = earlyWebGet(t, client, c.site.web.URL+"/notification-delivery", http.StatusOK)
	select {
	case <-received:
		t.Fatal("GET sent email")
	default:
	}
	c.site.service.processSMTP(context.Background())
	body = earlyWebGet(t, client, c.site.web.URL+"/notification-delivery", http.StatusOK)
	if !strings.Contains(string(body), "SMTP accepted") || strings.Contains(string(body), profile.Password) {
		t.Fatal("delivery page omitted actual result or leaked credentials")
	}
}
func TestSMTPInterruptedAttemptRequiresExplicitRetry(t *testing.T) {
	c := integrationNewCluster(t)
	service := c.site.service
	id, err := service.queueSMTP()
	if err != nil {
		t.Fatal(err)
	}
	var d smtpDelivery
	_ = service.Store.smtpRead("smtp-delivery:"+id, &d)
	d.State = "attempting"
	d.Attempts = 1
	if err = service.Store.smtpDocument("smtp-delivery:"+id, d); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.runSMTP(ctx)
	_ = service.Store.smtpRead("smtp-delivery:"+id, &d)
	if d.State != "failed" || d.Attempts != 1 || !strings.Contains(d.Result, "unknown") {
		t.Fatalf("interrupted delivery was replayed or falsely accepted: %+v", d)
	}
}

func TestSMTPParentRequestDeduplicatesAndDoesNotExposeProfile(t *testing.T) {
	c := integrationNewCluster(t)
	profile, received, _ := smtpFixture(t)
	if err := c.system.service.Store.smtpDocument("smtp-profile", profile); err != nil {
		t.Fatal(err)
	}
	request := smtpDelivery{ID: "stable-child-attempt", Subject: "One explicit test", Body: "Test", Hops: 1}
	for range 2 {
		var receipt smtpDelivery
		if err := c.zone.service.upstreamRequest(context.Background(), http.MethodPost, "/internal/smtp", request, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.State != "smtp_accepted" || receipt.Body != "" || receipt.Subject != "" {
			t.Fatalf("bad receipt: %+v", receipt)
		}
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("no message")
	}
	select {
	case <-received:
		t.Fatal("same parent attempt sent twice")
	default:
	}
}
func TestSMTPRetentionPreservesPendingAndUsesExistingLimit(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.system.service
	retention, err := s.activity.Retention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	retention.MaxEvents = 1
	if err = s.activity.SaveRetention(context.Background(), retention); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"failed", "smtp_accepted", "queued", "attempting"} {
		id, err := s.queueSMTP()
		if err != nil {
			t.Fatal(err)
		}
		var d smtpDelivery
		_ = s.Store.smtpRead("smtp-delivery:"+id, &d)
		d.State = state
		if err = s.Store.smtpDocument("smtp-delivery:"+id, d); err != nil {
			t.Fatal(err)
		}
	}
	s.pruneSMTP(context.Background())
	deliveries, err := s.Store.smtpDeliveries()
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 3 {
		t.Fatal("retention failed to bound terminal records", len(deliveries))
	}
	states := map[string]bool{}
	for _, d := range deliveries {
		states[d.State] = true
	}
	if !states["queued"] || !states["attempting"] {
		t.Fatal("retention deleted outstanding work")
	}
}

func TestSMTPWorkerSendsQueuedRequestAndStops(t *testing.T) {
	c := integrationNewCluster(t)
	service := c.system.service
	profile, received, _ := smtpFixture(t)
	if err := service.Store.smtpDocument("smtp-profile", profile); err != nil {
		t.Fatal(err)
	}
	id, err := service.queueSMTP()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); service.runSMTP(ctx) }()
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not process persisted request")
	}
	deadline := time.After(3 * time.Second)
	for {
		var d smtpDelivery
		_ = service.Store.smtpRead("smtp-delivery:"+id, &d)
		if d.State == "smtp_accepted" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not persist SMTP result")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

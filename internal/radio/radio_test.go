package radio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("RADIO_NATIVE_HELPER") == "1" {
		nativeHelper()
		return
	}
	os.Exit(m.Run())
}
func nativeHelper() {
	for _, arg := range os.Args[1:] {
		if arg == "--c4fm-test-check" {
			c4fmNativeCheck()
			return
		}
		if arg == "--c4fm-test" {
			c4fmNativeHelper()
			return
		}
	}
	dir, _ := os.Getwd()
	name := filepath.Base(dir)
	if name == os.Getenv("RADIO_FAIL_CHANNEL") {
		fmt.Fprintln(os.Stderr, "native startup deliberately failed")
		os.Exit(2)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	if name == os.Getenv("RADIO_DELAY_CHANNEL") {
		delay, _ := time.ParseDuration(os.Getenv("RADIO_STARTUP_DELAY"))
		select {
		case <-time.After(delay):
		case <-stop:
			return
		}
	}
	if name == "media" {
		fmt.Println("I: (HOST) FNE is up and running on offline-test")
		<-stop
		return
	}
	config, _ := os.ReadFile("config.yml")
	portMatch := regexp.MustCompile(`restPort: ([0-9]+)`).FindStringSubmatch(string(config))
	secretMatch := regexp.MustCompile(`restPassword: "([a-f0-9]+)"`).FindStringSubmatch(string(config))
	if len(portMatch) < 2 || len(secretMatch) < 2 {
		os.Exit(3)
	}
	sum := sha256.Sum256([]byte(secretMatch[1]))
	auth := hex.EncodeToString(sum[:])
	var policyMu sync.Mutex
	policyRows := initialInhibition(renderedInhibitFixture(string(config)))
	listener, err := net.Listen("tcp", "127.0.0.1:"+portMatch[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "native helper could not listen on %s: %v\n", portMatch[1], err)
		os.Exit(4)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth" {
			var v map[string]string
			_ = json.NewDecoder(r.Body).Decode(&v)
			if v["auth"] != auth {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": 401})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "token": "native-test-token"})
			return
		}
		if r.Header.Get("X-DVM-Auth-Token") != "native-test-token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 401})
			return
		}
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		out := map[string]any{"status": 200}
		switch r.URL.Path {
		case "/status":
			out["p25Enabled"] = true
			out["tx"] = false
			out["lastSrcId"] = 0
			out["lastDstId"] = 0
		case "/p25/report-affiliations":
			out["affiliations"] = []any{}
		case "/p25/site-state":
			out["zoneConnected"] = body["zoneConnected"]
		case "/p25/control-quiesce":
			if _, err := os.Stat("reject-quiesce"); err == nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				out["status"] = http.StatusServiceUnavailable
				out["message"] = "native quiescence deliberately unavailable"
				break
			}
			if _, err := os.Stat("unknown-quiesce"); err == nil {
				break // old/unknown worker answers without explicit confirmation
			}
			enabled := false
			if r.Method == http.MethodPut {
				enabled, _ = body["enabled"].(bool)
				if enabled {
					_ = os.WriteFile("quiesced", []byte("true"), 0600)
				} else {
					_ = os.Remove("quiesced")
				}
			} else {
				_, err := os.Stat("quiesced")
				enabled = err == nil
			}
			_, busy := os.Stat("busy-permit")
			out["quiesced"], out["idle"] = enabled, busy != nil
			if _, err := os.Stat("delayed-quiesce"); err == nil && r.Method == http.MethodPut && enabled {
				out["idle"] = false // real worker needs later protocol-clock observations
			}
		case "/p25/aliases":
			if _, err := os.Stat("reject-aliases"); err == nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				out["status"] = http.StatusServiceUnavailable
				out["message"] = "alias update deliberately failed"
			} else {
				raw, _ := json.Marshal(body)
				if err := os.WriteFile("aliases-applied.json", raw, 0600); err != nil {
					out["status"] = http.StatusServiceUnavailable
				}
			}
		case "/p25/inhibit-policy":
			policyMu.Lock()
			defer policyMu.Unlock()
			if r.Method == "PUT" {
				value, _ := json.Marshal(body)
				var request inhibitRequest
				_ = json.Unmarshal(value, &request)
				desired := []int{}
				for _, row := range request.Radios {
					if row.Inhibited {
						desired = append(desired, row.RadioID)
					}
				}
				restore := inhibitionRestore(policyRows)
				if request.ControlState != nil {
					restore = *request.ControlState
				}
				policyRows = initialInhibition(InhibitPolicy{Desired: desired, Restore: restore})
			}
			out["record_count"], out["record_limit"], out["radios"] = len(policyRows), InhibitRecordLimit, policyRows
		case "/p25/rid":
			out["message"] = "OK"
			if body["command"] == "inhibit" || body["command"] == "uninhibit" {
				policyMu.Lock()
				rid := int(body["dstId"].(float64))
				found := false
				for i := range policyRows {
					if policyRows[i].RadioID == rid {
						found = true
						policyRows[i].Blocked = true
						policyRows[i].PendingUninhibit = body["command"] == "uninhibit"
						policyRows[i].Procedure = "queued"
						policyRows[i].Attempts = 1
					}
				}
				if !found {
					policyRows = append(policyRows, InhibitState{RadioID: rid, Blocked: true, PendingUninhibit: true, Procedure: "queued", Attempts: 1})
				}
				policyMu.Unlock()
			}
		default:
			w.WriteHeader(404)
			out["status"] = 404
		}
		_ = json.NewEncoder(w).Encode(out)
	})}
	go server.Serve(listener)
	fmt.Println("I: (HOST) Host is up and running on offline-test")
	<-stop
	if os.Getenv("RADIO_IGNORE_STOP") == "1" {
		select {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func fixtureProfile() Profile {
	return Profile{System: System{ID: "system-1", Name: "Test", WACN: 1, SystemID: 1, Callsign: "N0TEST", Radios: []Radio{{RID: 10001, Alias: "Portable", Enabled: true, Kind: "subscriber"}, {RID: 10002, Alias: "Console", Enabled: true, Kind: "console"}}, Talkgroups: []Talkgroup{{TGID: 1001, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Test 1", Enabled: true, SiteAccess: "all", SiteIDs: []string{}}, {TGID: 1099, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Denied", Enabled: false, SiteAccess: "all", SiteIDs: []string{}}}}, Zone: Zone{ID: "zone-1", SystemID: "system-1", RFSSID: 1, ChannelIdentifiers: []Identifier{{Identifier: 2, BaseHz: 443437500, SpacingHz: 12500, OffsetHz: 5000000, BandwidthHz: 12500}}, Sites: []SiteIdentity{{ID: "site-1", Name: "Main", SiteID: 1, NAC: 0x293}}}, Site: Site{BroadcastBandPlan: true, ID: "site-1", Modems: []Modem{{ID: "control", Name: "Control modem", Device: "/dev/serial/by-id/control", Baud: 115200, Calibration: Calibration{RXLevel: 50, TXLevel: 50}}, {ID: "voice", Name: "Voice modem", Device: "/dev/serial/by-id/voice", Baud: 115200, Calibration: Calibration{RXLevel: 45, TXLevel: 48, RXOffsetHz: 12, TXOffsetHz: -24}}}, Channels: []Channel{{ID: "control-1", Name: "Control", Role: "control", Identifier: 2, ChannelNo: 1, TXHz: 443450000, RXHz: 448450000, PowerPercent: 95, ModemID: "control", Enabled: true}, {ID: "voice-1", Name: "Voice", Role: "voice", Identifier: 2, ChannelNo: 81, TXHz: 444450000, RXHz: 449450000, PowerPercent: 80, ModemID: "voice", Enabled: true}}, Identification: Identification{Enabled: false, Mode: "ham", WPM: 20, QuietSeconds: 60}}}
}
func freeBase(t *testing.T) int {
	t.Helper()
	for i := 0; i < 100; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		// Keep listening endpoints below the ephemeral client range. Otherwise
		// a readiness request to the first worker can temporarily consume the
		// next worker's not-yet-listening port as its local TCP source port.
		port = 18000 + (port%4000)*2
		ports := []net.Listener{}
		ok := true
		for j := 0; j < 6; j++ {
			v, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+j))
			if e != nil {
				ok = false
				break
			}
			ports = append(ports, v)
		}
		for _, v := range ports {
			v.Close()
		}
		if ok {
			return port
		}
	}
	t.Fatal("no test ports")
	return 0
}
func testController(t *testing.T, events chan Event) *Controller {
	t.Helper()
	t.Setenv("RADIO_NATIVE_HELPER", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{StateDir: t.TempDir(), EnginePath: binary, FNEPath: binary, BaseRPCPort: freeBase(t), StartupTimeout: 3 * time.Second}
	if events != nil {
		opts.OnEvent = func(e Event) {
			select {
			case events <- e:
			default:
			}
		}
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRendererUsesRealChannelIdentityAndSeparatedLocalPorts(t *testing.T) {
	p := fixtureProfile()
	c := testController(t, nil)
	n, err := c.NetworkFor(p)
	if err != nil {
		t.Fatal(err)
	}
	n.RPCSecret = strings.Repeat("b", 64)
	files, err := RenderChannel(p, p.Site.Channels[1], n, 41002, 41003, "hardware")
	if err != nil {
		t.Fatal(err)
	}
	s := files["config.yml"]
	for _, value := range []string{"channelNo: \"51\"", "channelId: 2", "rpcPort: 41002", "restPort: 41003", "rpcPort: 41000", "authoritative: false", "enable: false\n            dedicated: false", "rfPower: 80", "rxTuning: 12", "txTuning: -24", "p25TxLevel: 48.000", "managedSite: true"} {
		if !strings.Contains(s, value) {
			t.Fatalf("missing %q", value)
		}
	}
	if strings.Contains(s, "rpcPassword: "+quoted(n.Secret)) {
		t.Fatal("RPC and media credentials were conflated")
	}
	if !strings.Contains(files["iden_table.dat"], "2,443437500,12.500000,5.000000,12.500000") {
		t.Fatal("channel table units wrong")
	}
	fne, err := RenderFNE(p, n)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fne["fne.yml"], "restrictGrantToAffiliatedOnly: true") {
		t.Fatal("RF affiliation gate lost")
	}
	if !strings.Contains(fne["fne.yml"], "- 100100384") {
		t.Fatal("codec peer not enrolled")
	}
}

func TestMissingExecutablesDoNotClaimAvailableOperation(t *testing.T) {
	c, err := New(Options{StateDir: t.TempDir(), EnginePath: "/missing/native-host", FNEPath: "/missing/native-media"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Status().Available {
		t.Fatal("missing engines reported available")
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err == nil {
		t.Fatal("missing engines accepted startup")
	}
	if !c.Status().StartedAt.IsZero() || c.Status().FNEPID != 0 {
		t.Fatal("missing engines began a native run")
	}
}

func TestProfileRejectsUnrepresentablePairsAndSharedModemBeforeStart(t *testing.T) {
	cases := []func(*Profile){func(p *Profile) { p.Site.Channels[1].RXHz++ }, func(p *Profile) { p.Site.Channels[1].ChannelNo = 1 }, func(p *Profile) { p.Site.Channels[1].ModemID = "control" }, func(p *Profile) { p.Zone.ChannelIdentifiers[0].OffsetHz++ }, func(p *Profile) { p.Site.Identification.WPM = 19 }, func(p *Profile) { p.System.WACN = 0xFFFFF }, func(p *Profile) { p.Site.Channels[1].ID = ".." }, func(p *Profile) { p.System.Radios[0].Kind = "" }}
	for _, mutate := range cases {
		p := fixtureProfile()
		mutate(&p)
		if Validate(p) == nil {
			t.Fatal("invalid hardware profile accepted")
		}
	}
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "hardware"); err == nil {
		t.Fatal("RF permission was bypassed")
	}
	if c.Status().FNEPID != 0 {
		t.Fatal("hardware rejection launched media")
	}
}

func TestLocalEnginesSurviveZoneOutageAndStopWithoutLeaks(t *testing.T) {
	c := testController(t, nil)
	p := fixtureProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	if before.State != "running" || !before.FNEReady || len(before.Channels) != 2 {
		t.Fatalf("not ready: %+v", before)
	}
	c.SetZoneConnected(true)
	if c.Status().TrunkingMode != "wide_area" {
		t.Fatal("missing connected state")
	}
	c.SetZoneConnected(false)
	after := c.Status()
	if after.TrunkingMode != "site_trunking" || after.FNEPID != before.FNEPID {
		t.Fatal("Zone outage replaced local media")
	}
	for i := range before.Channels {
		if after.Channels[i].PID != before.Channels[i].PID {
			t.Fatal("Zone outage replaced RF process")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	status := c.Status()
	if status.State != "stopped" || status.FNEPID != 0 {
		t.Fatalf("not stopped: %+v", status)
	}
	for _, ch := range before.Channels {
		if syscall.Kill(ch.PID, 0) == nil {
			t.Fatal("radio child survived stop")
		}
	}
	if syscall.Kill(before.FNEPID, 0) == nil {
		t.Fatal("local FNE survived stop")
	}
}

func TestVoiceRestartAllowsNativeWarmupLongerThanStatusWatchdog(t *testing.T) {
	c := testController(t, nil)
	c.opts.StartupTimeout = 9 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := c.Start(ctx, fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	if err := c.StopChannel(ctx, "voice-1"); err != nil {
		t.Fatal(err)
	}
	// Real DVM startup includes modem initialization and a 15-second DSP
	// warmup. A six-second offline helper reproduces the watchdog boundary.
	t.Setenv("RADIO_DELAY_CHANNEL", "voice-1")
	t.Setenv("RADIO_STARTUP_DELAY", "6s")
	if err := c.StartChannel(ctx, "voice-1"); err != nil {
		t.Fatalf("voice startup was interrupted during warmup: %v", err)
	}
	for _, ch := range c.Status().Channels {
		if ch.ID == "voice-1" && (!ch.Ready || ch.State != "ready") {
			t.Fatalf("voice did not return to service: %+v", ch)
		}
	}
}

func TestAccessChangeWithdrawsThenRestartsWithNewPolicy(t *testing.T) {
	events := make(chan Event, 100)
	c := testController(t, events)
	p := fixtureProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	old := c.Status()
	alias := fixtureProfile()
	alias.System.Radios[0].Alias = "New name"
	if err := c.ApplyPolicy(context.Background(), alias); err != nil {
		t.Fatal(err)
	}
	if c.Status().Channels[0].PID != old.Channels[0].PID {
		t.Fatal("alias-only edit restarted RF")
	}
	p.System.Radios[0].Enabled = false
	if err := c.ApplyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	current := c.Status()
	if current.State != "running" || current.Channels[0].PID == old.Channels[0].PID {
		t.Fatal("changed access did not restart native engines")
	}
	if syscall.Kill(old.Channels[0].PID, 0) == nil {
		t.Fatal("old allowed engine survived policy withdrawal")
	}
	deadline := time.After(time.Second)
	seen := false
	for !seen {
		select {
		case e := <-events:
			seen = e.Kind == "policy_applying"
		case <-deadline:
			t.Fatal("policy transition not reported")
		}
	}
}

func TestCallerMutationCannotBypassPolicyWithdrawal(t *testing.T) {
	c := testController(t, nil)
	p := fixtureProfile()
	p.System.Radios = append(p.System.Radios, Radio{RID: 10003, Alias: "Other subscriber", Kind: "subscriber", Enabled: true})
	p.System.Talkgroups[0].AffiliationAccess = "selected"
	p.System.Talkgroups[0].AffiliationRadioIDs = []int{10001}
	p.System.Talkgroups[0].TransmitAccess = "selected"
	p.System.Talkgroups[0].TransmitRadioIDs = []int{10001}
	p.System.Talkgroups[0].SiteAccess = "selected"
	p.System.Talkgroups[0].SiteIDs = []string{"site-1"}
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	// Editing the caller's form value must not rewrite the operating snapshot.
	p.System.Radios[0].Enabled = false
	p.System.Talkgroups[0].SiteIDs[0] = "another-site"
	p.System.Talkgroups[0].AffiliationRadioIDs[0] = 10003
	p.System.Talkgroups[0].TransmitRadioIDs[0] = 10003
	c.mu.Lock()
	stillAllowed := c.profile.System.Radios[0].Enabled && c.profile.System.Talkgroups[0].SiteIDs[0] == "site-1" && c.profile.System.Talkgroups[0].AffiliationRadioIDs[0] == 10001 && c.profile.System.Talkgroups[0].TransmitRadioIDs[0] == 10001
	c.mu.Unlock()
	if !stillAllowed {
		t.Fatal("caller mutation rewrote the Controller's operating policy")
	}
	if err := c.ApplyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if after := c.Status(); after.State != "running" || after.FNEPID == before.FNEPID {
		t.Fatal("edited caller value bypassed native policy replacement")
	}
	if syscall.Kill(before.Channels[0].PID, 0) == nil {
		t.Fatal("previously allowed engine survived policy replacement")
	}
}

func TestFailedStartupReapsOtherChildrenAndReleasesOwnerLock(t *testing.T) {
	t.Setenv("RADIO_FAIL_CHANNEL", "control-1")
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err == nil {
		t.Fatal("failed CC accepted")
	}
	status := c.Status()
	if status.State != "fault" || status.FNEPID != 0 {
		t.Fatalf("startup failure not contained: %+v", status)
	}
	for _, ch := range status.Channels {
		if ch.PID != 0 {
			t.Fatal("startup left native child")
		}
	}
	if _, err := New(c.opts); err == nil {
		t.Fatal("duplicate Site radio owner accepted")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := New(c.opts)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

func TestRFInterestNeverCountsConsoleIdentities(t *testing.T) {
	c := testController(t, nil)
	c.profile = fixtureProfile()
	c.status.InhibitionReady = true
	c.affiliations[10002] = Affiliation{RID: 10002, TGID: 1001}
	if c.HasRFInterest(1001) {
		t.Fatal("console counted as physical RF listener")
	}
	c.affiliations[10001] = Affiliation{RID: 10001, TGID: 1001}
	if !c.HasRFInterest(1001) {
		t.Fatal("accepted physical affiliation ignored")
	}
	c.profile.System.Radios[0].Enabled = false
	if c.HasRFInterest(1001) {
		t.Fatal("denied RID counted as listener")
	}
}

func TestNativeClientReauthenticatesKnownRejectionButNeverRepeatsUnknownGrant(t *testing.T) {
	var auths, grants atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			auths.Add(1)
			fmt.Fprint(w, `{"status":200,"token":"ok"}`)
			return
		}
		if r.URL.Path == "/status" {
			if auths.Load() == 1 {
				fmt.Fprint(w, `{"status":401}`)
			} else {
				fmt.Fprint(w, `{"status":200,"tx":false}`)
			}
			return
		}
		grants.Add(1)
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, `{"status":200,"accepted":true}`)
	}))
	defer server.Close()
	client, err := NewNativeClient(strings.TrimPrefix(server.URL, "http://"), "private-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Get(context.Background(), "/status"); err != nil {
		t.Fatal(err)
	}
	if auths.Load() != 2 {
		t.Fatal("legacy body401 not recognized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err = client.DispatchGrant(ctx, map[string]any{"srcId": 10002, "dstId": 1001})
	if err == nil || grants.Load() != 1 {
		t.Fatal("unknown grant outcome retried")
	}
}

func TestNativeAffiliationSnapshotSupportsLargeSubscriberTables(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			fmt.Fprint(w, `{"status":200,"token":"ok"}`)
			return
		}
		rows := make([]map[string]int, 600)
		for i := range rows {
			rows[i] = map[string]int{"srcId": 10001 + i, "grpId": 1001}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "affiliations": rows})
	}))
	defer server.Close()
	client, err := NewNativeClient(strings.TrimPrefix(server.URL, "http://"), "private-test")
	if err != nil {
		t.Fatal(err)
	}
	data, err := client.Get(context.Background(), "/p25/report-affiliations")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]int
	if json.Unmarshal(data["affiliations"], &rows) != nil || len(rows) != 600 {
		t.Fatal("valid native affiliation snapshot was truncated")
	}
}

func TestAcceptedResponseRefreshesRoamingTimeWithoutDuplicateTableEvent(t *testing.T) {
	events := make(chan Event, 20)
	c := testController(t, events)
	c.mu.Lock()
	c.profile = fixtureProfile()
	c.status.State = "running"
	c.mu.Unlock()
	source := currentObservedChild(t, c, "control-1", "control")
	response := "I: (RF) P25, TSDU, IOSP_GRP_AFF (Group Affiliation Response), anncId = 0, srcId = 10001, dstId = 1001"
	c.observe(source, response)
	first := c.Affiliations()[0].At
	c.observe(source, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	time.Sleep(2 * time.Millisecond)
	c.observe(source, response)
	second := c.Affiliations()[0].At
	if !second.After(first) {
		t.Fatal("genuine reacceptance did not refresh roaming time")
	}
	c.observe(source, "W: (RF) P25, IOSP_GRP_AFF (Group Affiliation Response) denial, RID rejection, srcId = 10001, dstId = 1001")
	if !c.Affiliations()[0].At.Equal(second) {
		t.Fatal("denied request refreshed accepted presence")
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	accepted, denied := 0, 0
	for accepted < 2 || denied < 1 {
		select {
		case event := <-events:
			if event.Kind == "affiliated" {
				accepted++
			}
			if event.Kind == "request_denied" {
				denied++
			}
		case <-deadline.C:
			t.Fatal("accepted/denied observations missing")
		}
	}
	select {
	case event := <-events:
		if event.Kind == "affiliated" {
			t.Fatal("response and table update duplicated a transaction")
		}
	default:
	}
}

func TestAcceptedEventsDoNotConfuseRequestsWithAffiliations(t *testing.T) {
	requests := []string{"I: (RF) P25, ISP_U_REG (Unit Registration Request), srcId = 10001", "I: (RF) P25, ISP_GRP_AFF (Group Affiliation Request), srcId = 10001, dstId = 1001"}
	for _, line := range requests {
		if e := ParseEvent(line); e != nil && (e.Kind == "registered" || e.Kind == "affiliated") {
			t.Fatal("request became accepted state")
		}
	}
	e := ParseEvent("I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	if e == nil || e.Kind != "affiliated" || e.RID != 10001 || e.TGID != 1001 {
		t.Fatal("accepted table observation missing")
	}
	e = ParseEvent("I: (RF) P25, ISP_GRP_AFF_Q_RSP (Group Affiliation Query Response), srcId = 10001, dstId = 1002, anncId = 0")
	if e == nil || e.Kind != "affiliated" || e.RID != 10001 || e.TGID != 1002 {
		t.Fatal("validated affiliation query confirmation missing")
	}
	e = ParseEvent("I: (RF) P25, ISP_MSG_UPDT (Message Update Request), message = $0001, srcId = 10001, dstId = 16777212")
	if e == nil || e.Kind != "message" || e.Code == nil || *e.Code != 1 {
		t.Fatal("message ID not preserved")
	}
}

func TestVoiceChannelRestartDoesNotReplaceControlOrMedia(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	if err := c.StopChannel(context.Background(), "control-1"); err == nil {
		t.Fatal("ordinary channel operation stopped the control channel")
	}
	c.mu.Lock()
	c.status.Channels[1].Busy = true
	c.mu.Unlock()
	if err := c.StopChannel(context.Background(), "voice-1"); err == nil {
		t.Fatal("busy voice channel was withdrawn")
	}
	c.mu.Lock()
	c.status.Channels[1].Busy = false
	c.mu.Unlock()
	if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	stopped := c.Status()
	if stopped.State != "running" || stopped.Channels[0].PID != before.Channels[0].PID || stopped.FNEPID != before.FNEPID || stopped.Channels[1].PID != 0 {
		t.Fatalf("voice stop changed unrelated resources: %+v", stopped)
	}
	if syscall.Kill(before.Channels[1].PID, 0) == nil {
		t.Fatal("withdrawn voice process survived")
	}
	if err := c.StartChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	after := c.Status()
	if after.Channels[1].PID == 0 || after.Channels[1].PID == before.Channels[1].PID || !after.Channels[1].Ready || after.Channels[0].PID != before.Channels[0].PID || after.FNEPID != before.FNEPID {
		t.Fatal("voice channel did not recover independently")
	}
}

func TestLateNativeResponseCannotRestoreStoppedOrReplacementChannel(t *testing.T) {
	c := testController(t, nil)
	old := &child{id: "voice-1", role: "voice", done: make(chan struct{})}
	current := &child{id: "voice-1", role: "voice", done: make(chan struct{})}
	c.mu.Lock()
	c.status.State = "running"
	c.status.Channels = []ChannelStatus{{ID: "voice-1", Role: "voice", State: "starting"}}
	c.children["voice-1"] = current
	c.mu.Unlock()
	c.applyNativeStatus(old, map[string]json.RawMessage{"tx": json.RawMessage(`true`)})
	c.nativeExited(old)
	if status := c.Status(); status.Channels[0].Ready || status.Channels[0].State != "starting" {
		t.Fatal("old process response changed its replacement")
	}
	current.mu.Lock()
	current.intentional = true
	current.mu.Unlock()
	c.applyNativeStatus(current, map[string]json.RawMessage{"tx": json.RawMessage(`true`)})
	if c.Status().Channels[0].Ready {
		t.Fatal("late response restored an intentionally stopping channel")
	}
	c.mu.Lock()
	c.children = map[string]*child{}
	c.status.State = "stopped"
	c.mu.Unlock()
	c.applyNativeStatus(current, map[string]json.RawMessage{"tx": json.RawMessage(`true`)})
	if c.Status().Channels[0].Ready {
		t.Fatal("late response restored stopped channel")
	}
}

func TestPolicyRevocationWithPendingRFEditStopsOldAllows(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	p := fixtureProfile()
	p.System.Radios[0].Enabled = false
	p.Site.Channels[1].PowerPercent = 60
	if err := c.ApplyPolicy(context.Background(), p); err == nil {
		t.Fatal("pending physical change was applied automatically")
	}
	if c.Status().State != "stopped" || syscall.Kill(before.Channels[0].PID, 0) == nil {
		t.Fatal("old permitted policy remained live")
	}
}

func TestVoiceFailureCanRecoverWithoutControlRestart(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	_ = syscall.Kill(before.Channels[1].PID, syscall.SIGKILL)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && c.Status().Channels[1].State != "fault" {
		time.Sleep(10 * time.Millisecond)
	}
	if c.Status().Channels[1].State != "fault" || c.Status().State != "running" {
		t.Fatal("voice failure was not contained")
	}
	if err := c.StartChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	if c.Status().Channels[0].PID != before.Channels[0].PID || c.Status().FNEPID != before.FNEPID {
		t.Fatal("voice recovery restarted unrelated processes")
	}
}

func TestFaultRestartReapsEveryPreviousResourceBeforeReplacement(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	// A fault is reported before its asynchronous whole-Site withdrawal can
	// acquire the operation mutex. An immediate administrator restart must
	// still reap that run's surviving voice and media processes.
	c.opMu.Lock()
	c.mu.Lock()
	oldControl := c.children["control-1"]
	c.mu.Unlock()
	c.nativeExited(oldControl)
	err := c.start(context.Background(), fixtureProfile(), "null")
	c.opMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	for _, pid := range []int{before.FNEPID, before.Channels[0].PID, before.Channels[1].PID} {
		if syscall.Kill(pid, 0) == nil {
			t.Fatal("previous faulted run retained a live child")
		}
	}
	if c.Status().State != "running" {
		t.Fatal("replacement did not start")
	}
}

func TestRemovingEverySiteTalkgroupWithdrawsStaleAllows(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	p := fixtureProfile()
	for i := range p.System.Talkgroups {
		p.System.Talkgroups[i].SiteAccess = "selected"
		p.System.Talkgroups[i].SiteIDs = []string{}
	}
	if err := c.ApplyPolicy(context.Background(), p); err == nil {
		t.Fatal("native engine accepted an empty rules list")
	}
	if c.Status().State != "stopped" || syscall.Kill(before.Channels[0].PID, 0) == nil {
		t.Fatal("last-talkgroup removal left the previous permitted engine live")
	}
}

func TestRendererSiteIdentifierBroadcastSwitchRetainsFrequencyTable(t *testing.T) {
	p := fixtureProfile()
	c := testController(t, nil)
	network, err := c.NetworkFor(p)
	if err != nil {
		t.Fatal(err)
	}
	network.RPCSecret = strings.Repeat("b", 64)
	lookups := RenderLookups(p)
	for _, enabled := range []bool{true, false} {
		p.Site.BroadcastBandPlan = enabled
		files, err := RenderChannel(p, p.Site.Channels[0], network, 41000, 41001, "null")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(files["config.yml"], fmt.Sprintf("broadcastIdentifiers: %t", enabled)) {
			t.Fatal("Site switch was not sent to native owner")
		}
		if files["iden_table.dat"] != lookups["iden_table.dat"] {
			t.Fatal("advertisement policy removed frequency definitions")
		}
	}
}

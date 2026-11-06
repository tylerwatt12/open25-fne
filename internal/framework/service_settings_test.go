package framework

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSettingsSaveDoesNotBlockLiveVoice(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	call, q := sipRFCall(t, c, "settings-save-live-voice", false, false)
	s := c.zone.service
	settings := s.Config().Settings()
	settings.PollSeconds++
	conn, err := s.Store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	saved := make(chan error, 1)
	go func() { saved <- s.SaveSettings(settings) }()
	defer func() {
		_ = conn.Close()
		select {
		case err := <-saved:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("settings save did not finish after storage was released")
		}
	}()
	// The file commit precedes the event write waiting on the reserved DB
	// connection. Configuration readers and media must stay independent of it.
	mediaWait(t, time.Second, func() bool {
		cfg, err := ReadFileConfig(c.zone.configPath)
		return err == nil && cfg.PollSeconds == settings.PollSeconds
	}, "settings file commit")
	select {
	case <-saved:
		t.Fatal("settings save did not wait for the reserved storage connection")
	default:
	}
	config := make(chan FileConfig, 1)
	go func() { config <- s.Config() }()
	select {
	case cfg := <-config:
		if cfg.PollSeconds != settings.PollSeconds {
			t.Fatal("committed settings were not published")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("storage blocked the configuration used by live voice")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := call.sip.Renew(ctx); err != nil {
		t.Fatal("storage interrupted the voice lease", err)
	}
	sipWriteBurst(t, call, q, 1, 9)
	for i, frame := range sipActualFrames(t, c, "site-2", 9) {
		want := sipRFFrame(call, q, 1, uint64(i))
		if !bytes.Equal(frame.Encoded, want.Encoded) || frame.SourceID != want.SourceID || frame.TalkgroupID != want.TalkgroupID {
			t.Fatal("live voice changed while settings storage was blocked")
		}
	}
}

func TestSettingsSavePreservesEnteredAddressesAndPolling(t *testing.T) {
	s := &Service{Store: storeOpen(t, RoleZone), config: FileConfig{Role: RoleZone}, configPath: filepath.Join(t.TempDir(), "service.json")}
	entered := Settings{UpstreamURL: "unreachable address", PublicURL: "", InternalURL: "https://internal.invalid/path", PollSeconds: -3}
	if err := s.SaveSettings(entered); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored FileConfig
	if err = DecodeStrict(raw, &stored); err != nil || stored.Settings() != entered || s.Config().Settings() != entered {
		t.Fatal("entered connection settings changed", err)
	}
}

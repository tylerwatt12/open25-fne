//go:build linux

package dispatch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

// TestNativeCodecContinuousPCM is deliberately opt-in. It runs the real bundled
// codec and a private loopback FNE, without starting a host or opening a modem.
// A mock cannot reproduce a native watchdog ending a continuously fed call.
func TestNativeCodecContinuousPCM(t *testing.T) {
	native := os.Getenv("TRUNKSYSTEM_NATIVE_DIR")
	if native == "" {
		t.Skip("set TRUNKSYSTEM_NATIVE_DIR to a reviewed native payload")
	}
	native, err := filepath.Abs(native)
	if err != nil {
		t.Fatal(err)
	}
	duration := 8 * time.Second
	if value := os.Getenv("TRUNKSYSTEM_NATIVE_TEST_SECONDS"); value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 8 || seconds > 30 {
			t.Fatal("native qualification duration must be 8-30 seconds")
		}
		duration = time.Duration(seconds) * time.Second
	}
	state := t.TempDir()
	if evidence := os.Getenv("TRUNKSYSTEM_NATIVE_EVIDENCE"); evidence != "" {
		if err := os.MkdirAll(evidence, 0700); err != nil {
			t.Fatal(err)
		}
		state, err = os.MkdirTemp(evidence, "codec-")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("private native evidence: %s", state)
	}
	p := nativeCodecProfile()
	reserved, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := reserved.LocalAddr().(*net.UDPAddr).Port
	reserved.Close()
	controller, err := radio.New(radio.Options{StateDir: filepath.Join(state, "controller"), EnginePath: filepath.Join(native, "dvmhost"), FNEPath: filepath.Join(native, "dvmfne"), FNEPort: port, AllowRF: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	network, err := controller.NetworkFor(p)
	if err != nil {
		t.Fatal(err)
	}
	files, err := radio.RenderFNE(p, network)
	if err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(state, "media")
	if err = os.Mkdir(media, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range files {
		if err = os.WriteFile(filepath.Join(media, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(state, "fne.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	command := exec.Command(filepath.Join(native, "dvmfne"), "-c", "fne.yml")
	command.Dir, command.Stdout, command.Stderr = media, log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-done
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		text, _ := os.ReadFile(logPath)
		if strings.Contains(string(text), "FNE is up and running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("private FNE did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	codec, err := NewCodec(CodecOptions{Binary: filepath.Join(native, "dvmbridge"), StateDir: filepath.Join(state, "codec"), Identity: "Native codec qualification", MasterAddress: network.Address, MasterPort: network.Port, MasterPassword: network.Secret, PeerIDs: map[int]int{1001: network.ConsolePeerIDs[1001]}, RadioID: 10002, WACN: 1, SystemID: 1, StartupTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = codec.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), duration+25*time.Second)
	defer cancel()
	if err = codec.Start(ctx); err != nil {
		t.Fatal(err)
	}
	codecLog := filepath.Join(codec.runDir, "talkgroup-1001.log")
	frames := int(duration / FrameDuration)
	ticker := time.NewTicker(FrameDuration)
	defer ticker.Stop()
	last := time.Now()
	largest := time.Duration(0)
	for index := 0; index < frames; index++ {
		<-ticker.C
		now := time.Now()
		largest = max(largest, now.Sub(last))
		last = now
		pcm := make([]byte, FrameBytes)
		for sample := 0; sample < FrameBytes/2; sample++ {
			value := int16(6000 * math.Sin(2*math.Pi*700*float64(index*160+sample)/8000))
			binary.LittleEndian.PutUint16(pcm[sample*2:], uint16(value))
		}
		if err = codec.Write(ctx, Frame{CallID: "native-continuous-pcm", SourceID: 10002, TalkgroupID: 1001, Sequence: uint64(index + 1), PCM: pcm, CapturedAt: now}); err != nil {
			t.Fatal(err)
		}
		if index%5 == 0 {
			text, _ := os.ReadFile(codecLog)
			if strings.Contains(string(text), "UDP Traffic, call end,") {
				t.Fatalf("native codec ended while PCM was still arriving: frame %d/%d, maximum producer gap %v", index+1, frames, largest)
			}
		}
	}
	if largest > 100*time.Millisecond {
		t.Fatalf("producer could not maintain continuous PCM: largest gap %v", largest)
	}
	if err = codec.Drain(ctx, "native-continuous-pcm", []int{1001}); err != nil {
		t.Fatal(err)
	}
	drainedAt := time.Now()
	deadline = time.Now().Add(time.Second)
	var text []byte
	for {
		text, _ = os.ReadFile(codecLog)
		if strings.Contains(string(text), "UDP Traffic, call end,") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native codec did not end after PCM stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if starts, ends := strings.Count(string(text), "UDP Traffic, call start,"), strings.Count(string(text), "UDP Traffic, call end,"); starts != 1 || ends != 1 {
		t.Fatalf("native call boundary differs: %d starts, %d ends", starts, ends)
	}
	var endedAt time.Time
	for _, line := range strings.Split(string(text), "\n") {
		if strings.Contains(line, "UDP Traffic, call end,") && len(line) > 26 {
			endedAt, err = time.ParseInLocation("2006-01-02 15:04:05.000", line[3:26], time.Local)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if endedAt.IsZero() || endedAt.Before(last.Add(-2*time.Millisecond)) {
		t.Fatal("native end occurred before the last PCM frame")
	}
	if endedAt.After(drainedAt.Add(2 * time.Millisecond)) {
		t.Fatalf("Drain returned before the native tail finished: end lag %v", endedAt.Sub(drainedAt))
	}
	result := map[string]any{"frames": frames, "continuous_seconds": duration.Seconds(), "max_producer_gap_ms": float64(largest) / float64(time.Millisecond), "last_pcm_at": last, "native_end_at": endedAt, "drain_returned_at": drainedAt, "native_tail_ms": float64(endedAt.Sub(last)) / float64(time.Millisecond), "one_start_and_end": true, "drain_includes_native_end": true, "hardware_or_rf_access": false}
	proof, _ := json.MarshalIndent(result, "", "  ")
	if err = os.WriteFile(filepath.Join(state, "qualification.json"), append(proof, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d real PCM frames, %s continuous call, maximum producer gap %s; native tail %s included in Drain; one start/end, no RF hardware", frames, duration, largest, endedAt.Sub(last))
}

func nativeCodecProfile() radio.Profile {
	return radio.Profile{
		System: radio.System{ID: "codec-test-system", WACN: 1, SystemID: 1, Callsign: "N0TEST", Radios: []radio.Radio{{RID: 10001, Kind: "subscriber", Enabled: true}, {RID: 10002, Kind: "console", Enabled: true}}, Talkgroups: []radio.Talkgroup{{TGID: 1001, Alias: "Test1", Enabled: true, SiteAccess: "all"}}},
		Zone:   radio.Zone{ID: "codec-test-zone", SystemID: "codec-test-system", RFSSID: 1, ChannelIdentifiers: []radio.Identifier{{Identifier: 2, BaseHz: 443437500, SpacingHz: 12500, OffsetHz: 5000000, BandwidthHz: 12500}}, Sites: []radio.SiteIdentity{{ID: "codec-test-site", SiteID: 99, NAC: 659}}},
		Site:   radio.Site{ID: "codec-test-site", Modems: []radio.Modem{{ID: "control", Device: "/no-hardware/control", Baud: 115200}, {ID: "voice", Device: "/no-hardware/voice", Baud: 115200}}, Channels: []radio.Channel{{ID: "control", Role: "control", Identifier: 2, ChannelNo: 1, TXHz: 443450000, RXHz: 448450000, PowerPercent: 1, ModemID: "control", Enabled: true}, {ID: "voice", Role: "voice", Identifier: 2, ChannelNo: 81, TXHz: 444450000, RXHz: 449450000, PowerPercent: 1, ModemID: "voice", Enabled: true}}, Identification: radio.Identification{Mode: "ham", WPM: 20, QuietSeconds: 60}},
	}
}

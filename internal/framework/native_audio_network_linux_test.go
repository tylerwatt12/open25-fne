//go:build linux

package framework

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
)

type nativeAudioRound struct {
	Direction           string               `json:"direction"`
	RequestedSecure     bool                 `json:"requested_secure"`
	Secure              bool                 `json:"secure"`
	AlgorithmID         int                  `json:"algorithm_id"`
	KeyID               int                  `json:"key_id"`
	SelectedKey         dispatch.KeyIdentity `json:"selected_key"`
	TransmitAlgorithmID int                  `json:"transmit_algorithm_id"`
	TransmitKeyID       int                  `json:"transmit_key_id"`
	TransmitMetadata    bool                 `json:"transmit_metadata_correct"`
	Sent                int                  `json:"sent_rtp_frames"`
	Received            int                  `json:"received_rtp_frames"`
	PeakHz              int                  `json:"peak_hz"`
	RMS                 float64              `json:"rms"`
	SecurityMetadata    bool                 `json:"security_metadata_correct"`
}

func nativeQualificationTone(index int) []byte {
	pcm := make([]byte, dispatch.FrameBytes)
	for i := 0; i < dispatch.FrameSamples; i++ {
		sample := int16(5000 * math.Sin(2*math.Pi*700*float64(index*dispatch.FrameSamples+i)/dispatch.SampleRate))
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(sample))
	}
	return dispatch.PCMToMuLaw(pcm)
}
func nativeQualificationSpectrum(audio []byte) (int, float64) {
	pcm := dispatch.MuLawToPCM(audio)
	samples := make([]float64, len(pcm)/2)
	var rms float64
	for i := range samples {
		samples[i] = float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
		rms += samples[i] * samples[i]
	}
	if len(samples) == 0 {
		return 0, 0
	}
	rms = math.Sqrt(rms / float64(len(samples)))
	peak, power := 0, float64(0)
	for hz := 100; hz <= 2000; hz += 25 {
		coefficient := 2 * math.Cos(2*math.Pi*float64(hz)/8000)
		a, b := 0.0, 0.0
		for _, v := range samples {
			next := v + coefficient*a - b
			b, a = a, next
		}
		p := a*a + b*b - coefficient*a*b
		if p > power {
			peak, power = hz, p
		}
	}
	return peak, rms
}
func nativeBrowserAudioRound(t *testing.T, tx, rx *mediaBrowser, talkgroup, algorithmID, keyID int, requestedSecure, secure bool, selected dispatch.KeyIdentity, direction string, index int) nativeAudioRound {
	t.Helper()
	count := nativeTestFrameCount(t)
	if !secure {
		algorithmID, keyID = 0x80, 0 // Received native metadata retains the P25 clear ALGID.
	}
	for {
		select {
		case <-rx.audio:
		default:
			goto audioEmpty
		}
	}
audioEmpty:
	for {
		select {
		case <-rx.security:
		default:
			goto metadataEmpty
		}
	}
metadataEmpty:
	request := strings.ReplaceAll(direction, "-", "") + strconv.Itoa(index)
	if e := tx.send(map[string]any{"type": "ptt_start", "request_id": request, "talkgroups": []int{talkgroup}, "secure": requestedSecure, "algorithm_id": selected.AlgorithmID, "key_id": selected.KeyID}); e != nil {
		t.Fatal(e)
	}
	accepted := tx.waitState(t, func(s dispatch.State) bool { return s.TX.State == "transmitting" && s.TX.RequestID == request }, "live QA grant")
	transmitAlgorithm, transmitKey := 0, 0
	if secure {
		transmitAlgorithm, transmitKey = algorithmID, keyID
	}
	transmitMetadata := accepted.TX.Secure == secure && accepted.TX.AlgorithmID == transmitAlgorithm && accepted.TX.KeyID == transmitKey
	if !transmitMetadata {
		t.Fatalf("accepted transmit metadata did not match resolved mode: requested secure=%v, expected secure=%v ALGID=%d KID=%d; state=%+v", requestedSecure, secure, transmitAlgorithm, transmitKey, accepted.TX)
	}
	received := make(chan []byte, 1)
	stop := make(chan struct{})
	go func() {
		out := []byte{}
		for {
			select {
			case a := <-rx.audio:
				if a.stream == "tg-"+strconv.Itoa(talkgroup) {
					out = append(out, a.payload...)
				}
			case <-stop:
				received <- out
				return
			}
		}
	}()
	ticker := time.NewTicker(dispatch.FrameDuration)
	defer ticker.Stop()
	for i := 0; i < count; i++ {
		<-ticker.C
		if i%10 == 0 {
			if e := tx.send(map[string]any{"type": "ptt_renew", "request_id": request}); e != nil {
				t.Fatal(e)
			}
		}
		// A browser RTP clock keeps advancing while its microphone is muted.
		// Do not make later calls look like delayed audio by pausing the clock.
		if tx.rtpEpoch.IsZero() {
			tx.rtpEpoch = time.Now()
		}
		if i == 0 {
			tx.stamp = uint32(time.Since(tx.rtpEpoch) * dispatch.SampleRate / time.Second)
		}
		packet := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 123456, SequenceNumber: tx.seq, Timestamp: tx.stamp}, Payload: nativeQualificationTone(i)}
		tx.seq++
		tx.stamp += dispatch.FrameSamples
		if e := tx.mic.WriteRTP(packet); e != nil {
			t.Fatal(e)
		}
	}
	// Release promptly while source media is still fresh. The route's normal
	// End drains the native encoder tail before releasing its call segment.
	if e := tx.send(map[string]any{"type": "ptt_stop", "request_id": request}); e != nil {
		t.Fatal(e)
	}
	tx.waitState(t, func(s dispatch.State) bool { return s.TX.State == "idle" }, "live QA release")
	time.Sleep(300 * time.Millisecond)
	close(stop)
	audio := <-received
	peak, rms := nativeQualificationSpectrum(audio)
	metadata := false
	for {
		select {
		case m := <-rx.security:
			if m.Active && m.TalkgroupID == talkgroup && m.Encrypted == secure && ((secure && m.KeyID == keyID && m.AlgorithmID == algorithmID) || (!secure && m.KeyID == 0 && m.AlgorithmID == algorithmID)) {
				metadata = true
			}
		default:
			goto done
		}
	}
done:
	round := nativeAudioRound{Direction: direction, RequestedSecure: requestedSecure, Secure: secure, AlgorithmID: algorithmID, KeyID: keyID, SelectedKey: selected, TransmitAlgorithmID: accepted.TX.AlgorithmID, TransmitKeyID: accepted.TX.KeyID, TransmitMetadata: transmitMetadata, Sent: count, Received: len(audio) / dispatch.FrameSamples, PeakHz: peak, RMS: rms, SecurityMetadata: metadata}
	if round.Received < count || peak < 650 || peak > 750 || rms < 100 || !metadata {
		t.Fatalf("live native audio qualification failed: %+v", round)
	}
	return round
}

// Actual browser WebRTC -> networkRouter -> native IMBE crypto -> authenticated
// Zone media -> native decrypt/vocoder -> receiver WebRTC. No RF engine runs.
func TestNativeBrowserClearAndTrafficAlgorithmsNetworkAudio(t *testing.T) {
	binary := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	if binary == "" {
		t.Skip("set reviewed native packet codec executable")
	}
	c := newMediaCluster(t)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, node := range []*integrationService{c.dispatch, c.zone} {
			if node.service.activity != nil {
				events, _ := node.service.activity.Events(activity.Query{Limit: 20})
				for _, event := range events {
					t.Logf("%s %s: %s", node.service.Config().Role, event.Type, event.Message)
				}
			}
		}
	})
	var system SystemConfig
	if e := c.system.service.owned(&system); e != nil {
		t.Fatal(e)
	}
	for i := range system.Talkgroups {
		if system.Talkgroups[i].TGID == 1001 {
			system.Talkgroups[i].AdmissionMode = "both"
			system.Talkgroups[i].TransmitMode = "selectable"
			system.Talkgroups[i].KeyID = 65000
			system.Talkgroups[i].AlgorithmID = 132
		}
	}
	if e := c.system.service.Store.SaveConfig(storeDoc(t, system)); e != nil {
		t.Fatal(e)
	}
	for i := range c.users {
		c.users[i].Permissions = append(c.users[i].Permissions, "secure_ptt")
		if e := c.system.service.Store.SaveUser(c.users[i].ID, c.users[i], "media-integration-password"); e != nil {
			t.Fatal(e)
		}
	}
	private, e := endpointKey(c.dispatch.service.Config().StateDir)
	if e != nil {
		t.Fatal(e)
	}
	creds, e := readCredentials(c.system.service.Config().CredentialFile)
	if e != nil {
		t.Fatal(e)
	}
	creds.CryptoRecipients = map[string]string{c.dispatch.service.Config().InstanceID: hex.EncodeToString(private.PublicKey().Bytes())}
	if e = WritePrivateJSON(c.system.service.Config().CredentialFile, creds); e != nil {
		t.Fatal(e)
	}
	pinFixtureSystem(t, c.system.service, c.dispatch.service)
	key := publicFixtureKey()
	key.KeyID = 65000
	key.AlgorithmID = 132
	if e = c.system.service.writeTrafficKeys([]trafficKey{key, {AlgorithmID: 132, KeyID: 0, Name: "Alternate AES network fixture", Enabled: true, Material: strings.Repeat("fedcba9876543210", 4)}, {AlgorithmID: 129, KeyID: 65000, Name: "DES network fixture", Enabled: true, Material: "0123456789abcdef"}, {AlgorithmID: 170, KeyID: 65000, Name: "ADP network fixture", Enabled: true, Material: "0123456789"}, {AlgorithmID: 131, KeyID: 65000, Name: "Published three-key TDEA network fixture", Enabled: true, Material: "0123456789ABCDEF23456789ABCDEF01456789ABCDEF0123"}, {AlgorithmID: 131, KeyID: 0, Name: "Different public TDEA KID0", Enabled: true, Material: "456789ABCDEF012323456789ABCDEF010123456789ABCDEF"}}); e != nil {
		t.Fatal(e)
	}
	integrationSyncAll(t, c.integrationCluster)
	c.dispatch.service.mu.Lock()
	c.dispatch.service.config.Media.CodecBinary = binary
	c.dispatch.service.mu.Unlock()
	if e = c.dispatch.service.reconcileCrypto(); e != nil {
		t.Fatal(e)
	}
	tx, rx := newMediaBrowser(t, c.dispatch, c.users[0]), newMediaBrowser(t, c.dispatch, c.users[1])
	for i, secure := range []bool{false, true} {
		algorithm := 0
		if secure {
			algorithm = 132
		}
		round := nativeBrowserAudioRound(t, tx, rx, 1001, algorithm, 65000, secure, secure, dispatch.KeyIdentity{}, "native-network", i)
		t.Logf("secure=%v: %d sent, %d received, peak=%dHz, security metadata=%v", secure, round.Sent, round.Received, round.PeakHz, round.SecurityMetadata)
	}
	selected := dispatch.KeyIdentity{AlgorithmID: 132, KeyID: 0}
	if !slices.Contains(tx.keys, selected) || !slices.Contains(rx.keys, selected) {
		t.Fatal("session omitted the actually loaded alternate KID0")
	}
	for index, endpoints := range [][2]*mediaBrowser{{tx, rx}, {rx, tx}} {
		round := nativeBrowserAudioRound(t, endpoints[0], endpoints[1], 1001, 132, 0, true, true, selected, "native-operator-key", index)
		t.Logf("operator ALGID=%d KID=%d, group assigned KID65000: %d sent, %d received, peak=%dHz, TX metadata=%v RX metadata=%v", round.TransmitAlgorithmID, round.TransmitKeyID, round.Sent, round.Received, round.PeakHz, round.TransmitMetadata, round.SecurityMetadata)
	}
	// The receiving endpoint has just used its chosen encrypted key. The next
	// clear call must still play with its actual clear indication.
	clear := nativeBrowserAudioRound(t, tx, rx, 1001, 0, 0, false, false, selected, "native-clear-after-selected-key", 0)
	t.Logf("clear receive after operator-selected encrypted transmit: %d sent, %d received, peak=%dHz, TX metadata=%v RX metadata=%v", clear.Sent, clear.Received, clear.PeakHz, clear.TransmitMetadata, clear.SecurityMetadata)
	for index, mode := range []string{"secure", "clear"} {
		for i := range system.Talkgroups {
			if system.Talkgroups[i].TGID == 1001 {
				system.Talkgroups[i].TransmitMode = mode
			}
		}
		if e = c.system.service.Store.SaveConfig(storeDoc(t, system)); e != nil {
			t.Fatal(e)
		}
		integrationSyncAll(t, c.integrationCluster)
		secure := mode == "secure"
		round := nativeBrowserAudioRound(t, tx, rx, 1001, 132, 65000, !secure, secure, dispatch.KeyIdentity{}, "native-strapped-"+mode, index+2)
		t.Logf("strapping=%s requested secure=%v actual secure=%v: %d sent, %d received, peak=%dHz, TX metadata=%v RX metadata=%v", mode, !secure, secure, round.Sent, round.Received, round.PeakHz, round.TransmitMetadata, round.SecurityMetadata)
	}
	for index, algorithm := range []int{129, 170, 131} {
		for i := range system.Talkgroups {
			if system.Talkgroups[i].TGID == 1001 {
				system.Talkgroups[i].AlgorithmID = algorithm
				system.Talkgroups[i].TransmitMode = "selectable"
			}
		}
		if e = c.system.service.Store.SaveConfig(storeDoc(t, system)); e != nil {
			t.Fatal(e)
		}
		integrationSyncAll(t, c.integrationCluster)
		if algorithm == 131 {
			c.affiliations(t, true)
		}
		round := nativeBrowserAudioRound(t, tx, rx, 1001, algorithm, 65000, true, true, dispatch.KeyIdentity{}, "native-multiple-algorithms", index+4)
		if algorithm == 131 {
			assertTDEASiteWords(t, c, round)
		}
		t.Logf("algorithm=%02x: %d sent, %d received, peak=%dHz, security metadata=%v", algorithm, round.Sent, round.Received, round.PeakHz, round.SecurityMetadata)
	}
	selectedTDEA := dispatch.KeyIdentity{AlgorithmID: 131, KeyID: 0}
	if !slices.Contains(tx.keys, selectedTDEA) || !slices.Contains(rx.keys, selectedTDEA) {
		t.Fatal("session omitted loaded TDEA KID0")
	}
	for index, endpoints := range [][2]*mediaBrowser{{tx, rx}, {rx, tx}} {
		round := nativeBrowserAudioRound(t, endpoints[0], endpoints[1], 1001, 131, 0, true, true, selectedTDEA, "native-tdea-selected-key", index)
		assertTDEASiteWords(t, c, round)
		t.Logf("TDEA selected KID0 direction%d: %d sent, %d received, peak=%dHz, TX metadata=%v RX metadata=%v", index, round.Sent, round.Received, round.PeakHz, round.TransmitMetadata, round.SecurityMetadata)
	}
	codec, _ := c.dispatch.service.packetCodec()
	if codec == nil || !codec.Alive() {
		t.Fatal("native codec did not survive all complete calls")
	}
	_ = codec.End(context.Background(), "native-regression-end")
}

// Site endpoints consume opaque FullRate words; only Dispatch owns the keys.
// These are real authenticated network routes with fake final RF endpoints.
func assertTDEASiteWords(t *testing.T, c *mediaCluster, round nativeAudioRound) {
	t.Helper()
	count := (round.Sent + 8) / 9 * 9 // Native Finish completes the last nine-word LDU.
	first := sipActualFrames(t, c, "site-1", count)
	second := sipActualFrames(t, c, "site-2", count)
	for i := range first {
		for _, frame := range []dispatch.Frame{first[i], second[i]} {
			if !frame.Encrypted || frame.AlgorithmID != 131 || int(frame.KeyID) != round.KeyID || len(frame.Encoded) != 11 || len(frame.PCM) != 0 || frame.VoiceIndex != uint8(i%18) {
				t.Fatalf("Site TDEA word%d changed public identity/ciphertext shape: %+v", i, frame)
			}
		}
		if !slices.Equal(first[i].Encoded, second[i].Encoded) {
			t.Fatalf("Sites received different TDEA ciphertext at word%d", i)
		}
	}
	mediaWait(t, time.Second, func() bool {
		return len(c.zone.service.broker.router.Active()) == 0 && len(c.site.service.broker.router.Active()) == 0 && len(c.otherSite.service.broker.router.Active()) == 0
	}, "TDEA three-leg allocation retirement")
	t.Logf("Both software Sites received%d opaque TDEA words with ALGID131/KID%d; no PCM or modem transmission", count, round.KeyID)
}

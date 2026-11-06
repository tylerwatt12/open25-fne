package dispatch

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"open25.local/open25-fne/internal/nativearchive"
)

type CodecOptions struct {
	Binary            string
	StateDir          string
	Identity          string
	MasterAddress     string
	MasterPort        int
	MasterPassword    string
	PeerIDs           map[int]int
	RadioID           int
	WACN              int
	SystemID          int
	VoiceMFID         int
	Opaque            bool
	StartupTimeout    time.Duration
	OnFrame           func(Frame)
	OnReceiveActivity func(CodecReceiveActivity)
	OnEvent           func(CodecEvent)
}

type CodecEvent struct {
	At          time.Time `json:"at"`
	TalkgroupID int       `json:"talkgroup_id"`
	State       string    `json:"state"`
	Message     string    `json:"message"`
}
type CodecStatus struct {
	TalkgroupID int    `json:"talkgroup_id"`
	Ready       bool   `json:"ready"`
	PID         int    `json:"pid"`
	Error       string `json:"error,omitempty"`
}

type codecResource struct {
	tg         int
	read       *net.UDPConn
	write      *net.UDPConn
	command    *exec.Cmd
	output     *os.File
	done       chan struct{}
	ready      bool      // codec.mu
	error      string    // codec.mu
	lastCall   string    // codec.mu
	lastSource int       // codec.mu
	lastWrite  time.Time // codec.mu
	ended      bool      // codec.mu; an explicit native End was written for lastCall
}

// Codec is a software-only P25 IMBE endpoint attached to a Site's local FNE.
// It has no modem path and never grants an RF resource. Native source IDs are
// supplied per PCM packet; one bridge may serve several authorized operators.
type Codec struct {
	mu              sync.Mutex
	options         CodecOptions
	ctx             context.Context
	cancel          context.CancelFunc
	resources       map[int]*codecResource
	runDir          string
	started, closed bool
	wg              sync.WaitGroup
	frames          chan codecReceiveUpdate
	events          chan CodecEvent
	closeOnce       sync.Once
	closeError      error
	archive         *nativearchive.Writer
	archiveReaders  int
}

func NewCodec(options CodecOptions) (*Codec, error) {
	if options.VoiceMFID != 0 && options.VoiceMFID != 1 {
		return nil, errors.New("standard voice MFID must be 00 or 01")
	}
	if options.Binary == "" || !filepath.IsAbs(options.Binary) || options.StateDir == "" || options.Identity == "" || strings.ContainsAny(options.Identity, "\r\n") {
		return nil, errors.New("codec binary, state directory and identity are required")
	}
	info, err := os.Stat(options.Binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, errors.New("native codec executable is unavailable")
	}
	ip := net.ParseIP(options.MasterAddress)
	if ip == nil || !ip.IsLoopback() || options.MasterPort < 1 || options.MasterPort > 65535 || len(options.MasterPassword) < 32 {
		return nil, errors.New("codec requires the Site's private loopback FNE assignment")
	}
	if options.RadioID < 1 || options.RadioID > 0xfffffb || options.WACN < 1 || options.WACN > 0xffffe || options.SystemID < 1 || options.SystemID > 0xffe {
		return nil, errors.New("codec radio or system identity is invalid")
	}
	groups := []int{}
	peers := map[int]bool{}
	copyPeers := map[int]int{}
	for tg, peer := range options.PeerIDs {
		groups = append(groups, tg)
		if peer < 1 || peer > 999999999 || peers[peer] {
			return nil, errors.New("codec peer IDs must be unique")
		}
		peers[peer] = true
		copyPeers[tg] = peer
	}
	if len(groups) == 0 {
		return nil, errors.New("configure at least one codec talkgroup")
	}
	if _, err = groupList(groups); err != nil {
		return nil, err
	}
	options.PeerIDs = copyPeers
	if options.StartupTimeout <= 0 {
		options.StartupTimeout = 15 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Codec{options: options, ctx: ctx, cancel: cancel, resources: map[int]*codecResource{}, frames: make(chan codecReceiveUpdate, 64), events: make(chan CodecEvent, 64)}
	return c, nil
}

func (c *Codec) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.started || c.closed {
		c.mu.Unlock()
		return errors.New("codec is already started or closed")
	}
	c.started = true
	c.mu.Unlock()
	if err := os.MkdirAll(c.options.StateDir, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(c.options.StateDir, "codec-")
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("codec was closed during startup")
	}
	c.runDir = dir
	c.archive = nativearchive.New()
	c.wg.Add(2)
	c.mu.Unlock()
	go c.publishFrames()
	go c.publishEvents()
	fail := func(err error) error { _ = c.Close(); return err }
	groups := []int{}
	for tg := range c.options.PeerIDs {
		groups = append(groups, tg)
	}
	sort.Ints(groups)
	for _, tg := range groups {
		reader, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return fail(err)
		}
		reserve, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			reader.Close()
			return fail(err)
		}
		writePort := reserve.LocalAddr().(*net.UDPAddr).Port
		writer, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: writePort})
		if err != nil {
			reader.Close()
			reserve.Close()
			return fail(err)
		}
		resource := &codecResource{tg: tg, read: reader, write: writer, done: make(chan struct{})}
		closeFDs := func() { reader.Close(); writer.Close(); reserve.Close() }
		config := RenderCodecConfig(c.options, tg, writePort, reader.LocalAddr().(*net.UDPAddr).Port)
		path := filepath.Join(dir, fmt.Sprintf("talkgroup-%d.yml", tg))
		if err = os.WriteFile(path, []byte(config), 0600); err != nil {
			closeFDs()
			return fail(err)
		}
		logPath := filepath.Join(dir, fmt.Sprintf("talkgroup-%d.log", tg))
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			closeFDs()
			return fail(err)
		}
		// Validate/create the archive before native startup; operational scanners never write it.
		if err = log.Close(); err != nil {
			closeFDs()
			return fail(err)
		}
		output, pipe, err := os.Pipe()
		if err != nil {
			closeFDs()
			return fail(err)
		}
		command := exec.Command(c.options.Binary, "-f", "-c", path)
		command.Dir = dir
		command.SysProcAttr = codecProcessAttributes()
		command.Stdout = pipe
		command.Stderr = pipe
		resource.command, resource.output = command, output
		reserve.Close()
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			pipe.Close()
			output.Close()
			closeFDs()
			return fail(errors.New("codec was closed during startup"))
		}
		if err = command.Start(); err != nil {
			c.mu.Unlock()
			pipe.Close()
			output.Close()
			closeFDs()
			return fail(errors.New("native codec could not start"))
		}
		c.resources[tg] = resource
		c.archiveReaders++
		c.wg.Add(3)
		c.mu.Unlock()
		pipe.Close()
		go c.scanOutput(resource, output, logPath)
		go func(r *codecResource) {
			defer c.wg.Done()
			err := r.command.Wait()
			c.mu.Lock()
			r.ready = false
			if !c.closed {
				r.error = "Native codec stopped."
				if err != nil {
					r.error = "Native codec process exited."
				}
			}
			message := r.error
			c.mu.Unlock()
			close(r.done)
			c.notify(r.tg, "stopped", message)
		}(resource)
		go c.receive(resource)
	}
	startup, cancel := context.WithTimeout(ctx, c.options.StartupTimeout)
	defer cancel()
	timer := time.NewTicker(25 * time.Millisecond)
	defer timer.Stop()
	for {
		ready := true
		for _, status := range c.Status() {
			if status.Error != "" {
				return fail(errors.New(status.Error))
			}
			ready = ready && status.Ready
		}
		if ready {
			return nil
		}
		select {
		case <-startup.Done():
			return fail(errors.New("native codec did not connect to the Site network"))
		case <-c.ctx.Done():
			return fail(errors.New("native codec was stopped"))
		case <-timer.C:
		}
	}
}

func (c *Codec) scanOutput(resource *codecResource, reader *os.File, logPath string) {
	defer c.wg.Done()
	defer reader.Close()
	defer func() {
		c.mu.Lock()
		archive := c.archive
		if archive != nil && c.archiveReaders > 0 {
			c.archiveReaders--
		}
		finished := c.closed && c.archiveReaders == 0
		c.mu.Unlock()
		if finished && archive != nil {
			archive.Stop()
		}
	}()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 65536)
	for scanner.Scan() {
		line := scanner.Text()
		c.observeLog(resource, line)
		if c.archive != nil {
			c.archive.Copy(logPath, nativearchive.Redact(line, c.options.MasterPassword), false)
		}
	}
}

// Observation remains on the scanner; only its diagnostic file copy is queued.
func (c *Codec) observeLog(resource *codecResource, line string) {
	lower := strings.ToLower(line)
	state := ""
	c.mu.Lock()
	stopped := c.closed
	select {
	case <-resource.done:
		stopped = true
	default:
	}
	if stopped {
		c.mu.Unlock()
		return
	}
	if strings.Contains(lower, "logged into the master successfully") {
		if !resource.ready {
			state = "ready"
		}
		resource.ready = true
		resource.error = ""
	}
	if strings.Contains(lower, "connection to the master has timed out") || strings.Contains(lower, "master nak") || strings.Contains(lower, "master closing down") {
		if resource.ready {
			state = "disconnected"
		}
		resource.ready = false
		resource.error = "Native codec lost its Site network connection."
	}
	c.mu.Unlock()
	if state != "" {
		c.notify(resource.tg, state, "")
	}
}

func (c *Codec) DiagnosticArchiveStatus() nativearchive.Status {
	c.mu.Lock()
	archive := c.archive
	c.mu.Unlock()
	if archive == nil {
		return nativearchive.Status{}
	}
	return archive.Status()
}

func (c *Codec) Status() []CodecStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []CodecStatus{}
	for tg, r := range c.resources {
		pid := 0
		if r.command != nil && r.command.Process != nil {
			pid = r.command.Process.Pid
		}
		out = append(out, CodecStatus{tg, r.ready, pid, r.error})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TalkgroupID < out[j].TalkgroupID })
	return out
}

func EncodeCodecPacket(frame Frame) ([]byte, error) {
	if len(frame.Encoded) > 0 || frame.End {
		return EncodeOpaqueCodecPacket(frame)
	}
	if len(frame.PCM) != FrameBytes || frame.TalkgroupID < 1 || frame.TalkgroupID > 65534 || frame.SourceID < 1 || frame.SourceID > 0xfffffb || frame.Encrypted {
		return nil, errors.New("codec requires a clear 20 ms PCM frame with valid source and group")
	}
	out := make([]byte, FrameBytes+12)
	binary.BigEndian.PutUint32(out, FrameBytes)
	copy(out[4:], frame.PCM)
	binary.BigEndian.PutUint32(out[FrameBytes+4:], uint32(frame.TalkgroupID))
	binary.BigEndian.PutUint32(out[FrameBytes+8:], uint32(frame.SourceID))
	return out, nil
}
func DecodeCodecPacket(packet []byte) (Frame, error) {
	if len(packet) >= 4 && string(packet[:4]) == "DV25" {
		return DecodeOpaqueCodecPacket(packet)
	}
	if len(packet) != FrameBytes+12 || binary.BigEndian.Uint32(packet) != FrameBytes {
		return Frame{}, errors.New("invalid native PCM packet")
	}
	frame := Frame{TalkgroupID: int(binary.BigEndian.Uint32(packet[FrameBytes+4:])), SourceID: int(binary.BigEndian.Uint32(packet[FrameBytes+8:])), PCM: append([]byte(nil), packet[4:FrameBytes+4]...), CapturedAt: time.Now()}
	if frame.TalkgroupID < 1 || frame.TalkgroupID > 65534 || frame.SourceID < 1 || frame.SourceID > 0xfffffb {
		return Frame{}, errors.New("invalid native PCM identity")
	}
	return frame, nil
}

func (c *Codec) Write(ctx context.Context, frame Frame) error {
	if c.options.Opaque && !frame.End && len(frame.Encoded) != 11 {
		return errors.New("Site codec requires encoded P25 media")
	}
	packet, err := EncodeCodecPacket(frame)
	if err != nil {
		return err
	}
	if frame.CallID == "" {
		return errors.New("codec frame requires a call identity")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if !frame.CapturedAt.IsZero() && time.Since(frame.CapturedAt) > 250*time.Millisecond {
		return errors.New("codec audio is stale")
	}
	c.mu.Lock()
	r := c.resources[frame.TalkgroupID]
	if c.closed || r == nil || !r.ready {
		c.mu.Unlock()
		return ErrUnavailable
	}
	if frame.End && (r.lastCall != frame.CallID || r.lastSource != frame.SourceID) {
		c.mu.Unlock()
		return ErrPermission
	}
	if r.lastCall == frame.CallID && r.ended {
		c.mu.Unlock()
		if frame.End {
			return nil
		}
		return ErrUnavailable
	}
	if r.lastCall != "" && r.lastCall != frame.CallID && !r.ended && time.Since(r.lastWrite) < 220*time.Millisecond {
		c.mu.Unlock()
		return ErrBusy
	}
	if r.lastCall == frame.CallID && r.lastSource != 0 && r.lastSource != frame.SourceID {
		c.mu.Unlock()
		return errors.New("codec call source identity changed")
	}
	r.lastCall = frame.CallID
	r.lastSource = frame.SourceID
	r.lastWrite = time.Now()
	r.ended = false
	c.mu.Unlock()
	deadline := time.Now().Add(20 * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = r.write.SetWriteDeadline(deadline)
	if n, err := r.write.Write(packet); err != nil || n != len(packet) {
		return errors.New("native codec audio write failed")
	}
	if frame.End {
		c.mu.Lock()
		if r.lastCall == frame.CallID {
			r.ended = true
		}
		c.mu.Unlock()
	}
	return nil
}

// Drain allows the native codec's bounded 180 ms voice tail to finish before
// its matching RF grant is released. It never sends additional audio or PTT.
func (c *Codec) Drain(ctx context.Context, callID string, groups []int) error {
	for {
		c.mu.Lock()
		wait := time.Duration(0)
		for _, tg := range groups {
			if r := c.resources[tg]; r != nil && r.lastCall == callID {
				wait = max(wait, time.Until(r.lastWrite.Add(220*time.Millisecond)))
			}
		}
		c.mu.Unlock()
		if wait <= 0 {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-c.ctx.Done():
			timer.Stop()
			return ErrUnavailable
		case <-timer.C:
		}
	}
	c.mu.Lock()
	for _, tg := range groups {
		if r := c.resources[tg]; r != nil && r.lastCall == callID {
			r.lastCall = ""
		}
	}
	c.mu.Unlock()
	return nil
}

func (c *Codec) receive(r *codecResource) {
	defer c.wg.Done()
	buffer := make([]byte, 2048)
	call := ""
	source := 0
	sequence := uint64(0)
	last := time.Time{}
	opaque := opaqueReceiveState{}
	endPCM := func() {
		if call != "" {
			c.queueFrame(Frame{CallID: call, TalkgroupID: r.tg, SourceID: source, End: true, CapturedAt: time.Now()})
			call = ""
		}
	}
	expireOpaque := func(now time.Time) {
		if ended, ok := opaque.expire(now); ok {
			c.queueFrame(ended)
			c.notify(r.tg, "receive_liveness_expired", "The native receive stream stopped reporting activity.")
		}
	}
	for {
		_ = r.read.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, peer, err := r.read.ReadFromUDP(buffer)
		now := time.Now()
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			expireOpaque(now)
			if !last.IsZero() && now.Sub(last) > 350*time.Millisecond {
				endPCM()
				last = time.Time{}
			}
			continue
		}
		expireOpaque(now)
		// Unrelated or malformed traffic must not hold a PCM burst open.
		if !last.IsZero() && now.Sub(last) > 350*time.Millisecond {
			endPCM()
			last = time.Time{}
		}
		if !peer.IP.IsLoopback() {
			continue
		}
		c.mu.Lock()
		ownSource, ownTime := r.lastSource, r.lastWrite
		c.mu.Unlock()
		packet := buffer[:n]
		if len(packet) >= 5 && string(packet[:4]) == "DV25" && packet[4] == 17 {
			a, err := decodeOpaqueLiveness(packet)
			if err != nil || a.TalkgroupID != r.tg || (a.SourceID == ownSource && now.Sub(ownTime) < 400*time.Millisecond) {
				continue
			}
			if activity, ok := opaque.activity(a, now); ok {
				c.queueReceiveUpdate(codecReceiveUpdate{activity: &activity})
			}
			continue
		}
		frame, err := DecodeCodecPacket(packet)
		if err != nil || frame.TalkgroupID != r.tg {
			continue
		}
		if frame.SourceID == ownSource && now.Sub(ownTime) < 400*time.Millisecond {
			continue
		}
		if len(frame.Encoded) > 0 || frame.End {
			endPCM()
			frames, err := opaque.voice(frame, now)
			for _, f := range frames {
				c.queueFrame(f)
			}
			if err != nil {
				c.notify(r.tg, "audio_loss", "A new receive stream could not be identified.")
			}
			continue
		}
		if c.options.Opaque {
			continue
		}
		if ended, ok := opaque.end(now, true); ok {
			c.queueFrame(ended)
		}
		if call == "" || frame.SourceID != source {
			endPCM()
			call, err = randomID()
			if err != nil {
				c.notify(r.tg, "audio_loss", "A new receive stream could not be identified.")
				continue
			}
			source, sequence = frame.SourceID, 0
		}
		last = now
		sequence++
		frame.CallID, frame.Sequence = call, sequence
		c.queueFrame(frame)
	}
}

func (c *Codec) queueFrame(frame Frame) {
	frame.P25 = frame.P25.Clone()
	c.queueReceiveUpdate(codecReceiveUpdate{frame: frame})
}
func (c *Codec) queueReceiveUpdate(update codecReceiveUpdate) {
	select {
	case <-c.ctx.Done():
		return
	case c.frames <- update:
	default:
		tg := update.frame.TalkgroupID
		if update.activity != nil {
			tg = update.activity.TalkgroupID
		}
		c.notify(tg, "audio_loss", "Audio routing could not keep up; stale audio was discarded.")
	}
}
func (c *Codec) publishFrames() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case update := <-c.frames:
			if update.activity != nil {
				if time.Now().Before(update.activity.ValidUntil) && c.options.OnReceiveActivity != nil {
					c.options.OnReceiveActivity(*update.activity)
				}
				continue
			}
			frame := update.frame
			if !frame.End && time.Since(frame.CapturedAt) > 250*time.Millisecond {
				continue
			}
			if c.options.OnFrame != nil {
				c.options.OnFrame(frame)
			}
		}
	}
}
func (c *Codec) notify(tg int, state, message string) {
	select {
	case <-c.ctx.Done():
		return
	case c.events <- CodecEvent{time.Now().UTC(), tg, state, message}:
	default:
	}
}
func (c *Codec) publishEvents() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case event := <-c.events:
			if c.options.OnEvent != nil {
				c.options.OnEvent(event)
			}
		}
	}
}

func (c *Codec) Close() error {
	return c.closeWithTimeouts(2*time.Second, time.Second)
}

func (c *Codec) closeWithTimeouts(grace, forced time.Duration) error {
	c.closeOnce.Do(func() {
		started := time.Now()
		graceDeadline := started.Add(grace)
		stopDeadline := graceDeadline.Add(forced)
		c.mu.Lock()
		c.closed = true
		resources := []*codecResource{}
		for _, r := range c.resources {
			r.ready = false
			resources = append(resources, r)
		}
		archive, stopArchive := c.archive, c.archiveReaders == 0
		c.mu.Unlock()
		if archive != nil {
			if stopArchive {
				archive.Stop()
			}
			defer func() {
				ctx, cancel := context.WithDeadline(context.Background(), stopDeadline)
				defer cancel()
				// Archive drain is reported separately from operational child/worker shutdown.
				_ = archive.Close(ctx)
			}()
		}
		c.cancel()
		for _, r := range resources {
			r.read.Close()
			r.write.Close()
			if r.output != nil {
				// A descendant may retain the native process's stdout pipe.
				// The scanner must not keep shutdown waiting for that writer.
				_ = r.output.Close()
			}
			if r.command != nil && r.command.Process != nil && !codecDone(r.done) {
				signalCodecProcess(r.command.Process, syscall.SIGTERM)
			}
		}
		pending := []*codecResource{}
		for _, r := range resources {
			if r.command == nil || r.command.Process == nil {
				continue
			}
			if !waitCodecDone(r.done, graceDeadline) {
				pending = append(pending, r)
			}
		}
		for _, r := range pending {
			if !codecDone(r.done) {
				signalCodecProcess(r.command.Process, syscall.SIGKILL)
			}
		}
		joined := make(chan struct{})
		go func() { c.wg.Wait(); close(joined) }()
		workersStopped := waitCodecDone(joined, stopDeadline)
		for _, r := range resources {
			if r.command != nil && r.command.Process != nil && !codecDone(r.done) {
				c.closeError = errors.New("native codec process did not stop")
				return
			}
		}
		if !workersStopped && !codecDone(joined) {
			c.closeError = errors.New("native audio workers did not stop")
		}
	})
	return c.closeError
}

func signalCodecProcess(process *os.Process, signal syscall.Signal) {
	if err := syscall.Kill(-process.Pid, signal); err != nil {
		_ = process.Signal(signal)
	}
}

func codecDone(done <-chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func waitCodecDone(done <-chan struct{}, deadline time.Time) bool {
	if codecDone(done) {
		return true
	}
	timer := time.NewTimer(max(0, time.Until(deadline)))
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		// Completion and the deadline may become ready together. Prefer the
		// confirmed final state instead of randomly reporting a timeout.
		return codecDone(done)
	}
}

func yamlString(value string) string { raw, _ := json.Marshal(value); return string(raw) }
func RenderCodecConfig(o CodecOptions, tg, receivePort, sendPort int) string {
	return fmt.Sprintf(`daemon: false
iAgreeNotToBeStupid: true
log:
    displayLevel: 2
    fileLevel: 1
    filePath: .
    activityFilePath: .
    fileRoot: Codec-%d
network:
    id: %d
    address: %s
    port: %d
    password: %s
    encrypted: false
    allowDiagnosticTransfer: false
    sourceId: %d
    destinationId: %d
    slot: 1
    overrideSourceIdFromMDC: false
    overrideSourceIdFromUDP: true
    resetCallForSourceIdChange: true
system:
    identity: %s
    voiceMfid: %d
    netId: "%05X"
    sysId: "%03X"
    grantDemand: false
    txMode: 2
    rxAudioGain: 1.0
    vocoderDecoderAudioGain: 3.0
    vocoderDecoderAutoGain: false
    txAudioGain: 1.0
    vocoderEncoderAudioGain: 3.0
    localAudio: false
    detectAnalogMDC1200: false
    dropTimeMs: 180
    udpAudio: true
    udpMetadata: true
    opaqueP25: %t
    udpSendAddress: 127.0.0.1
    udpSendPort: %d
    udpReceiveAddress: 127.0.0.1
    udpReceivePort: %d
    udpRTPFrames: false
    udpUseULaw: false
    udpUsrp: false
    udpFrameTiming: false
    tek:
        enable: false
`, tg, o.PeerIDs[tg], yamlString(o.MasterAddress), o.MasterPort, yamlString(o.MasterPassword), o.RadioID, tg, yamlString(o.Identity), o.VoiceMFID, o.WACN, o.SystemID, o.Opaque, sendPort, receivePort)
}

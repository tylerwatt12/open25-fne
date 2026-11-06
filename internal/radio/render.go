package radio

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

func quoted(s string) string { v, _ := json.Marshal(s); return string(v) }
func siteIdentity(p Profile) (SiteIdentity, error) {
	for _, s := range p.Zone.Sites {
		if s.ID == p.Site.ID {
			return s, nil
		}
	}
	return SiteIdentity{}, errors.New("Site is not assigned to this Zone")
}
func groupAllowedAtSite(g Talkgroup, siteID string) bool {
	return g.SiteAccess == "all" || slices.Contains(g.SiteIDs, siteID)
}
func allowedGroups(p Profile) []Talkgroup {
	out := []Talkgroup{}
	for _, g := range p.System.Talkgroups {
		if groupAllowedAtSite(g, p.Site.ID) {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TGID < out[j].TGID })
	return out
}
func orderedChannels(p Profile) []Channel {
	out := []Channel{}
	for _, ch := range p.Site.Channels {
		if ch.Enabled {
			out = append(out, ch)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role == "control"
		}
		return out[i].ID < out[j].ID
	})
	return out
}
func findModem(p Profile, id string) (Modem, bool) {
	for _, m := range p.Site.Modems {
		if m.ID == id {
			return m, true
		}
	}
	return Modem{}, false
}
func Validate(p Profile) error {
	identity, err := siteIdentity(p)
	if err != nil {
		return err
	}
	if p.System.ID == "" || p.Zone.SystemID != p.System.ID || p.Zone.RFSSID < 1 || p.Zone.RFSSID > 254 || identity.SiteID < 1 || identity.SiteID > 254 || identity.NAC < 0 || identity.NAC > 0xFFF || identity.NAC == 0xF7E || identity.NAC == 0xF7F {
		return errors.New("system, RFSS, Site or NAC identity is invalid")
	}
	if p.System.VoiceMFID != 0 && p.System.VoiceMFID != 1 {
		return errors.New("standard voice MFID must be 00 or 01")
	}
	if p.System.BSIProfile != "" && p.System.BSIProfile != "motorola" {
		return errors.New("only the Motorola P25 BSI profile is implemented")
	}
	if p.System.WACN < 1 || p.System.WACN > 0xFFFFE || p.System.SystemID < 1 || p.System.SystemID > 0xFFE || strings.TrimSpace(p.System.Callsign) == "" || strings.ContainsAny(p.System.Callsign, "\r\n\x00") {
		return errors.New("valid home WACN, System ID and callsign are required")
	}
	if p.Site.Identification.Mode != "ham" && p.Site.Identification.Mode != "bsi" {
		return errors.New("choose Ham CWID or Commercial BSI")
	}
	if p.Site.Identification.WPM != 20 {
		return errors.New("Ham CWID requires 20 WPM")
	}
	if p.Site.Identification.QuietSeconds < 1 || p.Site.Identification.QuietSeconds > 60 {
		return errors.New("identification quiet period must be 1-60 seconds")
	}
	radios := map[int]bool{}
	for _, r := range p.System.Radios {
		if r.RID < 1 || r.RID > 0xFFFFFB || radios[r.RID] || (r.Kind != "subscriber" && r.Kind != "console" && r.Kind != "mobile" && r.Kind != "automation") {
			return errors.New("Radio IDs must be unique and unreserved, with subscriber, console, mobile or automation kind")
		}
		radios[r.RID] = true
	}
	if err := ValidateInhibition(p.System.Radios, p.Inhibition); err != nil {
		return err
	}
	groups := map[int]bool{}
	for _, g := range p.System.Talkgroups {
		if g.TGID < 1 || g.TGID > 65534 || groups[g.TGID] || (g.SiteAccess != "all" && g.SiteAccess != "selected") {
			return errors.New("talkgroups must be unique and unreserved, with all or selected Site access")
		}
		groups[g.TGID] = true
		for _, rule := range []struct {
			mode string
			ids  []int
		}{{g.AffiliationAccess, g.AffiliationRadioIDs}, {g.TransmitAccess, g.TransmitRadioIDs}} {
			if rule.mode != "" && rule.mode != "all" && rule.mode != "selected" {
				return errors.New("choose all or selected subscriber access")
			}
			seen := map[int]bool{}
			for _, id := range rule.ids {
				if seen[id] || !slices.ContainsFunc(p.System.Radios, func(r Radio) bool { return r.RID == id && r.Kind == "subscriber" }) {
					return errors.New("subscriber access must select unique physical Radio IDs in the system directory")
				}
				seen[id] = true
			}
		}

	}
	plans := map[int]Identifier{}
	for _, x := range p.Zone.ChannelIdentifiers {
		if x.Identifier < 0 || x.Identifier > 15 || x.BaseHz <= 0 || x.BaseHz%5 != 0 || x.SpacingHz <= 0 || x.SpacingHz%125 != 0 || x.OffsetHz == 0 || x.OffsetHz%x.SpacingHz != 0 {
			return errors.New("Channel Identifier cannot be represented by the native Phase 1 encoder")
		}
		if _, ok := plans[x.Identifier]; ok {
			return errors.New("duplicate Channel Identifier")
		}
		plans[x.Identifier] = x
	}
	channels := orderedChannels(p)
	if len(channels) == 0 || len(channels) > 16 {
		return errors.New("configure one control channel and at most fifteen voice channels")
	}
	controls := 0
	ids := map[string]bool{}
	modems := map[string]bool{}
	rf := map[string]bool{}
	for _, ch := range channels {
		if ch.ID == "" || ch.ID == "." || ch.ID == ".." || ids[ch.ID] || strings.ContainsAny(ch.ID, "/\\\r\n\x00") {
			return errors.New("RF channel IDs must be unique filename-safe records")
		}
		ids[ch.ID] = true
		if ch.Role == "control" {
			controls++
		} else if ch.Role != "voice" {
			return errors.New("choose control or voice channel role")
		}
		m, ok := findModem(p, ch.ModemID)
		if !ok || m.Baud != 115200 || m.Device == "" || modems[m.Device] {
			return errors.New("each channel requires a distinct 115200-baud modem")
		}
		modems[m.Device] = true
		x, ok := plans[ch.Identifier]
		if !ok || ch.ChannelNo < 1 || ch.ChannelNo > 4095 || ch.TXHz != x.BaseHz+x.SpacingHz*int64(ch.ChannelNo) || ch.RXHz != ch.TXHz+x.OffsetHz {
			return errors.New("RF pair does not match its assigned Channel Identifier")
		}
		if ch.TXHz < 420000000 || ch.TXHz > 450000000 || ch.RXHz < 420000000 || ch.RXHz > 450000000 {
			return errors.New("the qualified hotspot RF profile is limited to 420-450 MHz")
		}
		key := fmt.Sprintf("%d:%d", ch.Identifier, ch.ChannelNo)
		if rf[key] {
			return errors.New("RF channels must use distinct channel numbers")
		}
		rf[key] = true
		if ch.PowerPercent < 1 || ch.PowerPercent > 100 {
			return errors.New("modem power must be 1-100 percent")
		}
		c := m.Calibration
		if c.RXLevel < 0 || c.RXLevel > 100 || c.TXLevel < 0 || c.TXLevel > 100 {
			return errors.New("modem levels must be 0-100 percent")
		}
	}
	if controls != 1 {
		return errors.New("exactly one enabled control channel is required")
	}
	if len(allowedGroups(p)) == 0 {
		return errors.New("no talkgroups are assigned to this Site")
	}
	return nil
}

func newSecret() (string, error) {
	v := make([]byte, 32)
	if _, err := rand.Read(v); err != nil {
		return "", err
	}
	return hex.EncodeToString(v), nil
}
func validSecret(s string) bool {
	raw, err := hex.DecodeString(s)
	return err == nil && len(raw) == 32 && len(s) == 64
}

func RenderLookups(p Profile) map[string]string {
	var rid, tg, iden strings.Builder
	rid.WriteString("# RID,Enabled,Alias,IP Address,\n")
	ridCSV := csv.NewWriter(&rid)
	for _, r := range p.System.Radios {
		enabled := 0
		if r.Enabled {
			enabled = 1
		}
		alias := r.Alias
		if !IsValidUserAlias(alias) {
			alias = ""
		}
		_ = ridCSV.Write([]string{strconv.Itoa(r.RID), strconv.Itoa(enabled), alias, "", ""})
	}
	ridCSV.Flush()
	// Preserve global validity in each rule while active remains Site-local.
	// Inactive entries never admit grants, traffic, or listener demand.
	groups := slices.Clone(p.System.Talkgroups)
	sort.Slice(groups, func(i, j int) bool { return groups[i].TGID < groups[j].TGID })
	if len(groups) == 0 {
		tg.WriteString("groupVoice: []\n")
	} else {
		tg.WriteString("groupVoice:\n")
	}
	for _, g := range groups {
		if g.Alias == "" {
			g.Alias = fmt.Sprintf("TG%d", g.TGID)
		}
		fmt.Fprintf(&tg, "  - name: %s\n    alias: %s\n    config:\n      active: %t\n      system_valid: %t\n      affiliated: false\n      rid_permitted: []\n      subscriber_access:\n", quoted(g.Alias), quoted(g.Alias), g.Enabled && groupAllowedAtSite(g, p.Site.ID), g.Enabled)
		for _, rule := range []struct {
			name, mode string
			ids        []int
		}{{"affiliation", g.AffiliationAccess, g.AffiliationRadioIDs}, {"transmit", g.TransmitAccess, g.TransmitRadioIDs}} {
			// Materialize the effective physical population even for All. A raw
			// RF/FNE sender cannot borrow an enabled application identity.
			ids := []int{}
			for _, r := range p.System.Radios {
				if r.Enabled && r.Kind == "subscriber" && (rule.mode != "selected" || slices.Contains(rule.ids, r.RID)) {
					ids = append(ids, r.RID)
				}
			}
			slices.Sort(ids)

			fmt.Fprintf(&tg, "        %s:\n          restricted: true\n", rule.name)
			if len(ids) == 0 {
				tg.WriteString("          radio_ids: []\n")
			} else {
				// The pinned native YAML parser supports block sequences;
				// a nonempty JSON/flow list is a scalar with size zero.
				tg.WriteString("          radio_ids:\n")
				for _, id := range ids {
					fmt.Fprintf(&tg, "            - %d\n", id)
				}
			}
		}
		fmt.Fprintf(&tg, "    source:\n      tgid: %d\n      slot: 1\n", g.TGID)
	}
	for _, x := range p.Zone.ChannelIdentifiers {
		fmt.Fprintf(&iden, "%d,%d,%.6f,%.6f,%.6f,\n", x.Identifier, x.BaseHz, float64(x.SpacingHz)/1000, float64(x.OffsetHz)/1000000, float64(x.BandwidthHz)/1000)
	}
	return map[string]string{"rid_acl.dat": rid.String(), "talkgroup_rules.yml": tg.String(), "iden_table.dat": iden.String()}
}

// RenderChannel writes the native schema directly. No old renderer, string
// substitution pipeline or legacy catalogue is part of the new application.
func RenderChannel(p Profile, ch Channel, n Network, rpcPort, restPort int, mode string) (map[string]string, error) {
	if err := Validate(p); err != nil {
		return nil, err
	}
	if mode != "null" && mode != "hardware" {
		return nil, errors.New("choose null or hardware operation")
	}
	ip := net.ParseIP(n.Address)
	if !validSecret(n.Secret) || !validSecret(n.RPCSecret) || n.RPCSecret == n.Secret || ip == nil || !ip.IsLoopback() || n.Port < 1024 || n.Port > 65535 {
		return nil, errors.New("private local media assignment is invalid")
	}
	identity, _ := siteIdentity(p)
	if identity.LRA < 0 || identity.LRA > 255 {
		return nil, fmt.Errorf("configured Location Registration Area %d cannot be encoded in the P25 one-octet field", identity.LRA)
	}
	m, ok := findModem(p, ch.ModemID)
	if !ok {
		return nil, errors.New("modem not assigned")
	}
	control := ch.Role == "control"
	rpcSecret := n.RPCSecret
	peerID := n.ControlPeerID
	if !control {
		peerID = n.VoicePeerIDs[ch.ID]
	}
	if peerID < 1 {
		return nil, errors.New("native peer identity is missing")
	}
	channels := orderedChannels(p)
	ccRPC := rpcPort
	for i, x := range channels {
		if x.Role == "control" {
			ccRPC = rpcPort - 2*indexChannel(channels, ch.ID) + 2*i
			break
		}
	}
	var voice strings.Builder
	if control {
		for i, v := range channels {
			if v.Role == "voice" {
				fmt.Fprintf(&voice, "            - channelId: %d\n              channelNo: %s\n              rpcAddress: 127.0.0.1\n              rpcPort: %d\n              rpcPassword: %s\n", v.Identifier, quoted(fmt.Sprintf("%X", v.ChannelNo)), rpcPort-2*indexChannel(channels, ch.ID)+2*i, quoted(rpcSecret))
			}
		}
	} else {
		fmt.Fprintf(&voice, "            - channelId: %d\n              channelNo: %s\n              rpcAddress: 127.0.0.1\n              rpcPort: %d\n              rpcPassword: %s\n", ch.Identifier, quoted(fmt.Sprintf("%X", ch.ChannelNo)), rpcPort, quoted(rpcSecret))
	}
	voiceText := "        voiceChNo: []\n"
	if voice.Len() > 0 {
		voiceText = "        voiceChNo:\n" + voice.String()
	}
	controlRef := ""
	if control {
		controlRef = "        backupControlChannels: []\n"
		var backup strings.Builder
		for _, candidate := range controlCandidates(p) {
			if candidate.ID != ch.ID {
				fmt.Fprintf(&backup, "            - channelId: %d\n              channelNo: %s\n", candidate.Identifier, quoted(fmt.Sprintf("%X", candidate.ChannelNo)))
			}
		}
		if backup.Len() > 0 {
			controlRef = "        backupControlChannels:\n" + backup.String()
		}
	}
	if !control {
		controlRef = fmt.Sprintf("        controlCh:\n            rpcAddress: 127.0.0.1\n            rpcPort: %d\n            rpcPassword: %s\n            notifyEnable: true\n            presence: 5\n", ccRPC, quoted(rpcSecret))
	}
	transport := "            type: \"null\"\n"
	if mode == "hardware" {
		transport = fmt.Sprintf("            type: uart\n            uart:\n                port: %s\n                speed: 115200\n", quoted(m.Device))
	}
	files := RenderLookups(p)
	cal := m.Calibration
	var subscriberIDs strings.Builder
	for _, subscriber := range p.System.Radios {
		if subscriber.Kind == "subscriber" {
			fmt.Fprintf(&subscriberIDs, "            - %d\n", subscriber.RID)
		}
	}
	subscribers := "        dispatchSubscribers: []\n"
	if subscriberIDs.Len() > 0 {
		subscribers = "        dispatchSubscribers:\n" + subscriberIDs.String()
	}
	subscribers += renderInhibition(p.Inhibition)
	var priorities, allStart, security strings.Builder
	for _, g := range allowedGroups(p) {
		priority := g.Priority
		if priority == 0 {
			priority = 10
		}
		fmt.Fprintf(&priorities, "            - talkgroup: %d\n              priority: %d\n", g.TGID, priority)
		mode := g.AdmissionMode
		if mode == "both" {
			mode = "selectable"
		}
		fmt.Fprintf(&security, "            - talkgroup: %d\n              mode: %s\n", g.TGID, quoted(mode))
		if g.StartPolicy == "all_start" {
			fmt.Fprintf(&allStart, "            - %d\n", g.TGID)
		}
	}
	if priorities.Len() > 0 {
		subscribers += "        dispatchTalkgroupPriorities:\n" + priorities.String()
	}
	if allStart.Len() > 0 {
		subscribers += "        rfAllStartTalkgroups:\n" + allStart.String()
	}
	if security.Len() > 0 {
		subscribers += "        rfTalkgroupSecurity:\n" + security.String()
	}
	bsiProfile := p.System.BSIProfile
	if bsiProfile == "" {
		bsiProfile = "motorola"
	}
	files["config.yml"] = fmt.Sprintf(`daemon: false
iAgreeNotToBeStupid: true
log:
    displayLevel: 2
    fileLevel: 1
    filePath: .
    activityFilePath: .
    fileRoot: SiteRadio
network:
    enable: true
    id: %d
    address: %s
    port: %d
    password: %s
    encrypted: false
    updateLookups: false
    saveLookups: false
    allowActivityTransfer: false
    allowDiagnosticTransfer: false
    rpcAddress: 127.0.0.1
    rpcPort: %d
    rpcPassword: %s
    restEnable: true
    restAddress: 127.0.0.1
    restPort: %d
    restPassword: %s
    restSsl: false
    restDebug: false
protocols:
    dmr:
        enable: false
    nxdn:
        enable: false
    p25:
        enable: true
        managedSite: true
        convNetGrantDemand: %t
        dispatchEnable: %t
%s        inhibitUnauthorized: false
        verifyReg: %t
        verifyAff: %t
        noMessageAck: false
        sndcpSupport: false
        control:
            enable: %t
            dedicated: %t
            voiceOnControl: false
            broadcast: %t
            broadcastIdentifiers: %t
            disableTSDUMBF: false
            redundantImmediate: true
system:
    identity: %s
    voiceMfid: %d
    duplex: true
    fixedMode: true
    iden_table:
        file: iden_table.dat
        time: 0
    radio_id:
        file: rid_acl.dat
        time: 0
        acl: true
    talkgroup_id:
        file: talkgroup_rules.yml
        time: 0
        acl: true
    config:
        authoritative: %t
        supervisor: %t
        channelId: %d
        channelNo: %s
%s%s        netId: %s
        sysId: %s
        rfssId: %s
        siteId: %s
        lra: %d
        nac: %s
    cwId:
        enable: %t
        mode: %s
        time: 10
        quietSeconds: %d
        drainSeconds: 540
        callsign: %s
    bsi:
        enable: %t
        profile: %s
    modem:
        rxLevel: %.3f
        p25TxLevel: %.3f
        rxDCOffset: %d
        txDCOffset: %d
        rxInvert: %t
        txInvert: %t
        pttInvert: %t
        hotspot:
            rfPower: %d
            rxTuning: %d
            txTuning: %d
        protocol:
%s            mode: air
`, peerID, quoted(n.Address), n.Port, quoted(n.Secret), rpcPort, quoted(rpcSecret), restPort, quoted(rpcSecret), !control, control, subscribers, control, control, control, control, control, control && p.Site.BroadcastBandPlan, quoted(p.System.Callsign), p.System.VoiceMFID, control, control && hasVoice(channels), ch.Identifier, quoted(fmt.Sprintf("%X", ch.ChannelNo)), voiceText, controlRef, quoted(fmt.Sprintf("%05X", p.System.WACN)), quoted(fmt.Sprintf("%03X", p.System.SystemID)), quoted(fmt.Sprintf("%X", p.Zone.RFSSID)), quoted(fmt.Sprintf("%X", identity.SiteID)), identity.LRA, quoted(fmt.Sprintf("%03X", identity.NAC)), p.Site.Identification.Enabled && p.Site.Identification.Mode == "ham" && mode == "hardware", quoted(p.Site.Identification.Mode), p.Site.Identification.QuietSeconds, quoted(p.System.Callsign), p.Site.Identification.Enabled && p.Site.Identification.Mode == "bsi" && control && mode == "hardware", quoted(bsiProfile), cal.RXLevel, cal.TXLevel, cal.RXDCOffset, cal.TXDCOffset, cal.RXInvert, cal.TXInvert, cal.PTTInvert, ch.PowerPercent, cal.RXOffsetHz, cal.TXOffsetHz, transport)
	return files, nil
}
func indexChannel(channels []Channel, id string) int {
	for i, ch := range channels {
		if ch.ID == id {
			return i
		}
	}
	return 0
}
func hasVoice(channels []Channel) bool {
	for _, ch := range channels {
		if ch.Role == "voice" {
			return true
		}
	}
	return false
}

func RenderFNE(p Profile, n Network) (map[string]string, error) {
	if err := Validate(p); err != nil {
		return nil, err
	}
	if !validSecret(n.Secret) {
		return nil, errors.New("private media credential is invalid")
	}
	files := RenderLookups(p)
	delete(files, "iden_table.dat")
	voiceIDs := []int{}
	for _, id := range n.VoicePeerIDs {
		voiceIDs = append(voiceIDs, id)
	}
	sort.Ints(voiceIDs)
	consoleIDs := []int{}
	for _, id := range n.ConsolePeerIDs {
		consoleIDs = append(consoleIDs, id)
	}
	sort.Ints(consoleIDs)
	groupIDs := []int{}
	for _, g := range allowedGroups(p) {
		if g.Enabled {
			groupIDs = append(groupIDs, g.TGID)
		}
	}
	list := func(key string, ids []int) string {
		if len(ids) == 0 {
			return "          " + key + ": []\n"
		}
		var out strings.Builder
		fmt.Fprintf(&out, "          %s:\n", key)
		for _, id := range ids {
			fmt.Fprintf(&out, "            - %d\n", id)
		}
		return out.String()
	}
	files["fne.yml"] = fmt.Sprintf(`daemon: false
iAgreeNotToBeStupid: true
log:
    displayLevel: 2
    fileLevel: 1
    filePath: .
    activityFilePath: .
    fileRoot: SiteMedia
system:
    identity: %s
    restEnable: false
    pingTime: 5
    maxMissedPings: 5
    sendTalkgroups: false
    allowActivityTransfer: false
    allowDiagnosticTransfer: false
    radio_id:
        file: rid_acl.dat
        time: 0
    peer_acl:
        enable: false
master:
    peerId: %d
    address: %s
    port: %d
    password: %s
    workers: 2
    rejectUnknownRID: true
    allowDMRTraffic: false
    allowP25Traffic: true
    allowNXDNTraffic: false
    allowAnalogTraffic: false
    restrictGrantToAffiliatedOnly: true
    allowConvSiteAffOverride: false
    p25System:
        netId: %d
        sysId: %d
    p25Sites:
        - controlPeerId: %d
%s%s%s    talkgroup_rules:
        file: talkgroup_rules.yml
        time: 0
    adj_site_map:
    crypto_container:
        enable: false
peers: []
vtun:
    enable: false
`, quoted(p.System.Callsign), 900000000+p.System.SystemID, quoted(n.Address), n.Port, quoted(n.Secret), p.System.WACN, p.System.SystemID, n.ControlPeerID, list("voicePeerIds", voiceIDs), list("consolePeerIds", consoleIDs), list("talkgroups", groupIDs))
	return files, nil
}

func writeFiles(dir string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for name, value := range files {
		if filepath.Base(name) != name {
			return errors.New("invalid native filename")
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.WriteString(value)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

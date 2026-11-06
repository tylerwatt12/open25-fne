package framework

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25crypto"
	"open25.local/open25-fne/internal/routing"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// OperatorActor is carried only between enrolled services, never accepted from
// a browser or included in a snapshot. Proof binds the original person account.
type OperatorActor struct {
	UserID           string `json:"user_id"`
	AutomationID     string `json:"automation_id,omitempty"`
	AutomationAction string `json:"automation_action,omitempty"`
	Proof            string `json:"proof"`
	RadioID          int    `json:"radio_id"`
	Administrator    bool   `json:"administrator,omitempty"`
	Username         string `json:"username,omitempty"`
}

type EmergencyIncident struct {
	ID             string    `json:"id"`
	RadioID        int       `json:"radio_id"`
	TalkgroupID    int       `json:"talkgroup_id"`
	ScopeID        string    `json:"scope_id"`
	SourceKind     string    `json:"source_kind"`
	DeclaredBy     string    `json:"declared_by,omitempty"`
	PositionID     string    `json:"position_id,omitempty"`
	At             time.Time `json:"at"`
	LastSeen       time.Time `json:"last_seen"`
	AcknowledgedAt time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy string    `json:"acknowledged_by,omitempty"`
	ClearedAt      time.Time `json:"cleared_at,omitempty"`
	ClearedBy      string    `json:"cleared_by,omitempty"`
}

type RadioLocation struct {
	RadioID     int       `json:"radio_id"`
	TalkgroupID int       `json:"talkgroup_id"`
	Latitude    float64   `json:"latitude"`
	Longitude   float64   `json:"longitude"`
	Accuracy    float64   `json:"accuracy"`
	FixAt       time.Time `json:"fix_at"`
	ReceivedAt  time.Time `json:"received_at"`
	ScopeID     string    `json:"scope_id"`
	SourceKind  string    `json:"source_kind"`
}

type RuntimePatch struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Talkgroups     []int     `json:"talkgroups"`
	AlgorithmID    int       `json:"algorithm_id"`
	KeyID          int       `json:"key_id"`
	CreatedBy      string    `json:"created_by"`
	CreatedRadioID int       `json:"created_radio_id"`
	PositionID     string    `json:"position_id,omitempty"`
	At             time.Time `json:"at"`
	Active         bool      `json:"active"`
	ScopeID        string    `json:"scope_id"`
	State          string    `json:"state"`
	Reason         string    `json:"reason,omitempty"`
	EndedAt        time.Time `json:"ended_at,omitempty"`
	EndedBy        string    `json:"ended_by,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
	UpdatedBy      string    `json:"updated_by,omitempty"`
	CanEdit        bool      `json:"can_edit,omitempty"`
}

type OperatorSnapshot struct {
	Units        []OperatorUnit        `json:"units,omitempty"`
	Emergencies  []EmergencyIncident   `json:"emergencies"`
	Locations    []RadioLocation       `json:"locations"`
	Patches      []RuntimePatch        `json:"patches"`
	CallDelivery []CallDelivery        `json:"call_delivery"`
	Commands     []radioCommandReceipt `json:"commands,omitempty"`
	CommandSites []commandSiteStatus   `json:"command_sites,omitempty"`
}
type CallDelivery struct {
	CallID     string             `json:"call_id"`
	RadioID    int                `json:"radio_id"`
	Talkgroups []int              `json:"talkgroups"`
	Sites      []routing.Delivery `json:"sites"`
}

// OperatorInput is the current browser/native-client contract. Server-owned
// identity, actor and timestamps are not accepted inside its patch/location.
type OperatorInput struct {
	Action      string         `json:"action,omitempty"`
	ID          string         `json:"id,omitempty"`
	RadioID     int            `json:"radio_id,omitempty"`
	TalkgroupID int            `json:"talkgroup_id,omitempty"`
	SiteID      string         `json:"site_id,omitempty"`
	Code        *int           `json:"code,omitempty"`
	Location    *LocationInput `json:"location,omitempty"`
	Patch       *PatchInput    `json:"patch,omitempty"`
}
type LocationInput struct {
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	Accuracy  float64   `json:"accuracy"`
	FixAt     time.Time `json:"fix_at"`
}
type PatchInput struct {
	Name               string `json:"name"`
	Talkgroups         []int  `json:"talkgroups"`
	AlgorithmID        int    `json:"algorithm_id"`
	KeyID              int    `json:"key_id"`
	PreviousTalkgroups []int  `json:"previous_talkgroups,omitempty"`
}
type operatorEnvelope struct {
	Actor OperatorActor `json:"actor"`
	Input OperatorInput `json:"input"`
}
type operatorAuthority struct {
	actor    OperatorActor
	user     User
	position *ConsolePosition
	admin    bool
	events   *[]activity.Event
}

func emptyOperatorSnapshot() OperatorSnapshot {
	return OperatorSnapshot{Emergencies: []EmergencyIncident{}, Locations: []RadioLocation{}, Patches: []RuntimePatch{}}
}
func (s *Service) operatorLocalUnlocked() OperatorSnapshot {
	value := emptyOperatorSnapshot()
	s.readCache("operator-local", &value)
	return value
}
func (s *Service) operatorCombinedUnlocked() OperatorSnapshot {
	upstream := emptyOperatorSnapshot()
	s.readCache("operator-upstream", &upstream)
	return mergeOperatorSnapshots(upstream, s.operatorLocalUnlocked())
}
func (s *Service) operatorLocalSnapshot() OperatorSnapshot {
	s.featureMu.Lock()
	defer s.featureMu.Unlock()
	return s.operatorLocalUnlocked()
}
func (s *Service) operatorSnapshot() OperatorSnapshot {
	s.featureMu.Lock()
	value := s.operatorCombinedUnlocked()
	s.featureMu.Unlock()
	value.CallDelivery = []CallDelivery{}
	if s.broker != nil {
		for _, q := range s.broker.router.Active() {
			s.broker.mu.Lock()
			owner := s.broker.owners[q.ID]
			s.broker.mu.Unlock()
			value.CallDelivery = append(value.CallDelivery, CallDelivery{q.LogicalCallID, q.RadioID, q.Talkgroups, s.broker.router.Delivery(owner, dispatch.Grant{ID: q.ID, Talkgroups: q.Talkgroups, Secure: q.Secure, AlgorithmID: q.AlgorithmID, KeyID: q.KeyID})})
		}
	}
	return value
}

func firstTime(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
func mergeOperatorSnapshots(a, b OperatorSnapshot) OperatorSnapshot {
	out := emptyOperatorSnapshot()
	emergencies, patches, locations := map[string]EmergencyIncident{}, map[string]RuntimePatch{}, map[int]RadioLocation{}
	for _, snapshot := range []OperatorSnapshot{a, b} {
		for _, incident := range snapshot.Emergencies {
			if old, ok := emergencies[incident.ID]; ok {
				if old.RadioID != incident.RadioID || old.TalkgroupID != incident.TalkgroupID || old.ScopeID != incident.ScopeID {
					continue
				}
				if old.LastSeen.After(incident.LastSeen) {
					incident.LastSeen = old.LastSeen
				}
				if !old.AcknowledgedAt.IsZero() && (incident.AcknowledgedAt.IsZero() || old.AcknowledgedAt.Before(incident.AcknowledgedAt)) {
					incident.AcknowledgedAt, incident.AcknowledgedBy = old.AcknowledgedAt, old.AcknowledgedBy
				}
				if !old.ClearedAt.IsZero() && (incident.ClearedAt.IsZero() || old.ClearedAt.Before(incident.ClearedAt)) {
					incident.ClearedAt, incident.ClearedBy = old.ClearedAt, old.ClearedBy
				}
				incident.At = firstTime(old.At, incident.At)
			}
			emergencies[incident.ID] = incident
		}
		for _, patch := range snapshot.Patches {
			patch.Talkgroups = slices.Clone(patch.Talkgroups)
			if old, ok := patches[patch.ID]; ok {
				if !samePatchIdentity(old, patch) {
					continue
				}
				patch = mergePatchRecord(old, patch)
			}
			if !patch.EndedAt.IsZero() {
				patch.Active, patch.State = false, "ended"
			}
			patches[patch.ID] = patch
		}
		for _, location := range snapshot.Locations {
			old, ok := locations[location.RadioID]
			if !ok || location.FixAt.After(old.FixAt) || (location.FixAt.Equal(old.FixAt) && location.ReceivedAt.After(old.ReceivedAt)) {
				locations[location.RadioID] = location
			}
		}
	}
	for _, value := range emergencies {
		out.Emergencies = append(out.Emergencies, value)
	}
	for _, value := range patches {
		out.Patches = append(out.Patches, value)
	}
	for _, value := range locations {
		out.Locations = append(out.Locations, value)
	}
	sort.Slice(out.Emergencies, func(i, j int) bool { return out.Emergencies[i].ID < out.Emergencies[j].ID })
	sort.Slice(out.Patches, func(i, j int) bool { return out.Patches[i].ID < out.Patches[j].ID })
	sort.Slice(out.Locations, func(i, j int) bool { return out.Locations[i].RadioID < out.Locations[j].RadioID })
	return out
}

func (s *Service) operatorScopeAllowed(peer PeerCredential) bool {
	if peer.Role == RoleApplication {
		return s.Config().Role == RoleZone || s.Config().Role == RoleSite
	}
	if s.Config().Role != RoleZone || peer.Role != RoleSite {
		return false
	}
	zone := s.view().Zone
	return zone != nil && siteExists(zone, peer.InstanceID)
}
func (s *Service) operatorActor(actor OperatorActor) (operatorAuthority, error) {
	if actor.AutomationID != "" || actor.AutomationAction != "" {
		return operatorAuthority{}, errors.New("automation accounts cannot perform operator actions")
	}
	if actor.Administrator {
		username, hash, err := s.Store.Account()
		digest := sha256.Sum256(hash)
		if err == nil && actor.Username == username && actor.RadioID == 0 && subtle.ConstantTimeCompare([]byte(actor.Proof), []byte(hex.EncodeToString(digest[:]))) == 1 {
			return operatorAuthority{actor: actor, admin: true}, nil
		}
		return operatorAuthority{}, errors.New("administrator authority is not accepted")
	}
	view := s.view()
	record, ok := view.Users[actor.UserID]
	if !ok || !record.User.Enabled || subtle.ConstantTimeCompare([]byte(actor.Proof), []byte(userProof(record.User, record.PasswordHash))) != 1 {
		return operatorAuthority{}, errors.New("the application identity or permissions have changed")
	}
	user, position, err := resolveUserRadio(record.User, view.System, actor.RadioID)
	if err != nil {
		return operatorAuthority{}, err
	}
	return operatorAuthority{actor: actor, user: user, position: position}, nil
}
func (s *Service) operatorGroupAllowed(authority operatorAuthority, tg int) bool {
	if !authority.admin && !slices.Contains(authority.user.Talkgroups, tg) {
		return false
	}
	view := s.view()
	if !slices.ContainsFunc(view.System.Talkgroups, func(g Talkgroup) bool { return g.TGID == tg && g.Enabled }) {
		return false
	}
	return s.Config().Role != RoleSite || groupAtSite(view.System, tg, s.Config().InstanceID)
}
func operatorPermitted(authority operatorAuthority, permission string) bool {
	return !authority.admin && slices.Contains(authority.user.Permissions, permission)
}
func (s *Service) filterOperatorState(value OperatorSnapshot, authority operatorAuthority) OperatorSnapshot {
	if authority.admin {
		return value
	}
	out := emptyOperatorSnapshot()
	for _, incident := range value.Emergencies {
		if s.operatorGroupAllowed(authority, incident.TalkgroupID) {
			out.Emergencies = append(out.Emergencies, incident)
		}
	}
	for _, location := range value.Locations {
		if s.operatorGroupAllowed(authority, location.TalkgroupID) && (location.RadioID == authority.user.RadioID || operatorPermitted(authority, "location_view")) {
			out.Locations = append(out.Locations, location)
		}
	}
	for _, patch := range value.Patches {
		if slices.ContainsFunc(patch.Talkgroups, func(tg int) bool { return !s.operatorGroupAllowed(authority, tg) }) {
			continue
		}
		patch.CanEdit = s.patchEditAllowed(patch, authority)
		out.Patches = append(out.Patches, patch)
	}
	for _, call := range value.CallDelivery {
		groups := []int{}
		for _, tg := range call.Talkgroups {
			if s.operatorGroupAllowed(authority, tg) {
				groups = append(groups, tg)
			}
		}
		if len(groups) > 0 {
			call.Talkgroups = groups
			out.CallDelivery = append(out.CallDelivery, call)
		}
	}
	return out
}

func (s *Service) operatorHTTP(w http.ResponseWriter, r *http.Request, session Session) {
	topic := strings.TrimPrefix(r.URL.Path, "/api/operator/")
	input := OperatorInput{}
	if topic == "state" || topic == "radio-commands" || topic == "units" {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		if topic == "units" {
			for name, values := range r.URL.Query() {
				if name != "talkgroup_id" || len(values) != 1 {
					fail(w, 400, errors.New("use one talkgroup_id filter"))
					return
				}
				id, err := strconv.Atoi(values[0])
				if err != nil || id < 1 || id > 65534 {
					fail(w, 400, errors.New("select a permitted talkgroup"))
					return
				}
				input.TalkgroupID = id
			}
		}
	} else {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		if err := readJSON(w, r, &input); err != nil {
			fail(w, 400, err)
			return
		}
	}
	actor := OperatorActor{}
	if session.UserID == "" {
		if topic != "state" && topic != "units" {
			fail(w, 403, errors.New("sign in with an application account to operate radios"))
			return
		}
		username, hash, err := s.Store.Account()
		if err != nil || username != session.Username {
			fail(w, 403, errors.New("administrator account is unavailable"))
			return
		}
		digest := sha256.Sum256(hash)
		actor = OperatorActor{Administrator: true, Username: username, Proof: hex.EncodeToString(digest[:])}
	} else {
		user, hash, err := s.Store.User(session.UserID)
		if err != nil || user.Username != session.Username {
			fail(w, 403, errors.New("application account is unavailable"))
			return
		}
		resolved, _, err := resolveUserPosition(user, s.view().System, session.PositionID)
		if err != nil {
			fail(w, 403, err)
			return
		}
		actor = OperatorActor{UserID: user.ID, Proof: userProof(user, hash), RadioID: resolved.RadioID}
	}
	if s.Config().Role == RoleApplication {
		if s.mediaClient == nil || s.mediaClient.URL() == "" {
			fail(w, 503, errors.New("the controller is unavailable"))
			return
		}
		var result OperatorSnapshot
		if err := s.requestAt(r.Context(), s.mediaClient.URL(), http.MethodPost, "/internal/operator/"+topic, operatorEnvelope{actor, input}, &result, nil); err != nil {
			fail(w, 503, err)
			return
		}
		respond(w, 200, result)
		return
	}
	if s.Config().Role != RoleZone && s.Config().Role != RoleSite {
		fail(w, 403, errors.New("operator state belongs to Zone and Site controllers"))
		return
	}
	authority, err := s.operatorActor(actor)
	if err != nil {
		fail(w, 403, err)
		return
	}
	result, err := s.applyOperator(topic, input, authority)
	if err != nil {
		fail(w, 400, err)
		return
	}
	respond(w, 200, result)
}

func (s *Service) operatorInternal(w http.ResponseWriter, r *http.Request, peer PeerCredential) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if !s.operatorScopeAllowed(peer) {
		fail(w, 403, errors.New("this service cannot submit operator actions here"))
		return
	}
	var envelope operatorEnvelope
	if err := readJSON(w, r, &envelope); err != nil {
		fail(w, 400, err)
		return
	}
	authority, err := s.operatorActor(envelope.Actor)
	if err != nil {
		fail(w, 403, err)
		return
	}
	topic := strings.TrimPrefix(r.URL.Path, "/internal/operator/")
	result, err := s.applyOperator(topic, envelope.Input, authority)
	if err != nil {
		fail(w, 400, err)
		return
	}
	respond(w, 200, result)
}

func (s *Service) applyOperator(topic string, input OperatorInput, authority operatorAuthority) (OperatorSnapshot, error) {
	if topic == "units" {
		units, err := s.operatorUnits(authority, input.TalkgroupID)
		return OperatorSnapshot{Units: units}, err
	}
	if topic == "radio-command" {
		return s.operatorRadioCommand(input, authority)
	}
	if topic == "radio-commands" {
		return s.operatorRadioCommands(authority)
	}
	if topic == "state" {
		return s.filterOperatorState(s.operatorSnapshot(), authority), nil
	}
	if topic == "patch" {
		s.patchMu.Lock()
		defer s.patchMu.Unlock()
	}
	s.featureMu.Lock()
	defer s.featureMu.Unlock()
	local := s.operatorLocalUnlocked()
	combined := s.operatorCombinedUnlocked()
	if topic == "state" {
		return s.filterOperatorState(combined, authority), nil
	}
	if authority.admin {
		return OperatorSnapshot{}, errors.New("operator actions require a separately assigned application identity")
	}
	now := time.Now().UTC()
	events := []activity.Event{}
	authority.events = &events
	var err error
	switch topic {
	case "emergency":
		err = s.mutateEmergency(&local, combined, input, authority, now)
	case "location":
		err = s.mutateLocation(&local, combined, input, authority, now)
	case "patch":
		authority, err = s.currentPatchAuthority(authority)
		if err == nil {
			err = s.mutatePatch(&local, combined, input, authority, now)
		}
	default:
		return OperatorSnapshot{}, errors.New("unknown operator action")
	}
	if err != nil {
		return OperatorSnapshot{}, err
	}
	if err = s.writeOperatorCacheLocked("operator-local", local); err != nil {
		return OperatorSnapshot{}, err
	}
	for _, event := range events {
		s.emit(event)
	}
	return s.filterOperatorState(s.operatorCombinedUnlocked(), authority), nil
}

func operatorID(prefix string) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	return prefix + "-" + token[:24], nil
}
func (s *Service) operatorEvent(authority operatorAuthority, tg int, kind, message string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	details["user_id"] = authority.actor.UserID
	if authority.position != nil {
		details["position_id"] = authority.position.ID
	}
	if authority.events != nil {
		*authority.events = append(*authority.events, activity.Event{RadioID: authority.actor.RadioID, TalkgroupID: tg, Type: kind, Message: message, Audience: "dispatcher", SourceKind: "application", Outcome: "accepted", Details: details})
	}
}
func setIncident(local *OperatorSnapshot, value EmergencyIncident) {
	for i := range local.Emergencies {
		if local.Emergencies[i].ID == value.ID {
			local.Emergencies[i] = value
			return
		}
	}
	local.Emergencies = append(local.Emergencies, value)
}
func (s *Service) mutateEmergency(local *OperatorSnapshot, combined OperatorSnapshot, input OperatorInput, authority operatorAuthority, now time.Time) error {
	if input.Action == "declare" {
		if !operatorPermitted(authority, "emergency") || !s.operatorGroupAllowed(authority, input.TalkgroupID) || (input.RadioID != 0 && input.RadioID != authority.actor.RadioID) {
			return errors.New("emergency declaration is not permitted for this identity or talkgroup")
		}
		for _, old := range combined.Emergencies {
			if old.RadioID == authority.actor.RadioID && old.TalkgroupID == input.TalkgroupID && old.ClearedAt.IsZero() {
				old.LastSeen = now
				setIncident(local, old)
				return nil
			}
		}
		id := input.ID
		if id == "" {
			var err error
			id, err = operatorID("emergency")
			if err != nil {
				return err
			}
		}
		if !recordID.MatchString(id) || slices.ContainsFunc(combined.Emergencies, func(e EmergencyIncident) bool { return e.ID == id }) {
			return errors.New("emergency ID is invalid or already used")
		}
		incident := EmergencyIncident{ID: id, RadioID: authority.actor.RadioID, TalkgroupID: input.TalkgroupID, ScopeID: s.Config().InstanceID, SourceKind: "application", DeclaredBy: authority.actor.UserID, At: now, LastSeen: now}
		if authority.position != nil {
			incident.PositionID = authority.position.ID
		}
		setIncident(local, incident)
		s.operatorEvent(authority, input.TalkgroupID, "emergency", "Emergency declared.", map[string]any{"incident_id": id})
		return nil
	}
	if input.Action != "acknowledge" && input.Action != "clear" {
		return errors.New("choose declare, acknowledge or clear")
	}
	permission := "emergency_ack"
	if input.Action == "clear" {
		permission = "emergency_clear"
	}
	if !operatorPermitted(authority, permission) {
		return errors.New("this emergency action is not permitted")
	}
	for _, incident := range combined.Emergencies {
		if incident.ID != input.ID {
			continue
		}
		if !s.operatorGroupAllowed(authority, incident.TalkgroupID) {
			return errors.New("the emergency talkgroup is not permitted")
		}
		if input.Action == "acknowledge" && incident.AcknowledgedAt.IsZero() {
			incident.AcknowledgedAt, incident.AcknowledgedBy = now, authority.actor.UserID
		}
		if input.Action == "clear" && incident.ClearedAt.IsZero() {
			incident.ClearedAt, incident.ClearedBy = now, authority.actor.UserID
		}
		setIncident(local, incident)
		s.operatorEvent(authority, incident.TalkgroupID, "emergency_"+input.Action, "Emergency "+input.Action+" recorded.", map[string]any{"incident_id": incident.ID, "declaring_radio_id": incident.RadioID, "radio_clear_confirmed": false})
		return nil
	}
	return errors.New("the emergency incident is not available")
}

func (s *Service) mutateLocation(local *OperatorSnapshot, combined OperatorSnapshot, input OperatorInput, authority operatorAuthority, now time.Time) error {
	if input.Action != "report" || input.Location == nil || !operatorPermitted(authority, "location_report") || !s.operatorGroupAllowed(authority, input.TalkgroupID) || (input.RadioID != 0 && input.RadioID != authority.actor.RadioID) {
		return errors.New("location reporting is not permitted for this identity or talkgroup")
	}
	fix := *input.Location
	if math.IsNaN(fix.Latitude) || math.IsInf(fix.Latitude, 0) || math.IsNaN(fix.Longitude) || math.IsInf(fix.Longitude, 0) || math.IsNaN(fix.Accuracy) || math.IsInf(fix.Accuracy, 0) || fix.Latitude < -90 || fix.Latitude > 90 || fix.Longitude < -180 || fix.Longitude > 180 || fix.Accuracy < 0 || fix.Accuracy > 100000 {
		return errors.New("enter a valid latitude, longitude and accuracy")
	}
	if fix.FixAt.IsZero() {
		fix.FixAt = now
	}
	if fix.FixAt.After(now.Add(30*time.Second)) || fix.FixAt.Before(now.Add(-5*time.Minute)) {
		return errors.New("the location fix is stale or has a future timestamp")
	}
	for _, old := range combined.Locations {
		if old.RadioID == authority.actor.RadioID && old.FixAt.After(fix.FixAt) {
			return errors.New("an older fix cannot replace this radio's latest position")
		}
	}
	value := RadioLocation{RadioID: authority.actor.RadioID, TalkgroupID: input.TalkgroupID, Latitude: fix.Latitude, Longitude: fix.Longitude, Accuracy: fix.Accuracy, FixAt: fix.FixAt, ReceivedAt: now, ScopeID: s.Config().InstanceID, SourceKind: "application"}
	for i := range local.Locations {
		if local.Locations[i].RadioID == value.RadioID {
			local.Locations[i] = value
			s.operatorEvent(authority, input.TalkgroupID, "location", "Location received.", map[string]any{"fix_at": fix.FixAt, "latitude": fix.Latitude, "longitude": fix.Longitude, "accuracy": fix.Accuracy, "scope_id": value.ScopeID})
			return nil
		}
	}
	local.Locations = append(local.Locations, value)
	s.operatorEvent(authority, input.TalkgroupID, "location", "Location received.", map[string]any{"fix_at": fix.FixAt, "latitude": fix.Latitude, "longitude": fix.Longitude, "accuracy": fix.Accuracy, "scope_id": value.ScopeID})
	return nil
}

func (s *Service) patchPolicy(patch RuntimePatch) error {
	if !p25crypto.ValidSelection(patch.AlgorithmID != 0, patch.AlgorithmID, patch.KeyID) {
		return errors.New("invalid patch encryption identity")
	}
	if len(patch.Talkgroups) < 2 || len(patch.Talkgroups) > 16 {
		return errors.New("select two to sixteen talkgroups")
	}
	seen := map[int]bool{}
	for _, tg := range patch.Talkgroups {
		if seen[tg] {
			return errors.New("do not repeat a patched talkgroup")
		}
		seen[tg] = true
		index := slices.IndexFunc(s.view().System.Talkgroups, func(g Talkgroup) bool { return g.TGID == tg && g.Enabled })
		if index < 0 {
			return errors.New("a patched talkgroup is unavailable")
		}
		group := s.view().System.Talkgroups[index]
		if s.Config().Role == RoleSite && !groupAtSite(s.view().System, tg, s.Config().InstanceID) {
			return errors.New("a patched talkgroup is not allowed at this Site")
		}
		mode := group.AdmissionMode
		if patch.AlgorithmID == 0 && mode == "secure" {
			return errors.New("a secure-only talkgroup cannot be patched in clear mode")
		}
		if patch.AlgorithmID != 0 && (mode == "clear" || mode == "") {
			return errors.New("a clear-only talkgroup cannot be patched in encrypted mode")
		}
	}
	return nil
}
func overlapGroups(a, b []int) bool {
	return slices.ContainsFunc(a, func(tg int) bool { return slices.Contains(b, tg) })
}
func setPatch(local *OperatorSnapshot, value RuntimePatch) {
	for i := range local.Patches {
		if local.Patches[i].ID == value.ID {
			local.Patches[i] = value
			return
		}
	}
	local.Patches = append(local.Patches, value)
}

func (s *Service) patchMembersBusy(groups []int) bool {
	if s.broker == nil {
		return false
	}
	for _, call := range s.broker.router.Active() {
		if overlapGroups(call.Talkgroups, groups) {
			return true
		}
	}
	return false
}
func (s *Service) mutatePatch(local *OperatorSnapshot, combined OperatorSnapshot, input OperatorInput, authority operatorAuthority, now time.Time) error {
	if !operatorPermitted(authority, "patch") {
		return errors.New("patch operation is not permitted")
	}
	if input.Action == "update" {
		return s.updatePatchMembership(local, combined, input, authority, now)
	}
	if input.Action == "end" {
		for _, patch := range combined.Patches {
			if patch.ID != input.ID {
				continue
			}
			if patch.ScopeID != s.Config().InstanceID {
				return errors.New("end this patch at the controller that owns it")
			}
			for _, tg := range patch.Talkgroups {
				if !s.operatorGroupAllowed(authority, tg) {
					return errors.New("every patched talkgroup must be permitted")
				}
			}
			if patch.EndedAt.IsZero() && s.patchMembersBusy(patch.Talkgroups) {
				return errors.New("wait for the current call to finish before ending this patch")
			}
			if patch.EndedAt.IsZero() {
				patch.EndedAt, patch.EndedBy = now, authority.actor.UserID
			}
			patch.Active, patch.State = false, "ended"
			setPatch(local, patch)
			s.operatorEvent(authority, patch.Talkgroups[0], "patch_end", "Patch ended.", map[string]any{"patch_id": patch.ID, "talkgroups": patch.Talkgroups})
			return nil
		}
		return errors.New("the patch is not available")
	}
	if input.Action != "activate" || input.Patch == nil {
		return errors.New("choose activate, update or end")
	}
	value := *input.Patch
	if strings.TrimSpace(value.Name) == "" || len(value.Name) > 100 || strings.IndexFunc(value.Name, unicode.IsControl) >= 0 || !p25crypto.ValidSelection(value.AlgorithmID != 0, value.AlgorithmID, value.KeyID) {
		return errors.New("enter a patch name and valid shared key ID")
	}
	for _, tg := range value.Talkgroups {
		if !s.operatorGroupAllowed(authority, tg) {
			return errors.New("every patched talkgroup must be permitted")
		}
	}
	if value.AlgorithmID != 0 && !operatorPermitted(authority, "secure_ptt") {
		return errors.New("encrypted patch operation is not permitted")
	}
	id := input.ID
	var err error
	if id == "" {
		id, err = operatorID("patch")
		if err != nil {
			return err
		}
	}
	if !recordID.MatchString(id) {
		return errors.New("enter a valid patch ID")
	}
	if slices.ContainsFunc(combined.Patches, func(p RuntimePatch) bool { return p.ID == id }) {
		return errors.New("this patch ID already exists; end it and create a new patch")
	}
	patch := RuntimePatch{ID: id, Name: strings.TrimSpace(value.Name), Talkgroups: slices.Clone(value.Talkgroups), AlgorithmID: value.AlgorithmID, KeyID: value.KeyID, CreatedBy: authority.actor.UserID, CreatedRadioID: authority.actor.RadioID, At: now, Active: true, ScopeID: s.Config().InstanceID, State: "active"}
	sort.Ints(patch.Talkgroups)
	if authority.position != nil {
		patch.PositionID = authority.position.ID
	}
	if err = s.patchPolicy(patch); err != nil {
		return err
	}
	if s.patchMembersBusy(patch.Talkgroups) {
		return errors.New("wait for the current call to finish before joining these talkgroups")
	}
	for _, old := range combined.Patches {
		if old.Active && old.EndedAt.IsZero() && overlapGroups(old.Talkgroups, patch.Talkgroups) {
			return errors.New("a talkgroup already belongs to another active patch")
		}
	}
	setPatch(local, patch)
	s.operatorEvent(authority, patch.Talkgroups[0], "patch_start", "Patch activated.", map[string]any{"patch_id": patch.ID, "talkgroups": patch.Talkgroups, "algorithm_id": patch.AlgorithmID, "key_id": patch.KeyID, "scope_id": patch.ScopeID})
	return nil
}

// The native observation's identity remains fixed across replicated updates.
func sameRFIncident(a, b EmergencyIncident) bool {
	return a.SourceKind == "rf" && b.SourceKind == "rf" && a.ID == b.ID && a.ScopeID == b.ScopeID &&
		a.RadioID == b.RadioID && a.TalkgroupID == b.TalkgroupID && a.At.Equal(b.At)
}

func mergeIncidentCompletion(original, update EmergencyIncident) EmergencyIncident {
	if !update.AcknowledgedAt.IsZero() && (original.AcknowledgedAt.IsZero() || update.AcknowledgedAt.Before(original.AcknowledgedAt)) {
		original.AcknowledgedAt, original.AcknowledgedBy = update.AcknowledgedAt, update.AcknowledgedBy
	}
	if !update.ClearedAt.IsZero() && (original.ClearedAt.IsZero() || update.ClearedAt.Before(original.ClearedAt)) {
		original.ClearedAt, original.ClearedBy = update.ClearedAt, update.ClearedBy
	}
	return original
}

func retainedRFIndex(known []EmergencyIncident, scope string) map[string]EmergencyIncident {
	index := map[string]EmergencyIncident{}
	for _, original := range known {
		if original.ScopeID == scope && original.SourceKind == "rf" {
			index[original.ID] = original
		}
	}
	return index
}

func retainedRFIdentityAllows(known map[string]EmergencyIncident, update EmergencyIncident) bool {
	original, exists := known[update.ID]
	return !exists || sameRFIncident(original, update)
}

// Availability gates admit new incidents. Completion of an already trusted RF
// identity can survive later provisioning changes, without admitting new state
// or replacing its source, declaration, timestamp or last observation.
func retainedRFCompletion(known map[string]EmergencyIncident, update EmergencyIncident, scope string) (EmergencyIncident, bool) {
	if update.ScopeID != scope || update.SourceKind != "rf" || !recordID.MatchString(update.ID) || update.At.IsZero() ||
		update.RadioID < 1 || update.RadioID > 0xFFFFFB || update.TalkgroupID < 1 || update.TalkgroupID > 65534 ||
		update.LastSeen.Before(update.At) || (update.AcknowledgedAt.IsZero() && update.ClearedAt.IsZero()) {
		return EmergencyIncident{}, false
	}
	if original, exists := known[update.ID]; exists && sameRFIncident(original, update) {
		return mergeIncidentCompletion(original, update), true
	}
	return EmergencyIncident{}, false
}

// Site-local activations stay local when reported to Zone. They are not promoted
// into wide-area patches. End/clear records are monotonic across stale copies.
func (s *Service) mergeSiteOperatorSnapshot(siteID string, value OperatorSnapshot) error {
	if s.Config().Role != RoleZone || s.view().Zone == nil || !siteExists(s.view().Zone, siteID) {
		return errors.New("operator report Site is not enrolled")
	}
	s.patchMu.Lock()
	defer s.patchMu.Unlock()
	s.featureMu.Lock()
	defer s.featureMu.Unlock()
	local := s.operatorLocalUnlocked()
	knownRF := retainedRFIndex(local.Emergencies, siteID)
	filtered := emptyOperatorSnapshot()
	for _, e := range value.Emergencies {
		if !recordID.MatchString(e.ID) || e.At.IsZero() || e.LastSeen.Before(e.At) {
			continue
		}
		if e.ScopeID == siteID {
			if !retainedRFIdentityAllows(knownRF, e) {
				continue
			}
			if groupAtSite(s.view().System, e.TalkgroupID, siteID) && s.operatorRadioExists(e.RadioID) {
				filtered.Emergencies = append(filtered.Emergencies, e)
			} else if completed, ok := retainedRFCompletion(knownRF, e, siteID); ok {
				filtered.Emergencies = append(filtered.Emergencies, completed)
			}
		} else if e.ScopeID == s.view().Zone.ID && groupAtSite(s.view().System, e.TalkgroupID, siteID) && s.operatorRadioExists(e.RadioID) {
			// A Site may acknowledge/clear a cached incident while isolated, but
			// cannot create or replace another controller's incident identity.
			for _, original := range local.Emergencies {
				if original.ID == e.ID && original.ScopeID == e.ScopeID && original.RadioID == e.RadioID && original.TalkgroupID == e.TalkgroupID && original.At.Equal(e.At) {
					filtered.Emergencies = append(filtered.Emergencies, mergeIncidentCompletion(original, e))
					break
				}
			}
		}
	}
	for _, l := range value.Locations {
		if l.ScopeID == siteID && s.operatorRadioExists(l.RadioID) && groupAtSite(s.view().System, l.TalkgroupID, siteID) && validLocation(l.Latitude, l.Longitude, l.Accuracy) && !l.FixAt.IsZero() && !l.ReceivedAt.IsZero() {
			filtered.Locations = append(filtered.Locations, l)
		}
	}
	for _, p := range value.Patches {
		if p.ScopeID == siteID && recordID.MatchString(p.ID) && !p.At.IsZero() && s.patchPolicy(p) == nil && !slices.ContainsFunc(p.Talkgroups, func(tg int) bool { return !groupAtSite(s.view().System, tg, siteID) }) {
			filtered.Patches = append(filtered.Patches, p)
		}
	}
	return s.writeOperatorCacheLocked("operator-local", mergeOperatorSnapshots(local, filtered))
}
func (s *Service) acceptOperatorSnapshot(value OperatorSnapshot) error {
	// Current affiliations are computed by their controller for an explicit
	// scoped read. They never become a durable, replicated operator snapshot.
	value.Units = nil
	role := s.Config().Role
	if role != RoleSite && role != RoleApplication {
		return nil
	}
	s.patchMu.Lock()
	defer s.patchMu.Unlock()
	s.featureMu.Lock()
	defer s.featureMu.Unlock()
	local := s.operatorLocalUnlocked()
	upstream := emptyOperatorSnapshot()
	s.readCache("operator-upstream", &upstream)
	value.Patches = retainNewerPatchRecords(value.Patches, mergeOperatorSnapshots(upstream, local).Patches)
	changed := false
	if role == RoleSite {
		known := mergeOperatorSnapshots(upstream, local)
		siteID := s.Config().InstanceID
		knownRF := retainedRFIndex(known.Emergencies, siteID)
		zoneID := ""
		if s.view().Zone != nil {
			zoneID = s.view().Zone.ID
		}
		// Site management and Site-connected Application Server see their local operation,
		// together with Zone-wide incidents and patches relevant to this Site.
		filtered := emptyOperatorSnapshot()
		for _, e := range value.Emergencies {
			if e.ScopeID != zoneID && e.ScopeID != siteID {
				continue
			}
			if !retainedRFIdentityAllows(knownRF, e) {
				continue
			}
			if groupAtSite(s.view().System, e.TalkgroupID, siteID) {
				filtered.Emergencies = append(filtered.Emergencies, e)
			} else if completed, ok := retainedRFCompletion(knownRF, e, siteID); ok {
				filtered.Emergencies = append(filtered.Emergencies, completed)
			}
		}
		// Upstream snapshots replace their cache. Keep already trusted own-RF
		// history while unavailable and retain completion afterward, so omitted
		// or stale copies cannot clear by omission or reactivate an incident.
		history := emptyOperatorSnapshot()
		for _, original := range knownRF {
			if !groupAtSite(s.view().System, original.TalkgroupID, siteID) || !original.AcknowledgedAt.IsZero() || !original.ClearedAt.IsZero() {
				history.Emergencies = append(history.Emergencies, original)
			}
		}
		filtered.Emergencies = mergeOperatorSnapshots(filtered, history).Emergencies
		for _, l := range value.Locations {
			if (l.ScopeID == zoneID || l.ScopeID == s.Config().InstanceID) && groupAtSite(s.view().System, l.TalkgroupID, s.Config().InstanceID) {
				filtered.Locations = append(filtered.Locations, l)
			}
		}
		for _, p := range value.Patches {
			if p.ScopeID == zoneID || p.ScopeID == s.Config().InstanceID {
				filtered.Patches = append(filtered.Patches, p)
			}
		}
		value = filtered
		for i := range local.Patches {
			patch := &local.Patches[i]
			if patch.ScopeID != s.Config().InstanceID || !patch.Active || !patch.EndedAt.IsZero() {
				continue
			}
			for _, incoming := range value.Patches {
				if incoming.ScopeID == zoneID && incoming.Active && incoming.EndedAt.IsZero() && overlapGroups(patch.Talkgroups, incoming.Talkgroups) {
					patch.State, patch.Reason = "conflict", "A Zone patch now uses these talkgroups; end this Site-local patch before reactivating it."
					changed = true
				}
			}
		}
	}
	var acceptedLocal *OperatorSnapshot
	if changed {
		acceptedLocal = &local
	}
	if err := s.Store.acceptOperatorSnapshots(acceptedLocal, value); err != nil {
		return err
	}
	// Readers must see the reconciled local and upstream policy together.
	return s.publishOperatorPolicyLocked()
}

func (s *Service) activePatches() []RuntimePatch {
	patches := []RuntimePatch{}
	if policy := s.operatorPolicy.Load(); policy != nil {
		patches = slices.Clone(policy.patches)
	}
	role, id := s.Config().Role, s.Config().InstanceID
	zoneID := ""
	if s.view().Zone != nil {
		zoneID = s.view().Zone.ID
	}
	sort.SliceStable(patches, func(i, j int) bool { return patches[i].ScopeID == zoneID && patches[j].ScopeID != zoneID })
	out := []RuntimePatch{}
	used := []int{}
	for _, patch := range patches {
		if !patch.Active || !patch.EndedAt.IsZero() || patch.State == "conflict" || (role == RoleZone && patch.ScopeID != id) || (role == RoleSite && patch.ScopeID != id && patch.ScopeID != zoneID) {
			continue
		}
		if s.patchPolicy(patch) != nil || overlapGroups(used, patch.Talkgroups) {
			continue
		}
		patch.Talkgroups = slices.Clone(patch.Talkgroups)
		out = append(out, patch)
		used = append(used, patch.Talkgroups...)
	}
	return out
}
func (s *Service) expandPatchedGroups(groups []int) ([]int, error) {
	out := slices.Clone(groups)
	for _, patch := range s.activePatches() {
		if overlapGroups(out, patch.Talkgroups) {
			out = append(out, patch.Talkgroups...)
		}
	}
	sort.Ints(out)
	out = slices.Compact(out)
	if len(out) > 16 {
		return nil, errors.New("patch expansion exceeds the supported talkgroup count")
	}
	return out, nil
}
func (s *Service) activeEmergency(rid, tg int) bool {
	policy := s.operatorPolicy.Load()
	return policy != nil && policy.emergencies[[2]int{rid, tg}]
}

func (s *Service) operatorRadioExists(rid int) bool {
	return slices.ContainsFunc(s.view().System.Radios, func(r Radio) bool { return r.RID == rid && r.Enabled })
}

func validLocation(latitude, longitude, accuracy float64) bool {
	return !math.IsNaN(latitude) && !math.IsInf(latitude, 0) && !math.IsNaN(longitude) && !math.IsInf(longitude, 0) && !math.IsNaN(accuracy) && !math.IsInf(accuracy, 0) && latitude >= -90 && latitude <= 90 && longitude >= -180 && longitude <= 180 && accuracy >= 0 && accuracy <= 100000
}

// observeRFEmergency mirrors observed radio signaling. Operator incident clear
// remains separate from proof that a subscriber has left its emergency mode.
func (s *Service) observeRFEmergency(rid, tg int, active bool) error {
	if s.Config().Role != RoleSite {
		return errors.New("RF emergency observations belong to their Site")
	}
	if rid < 1 || rid > 0xFFFFFB || tg < 1 || tg > 65534 {
		return errors.New("RF emergency identity or group is invalid")
	}
	// A native-accepted cancellation may end its exact existing RF incident
	// after the group was removed or access changed. It cannot create one.
	if active && (!slices.ContainsFunc(s.view().System.Radios, func(r Radio) bool { return r.RID == rid && r.Kind == "subscriber" && r.Enabled }) || !groupAtSite(s.view().System, tg, s.Config().InstanceID)) {
		return errors.New("RF emergency identity or group is unavailable")
	}
	s.featureMu.Lock()
	defer s.featureMu.Unlock()
	local := s.operatorLocalUnlocked()
	knownRF := retainedRFIndex(s.operatorCombinedUnlocked().Emergencies, s.Config().InstanceID)
	changed := false
	now := time.Now().UTC()
	for i := range local.Emergencies {
		incident := &local.Emergencies[i]
		if incident.RadioID != rid || incident.TalkgroupID != tg || incident.SourceKind != "rf" {
			continue
		}
		// Application Server may have completed this own-RF identity upstream while its
		// original local copy still appears active. Keep that completion before
		// deciding whether this observation belongs to an existing incident.
		if known, ok := knownRF[incident.ID]; ok && sameRFIncident(*incident, known) {
			completed := mergeIncidentCompletion(*incident, known)
			changed = changed || completed != *incident
			*incident = completed
		}
		if !incident.ClearedAt.IsZero() {
			continue
		}
		incident.LastSeen = now
		if !active {
			incident.ClearedAt, incident.ClearedBy = now, "radio"
		}
		return s.writeOperatorCacheLocked("operator-local", local)
	}
	if !active {
		if changed {
			return s.writeOperatorCacheLocked("operator-local", local)
		}
		return nil
	}
	id, err := operatorID("emergency")
	if err != nil {
		return err
	}
	local.Emergencies = append(local.Emergencies, EmergencyIncident{ID: id, RadioID: rid, TalkgroupID: tg, ScopeID: s.Config().InstanceID, SourceKind: "rf", At: now, LastSeen: now})
	return s.writeOperatorCacheLocked("operator-local", local)
}

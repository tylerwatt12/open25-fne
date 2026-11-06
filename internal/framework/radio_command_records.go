package framework

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/radio"
)

const radioCommandWait = 15 * time.Second

// The Site owns command outcomes: a successful HTTP send is never a radio ACK.
// P25 responses do not echo an application operation ID. Only one matching
// target/action may be outstanding, and unrelated/generic ACKs never complete it.
type radioCommandReceipt struct {
	ID                     string    `json:"id"`
	State                  string    `json:"state"`
	Sent                   bool      `json:"sent"`
	AwaitingAcknowledgment bool      `json:"awaiting_acknowledgment"`
	SiteID                 string    `json:"site_id"`
	RadioID                int       `json:"radio_id"`
	TalkgroupID            int       `json:"talkgroup_id"`
	Action                 string    `json:"action"`
	Code                   *int      `json:"code,omitempty"`
	ActorUserID            string    `json:"actor_user_id,omitempty"`
	ActorUsername          string    `json:"actor_username,omitempty"`
	SourceRadioID          int       `json:"source_radio_id,omitempty"`
	PositionID             string    `json:"position_id,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
	ExpiresAt              time.Time `json:"expires_at"`
	SentAt                 time.Time `json:"sent_at,omitempty"`
	ResponseAt             time.Time `json:"response_at,omitempty"`
	ResponseKind           string    `json:"response_kind,omitempty"`
	ResponseEventID        string    `json:"response_event_id,omitempty"`
	ResponseCode           *int      `json:"response_code,omitempty"`
	Message                string    `json:"message"`
	AttemptedAt            time.Time `json:"attempted_at,omitempty"`
}

type radioCommandEnvelope struct {
	Input       radioCommandInput      `json:"input"`
	Actor       OperatorActor          `json:"actor"`
	TalkgroupID int                    `json:"talkgroup_id"`
	Inhibition  *radioInhibitionPolicy `json:"inhibition,omitempty"`
}

type commandSiteStatus struct {
	SiteID  string `json:"site_id"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}
type radioCommandListing struct {
	Commands []radioCommandReceipt `json:"commands"`
	Sites    []commandSiteStatus   `json:"sites"`
}

func commandPending(v radioCommandReceipt) bool {
	return v.State == "accepted" || v.State == "sent" || v.State == "unconfirmed"
}
func sameCommandCode(a, b *int) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func expireRadioCommands(rows []radioCommandReceipt, now time.Time) {
	for i := range rows {
		if commandPending(rows[i]) && !now.Before(rows[i].ExpiresAt) {
			previous := rows[i].State
			rows[i].State, rows[i].AwaitingAcknowledgment = "timed_out", false
			rows[i].Message = "No matching subscriber response was observed before this request expired."
			if previous == "unconfirmed" {
				// A lost Site/native reply leaves delivery uncertain, including
				// when the Zone has no local attempt timestamp. Expiry closes
				// the response window; it cannot establish an unsent command.
				rows[i].Message = "Command delivery remained unconfirmed when the response window expired."
			} else if rows[i].AttemptedAt.IsZero() {
				rows[i].State = "rejected"
				rows[i].Message = "Radio command not sent: it expired before the Site could send it."
			}
		}
	}
}
func (s *Store) radioCommandsLocked() ([]radioCommandReceipt, error) {
	var raw []byte
	err := s.db.QueryRow("SELECT document FROM framework_documents WHERE name='radio-commands'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []radioCommandReceipt{}, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []radioCommandReceipt
	err = json.Unmarshal(raw, &rows)
	return rows, err
}
func (s *Store) mutateRadioCommands(fn func(*[]radioCommandReceipt) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.radioCommandsLocked()
	if err != nil {
		return err
	}
	before, _ := json.Marshal(rows)
	if err = fn(&rows); err != nil {
		return err
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if bytes.Equal(before, raw) {
		return nil
	}
	_, err = s.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES('radio-commands',?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at", raw, nowStamp())
	return err
}
func (s *Store) radioCommands() ([]radioCommandReceipt, error) {
	var result []radioCommandReceipt
	err := s.mutateRadioCommands(func(rows *[]radioCommandReceipt) error { result = slices.Clone(*rows); return nil })
	return result, err
}
func (s *Service) commandEvent(v radioCommandReceipt) {
	audience := "operator"
	if v.ActorUserID != "" {
		audience = "dispatcher"
	}
	s.emit(activity.Event{ID: v.ID + "-" + v.State, RadioID: v.RadioID, TalkgroupID: v.TalkgroupID, SiteID: v.SiteID, Type: "radio_command", Audience: audience, SourceKind: "application", Outcome: v.State, Message: v.Message, Details: map[string]any{"operation_id": v.ID, "action": v.Action, "code": v.Code, "user_id": v.ActorUserID, "source_radio_id": v.SourceRadioID, "position_id": v.PositionID, "response_kind": v.ResponseKind, "response_event_id": v.ResponseEventID}})
}

func (s *Service) expireRadioCommandRecords() {
	var changed []radioCommandReceipt
	err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		before := slices.Clone(*rows)
		expireRadioCommands(*rows, time.Now().UTC())
		for i, row := range *rows {
			if row.State != before[i].State {
				changed = append(changed, row)
			}
		}
		return nil
	})
	if err != nil {
		s.runtimeError(err)
		return
	}
	for _, row := range changed {
		s.commandEvent(row)
	}
}
func (s *Service) commandUpdated(id string, fn func(*radioCommandReceipt)) (radioCommandReceipt, error) {
	var result radioCommandReceipt
	err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		for i := range *rows {
			if (*rows)[i].ID == id {
				fn(&(*rows)[i])
				result = (*rows)[i]
				return nil
			}
		}
		return errors.New("radio command record is unavailable")
	})
	if err == nil {
		s.commandEvent(result)
	}
	return result, err
}
func (s *Service) observeRadioCommand(e radio.Event) {
	if s.Store == nil || s.Config().Role != RoleSite || e.RID == 0 || e.Direction == "sent" {
		return
	}
	action := ""
	state := "acknowledged"
	switch e.Kind {
	case "radio_check_ack":
		if e.RequestorID != radio.NativeFNEID {
			return
		}
		action = "radio_check"
	case "radio_check_no_response", "inhibit_no_response", "uninhibit_no_response":
		if e.Direction != "state" || e.RequestorID != radio.NativeFNEID || e.Attempts != 5 {
			return
		}
		action, state = strings.TrimSuffix(e.Kind, "_no_response"), "timed_out"
	case "status_query_response":
		if e.Direction != "received" || e.RequestorID != radio.NativeFNEID || e.Code == nil || *e.Code < 0 || *e.Code > 65535 {
			return
		}
		action, state = "status_query", "response_observed"
	case "status_query_no_response":
		if e.Direction != "state" || e.RequestorID != radio.NativeFNEID || e.Attempts != 1 {
			return
		}
		action, state = "status_query", "timed_out"
	case "page_ack":
		action = "page"
	case "message_ack", "message_no_response":
		if e.RequestorID != radio.NativeFNEID || e.Code == nil || *e.Code < 0 || *e.Code > 65535 || e.Attempts != 1 {
			return
		}
		if e.Kind == "message_ack" && e.Direction != "received" {
			return
		}
		action = "message"
		if e.Kind == "message_no_response" {
			if e.Direction != "state" {
				return
			}
			state = "timed_out"
		}
	case "inhibit_ack":
		if e.RequestorID != radio.NativeFNEID {
			return
		}
		action = "inhibit"
	case "uninhibit_ack":
		if e.RequestorID != radio.NativeFNEID {
			return
		}
		action = "uninhibit"
	case "registered":
		action = "registration_demand"
		state = "response_observed"
	case "affiliated":
		if e.Direction != "state" || !strings.Contains(e.Message, "(Group Affiliation Query Response)") {
			return
		}
		action = "affiliation_query"
		state = "response_observed"
	default:
		return
	}
	now := e.At
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var changed *radioCommandReceipt
	err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		for i := range *rows {
			v := &(*rows)[i]
			if !commandPending(*v) || v.RadioID != e.RID || v.Action != action || v.AttemptedAt.IsZero() || now.Before(v.AttemptedAt) || !now.Before(v.ExpiresAt) {
				continue
			}
			if e.SiteID != "" && e.SiteID != v.SiteID {
				continue
			}
			if action == "message" && !sameCommandCode(v.Code, e.Code) {
				continue
			}
			v.State, v.AwaitingAcknowledgment, v.ResponseAt, v.ResponseKind, v.ResponseEventID = state, false, now, e.Kind, e.ID
			v.Sent = true
			if v.SentAt.IsZero() {
				v.SentAt = v.AttemptedAt
			}
			if action == "status_query" && state == "response_observed" {
				code := *e.Code
				v.ResponseCode = &code
			}
			v.Message = "Matching subscriber acknowledgment observed."
			if state == "response_observed" {
				v.Message = "Subscriber state was reported after the request; the air protocol does not provide an application transaction ID."
			} else if state == "timed_out" {
				switch action {
				case "status_query":
					v.Message = "No matching subscriber status response before the Site's response deadline."
				case "message":
					v.Message = "No matching subscriber acknowledgment before the Site's message response deadline."
				case "inhibit":
					v.Message = "No subscriber response after the Site's inhibit retries; inhibition remains enforced."
				case "uninhibit":
					v.Message = "No subscriber response after the Site's re-enable retries; inhibition remains pending."
				default:
					v.Message = "No subscriber response after the Site's radio-check retries."
				}
			}
			copy := *v
			changed = &copy
			break
		}
		return nil
	})
	if err != nil {
		s.runtimeError(err)
	} else if changed != nil {
		s.commandEvent(*changed)
	}
}
func (s *Service) siteCommandRows() ([]radioCommandReceipt, error) {
	s.expireRadioCommandRecords()
	rows, err := s.Store.radioCommands()
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.After(rows[j].CreatedAt) })
	if len(rows) > 100 {
		rows = rows[:100]
	}
	return rows, nil
}
func (s *Service) radioCommandsInternal(w http.ResponseWriter, r *http.Request, peer PeerCredential) {
	zone := s.view().Zone
	if s.Config().Role != RoleSite || peer.Role != RoleZone || zone == nil || peer.InstanceID != zone.ID {
		fail(w, 403, errors.New("this Zone cannot read Site command outcomes"))
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	rows, err := s.siteCommandRows()
	if err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, rows)
}
func (s *Service) commandRows(ctx context.Context) (radioCommandListing, error) {
	result := radioCommandListing{Commands: []radioCommandReceipt{}, Sites: []commandSiteStatus{}}
	if s.Config().Role == RoleSite {
		rows, err := s.siteCommandRows()
		result.Commands = rows
		state := commandSiteStatus{SiteID: s.Config().InstanceID, State: "available"}
		if err != nil {
			state.State, state.Message = "unavailable", "Site command outcomes could not be read."
		}
		result.Sites = append(result.Sites, state)
		return result, err
	}
	zone := s.view().Zone
	if s.Config().Role != RoleZone || zone == nil {
		return result, errors.New("command outcomes belong to Zone and Site controllers")
	}
	// Site IDs are unique and range from 1 through 254 in an accepted Zone.
	// Fan out to that bounded set rather than queuing healthy Sites behind
	// unreachable ones. Both administrator and operator reads share this bound.
	if len(zone.Sites) > 254 {
		return result, errors.New("Zone command history contains too many Sites")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	credentials, credentialErr := readCredentials(s.Config().CredentialFile)
	peers := map[string]PeerCredential{}
	if credentialErr == nil {
		for _, peer := range credentials.Peers {
			if peer.Role == RoleSite {
				peers[peer.InstanceID] = peer
			}
		}
	}
	type siteResult struct {
		status commandSiteStatus
		rows   []radioCommandReceipt
	}
	sites := make([]siteResult, len(zone.Sites))
	var workers sync.WaitGroup
	for i, site := range zone.Sites {
		sites[i].status = commandSiteStatus{SiteID: site.ID, State: "unavailable"}
		if site.Endpoint == "" {
			sites[i].status.Message = "Site server address is not configured."
			continue
		}
		peer, ok := peers[site.ID]
		if credentialErr != nil || !ok {
			sites[i].status.Message = "Site enrollment is unavailable."
			continue
		}
		workers.Add(1)
		go func(index int, site SiteIdentity, peer PeerCredential) {
			defer workers.Done()
			query, stop := context.WithTimeout(ctx, 1500*time.Millisecond)
			defer stop()
			var rows []radioCommandReceipt
			if err := s.requestAt(query, site.Endpoint, "GET", "/internal/radio/commands", nil, &rows, &peer); err != nil {
				sites[index].status.Message = "Site command outcomes could not be read."
				if errors.Is(query.Err(), context.DeadlineExceeded) {
					sites[index].status.Message = "Site did not respond before the command-history deadline."
				}
				return
			}
			if len(rows) > 100 {
				sites[index].status.Message = "Site returned an invalid command-history response."
				return
			}
			sites[index].status.State = "available"
			for _, row := range rows {
				if row.SiteID == site.ID {
					sites[index].rows = append(sites[index].rows, row)
				}
			}
		}(i, site, peer)
	}
	workers.Wait()
	out := []radioCommandReceipt{}
	for _, site := range sites {
		result.Sites = append(result.Sites, site.status)
		out = append(out, site.rows...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > 100 {
		out = out[:100]
	}
	result.Commands = out
	return result, nil
}
func (s *Service) operatorRadioCommands(authority operatorAuthority) (OperatorSnapshot, error) {
	if authority.admin {
		return OperatorSnapshot{}, errors.New("application sign-in is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := s.commandRows(ctx)
	if err != nil {
		return OperatorSnapshot{}, err
	}
	result := s.filterOperatorState(s.operatorSnapshot(), authority)
	result.Commands = []radioCommandReceipt{}
	result.CommandSites = rows.Sites
	for _, row := range rows.Commands {
		if row.ActorUserID == authority.actor.UserID && row.SourceRadioID == authority.actor.RadioID && s.operatorGroupAllowed(authority, row.TalkgroupID) && operatorPermitted(authority, row.Action) {
			result.Commands = append(result.Commands, row)
		}
	}
	return result, nil
}

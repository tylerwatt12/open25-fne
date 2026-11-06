package framework

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"
)

type historyScope struct {
	Role       Role   `json:"role"`
	InstanceID string `json:"instance_id"`
	Coverage   string `json:"coverage"`
}
type historyReadEnvelope struct {
	Actor OperatorActor `json:"actor"`
	Path  string        `json:"path"`
	Query string        `json:"query"`
}
type historyReadReply struct {
	Status     int             `json:"status"`
	Scope      historyScope    `json:"scope"`
	Data       json.RawMessage `json:"data,omitempty"`
	CSV        string          `json:"csv,omitempty"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Filename   string          `json:"filename,omitempty"`
}

func historyReadPath(path string) bool {
	return slices.Contains([]string{"/api/history/events", "/api/history/calls", "/api/history/locations", "/api/history/statistics"}, path)
}
func (s *Service) historyScope() historyScope {
	coverage := "connected_controller"
	if s.Config().Role == RoleSystem {
		coverage = "system_archive"
	} else if s.Config().Role == RoleApplication {
		coverage = "application_local"
	}
	return historyScope{s.Config().Role, s.Config().InstanceID, coverage}
}

func (s *Service) respondHistoryPage(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		fail(w, 500, errors.New("history could not be encoded"))
		return
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		fail(w, 500, errors.New("history could not be encoded"))
		return
	}
	data["scope"] = s.historyScope()
	if aliases := s.historyPageAliases(value); aliases != nil {
		data["radio_aliases"] = aliases
	}
	respond(w, status, data)
}

// History follows the established operator connection. A disconnected console
// does not substitute a partial local event buffer for a controller's history.
func (s *Service) proxyHistoryHTTP(w http.ResponseWriter, r *http.Request, session Session) {
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	if s.mediaClient == nil || s.mediaClient.URL() == "" {
		fail(w, 503, errors.New("the controller history is unavailable"))
		return
	}
	user, hash, err := s.Store.User(session.UserID)
	if err != nil || user.Username != session.Username {
		fail(w, 403, errors.New("the application account is unavailable"))
		return
	}
	resolved, _, err := resolveUserPosition(user, s.view().System, session.PositionID)
	if err != nil {
		fail(w, 403, err)
		return
	}
	input := historyReadEnvelope{Actor: OperatorActor{UserID: user.ID, RadioID: resolved.RadioID, Proof: userProof(user, hash)}, Path: r.URL.Path, Query: r.URL.RawQuery}
	var reply historyReadReply
	if err = s.requestAt(r.Context(), s.mediaClient.URL(), "POST", "/internal/operator-history", input, &reply, nil); err != nil {
		fail(w, 503, errors.New("the connected controller could not return this history page"))
		return
	}
	if reply.Status < 200 || reply.Status > 599 || (reply.Scope.Role != RoleZone && reply.Scope.Role != RoleSite) || reply.Scope.InstanceID == "" {
		fail(w, 502, errors.New("the controller supplied an invalid history response"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-History-Scope-Role", string(reply.Scope.Role))
	w.Header().Set("X-History-Scope-ID", reply.Scope.InstanceID)
	if reply.CSV != "" {
		if reply.Filename != "events" && reply.Filename != "calls" && reply.Filename != "locations" {
			fail(w, 502, errors.New("the controller supplied an invalid export name"))
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+reply.Filename+`.csv"`)
		if reply.NextCursor != "" {
			w.Header().Set("X-Next-Cursor", reply.NextCursor)
		}
		w.WriteHeader(reply.Status)
		_, _ = w.Write([]byte(reply.CSV))
		return
	}
	var data map[string]any
	if json.Unmarshal(reply.Data, &data) != nil {
		fail(w, 502, errors.New("the controller supplied invalid history data"))
		return
	}
	data["scope"] = reply.Scope
	respond(w, reply.Status, data)
}

type historyCapture struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
}

func (c *historyCapture) Header() http.Header { return c.header }
func (c *historyCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}
func (c *historyCapture) Write(data []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	if c.body.Len()+len(data) > 3<<20 {
		c.overflow = true
		return 0, errors.New("history page exceeds transport limit")
	}
	return c.body.Write(data)
}

func (s *Service) operatorHistoryInternal(w http.ResponseWriter, r *http.Request, peer PeerCredential) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	if !s.operatorScopeAllowed(peer) {
		fail(w, 403, errors.New("this service cannot read operator history here"))
		return
	}
	var input historyReadEnvelope
	if err := readJSON(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	if !historyReadPath(input.Path) || len(input.Query) > 4096 {
		fail(w, 400, errors.New("select an available history view"))
		return
	}
	query, err := url.ParseQuery(input.Query)
	if err != nil {
		fail(w, 400, errors.New("invalid history filter"))
		return
	}
	for key, values := range query {
		if !slices.Contains([]string{"limit", "cursor", "radio_id", "talkgroup_id", "site_id", "audience", "type", "source_kind", "channel_id", "call_id", "since", "until", "format"}, key) || len(values) != 1 {
			fail(w, 400, errors.New("invalid history filter"))
			return
		}
	}
	authority, err := s.operatorActor(input.Actor)
	if err != nil || authority.admin {
		fail(w, 403, errors.New("operator history requires a current application identity"))
		return
	}
	session := Session{UserID: authority.user.ID, Username: authority.user.Username, Expires: time.Now().Add(time.Minute)}
	if authority.position != nil {
		session.PositionID = authority.position.ID
	}
	local := r.Clone(r.Context())
	local.Method = "GET"
	copyURL := *r.URL
	copyURL.Path, copyURL.RawQuery = input.Path, query.Encode()
	local.URL = &copyURL
	capture := &historyCapture{header: http.Header{}}
	s.historyHTTP(capture, local, session)
	if capture.overflow {
		fail(w, 413, errors.New("this history page is too large; request a smaller limit"))
		return
	}
	reply := historyReadReply{Status: capture.status, Scope: s.historyScope()}
	if reply.Status == 0 {
		reply.Status = 200
	}
	if capture.header.Get("Content-Type") == "text/csv; charset=utf-8" {
		reply.CSV = capture.body.String()
		reply.NextCursor = capture.header.Get("X-Next-Cursor")
		reply.Filename = input.Path[len("/api/history/"):]
	} else {
		reply.Data = append(json.RawMessage{}, capture.body.Bytes()...)
	}
	respond(w, 200, reply)
}

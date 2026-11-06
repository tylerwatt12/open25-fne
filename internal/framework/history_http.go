package framework

import (
	"context"
	"encoding/csv"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
)

// Names accompany only entities in this already scoped page. The current
// accepted directory is consulted without exposing unrelated radio entries.
func (s *Service) historyPageAliases(value any) map[int]string {
	observed := map[int]bool{}
	switch page := value.(type) {
	case activity.EventPage:
		for _, event := range page.Events {
			observed[event.RadioID] = true
		}
	case activity.CallPage:
		for _, call := range page.Calls {
			observed[call.RadioID] = true
		}
	case activity.LocationPage:
		for _, location := range page.Locations {
			observed[location.RadioID] = true
		}
	default:
		return nil
	}
	aliases := map[int]string{}
	for _, radio := range s.view().System.Radios {
		if radio.RID > 0 && observed[radio.RID] {
			aliases[radio.RID] = radio.Alias
		}
	}
	return aliases
}

func (s *Service) historyQuery(r *http.Request, session Session) (activity.Query, error) {
	values := r.URL.Query()
	q := activity.Query{Context: r.Context(), Limit: 200, SiteID: values.Get("site_id"), Audience: values.Get("audience"), Type: values.Get("type"), SourceKind: values.Get("source_kind"), ChannelID: values.Get("channel_id"), CallID: values.Get("call_id"), Cursor: values.Get("cursor")}
	for _, f := range []struct {
		name  string
		value *int
		max   int
	}{{"radio_id", &q.RadioID, 0xFFFFFE}, {"talkgroup_id", &q.TalkgroupID, 65535}, {"limit", &q.Limit, 1000}} {
		if raw := values.Get(f.name); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 1 || v > f.max {
				return q, errors.New("invalid " + f.name)
			}
			*f.value = v
		}
	}
	for _, f := range []struct {
		name  string
		value *time.Time
	}{{"since", &q.Since}, {"until", &q.Until}} {
		if raw := values.Get(f.name); raw != "" {
			v, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return q, errors.New("invalid " + f.name + "; use an RFC3339 date and time")
			}
			*f.value = v
		}
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && q.Since.After(q.Until) {
		return q, errors.New("since must be before until")
	}
	if q.Audience != "" && !slices.Contains([]string{"dispatcher", "operator", "diagnostic", "security"}, q.Audience) {
		return q, errors.New("invalid audience")
	}
	if s.Config().Role == RoleSite {
		q.SiteID = s.Config().InstanceID
	}
	if session.UserID != "" {
		record, ok := s.view().Users[session.UserID]
		if !ok {
			return q, dispatch.ErrPermission
		}
		user, _, err := resolveUserPosition(record.User, s.view().System, session.PositionID)
		if err != nil {
			return q, dispatch.ErrPermission
		}
		q.Restricted = true
		q.Groups = user.Talkgroups
		q.Audience = ""
		if !slices.Contains(user.Permissions, "location_view") {
			q.LocationRadioID = user.RadioID
		}
		if r.URL.Path == "/api/history/locations" && !slices.Contains(user.Permissions, "location_view") {
			if q.RadioID != 0 && q.RadioID != user.RadioID {
				return q, dispatch.ErrPermission
			}
			q.RadioID = user.RadioID
		}
	}
	return q, nil
}

func (s *Service) historyHTTP(w http.ResponseWriter, r *http.Request, session Session) {
	if s.Config().Role == RoleApplication && session.UserID != "" && historyReadPath(r.URL.Path) {
		s.proxyHistoryHTTP(w, r, session)
		return
	}
	w.Header().Set("X-History-Scope-Role", string(s.Config().Role))
	w.Header().Set("X-History-Scope-ID", s.Config().InstanceID)
	if r.URL.Path == "/api/history/health" {
		if session.UserID != "" {
			fail(w, http.StatusForbidden, dispatch.ErrPermission)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		respond(w, http.StatusOK, s.activity.IngestionHealth())
		return
	}
	if r.URL.Path == "/api/history/settings" {
		if session.UserID != "" {
			fail(w, 403, dispatch.ErrPermission)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		storageFailure := func(err error) {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				fail(w, http.StatusServiceUnavailable, errors.New("history storage is busy; try again"))
				return
			}
			fail(w, http.StatusInternalServerError, errors.New("history storage is unavailable"))
		}
		switch r.Method {
		case "GET":
			v, err := s.activity.HistoryStatus(ctx)
			if err != nil {
				storageFailure(err)
				return
			}
			respond(w, 200, v)
		case "PUT":
			var input activity.Retention
			if err := readJSON(w, r, &input); err != nil {
				fail(w, 400, err)
				return
			}
			if err := s.activity.SaveRetention(ctx, input); err != nil {
				storageFailure(err)
				return
			}
			v, err := s.activity.HistoryStatus(ctx)
			if err != nil {
				storageFailure(err)
				return
			}
			respond(w, 200, v)
		default:
			w.WriteHeader(405)
		}
		return
	}
	if !slices.Contains([]string{"/api/history/events", "/api/history/calls", "/api/history/locations", "/api/history/statistics"}, r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	q, err := s.historyQuery(r, session)
	if err != nil {
		status := 400
		if errors.Is(err, dispatch.ErrPermission) {
			status = 403
		}
		fail(w, status, err)
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "csv" {
		fail(w, 400, errors.New("format must be csv"))
		return
	}
	switch r.URL.Path {
	case "/api/history/events":
		page, err := s.activity.EventHistory(q)
		if err != nil {
			fail(w, 400, err)
			return
		}
		if format == "csv" {
			writer := historyCSVWriter(w, "events", page.NextCursor)
			if r.Context().Err() != nil || writer.Write([]string{"Event ID", "Time", "Origin", "Site", "Channel", "Radio ID", "Talkgroup ID", "Call ID", "Type", "Label", "Message", "Audience", "Severity", "Outcome", "Source"}) != nil {
				return
			}
			for _, e := range page.Events {
				if r.Context().Err() != nil || writer.Write([]string{csvText(e.ID), historyTime(e.At), csvText(e.Origin), csvText(e.SiteID), csvText(e.ChannelID), strconv.Itoa(e.RadioID), strconv.Itoa(e.TalkgroupID), csvText(e.CallID), csvText(e.Type), csvText(e.Label), csvText(e.Message), csvText(e.Audience), csvText(e.Severity), csvText(e.Outcome), csvText(e.SourceKind)}) != nil {
					return
				}
			}
			if r.Context().Err() == nil {
				writer.Flush()
			}
			return
		}
		s.respondHistoryPage(w, 200, page)
	case "/api/history/calls":
		page, err := s.activity.CallHistory(q)
		if err != nil {
			fail(w, 400, err)
			return
		}
		if format == "csv" {
			writer := historyCSVWriter(w, "calls", page.NextCursor)
			if r.Context().Err() != nil || writer.Write([]string{"Call ID", "Radio ID", "Source", "Call start", "Call end", "Call lifetime seconds", "Site", "Talkgroup ID", "Channel", "Delivery start", "Delivery end", "Outcome", "Allocated seconds", "Speech seconds"}) != nil {
				return
			}
			for _, c := range page.Calls {
				for _, d := range c.Destinations {
					if r.Context().Err() != nil || writer.Write([]string{csvText(c.ID), strconv.Itoa(c.RadioID), csvText(c.SourceKind), historyTime(c.StartedAt), historyEnd(c.EndedAt), historySeconds(c.LifetimeMS), csvText(d.SiteID), strconv.Itoa(d.TalkgroupID), csvText(d.ChannelID), historyTime(d.StartedAt), historyEnd(d.EndedAt), csvText(d.Outcome), historySeconds(d.AllocatedMS), historySeconds(d.SpeechMS)}) != nil {
						return
					}
				}
			}
			if r.Context().Err() == nil {
				writer.Flush()
			}
			return
		}
		s.respondHistoryPage(w, 200, page)
	case "/api/history/locations":
		page, err := s.activity.LocationHistory(q, time.Now().UTC())
		if err != nil {
			fail(w, 400, err)
			return
		}
		if format == "csv" {
			writer := historyCSVWriter(w, "locations", page.NextCursor)
			if r.Context().Err() != nil || writer.Write([]string{"Observation ID", "Radio ID", "Talkgroup ID", "Site", "Scope", "Latitude", "Longitude", "Accuracy metres", "Fix time", "Received time", "Source", "Age seconds", "Stale"}) != nil {
				return
			}
			for _, p := range page.Locations {
				if r.Context().Err() != nil || writer.Write([]string{csvText(p.ID), strconv.Itoa(p.RadioID), strconv.Itoa(p.TalkgroupID), csvText(p.SiteID), csvText(p.ScopeID), strconv.FormatFloat(p.Latitude, 'f', -1, 64), strconv.FormatFloat(p.Longitude, 'f', -1, 64), strconv.FormatFloat(p.Accuracy, 'f', -1, 64), historyTime(p.FixAt), historyTime(p.ReceivedAt), csvText(p.SourceKind), strconv.FormatInt(p.AgeSeconds, 10), strconv.FormatBool(p.Stale)}) != nil {
					return
				}
			}
			if r.Context().Err() == nil {
				writer.Flush()
			}
			return
		}
		s.respondHistoryPage(w, 200, page)
	case "/api/history/statistics":
		if format != "" {
			fail(w, 400, errors.New("CSV export is available for events, calls and locations"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		q.Context = ctx
		v, err := s.activity.Statistics(q)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				fail(w, http.StatusServiceUnavailable, errors.New("statistics took too long; select a shorter date range and try again"))
				return
			}
			fail(w, 400, err)
			return
		}
		s.respondHistoryPage(w, 200, v)
	}
}
func historyTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func historyEnd(t *time.Time) string {
	if t == nil {
		return ""
	}
	return historyTime(*t)
}
func historySeconds(n *int64) string {
	if n == nil {
		return ""
	}
	value, _ := webScaleDecimal(strconv.FormatInt(*n, 10), -3)
	return value
}
func csvText(s string) string {
	value := strings.TrimLeft(s, " \t\r\n")
	if value != "" && strings.ContainsRune("=+-@", rune(value[0])) {
		return "'" + s
	}
	return s
}
func historyCSVWriter(w http.ResponseWriter, name, cursor string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	if cursor != "" {
		w.Header().Set("X-Next-Cursor", cursor)
	}
	return csv.NewWriter(w)
}

package framework

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func TestHistoryCSVBytesScopeAndContinuation(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	fix := time.Now().UTC().Add(time.Hour)
	for _, kind := range []string{"events", "calls", "locations"} {
		t.Run(kind, func(t *testing.T) {
			s, user := historyFixture(t)
			user.Permissions = append(user.Permissions, "location_view")
			record := s.view().Users[user.ID]
			record.User = user
			s.view().Users[user.ID] = record
			for i, name := range []string{"new", "old", "forbidden"} {
				tg := 1001
				if name == "forbidden" {
					tg = 1002
				}
				e := activity.Event{ID: kind + "-" + name, At: at.Add(-time.Duration(i) * time.Minute), ReceivedAt: at, Origin: "site-1", SiteID: "site-1", ChannelID: "+voice", RadioID: 10001, TalkgroupID: tg, Type: "status", Label: "=Ready", Message: "first line,\nsecond \"line\"", Audience: "dispatcher", Severity: "notice", Outcome: "received", SourceKind: "rf"}
				switch kind {
				case "calls":
					e.CallID, e.Type = "call-"+name, "call_start"
				case "locations":
					e.Type, e.Details = "location", map[string]any{"latitude": 41.5, "longitude": -81.7, "accuracy": 5, "fix_at": fix.Add(-time.Duration(i) * time.Minute), "scope_id": "=scope"}
				}
				if err := s.activity.Append(e); err != nil {
					t.Fatal(err)
				}
				if kind == "calls" {
					e.ID += "-end"
					e.Type, e.At = "call_end", e.At.Add(time.Second)
					e.Details = map[string]any{"allocated_ms": 900, "speech_ms": 500}
					if err := s.activity.Append(e); err != nil {
						t.Fatal(err)
					}
				}
			}
			cursor := ""
			for i, name := range []string{"new", "old"} {
				query := url.Values{"limit": {"1"}, "cursor": {cursor}}
				path := "/api/history/" + kind + "?" + query.Encode()
				page := historyGet(s, path, Session{UserID: user.ID})
				var metadata struct {
					NextCursor string              `json:"next_cursor"`
					Locations  []activity.Location `json:"locations"`
				}
				if page.Code != 200 || json.Unmarshal(page.Body.Bytes(), &metadata) != nil {
					t.Fatalf("page: HTTP%d %s", page.Code, page.Body)
				}
				got := historyGet(s, path+"&format=csv", Session{UserID: user.ID})
				start := at.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
				var rows [][]string
				switch kind {
				case "events":
					rows = [][]string{{"Event ID", "Time", "Origin", "Site", "Channel", "Radio ID", "Talkgroup ID", "Call ID", "Type", "Label", "Message", "Audience", "Severity", "Outcome", "Source"}, {kind + "-" + name, start, "site-1", "site-1", "'+voice", "10001", "1001", "", "status", "'=Ready", "first line,\nsecond \"line\"", "dispatcher", "notice", "received", "rf"}}
				case "calls":
					rows = [][]string{{"Call ID", "Radio ID", "Source", "Call start", "Call end", "Call lifetime seconds", "Site", "Talkgroup ID", "Channel", "Delivery start", "Delivery end", "Outcome", "Allocated seconds", "Speech seconds"}, {"call-" + name, "10001", "rf", start, at.Add(-time.Duration(i)*time.Minute + time.Second).Format(time.RFC3339Nano), "1", "site-1", "1001", "'+voice", start, at.Add(-time.Duration(i)*time.Minute + time.Second).Format(time.RFC3339Nano), "received", "0.9", "0.5"}}
				case "locations":
					rows = [][]string{{"Observation ID", "Radio ID", "Talkgroup ID", "Site", "Scope", "Latitude", "Longitude", "Accuracy metres", "Fix time", "Received time", "Source", "Age seconds", "Stale"}, {kind + "-" + name, "10001", "1001", "site-1", "'=scope", "41.5", "-81.7", "5", fix.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339Nano), metadata.Locations[0].ReceivedAt.UTC().Format(time.RFC3339Nano), "rf", "0", "false"}}
				}
				var expected bytes.Buffer
				if err := csv.NewWriter(&expected).WriteAll(rows); err != nil {
					t.Fatal(err)
				}
				if got.Code != 200 || got.Body.String() != expected.String() {
					t.Fatalf("%s page%d CSV differs:\n%s\nwant:\n%s", kind, i, got.Body, &expected)
				}
				if got.Header().Get("Content-Type") != "text/csv; charset=utf-8" || got.Header().Get("Content-Disposition") != `attachment; filename="`+kind+`.csv"` || got.Header().Get("Cache-Control") != "no-store" || got.Header().Get("X-History-Scope-Role") != "zone" || got.Header().Get("X-History-Scope-ID") != "zone-1" || got.Header().Get("X-Next-Cursor") != metadata.NextCursor {
					t.Fatal("CSV response metadata changed", got.Header())
				}
				if i == 0 && metadata.NextCursor == "" || i == 1 && kind != "events" && metadata.NextCursor != "" {
					t.Fatal("scope or continuation changed")
				}
				cursor = metadata.NextCursor
			}
			if kind == "events" {
				last := historyGet(s, "/api/history/events?format=csv&limit=1&cursor="+url.QueryEscape(cursor), Session{UserID: user.ID})
				rows, err := csv.NewReader(strings.NewReader(last.Body.String())).ReadAll()
				if last.Code != 200 || err != nil || len(rows) != 1 || last.Header().Get("X-Next-Cursor") != "" {
					t.Fatal("end-of-page cursor leaked the forbidden group", last.Body, err)
				}
			}
		})
	}
}

func TestHistoryCSVSecondsAreExactAndJSONStaysMilliseconds(t *testing.T) {
	s, user := historyFixture(t)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	for _, id := range []string{"completed", "open"} {
		event := activity.Event{ID: id + "-start", CallID: id, At: at, Origin: "site-1", SiteID: "site-1", ChannelID: "voice-1", RadioID: 10001, TalkgroupID: 1001, Type: "call_start", Audience: "dispatcher", SourceKind: "rf"}
		if err := s.activity.Append(event); err != nil {
			t.Fatal(err)
		}
		if id == "completed" {
			event.ID, event.Type, event.At = id+"-end", "call_end", at.Add(1501*time.Millisecond)
			event.Details = map[string]any{"allocated_ms": 1200, "speech_ms": 1}
			if err := s.activity.Append(event); err != nil {
				t.Fatal(err)
			}
		}
		path := "/api/history/calls?call_id=" + id
		before := historyGet(s, path, Session{UserID: user.ID})
		var page activity.CallPage
		if before.Code != 200 || json.Unmarshal(before.Body.Bytes(), &page) != nil || len(page.Calls) != 1 {
			t.Fatalf("JSON call page: HTTP%d %s", before.Code, before.Body)
		}
		csvPage := historyGet(s, path+"&format=csv", Session{UserID: user.ID})
		rows, err := csv.NewReader(strings.NewReader(csvPage.Body.String())).ReadAll()
		if csvPage.Code != 200 || err != nil || len(rows) != 2 {
			t.Fatalf("CSV call page: HTTP%d %v %s", csvPage.Code, err, csvPage.Body)
		}
		if rows[0][5] != "Call lifetime seconds" || rows[0][12] != "Allocated seconds" || rows[0][13] != "Speech seconds" {
			t.Fatal("CSV duration units are not explicit", rows[0])
		}
		call, values := page.Calls[0], []string{rows[1][5], rows[1][12], rows[1][13]}
		if id == "completed" {
			if call.LifetimeMS == nil || *call.LifetimeMS != 1501 || call.Destinations[0].AllocatedMS == nil || *call.Destinations[0].AllocatedMS != 1200 || call.Destinations[0].SpeechMS == nil || *call.Destinations[0].SpeechMS != 1 || strings.Join(values, ",") != "1.501,1.2,0.001" {
				t.Fatal("CSV seconds or unchanged JSON milliseconds differ", string(before.Body.Bytes()), values)
			}
		} else if call.LifetimeMS != nil || call.Destinations[0].AllocatedMS != nil || call.Destinations[0].SpeechMS != nil || strings.Join(values, ",") != ",," {
			t.Fatal("unknown measurements became zero", call, values)
		}
		after := historyGet(s, path, Session{UserID: user.ID})
		if after.Code != before.Code || !bytes.Equal(after.Body.Bytes(), before.Body.Bytes()) {
			t.Fatal("CSV presentation changed JSON or stored call measurements", before.Body, after.Body)
		}
	}
}

func TestHistorySecondsPreservesInt64Precision(t *testing.T) {
	if historySeconds(nil) != "" {
		t.Fatal("unknown duration must remain blank")
	}
	for _, test := range []struct {
		value int64
		want  string
	}{{0, "0"}, {1, "0.001"}, {1000, "1"}, {1001, "1.001"}, {1200, "1.2"}, {-1, "-0.001"}, {-9223372036854775808, "-9223372036854775.808"}, {9223372036854775807, "9223372036854775.807"}} {
		if got := historySeconds(&test.value); got != test.want {
			t.Fatalf("%d milliseconds became %q seconds; want %q", test.value, got, test.want)
		}
	}
}

func expandedCSVFixture(t *testing.T) *Service {
	t.Helper()
	s, _ := historyFixture(t)
	at := time.Now().UTC().Add(-time.Minute)
	for i := range 256 {
		e := activity.Event{ID: fmt.Sprintf("expanded-%d", i), At: at, Origin: "site-1", SiteID: fmt.Sprintf("site-%03d", i), ChannelID: "voice-1", RadioID: 10001, TalkgroupID: 1001, CallID: "one-expanded-call", Type: "call_start", Audience: "dispatcher", SourceKind: "rf"}
		if err := s.activity.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.activity.CallHistory(activity.Query{Limit: 1})
	if err != nil || len(page.Calls) != 1 || len(page.Calls[0].Destinations) != 256 {
		t.Fatalf("expanded fixture must retain every destination: %+v %v", page, err)
	}
	return s
}

func TestHistoryCSVExpandedCallKeepsEveryDestination(t *testing.T) {
	s := expandedCSVFixture(t)
	w := historyGet(s, "/api/history/calls?limit=1&format=csv", Session{})
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if w.Code != 200 || err != nil || len(rows) != 257 {
		t.Fatalf("expanded CSV: HTTP%d rows%d %v", w.Code, len(rows), err)
	}
	for i, row := range rows[1:] {
		if len(row) != 14 || row[0] != "one-expanded-call" || row[6] != fmt.Sprintf("site-%03d", i) {
			t.Fatalf("destination%d missing or reordered: %v", i, row)
		}
	}
}

type interruptedCSVResponse struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	err    error
	writes int
}

func (w *interruptedCSVResponse) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 1 && w.cancel != nil {
		w.cancel()
	}
	if w.err != nil {
		return 0, w.err
	}
	return w.ResponseRecorder.Write(p)
}

func TestHistoryCSVStopsAfterCancellationOrWriteFailure(t *testing.T) {
	s := expandedCSVFixture(t)
	for _, failure := range []string{"cancel", "write"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := &interruptedCSVResponse{ResponseRecorder: httptest.NewRecorder()}
			if failure == "cancel" {
				w.cancel = cancel
			} else {
				w.err = errors.New("client stopped reading")
			}
			r := httptest.NewRequest(http.MethodGet, "/api/history/calls?limit=1&format=csv", nil).WithContext(ctx)
			s.historyHTTP(w, r, Session{})
			if w.writes != 1 {
				t.Fatalf("export continued after %s: %d writes", failure, w.writes)
			}
			if w.Body.Len() > 4096 || strings.Contains(w.Body.String(), "site-255") {
				t.Fatal("export continued through later destinations after client left")
			}
			if failure == "write" && w.Body.Len() != 0 {
				t.Fatal("failed CSV write was followed by another response")
			}
		})
	}
}

package framework

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const messagingHTTPPassword = "messaging-integration-password"

type messagingHTTPMessage struct {
	ID             int64     `json:"id"`
	ConversationID string    `json:"conversation_id"`
	ClientID       string    `json:"client_id"`
	SenderID       string    `json:"sender_id"`
	SenderName     string    `json:"sender_name"`
	Body           string    `json:"body"`
	CreatedAt      time.Time `json:"created_at"`
}

type messagingHTTPThread struct {
	ID            string                `json:"id"`
	Kind          string                `json:"kind"`
	Name          string                `json:"name"`
	Members       []string              `json:"members"`
	CreatedBy     string                `json:"created_by"`
	LastMessage   *messagingHTTPMessage `json:"last_message"`
	UnreadCount   int64                 `json:"unread_count"`
	ReadThroughID int64                 `json:"read_through_id"`
}

type messagingHTTPFixture struct {
	cluster                            *integrationCluster
	desk, ptt, sameDesk, outsider      User
	deskClient, pttClient, otherClient *integrationClient
}

func newMessagingHTTPFixture(t *testing.T) *messagingHTTPFixture {
	t.Helper()
	c := integrationNewCluster(t)
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	system.Radios = append(system.Radios,
		Radio{RID: 10002, Alias: "Message desk", Enabled: true, Kind: "console"},
		Radio{RID: 10003, Alias: "Message PTT", Enabled: true, Kind: "mobile"})
	system.ConsolePositions = []ConsolePosition{{ID: "message-desk", Name: "Message desk", RadioID: 10002, Enabled: true, Talkgroups: []int{1001, 1002}}}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	desk := fixtureUser()
	desk.ID, desk.Username = "message-desk-person", "message-dispatcher"
	desk.PositionIDs, desk.Profile.PreferredPositionID = []string{"message-desk"}, "message-desk"
	// The radio coded-message permission is separate from ordinary person chat.
	desk.Permissions = []string{"ptt"}
	ptt := cloneUser(t, desk)
	ptt.ID, ptt.Username, ptt.Role, ptt.RadioID = "message-ptt-person", "message-mobile", "mobile", 10003
	ptt.PositionIDs, ptt.Profile.PreferredPositionID = []string{}, ""
	sameDesk := cloneUser(t, desk)
	sameDesk.ID, sameDesk.Username = "message-next-shift", "message-next-shift"
	outsider := cloneUser(t, desk)
	outsider.ID, outsider.Username = "message-outsider", "message-outsider"
	disabled := cloneUser(t, desk)
	disabled.ID, disabled.Username, disabled.Enabled = "message-disabled", "message-disabled", false
	for _, user := range []User{desk, ptt, sameDesk, outsider, disabled} {
		if err := c.system.service.Store.SaveUser(user.ID, user, messagingHTTPPassword); err != nil {
			t.Fatal(err)
		}
	}
	integrationSyncAll(t, c)
	return &messagingHTTPFixture{cluster: c, desk: desk, ptt: ptt, sameDesk: sameDesk, outsider: outsider,
		deskClient: messagingHTTPLogin(t, c.dispatch, desk), pttClient: messagingHTTPLogin(t, c.dispatch, ptt), otherClient: messagingHTTPLogin(t, c.dispatch, outsider)}
}

func messagingHTTPLogin(t *testing.T, service *integrationService, person User) *integrationClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &integrationClient{base: service.web.URL, http: &http.Client{Jar: jar, Timeout: 3 * time.Second}}
	status, raw := integrationHTTP(t, c.http, http.MethodPost, c.base+"/api/login", map[string]string{"username": person.Username, "password": messagingHTTPPassword}, map[string]string{"Origin": c.base})
	if status != http.StatusOK {
		t.Fatalf("person login: HTTP %d %s", status, raw)
	}
	var login struct {
		CSRF string `json:"csrf"`
	}
	if err = json.Unmarshal(raw, &login); err != nil || login.CSRF == "" {
		t.Fatalf("person login did not return a CSRF token: %s %v", raw, err)
	}
	c.csrf = login.CSRF
	return c
}

func messagingHTTPCreate(t *testing.T, c *integrationClient, kind, name string, members []string) messagingHTTPThread {
	t.Helper()
	status, raw := c.request(t, http.MethodPost, "/api/messages/threads", map[string]any{"kind": kind, "name": name, "members": members})
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("create conversation: HTTP %d %s", status, raw)
	}
	var result struct {
		Thread messagingHTTPThread `json:"thread"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Thread.ID == "" {
		t.Fatalf("missing conversation: %s %v", raw, err)
	}
	return result.Thread
}

func messagingHTTPSend(t *testing.T, c *integrationClient, threadID, clientID, body string) messagingHTTPMessage {
	t.Helper()
	status, raw := c.request(t, http.MethodPost, "/api/messages/threads/"+threadID+"/messages", map[string]string{"client_id": clientID, "body": body})
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("send person message: HTTP %d %s", status, raw)
	}
	var result struct {
		Message    messagingHTTPMessage `json:"message"`
		Delivery   string               `json:"delivery"`
		RFDelivery string               `json:"rf_delivery"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Message.ID == 0 || result.Delivery != "stored" || result.RFDelivery != "pending" {
		t.Fatalf("message result claimed the wrong delivery: %s %v", raw, err)
	}
	return result.Message
}

func messagingHTTPList(t *testing.T, c *integrationClient) []messagingHTTPThread {
	t.Helper()
	status, raw := c.request(t, http.MethodGet, "/api/messages/threads", nil)
	if status != http.StatusOK {
		t.Fatalf("conversation list: HTTP %d %s", status, raw)
	}
	var result struct {
		Threads []messagingHTTPThread `json:"threads"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result.Threads
}

func TestMessagingHTTPPersonInboxKeepsIdentityContentAndExplicitRead(t *testing.T) {
	f := newMessagingHTTPFixture(t)
	status, raw := f.deskClient.request(t, http.MethodGet, "/api/messages/people", nil)
	if status != http.StatusOK {
		t.Fatalf("recipient list: HTTP %d %s", status, raw)
	}
	var directory struct {
		People []map[string]json.RawMessage `json:"people"`
	}
	if err := json.Unmarshal(raw, &directory); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, person := range directory.People {
		if len(person) != 2 || person["id"] == nil || person["username"] == nil {
			t.Fatalf("recipient directory exposed more than public person identity: %s", raw)
		}
		var id string
		if err := json.Unmarshal(person["id"], &id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if !slices.Contains(ids, f.ptt.ID) || slices.Contains(ids, "message-disabled") {
		t.Fatalf("recipient directory did not distinguish enabled people: %s", raw)
	}
	thread := messagingHTTPCreate(t, f.deskClient, "direct", "", []string{f.ptt.ID})
	if thread.CreatedBy != f.desk.ID || !slices.Contains(thread.Members, f.desk.ID) || !slices.Contains(thread.Members, f.ptt.ID) || len(thread.Members) != 2 {
		t.Fatalf("conversation used a radio/desk identity instead of its people: %#v", thread)
	}
	body := "Meet at café — 日本語\n<script>alert('plain text')</script>\nKeep  two  spaces."
	message := messagingHTTPSend(t, f.deskClient, thread.ID, "desk-first", body)
	if message.Body != body || message.SenderID != f.desk.ID || message.SenderName != f.desk.Username || message.ConversationID != thread.ID || message.CreatedAt.IsZero() {
		t.Fatalf("stored message changed its content or person identity: %#v", message)
	}
	if message.SenderID == strconv.Itoa(10002) {
		t.Fatal("desk radio identity became the message author")
	}
	for read := 0; read < 2; read++ {
		status, raw = f.pttClient.request(t, http.MethodGet, "/api/messages/threads/"+thread.ID, nil)
		var detail struct {
			Thread     messagingHTTPThread    `json:"thread"`
			Messages   []messagingHTTPMessage `json:"messages"`
			RFDelivery string                 `json:"rf_delivery"`
		}
		if err := json.Unmarshal(raw, &detail); status != http.StatusOK || err != nil || len(detail.Messages) != 1 || detail.Messages[0].Body != body || detail.RFDelivery != "pending" || detail.Thread.UnreadCount != 1 {
			t.Fatalf("GET changed read state or lost actual text: HTTP %d %s %v", status, raw, err)
		}
	}
	status, raw = f.pttClient.request(t, http.MethodPost, "/api/messages/threads/"+thread.ID+"/read", map[string]int64{"message_id": message.ID})
	var receipt struct {
		ReadThroughID int64 `json:"read_through_id"`
	}
	if err := json.Unmarshal(raw, &receipt); status != http.StatusOK || err != nil || receipt.ReadThroughID != message.ID {
		t.Fatalf("explicit read receipt: HTTP %d %s %v", status, raw, err)
	}
	threads := messagingHTTPList(t, f.pttClient)
	if len(threads) != 1 || threads[0].UnreadCount != 0 || threads[0].ReadThroughID != message.ID {
		t.Fatalf("read marker did not belong to the receiving person: %#v", threads)
	}
	retry := messagingHTTPSend(t, f.deskClient, thread.ID, "desk-first", body)
	if retry.ID != message.ID {
		t.Fatal("retry with the same client identity created a second message")
	}
	reply := messagingHTTPSend(t, f.pttClient, thread.ID, "ptt-reply", "Received from PTT")
	if reply.SenderID != f.ptt.ID {
		t.Fatal("PTT reply did not retain its human account")
	}
	secondSession := messagingHTTPLogin(t, f.cluster.dispatch, f.desk)
	threads = messagingHTTPList(t, secondSession)
	if len(threads) != 1 || threads[0].UnreadCount != 1 || threads[0].LastMessage == nil || threads[0].LastMessage.ID != reply.ID {
		t.Fatalf("a second app session did not see the person's shared inbox: %#v", threads)
	}
	if got := messagingHTTPList(t, f.otherClient); len(got) != 0 {
		t.Fatalf("another person on the same desk inherited the inbox: %#v", got)
	}
}

func TestMessagingHTTPProtectsParticipantsSessionsAndRoleBoundary(t *testing.T) {
	f := newMessagingHTTPFixture(t)
	status, _ := integrationHTTP(t, &http.Client{Timeout: 3 * time.Second}, http.MethodGet, f.deskClient.base+"/api/messages/people", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous directory access: HTTP %d", status)
	}
	admin := integrationLogin(t, f.cluster.dispatch, integrationPassword)
	for _, path := range []string{"/api/messages/people", "/api/messages/threads"} {
		if status, raw := admin.request(t, http.MethodGet, path, nil); status != http.StatusForbidden {
			t.Fatalf("administrator gained an implicit person identity: HTTP %d %s", status, raw)
		}
	}
	systemAdmin := integrationLogin(t, f.cluster.system, integrationPassword)
	if status, raw := systemAdmin.request(t, http.MethodGet, "/api/messages/people", nil); status != http.StatusNotFound {
		t.Fatalf("messaging appeared in another service role: HTTP %d %s", status, raw)
	}
	create := map[string]any{"kind": "direct", "name": "", "members": []string{f.ptt.ID}}
	status, _ = integrationHTTP(t, f.deskClient.http, http.MethodPost, f.deskClient.base+"/api/messages/threads", create, map[string]string{"Origin": f.deskClient.base})
	if status != http.StatusForbidden {
		t.Fatalf("write bypassed CSRF: HTTP %d", status)
	}
	status, _ = integrationHTTP(t, f.deskClient.http, http.MethodPost, f.deskClient.base+"/api/messages/threads", create, map[string]string{"Origin": "https://different.invalid", "X-CSRF-Token": f.deskClient.csrf})
	if status != http.StatusForbidden {
		t.Fatalf("cross-origin write accepted: HTTP %d", status)
	}
	thread := messagingHTTPCreate(t, f.deskClient, "direct", "", []string{f.ptt.ID})
	message := messagingHTTPSend(t, f.deskClient, thread.ID, "private", "Only these two people")
	for _, test := range []struct {
		method, suffix string
		body           any
	}{
		{http.MethodGet, "", nil},
		{http.MethodPost, "/messages", map[string]string{"client_id": "outsider", "body": "not a member"}},
		{http.MethodPost, "/read", map[string]int64{"message_id": message.ID}},
	} {
		status, raw := f.otherClient.request(t, test.method, "/api/messages/threads/"+thread.ID+test.suffix, test.body)
		if status != http.StatusNotFound || bytes.Contains(raw, []byte(message.Body)) {
			t.Fatalf("outsider accessed private conversation: HTTP %d %s", status, raw)
		}
	}
	status, raw := f.deskClient.request(t, http.MethodPost, "/api/messages/threads/"+thread.ID+"/messages", map[string]string{"client_id": "spoof", "body": "forged sender", "sender_id": f.ptt.ID})
	if status != http.StatusBadRequest {
		t.Fatalf("request supplied an authoritative sender: HTTP %d %s", status, raw)
	}
	disabled := cloneUser(t, f.ptt)
	disabled.Enabled = false
	if err := f.cluster.system.service.Store.SaveUser(disabled.ID, disabled, ""); err != nil {
		t.Fatal(err)
	}
	integrationSync(t, f.cluster.zone)
	integrationSync(t, f.cluster.dispatch)
	if status, raw := f.pttClient.request(t, http.MethodGet, "/api/messages/threads/"+thread.ID, nil); status != http.StatusUnauthorized {
		t.Fatalf("disabled person retained a live inbox session: HTTP %d %s", status, raw)
	}
}

func TestMessagingHTTPGroupConversationAndPagedHistory(t *testing.T) {
	f := newMessagingHTTPFixture(t)
	thread := messagingHTTPCreate(t, f.deskClient, "group", "Field coordination", []string{f.ptt.ID, f.sameDesk.ID})
	expectedMembers := []string{f.desk.ID, f.ptt.ID, f.sameDesk.ID}
	actualMembers := append([]string(nil), thread.Members...)
	slices.Sort(expectedMembers)
	slices.Sort(actualMembers)
	if thread.Kind != "group" || thread.Name != "Field coordination" || !reflect.DeepEqual(actualMembers, expectedMembers) {
		t.Fatalf("group did not retain its named people: %#v", thread)
	}
	first := messagingHTTPSend(t, f.deskClient, thread.ID, "group-first", "First group message")
	second := messagingHTTPSend(t, f.pttClient, thread.ID, "group-second", "Second group message")
	third := messagingHTTPSend(t, f.deskClient, thread.ID, "group-third", "Third group message")
	sameDesk := messagingHTTPLogin(t, f.cluster.dispatch, f.sameDesk)
	status, raw := sameDesk.request(t, http.MethodGet, "/api/messages/threads/"+thread.ID+"?limit=2", nil)
	var page struct {
		Messages     []messagingHTTPMessage `json:"messages"`
		NextBeforeID int64                  `json:"next_before_id"`
	}
	if err := json.Unmarshal(raw, &page); status != http.StatusOK || err != nil || len(page.Messages) != 2 {
		t.Fatalf("group history page: HTTP %d %s %v", status, raw, err)
	}
	got := []int64{page.Messages[0].ID, page.Messages[1].ID}
	slices.Sort(got)
	if !reflect.DeepEqual(got, []int64{second.ID, third.ID}) || page.NextBeforeID != second.ID {
		t.Fatalf("history cursor does not cover the newest bounded page: %s", raw)
	}
	status, raw = sameDesk.request(t, http.MethodGet, "/api/messages/threads/"+thread.ID+"?before_id="+strconv.FormatInt(page.NextBeforeID, 10)+"&limit=2", nil)
	if err := json.Unmarshal(raw, &page); status != http.StatusOK || err != nil || len(page.Messages) != 1 || page.Messages[0].ID != first.ID {
		t.Fatalf("older page repeated or lost stored messages: HTTP %d %s %v", status, raw, err)
	}
	if status, raw := f.otherClient.request(t, http.MethodGet, "/api/messages/threads/"+thread.ID, nil); status != http.StatusNotFound {
		t.Fatalf("group leaked to an uninvited person: HTTP %d %s", status, raw)
	}
}

func messagingHTTPStream(t *testing.T, c *integrationClient) <-chan string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/messages/events", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	r.Header.Set("Origin", c.base)
	client := &http.Client{Jar: c.http.Jar}
	response, err := client.Do(r)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		response.Body.Close()
		cancel()
		t.Fatalf("message events: HTTP %d %s", response.StatusCode, response.Header.Get("Content-Type"))
	}
	t.Cleanup(func() { cancel(); response.Body.Close() })
	events := make(chan string, 16)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(response.Body)
		event, data := "", ""
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			} else if line == "" && event != "" {
				select {
				case events <- event + ":" + data:
				case <-ctx.Done():
					return
				}
				event, data = "", ""
			}
		}
	}()
	return events
}

func messagingHTTPAwaitInbox(t *testing.T, events <-chan string) {
	t.Helper()
	select {
	case value, open := <-events:
		if !open || value != "inbox:{}" {
			t.Fatalf("event disclosed more than inbox invalidation or stream closed: %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("participant did not receive an inbox invalidation")
	}
}

func messagingHTTPNoInbox(t *testing.T, events <-chan string) {
	t.Helper()
	select {
	case value, open := <-events:
		t.Fatalf("unexpected inbox invalidation or stream close: %q open=%t", value, open)
	case <-time.After(75 * time.Millisecond):
	}
}

func TestMessagingHTTPEventsFollowCommittedParticipantChanges(t *testing.T) {
	f := newMessagingHTTPFixture(t)
	thread := messagingHTTPCreate(t, f.deskClient, "direct", "", []string{f.ptt.ID})
	deskEvents := messagingHTTPStream(t, f.deskClient)
	pttEvents := messagingHTTPStream(t, f.pttClient)
	otherEvents := messagingHTTPStream(t, f.otherClient)
	for _, events := range []<-chan string{deskEvents, pttEvents, otherEvents} {
		messagingHTTPAwaitInbox(t, events)
	}
	message := messagingHTTPSend(t, f.deskClient, thread.ID, "event-message", "Stored before this inbox changes")
	messagingHTTPAwaitInbox(t, deskEvents)
	messagingHTTPAwaitInbox(t, pttEvents)
	status, raw := f.pttClient.request(t, http.MethodGet, "/api/messages/threads/"+thread.ID, nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(message.Body)) {
		t.Fatalf("invalidation preceded the stored message: HTTP %d %s", status, raw)
	}
	messagingHTTPNoInbox(t, otherEvents)
	status, raw = f.pttClient.request(t, http.MethodPost, "/api/messages/threads/"+thread.ID+"/read", map[string]int64{"message_id": message.ID})
	if status != http.StatusOK {
		t.Fatalf("read marker: HTTP %d %s", status, raw)
	}
	messagingHTTPAwaitInbox(t, pttEvents)
	// An unchanged marker must not create a refresh/read/refresh loop.
	status, raw = f.pttClient.request(t, http.MethodPost, "/api/messages/threads/"+thread.ID+"/read", map[string]int64{"message_id": message.ID})
	if status != http.StatusOK {
		t.Fatalf("unchanged read marker: HTTP %d %s", status, raw)
	}
	messagingHTTPNoInbox(t, pttEvents)
	messagingHTTPNoInbox(t, deskEvents)
	messagingHTTPNoInbox(t, otherEvents)
}

func TestMessagingHTTPGroupCreatorChangesMembershipWithoutGivingEarlierHistory(t *testing.T) {
	f := newMessagingHTTPFixture(t)
	thread := messagingHTTPCreate(t, f.deskClient, "group", "Changing participants", []string{f.ptt.ID})
	old := messagingHTTPSend(t, f.deskClient, thread.ID, "before-joining", "Earlier private discussion")
	joining := messagingHTTPLogin(t, f.cluster.dispatch, f.sameDesk)
	events := messagingHTTPStream(t, joining)
	messagingHTTPAwaitInbox(t, events)
	path := "/api/messages/threads/" + thread.ID
	status, raw := f.pttClient.request(t, http.MethodPut, path+"/members", map[string]any{"members": []string{f.ptt.ID, f.sameDesk.ID}})
	if status != http.StatusNotFound {
		t.Fatalf("a participant changed the creator's group membership: HTTP %d %s", status, raw)
	}
	messagingHTTPNoInbox(t, events)
	setMembers := func(members []string) {
		t.Helper()
		status, raw := f.deskClient.request(t, http.MethodPut, path+"/members", map[string]any{"members": members})
		var changed struct {
			Thread messagingHTTPThread `json:"thread"`
		}
		if err := json.Unmarshal(raw, &changed); status != http.StatusOK || err != nil || !slices.Contains(changed.Thread.Members, f.desk.ID) {
			t.Fatalf("creator membership change lost its owner: HTTP %d %s %v", status, raw, err)
		}
		messagingHTTPAwaitInbox(t, events)
	}
	readJoining := func(want []int64, unread int64) {
		t.Helper()
		status, raw := joining.request(t, http.MethodGet, path, nil)
		var detail struct {
			Thread   messagingHTTPThread    `json:"thread"`
			Messages []messagingHTTPMessage `json:"messages"`
		}
		if err := json.Unmarshal(raw, &detail); status != http.StatusOK || err != nil {
			t.Fatalf("joined conversation: HTTP %d %s %v", status, raw, err)
		}
		got := []int64{}
		for _, message := range detail.Messages {
			got = append(got, message.ID)
		}
		if !reflect.DeepEqual(got, want) || detail.Thread.UnreadCount != unread || len(want) == 0 && detail.Thread.LastMessage != nil {
			t.Fatalf("participant received history/preview outside its membership period: %s", raw)
		}
	}
	setMembers([]string{f.ptt.ID, f.sameDesk.ID})
	readJoining([]int64{}, 0)
	status, raw = joining.request(t, http.MethodGet, path+"?before_id="+strconv.FormatInt(old.ID, 10), nil)
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		t.Fatalf("earlier history was usable as a cursor: HTTP %d %s", status, raw)
	}
	status, raw = joining.request(t, http.MethodPost, path+"/read", map[string]int64{"message_id": old.ID})
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		t.Fatalf("earlier history was usable as a read receipt: HTTP %d %s", status, raw)
	}
	firstJoin := messagingHTTPSend(t, f.deskClient, thread.ID, "after-joining", "Visible during the first membership")
	messagingHTTPAwaitInbox(t, events)
	readJoining([]int64{firstJoin.ID}, 1)
	setMembers([]string{f.ptt.ID})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		target, body := path, any(nil)
		if method == http.MethodPost {
			target, body = path+"/messages", map[string]string{"client_id": "removed", "body": "removed participant"}
		}
		if status, raw := joining.request(t, method, target, body); status != http.StatusNotFound {
			t.Fatalf("removed participant retained access: HTTP %d %s", status, raw)
		}
	}
	if listed := messagingHTTPList(t, joining); len(listed) != 0 {
		t.Fatal("removed conversation remained in the person's inbox")
	}
	messagingHTTPSend(t, f.deskClient, thread.ID, "while-removed", "Private while this participant was removed")
	messagingHTTPNoInbox(t, events)
	setMembers([]string{f.ptt.ID, f.sameDesk.ID})
	readJoining([]int64{}, 0)
	status, raw = joining.request(t, http.MethodGet, path+"?before_id="+strconv.FormatInt(firstJoin.ID, 10), nil)
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		t.Fatalf("rejoining restored former membership history: HTTP %d %s", status, raw)
	}
	future := messagingHTTPSend(t, f.deskClient, thread.ID, "after-rejoining", "Visible after rejoining")
	messagingHTTPAwaitInbox(t, events)
	readJoining([]int64{future.ID}, 1)
	status, raw = f.pttClient.request(t, http.MethodGet, path, nil)
	var existing struct {
		Messages []messagingHTTPMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &existing); status != http.StatusOK || err != nil || len(existing.Messages) != 4 || existing.Messages[0].ID != old.ID {
		t.Fatalf("membership change reset an unchanged participant's history: HTTP %d %s %v", status, raw, err)
	}
}

package messaging

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
)

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var synchronous int
	if err = s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("durability setting: %d %v", synchronous, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func direct(t *testing.T, s *Store, a, b string) Conversation {
	t.Helper()
	v, err := s.CreateDirect(context.Background(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func send(t *testing.T, s *Store, sender, name, conversation, client, body string) Message {
	t.Helper()
	v, _, err := s.Send(context.Background(), sender, name, conversation, client, body)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCommittedMessageAndReadStateSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "private", "messaging.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	thread := direct(t, s, "alice", "bob")
	body := "  Meet at gate 2.\r\n\nCafé e\u0301 — 東京 🙂\t\x00  "
	message := send(t, s, "alice", "Alice Before Rename", thread.ID, "from-device-1", body)
	if message.Body != body || message.SenderID != "alice" || message.SenderName != "Alice Before Rename" || message.ID < 1 || message.CreatedAt.IsZero() {
		t.Fatalf("original message not preserved: %#v", message)
	}
	if err = s.MarkRead(ctx, "bob", thread.ID, message.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	page, err := s.Messages(ctx, "bob", thread.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || !reflect.DeepEqual(page.Messages[0], message) {
		t.Fatalf("committed message after reopen: %#v %v", page, err)
	}
	state, err := s.Conversation(ctx, "bob", thread.ID)
	if err != nil || state.ReadThroughID != message.ID || state.UnreadCount != 0 || state.LastMessage == nil || *state.LastMessage != message {
		t.Fatalf("committed cursor after reopen: %#v %v", state, err)
	}
	retried, _, err := s.Send(ctx, "alice", "Alice After Rename", thread.ID, message.ClientID, body)
	if err != nil || retried != message {
		t.Fatalf("retry replaced immutable sender snapshot: %#v %v", retried, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("database permissions: %v %v", info, err)
	}
}

func TestDirectReuseAndPrivateGroupIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "messaging.db"))
	pair := direct(t, s, "alice", "bob")
	reverse := direct(t, s, "bob", "alice")
	if pair.ID != reverse.ID || !slices.Equal(pair.Members, []string{"alice", "bob"}) {
		t.Fatalf("direct thread was duplicated: %#v %#v", pair, reverse)
	}
	selected := []string{"carol", "bob", "bob"}
	group, err := s.CreateGroup(ctx, "alice", "Field team\nA", selected)
	if err != nil || group.Kind != "group" || group.Name != "Field team\nA" || !slices.Equal(group.Members, []string{"alice", "bob", "carol"}) {
		t.Fatalf("group membership: %#v %v", group, err)
	}
	selected[0] = "outsider"
	message := send(t, s, "bob", "Bob", group.ID, "bob-1", "private team message")
	for name, read := range map[string]func() error{
		"detail":      func() error { _, e := s.Conversation(ctx, "outsider", group.ID); return e },
		"messages":    func() error { _, e := s.Messages(ctx, "outsider", group.ID, 0, 10); return e },
		"send":        func() error { _, _, e := s.Send(ctx, "outsider", "Outsider", group.ID, "intrusion", "bad"); return e },
		"read cursor": func() error { return s.MarkRead(ctx, "outsider", group.ID, message.ID) },
	} {
		if err = read(); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s did not enforce membership: %v", name, err)
		}
	}
	outsider, err := s.Conversations(ctx, "outsider")
	if err != nil || len(outsider) != 0 {
		t.Fatalf("outsider listing: %#v %v", outsider, err)
	}
	alice, err := s.Conversations(ctx, "alice")
	if err != nil || len(alice) != 2 || alice[0].ID != group.ID || alice[0].UnreadCount != 1 || alice[1].ID != pair.ID {
		t.Fatalf("person's threads/unread ordering: %#v %v", alice, err)
	}
	bob, err := s.Conversation(ctx, "bob", group.ID)
	if err != nil || bob.UnreadCount != 0 {
		t.Fatalf("own message counted unread: %#v %v", bob, err)
	}
	carol, err := s.Conversation(ctx, "carol", group.ID)
	if err != nil || carol.UnreadCount != 1 {
		t.Fatalf("group participant state: %#v %v", carol, err)
	}
	if _, err = s.Conversation(ctx, "alice", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing thread: %v", err)
	}
}

func TestConcurrentDirectAndSendDeduplicationAcrossConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "messaging.db")
	stores := []*Store{openTestStore(t, path), openTestStore(t, path)}
	const requests = 24
	threads := make(chan Conversation, requests)
	errorsSeen := make(chan error, requests)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			a, b := "alice", "bob"
			if i%2 != 0 {
				a, b = b, a
			}
			value, err := stores[i%2].CreateDirect(ctx, a, b)
			if err != nil {
				errorsSeen <- err
				return
			}
			threads <- value
		}()
	}
	close(start)
	wg.Wait()
	close(threads)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	var thread Conversation
	for value := range threads {
		if thread.ID != "" && value.ID != thread.ID {
			t.Fatalf("concurrent direct creation duplicated thread: %s %s", thread.ID, value.ID)
		}
		thread = value
	}
	results := make(chan Message, requests)
	errorsSeen = make(chan error, requests)
	start = make(chan struct{})
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			value, _, err := stores[i%2].Send(ctx, "alice", fmt.Sprintf("Alice snapshot %d", i), thread.ID, "same-device-command", "only one logical message")
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- value
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	var stored Message
	for value := range results {
		if stored.ID != 0 && value != stored {
			t.Fatalf("dedup returned another message: %#v %#v", stored, value)
		}
		stored = value
	}
	page, err := stores[0].Messages(ctx, "bob", thread.ID, 0, 10)
	if err != nil || len(page.Messages) != 1 || page.Messages[0] != stored {
		t.Fatalf("duplicate durable messages: %#v %v", page, err)
	}
	if _, _, err = stores[0].Send(ctx, "alice", "Alice", thread.ID, stored.ClientID, "changed payload"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed dedup payload: %v", err)
	}
	other := direct(t, stores[0], "alice", "carol")
	if _, _, err = stores[0].Send(ctx, "alice", "Alice", other.ID, stored.ClientID, stored.Body); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed dedup destination: %v", err)
	}
	// A different person owns a separate client identifier namespace.
	send(t, stores[0], "bob", "Bob", thread.ID, stored.ClientID, "Bob's message")
}

func TestPaginationAndReadCursorAreStableWhileNewMessagesArrive(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "messaging.db"))
	thread := direct(t, s, "alice", "bob")
	var sent []Message
	for i := range 5 {
		sent = append(sent, send(t, s, "alice", "Alice", thread.ID, fmt.Sprint(i), fmt.Sprintf("message %d", i)))
	}
	page, err := s.Messages(ctx, "bob", thread.ID, 0, 2)
	if err != nil || !reflect.DeepEqual(page.Messages, sent[3:]) || page.NextBeforeID != sent[3].ID {
		t.Fatalf("first page: %#v %v", page, err)
	}
	newest := send(t, s, "alice", "Alice", thread.ID, "new", "arrived after page loaded")
	if err = s.MarkRead(ctx, "bob", thread.ID, sent[4].ID); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkRead(ctx, "bob", thread.ID, sent[1].ID); err != nil {
		t.Fatal(err)
	}
	state, err := s.Conversation(ctx, "bob", thread.ID)
	if err != nil || state.ReadThroughID != sent[4].ID || state.UnreadCount != 1 || state.LastMessage.ID != newest.ID {
		t.Fatalf("new arrival was marked read or stale cursor moved backwards: %#v %v", state, err)
	}
	older, err := s.Messages(ctx, "bob", thread.ID, page.NextBeforeID, 2)
	if err != nil || !reflect.DeepEqual(older.Messages, sent[1:3]) || older.NextBeforeID != sent[1].ID {
		t.Fatalf("older page changed after arrival: %#v %v", older, err)
	}
	oldest, err := s.Messages(ctx, "bob", thread.ID, older.NextBeforeID, 2)
	if err != nil || !reflect.DeepEqual(oldest.Messages, sent[:1]) || oldest.NextBeforeID != 0 {
		t.Fatalf("oldest page: %#v %v", oldest, err)
	}
	other := direct(t, s, "alice", "carol")
	foreign := send(t, s, "alice", "Alice", other.ID, "other", "private to Carol")
	for _, id := range []int64{newest.ID + 100, foreign.ID} {
		if err = s.MarkRead(ctx, "bob", thread.ID, id); !errors.Is(err, ErrInvalid) {
			t.Fatalf("non-visible read cursor %d accepted: %v", id, err)
		}
		if _, err = s.Messages(ctx, "bob", thread.ID, id, 2); !errors.Is(err, ErrInvalid) {
			t.Fatalf("non-visible pagination cursor %d accepted: %v", id, err)
		}
	}
	state, err = s.Conversation(ctx, "bob", thread.ID)
	if err != nil || state.ReadThroughID != sent[4].ID || state.UnreadCount != 1 {
		t.Fatalf("invalid cursor changed state: %#v %v", state, err)
	}
}

func TestCancelledAndFailedWritesAreNotAcknowledgedOrStored(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "messaging.db"))
	thread := direct(t, s, "alice", "bob")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.Send(cancelled, "alice", "Alice", thread.ID, "cancelled", "not accepted"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled send: %v", err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER reject_send AFTER INSERT ON messages BEGIN SELECT RAISE(ABORT,'injected write failure'); END"); err != nil {
		t.Fatal(err)
	}
	if message, _, err := s.Send(ctx, "alice", "Alice", thread.ID, "failed", "not accepted"); err == nil || message.ID != 0 {
		t.Fatalf("failed write was acknowledged: %#v %v", message, err)
	}
	page, err := s.Messages(ctx, "bob", thread.ID, 0, 10)
	if err != nil || len(page.Messages) != 0 {
		t.Fatalf("failed/cancelled messages leaked: %#v %v", page, err)
	}
}

func TestCreatorMembershipChangesHidePriorHistoryAndResetRejoins(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "messaging.db"))
	group, err := s.CreateGroup(ctx, "alice", "Field team", []string{"bob"})
	if err != nil {
		t.Fatal(err)
	}
	first := send(t, s, "alice", "Alice", group.ID, "before-1", "private before Carol")
	if err = s.MarkRead(ctx, "bob", group.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	lastBeforeJoin := send(t, s, "alice", "Alice", group.ID, "before-2", "still private before Carol")
	other := direct(t, s, "alice", "dave")
	globalBoundary := send(t, s, "alice", "Alice", other.ID, "other-thread", "latest globally committed message")
	value, affected, err := s.SetMembers(ctx, "alice", group.ID, []string{"bob", "carol"})
	if err != nil || !slices.Equal(value.Members, []string{"alice", "bob", "carol"}) || !slices.Equal(affected, value.Members) {
		t.Fatalf("creator membership/notification union: %#v %#v %v", value, affected, err)
	}
	bob, err := s.Conversation(ctx, "bob", group.ID)
	if err != nil || bob.HistoryAfterID != 0 || bob.ReadThroughID != first.ID || bob.UnreadCount != 1 || bob.LastMessage.ID != lastBeforeJoin.ID {
		t.Fatalf("retained member lost history/read state: %#v %v", bob, err)
	}
	carol, err := s.Conversation(ctx, "carol", group.ID)
	if err != nil || carol.HistoryAfterID != globalBoundary.ID || carol.ReadThroughID != globalBoundary.ID || carol.LastMessage != nil || carol.UnreadCount != 0 {
		t.Fatalf("new member saw prior summary: %#v %v", carol, err)
	}
	list, err := s.Conversations(ctx, "carol")
	if err != nil || len(list) != 1 || list[0].LastMessage != nil || list[0].UnreadCount != 0 || list[0].HistoryAfterID != globalBoundary.ID {
		t.Fatalf("prior history leaked through list: %#v %v", list, err)
	}
	page, err := s.Messages(ctx, "carol", group.ID, 0, 10)
	if err != nil || len(page.Messages) != 0 {
		t.Fatalf("new member saw prior messages: %#v %v", page, err)
	}
	if _, err = s.Messages(ctx, "carol", group.ID, lastBeforeJoin.ID, 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("old pagination cursor accepted: %v", err)
	}
	if err = s.MarkRead(ctx, "carol", group.ID, lastBeforeJoin.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("old read cursor accepted: %v", err)
	}
	if _, _, err = s.SetMembers(ctx, "bob", group.ID, []string{"eve"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-creator changed group membership: %v", err)
	}
	if _, _, err = s.SetMembers(ctx, "alice", other.ID, []string{"eve"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("direct membership changed: %v", err)
	}
	visible := send(t, s, "alice", "Alice", group.ID, "after-join", "Carol can see this")
	carolsOwn := send(t, s, "carol", "Carol Before Removal", group.ID, "carol-own", "my earlier membership")
	page, err = s.Messages(ctx, "carol", group.ID, 0, 10)
	if err != nil || !reflect.DeepEqual(page.Messages, []Message{visible, carolsOwn}) {
		t.Fatalf("new membership window incorrect: %#v %v", page, err)
	}
	if err = s.MarkRead(ctx, "carol", group.ID, visible.ID); err != nil {
		t.Fatal(err)
	}
	_, affected, err = s.SetMembers(ctx, "alice", group.ID, []string{"bob"})
	if err != nil || !slices.Equal(affected, []string{"alice", "bob", "carol"}) {
		t.Fatalf("removed person omitted from notifications: %#v %v", affected, err)
	}
	if _, err = s.Conversation(ctx, "carol", group.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("removed member could load detail: %v", err)
	}
	if _, err = s.Messages(ctx, "carol", group.ID, 0, 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("removed member could load messages: %v", err)
	}
	if _, _, err = s.Send(ctx, "carol", "Carol", group.ID, "removed-send", "must not send"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("removed member could send: %v", err)
	}
	if err = s.MarkRead(ctx, "carol", group.ID, visible.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("removed member could mark read: %v", err)
	}
	list, err = s.Conversations(ctx, "carol")
	if err != nil || len(list) != 0 {
		t.Fatalf("removed member retained inbox access: %#v %v", list, err)
	}
	whileRemoved := send(t, s, "alice", "Alice", group.ID, "while-removed", "Carol cannot see this")
	if _, _, err = s.SetMembers(ctx, "alice", group.ID, []string{"bob", "carol"}); err != nil {
		t.Fatal(err)
	}
	carol, err = s.Conversation(ctx, "carol", group.ID)
	if err != nil || carol.HistoryAfterID != whileRemoved.ID || carol.ReadThroughID != whileRemoved.ID || carol.LastMessage != nil || carol.UnreadCount != 0 {
		t.Fatalf("rejoin restored earlier history: %#v %v", carol, err)
	}
	if _, _, err = s.Send(ctx, "carol", "Carol After Rejoin", group.ID, carolsOwn.ClientID, carolsOwn.Body); !errors.Is(err, ErrConflict) {
		t.Fatalf("dedup replay exposed prior membership message: %v", err)
	}
	future := send(t, s, "alice", "Alice", group.ID, "after-rejoin", "new membership sees this")
	page, err = s.Messages(ctx, "carol", group.ID, 0, 10)
	if err != nil || !reflect.DeepEqual(page.Messages, []Message{future}) {
		t.Fatalf("rejoined history window: %#v %v", page, err)
	}
	bob, err = s.Conversation(ctx, "bob", group.ID)
	if err != nil || bob.HistoryAfterID != 0 || bob.ReadThroughID != first.ID {
		t.Fatalf("rejoin elsewhere changed retained member: %#v %v", bob, err)
	}
}

func TestConcurrentMembershipAdditionAndSendUseOneHistoryBoundary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "messaging.db")
	stores := []*Store{openTestStore(t, path), openTestStore(t, path)}
	group, err := stores[0].CreateGroup(ctx, "alice", "Concurrent team", []string{"bob"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		prior := send(t, stores[0], "alice", "Alice", group.ID, fmt.Sprintf("prior-%d", i), "definitely before addition")
		userID := fmt.Sprintf("new-person-%d", i)
		start := make(chan struct{})
		joined := make(chan error, 1)
		sent := make(chan Message, 1)
		notified := make(chan []string, 1)
		sendErr := make(chan error, 1)
		go func() {
			<-start
			_, _, e := stores[0].SetMembers(ctx, "alice", group.ID, []string{"bob", userID})
			joined <- e
		}()
		go func() {
			<-start
			message, recipients, e := stores[1].Send(ctx, "alice", "Alice", group.ID, fmt.Sprintf("racing-%d", i), "races the membership change")
			sent <- message
			notified <- recipients
			sendErr <- e
		}()
		close(start)
		if err = <-joined; err != nil {
			t.Fatal(err)
		}
		message := <-sent
		recipients := <-notified
		if err = <-sendErr; err != nil {
			t.Fatal(err)
		}
		state, e := stores[0].Conversation(ctx, userID, group.ID)
		if e != nil || state.HistoryAfterID < prior.ID {
			t.Fatalf("join lost prior committed boundary: %#v %v", state, e)
		}
		page, e := stores[1].Messages(ctx, userID, group.ID, 0, 100)
		if e != nil {
			t.Fatal(e)
		}
		if message.ID > state.HistoryAfterID {
			if !slices.Contains(recipients, userID) {
				t.Fatalf("committed message omitted its new participant's notification: %#v", recipients)
			}
			if len(page.Messages) != 1 || page.Messages[0] != message || state.LastMessage == nil || *state.LastMessage != message || state.UnreadCount != 1 {
				t.Fatalf("post-join message hidden: %#v %#v", state, page)
			}
		} else {
			if slices.Contains(recipients, userID) {
				t.Fatalf("pre-join message used later membership for notification: %#v", recipients)
			}
			if len(page.Messages) != 0 || state.LastMessage != nil || state.UnreadCount != 0 {
				t.Fatalf("pre-join message leaked: %#v %#v", state, page)
			}
		}
	}
}

func TestMessagePageSummarySharesMembershipSnapshotDuringRemoveRejoin(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "messaging.db")
	reader, writer := openTestStore(t, path), openTestStore(t, path)
	group, err := writer.CreateGroup(ctx, "alice", "Changing membership", []string{"bob"})
	if err != nil {
		t.Fatal(err)
	}
	send(t, writer, "alice", "Alice", group.ID, "initial-period", "Earlier membership body")
	stale, err := reader.Conversation(ctx, "bob", group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = writer.SetMembers(ctx, "alice", group.ID, nil); err != nil {
		t.Fatal(err)
	}
	absent := send(t, writer, "alice", "Alice", group.ID, "intervening-period", "Hidden while Bob is removed")
	if _, _, err = writer.SetMembers(ctx, "alice", group.ID, []string{"bob"}); err != nil {
		t.Fatal(err)
	}
	visible := send(t, writer, "alice", "Alice", group.ID, "new-period", "Current membership body")
	current, err := reader.Messages(ctx, "bob", group.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if stale.HistoryAfterID != 0 || stale.LastMessage == nil || current.Conversation.HistoryAfterID != absent.ID || current.Conversation.LastMessage == nil || *current.Conversation.LastMessage != visible || !reflect.DeepEqual(current.Messages, []Message{visible}) {
		t.Fatalf("a lookup from before removal must not supply the new page's metadata: stale=%#v page=%#v", stale, current)
	}
	changed := make(chan error, 1)
	go func() {
		for i := range 32 {
			if _, _, err := writer.SetMembers(ctx, "alice", group.ID, nil); err != nil {
				changed <- err
				return
			}
			if _, _, err := writer.Send(ctx, "alice", "Alice", group.ID, fmt.Sprintf("absent-%d", i), "Hidden while Bob is removed"); err != nil {
				changed <- err
				return
			}
			if _, _, err := writer.SetMembers(ctx, "alice", group.ID, []string{"bob"}); err != nil {
				changed <- err
				return
			}
			if _, _, err := writer.Send(ctx, "alice", "Alice", group.ID, fmt.Sprintf("visible-%d", i), "Current membership body"); err != nil {
				changed <- err
				return
			}
		}
		changed <- nil
	}()
	check := func(page MessagePage) {
		t.Helper()
		thread := page.Conversation
		if thread.ID != group.ID || !slices.Contains(thread.Members, "bob") || thread.UnreadCount != int64(len(page.Messages)) {
			t.Fatalf("summary and page do not describe the same membership: %#v", page)
		}
		for _, message := range page.Messages {
			if message.ID <= thread.HistoryAfterID || message.Body == "Hidden while Bob is removed" {
				t.Fatalf("message crossed the returned membership boundary: %#v", page)
			}
		}
		if len(page.Messages) == 0 {
			if thread.LastMessage != nil {
				t.Fatalf("an earlier-membership preview accompanied an empty current page: %#v", page)
			}
		} else if thread.LastMessage == nil || *thread.LastMessage != page.Messages[len(page.Messages)-1] {
			t.Fatalf("preview and newest page message came from different snapshots: %#v", page)
		}
	}
	for range 200 {
		page, err := reader.Messages(ctx, "bob", group.ID, 0, 100)
		if errors.Is(err, ErrForbidden) {
			continue // A snapshot taken during removal is correctly unavailable.
		}
		if err != nil {
			t.Fatal(err)
		}
		check(page)
	}
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	page, err := reader.Messages(ctx, "bob", group.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	check(page)
	if page.Conversation.HistoryAfterID == 0 || len(page.Messages) != 1 {
		t.Fatalf("final rejoin did not establish its own summary/page window: %#v", page)
	}
}

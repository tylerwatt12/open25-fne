package framework

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"open25.local/open25-fne/internal/messaging"
)

type messagePerson struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// messagesHTTP uses the existing session, origin and CSRF middleware. Person
// identity belongs to the account, independently of the current radio position.
func (s *Service) messagesHTTP(w http.ResponseWriter, r *http.Request, session Session) bool {
	if r.URL.Path != "/api/messages" && !strings.HasPrefix(r.URL.Path, "/api/messages/") {
		return false
	}
	if s.Config().Role != RoleApplication {
		http.NotFound(w, r)
		return true
	}
	if session.UserID == "" {
		fail(w, http.StatusForbidden, errors.New("sign in with a person account to use messages"))
		return true
	}
	person, _, err := s.Store.User(session.UserID)
	if err != nil || !person.Enabled || person.Username != session.Username {
		fail(w, http.StatusUnauthorized, errors.New("this application account is no longer available"))
		return true
	}
	if s.messages == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("messages are unavailable; keep your draft and try again"))
		return true
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/messages/")
	switch path {
	case "people":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		users, err := s.Store.Users()
		if err != nil {
			s.messageError(w, err)
			return true
		}
		people := []messagePerson{}
		for _, user := range users {
			if user.Enabled {
				people = append(people, messagePerson{ID: user.ID, Username: user.Username})
			}
		}
		respond(w, http.StatusOK, map[string]any{"people": people})
		return true
	case "events":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		s.messageEvents(w, r, person.ID)
		return true
	case "threads":
		switch r.Method {
		case http.MethodGet:
			threads, err := s.messages.Conversations(r.Context(), person.ID)
			if err != nil {
				s.messageError(w, err)
			} else {
				respond(w, http.StatusOK, map[string]any{"threads": threads})
			}
		case http.MethodPost:
			s.createMessageThread(w, r, person)
		default:
			w.Header().Set("Allow", "GET, POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return true
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "threads" || parts[1] == "" || (len(parts) == 3 && parts[2] != "messages" && parts[2] != "read" && parts[2] != "members") {
		http.NotFound(w, r)
		return true
	}
	method := http.MethodGet
	if len(parts) == 3 {
		method = http.MethodPost
		if parts[2] == "members" {
			method = http.MethodPut
		}
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return true
	}
	if len(parts) == 2 {
		before, limit := int64(0), 50
		if value := r.URL.Query().Get("before_id"); value != "" {
			before, err = strconv.ParseInt(value, 10, 64)
			if err != nil || before < 0 {
				s.messageError(w, messaging.ErrInvalid)
				return true
			}
		}
		if value := r.URL.Query().Get("limit"); value != "" {
			limit, err = strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 100 {
				s.messageError(w, messaging.ErrInvalid)
				return true
			}
		}
		page, err := s.messages.Messages(r.Context(), person.ID, parts[1], before, limit)
		if err != nil {
			s.messageError(w, err)
		} else {
			respond(w, http.StatusOK, map[string]any{"thread": page.Conversation, "messages": page.Messages, "next_before_id": page.NextBeforeID, "rf_delivery": "pending"})
		}
		return true
	}
	thread, err := s.messages.Conversation(r.Context(), person.ID, parts[1])
	if err != nil {
		s.messageError(w, err)
		return true
	}
	if parts[2] == "members" {
		var input struct {
			Members []string `json:"members"`
		}
		if err := readJSON(w, r, &input); err != nil {
			fail(w, http.StatusBadRequest, err)
			return true
		}
		if err := s.messagePeopleKnown(input.Members, thread.Members); err != nil {
			s.messageError(w, err)
			return true
		}
		updated, notify, err := s.messages.SetMembers(r.Context(), person.ID, thread.ID, input.Members)
		if err != nil {
			s.messageError(w, err)
		} else {
			s.notifyMessages(notify)
			respond(w, http.StatusOK, map[string]any{"thread": updated})
		}
		return true
	}
	if parts[2] == "messages" {
		var input struct {
			ClientID string `json:"client_id"`
			Body     string `json:"body"`
		}
		if err := readJSON(w, r, &input); err != nil {
			fail(w, http.StatusBadRequest, err)
			return true
		}
		message, recipients, err := s.messages.Send(r.Context(), person.ID, person.Username, thread.ID, input.ClientID, input.Body)
		if err != nil {
			s.messageError(w, err)
		} else {
			s.notifyMessages(recipients)
			respond(w, http.StatusCreated, map[string]any{"message": message, "delivery": "stored", "rf_delivery": "pending"})
		}
		return true
	}
	var input struct {
		MessageID int64 `json:"message_id"`
	}
	if err := readJSON(w, r, &input); err != nil {
		fail(w, http.StatusBadRequest, err)
		return true
	}
	if err := s.messages.MarkRead(r.Context(), person.ID, thread.ID, input.MessageID); err != nil {
		s.messageError(w, err)
		return true
	}
	after, err := s.messages.Conversation(r.Context(), person.ID, thread.ID)
	if err != nil {
		s.messageError(w, err)
		return true
	}
	if after.ReadThroughID != thread.ReadThroughID {
		s.notifyMessages([]string{person.ID})
	}
	respond(w, http.StatusOK, map[string]any{"read_through_id": after.ReadThroughID})
	return true
}

func (s *Service) createMessageThread(w http.ResponseWriter, r *http.Request, person User) {
	var input struct {
		Kind    string   `json:"kind"`
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	if err := readJSON(w, r, &input); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if input.Kind != "direct" && input.Kind != "group" || input.Kind == "direct" && len(input.Members) != 1 {
		s.messageError(w, messaging.ErrInvalid)
		return
	}
	if err := s.messagePeopleKnown(input.Members, nil); err != nil {
		s.messageError(w, err)
		return
	}
	var thread messaging.Conversation
	var err error
	if input.Kind == "direct" {
		thread, err = s.messages.CreateDirect(r.Context(), person.ID, input.Members[0])
	} else {
		thread, err = s.messages.CreateGroup(r.Context(), person.ID, input.Name, input.Members)
	}
	if err != nil {
		s.messageError(w, err)
		return
	}
	s.notifyMessages(thread.Members)
	respond(w, http.StatusCreated, map[string]any{"thread": thread})
}

func (s *Service) messagePeopleKnown(selected, existing []string) error {
	users, err := s.Store.Users()
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(users))
	for _, user := range users {
		known[user.ID] = user.Enabled
	}
	// Existing membership is preserved as submitted. Only a new recipient must
	// currently be enabled; disabling an account already prevents its session.
	for _, id := range existing {
		known[id] = true
	}
	for _, id := range selected {
		if !known[id] {
			return messaging.ErrInvalid
		}
	}
	return nil
}

func (s *Service) messageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, messaging.ErrForbidden), errors.Is(err, messaging.ErrNotFound):
		fail(w, http.StatusNotFound, errors.New("conversation not found"))
	case errors.Is(err, messaging.ErrConflict):
		fail(w, http.StatusConflict, err)
	case errors.Is(err, messaging.ErrInvalid):
		fail(w, http.StatusBadRequest, err)
	default:
		fail(w, http.StatusInternalServerError, errors.New("messages could not be saved or loaded; keep your draft and try again"))
	}
}

func (s *Service) notifyMessages(people []string) {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	for _, person := range people {
		for changed := range s.messageSubscribers[person] {
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}
}

// Events invalidate this person's inbox only. Conversation IDs, content and
// counters are obtained from the ordinary authenticated membership-scoped reads.
func (s *Service) messageEvents(w http.ResponseWriter, r *http.Request, personID string) {
	if _, ok := w.(http.Flusher); !ok {
		fail(w, http.StatusServiceUnavailable, errors.New("live message updates are unavailable; use Refresh messages"))
		return
	}
	changed := make(chan struct{}, 1)
	s.messageMu.Lock()
	if s.messageSubscribers == nil {
		s.messageSubscribers = map[string]map[chan struct{}]struct{}{}
	}
	if s.messageSubscribers[personID] == nil {
		s.messageSubscribers[personID] = map[chan struct{}]struct{}{}
	}
	s.messageSubscribers[personID][changed] = struct{}{}
	s.messageMu.Unlock()
	defer func() {
		s.messageMu.Lock()
		delete(s.messageSubscribers[personID], changed)
		if len(s.messageSubscribers[personID]) == 0 {
			delete(s.messageSubscribers, personID)
		}
		s.messageMu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(60 * time.Second))
	valid := func() bool {
		current, ok := s.session(r)
		return ok && current.UserID == personID
	}
	write := func(value string) bool {
		if !valid() {
			return false
		}
		if _, err := io.WriteString(w, value); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !write("event: inbox\ndata: {}\n\n") {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.messageStop:
			return
		case <-deadline.C:
			return
		case <-heartbeat.C:
			if !write(": keepalive\n\n") {
				return
			}
		case <-changed:
			if !write("event: inbox\ndata: {}\n\n") {
				return
			}
		}
	}
}

package framework

import (
	"encoding/json"
	"errors"
	"net/http"
)

type historyScope struct {
	Role       Role   `json:"role"`
	InstanceID string `json:"instance_id"`
	Coverage   string `json:"coverage"`
}

func (s *Service) historyScope() historyScope {
	return historyScope{s.Config().Role, s.Config().InstanceID, "system_archive"}
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

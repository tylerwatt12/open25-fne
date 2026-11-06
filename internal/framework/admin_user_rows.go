package framework

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type adminRecordWindow struct {
	Total, Offset int
	Indices       []int
	Editor        bool
}

// Read one editor or one bounded list page from the owning store. Never decode
// another user's full profile or a password verifier to render this page.
func (s *Service) webUserRows(r *http.Request) ([]any, *adminRecordWindow, error) {
	page := &adminRecordWindow{}
	query := r.URL.Query()
	where := ""
	args := []any{}
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/edit") {
		if query.Has("identity") {
			where = "json_extract(value,'$.user.id')=?"
			args = append(args, query.Get("identity"))
			page.Editor = true
		} else if query.Has("row") {
			index, err := strconv.Atoi(query.Get("row"))
			if err != nil || index < 0 {
				return nil, page, sql.ErrNoRows
			}
			where = "CAST(key AS INTEGER)=?"
			args = append(args, index)
			page.Editor = true
		} else {
			return []any{}, page, nil
		}
	} else if r.Method == http.MethodPost {
		return []any{}, page, nil
	}
	if !page.Editor {
		page.Offset, _ = strconv.Atoi(query.Get("offset"))
		if page.Offset < 0 {
			page.Offset = 0
		}
		q := strings.ToLower(strings.TrimSpace(query.Get("q")))
		if q == "" {
			err := s.Store.db.QueryRowContext(r.Context(), `SELECT coalesce(json_array_length(document),0) FROM framework_documents WHERE name='users'`).Scan(&page.Total)
			if err != nil && err != sql.ErrNoRows {
				return nil, page, err
			}
		} else {
			matches, err := s.Store.db.QueryContext(r.Context(), `SELECT CAST(key AS INTEGER),coalesce(json_extract(value,'$.user.id'),''),coalesce(json_extract(value,'$.user.username'),''),coalesce(json_extract(value,'$.user.profile.name'),'') FROM json_each((SELECT document FROM framework_documents WHERE name='users'))`)
			if err != nil {
				return nil, page, err
			}
			for matches.Next() {
				var index int
				var id, name, profile string
				if err = matches.Scan(&index, &id, &name, &profile); err != nil {
					break
				}
				if !strings.Contains(strings.ToLower(id+" "+name+" "+profile), q) {
					continue
				}
				if page.Total >= page.Offset && len(page.Indices) < adminChoicePageSize {
					page.Indices = append(page.Indices, index)
				}
				page.Total++
			}
			if err == nil {
				err = matches.Err()
			}
			matches.Close()
			if err != nil {
				return nil, page, err
			}
			if len(page.Indices) == 0 {
				return []any{}, page, nil
			}
			marks := []string{}
			for _, index := range page.Indices {
				marks = append(marks, "?")
				args = append(args, index)
			}
			where = "CAST(key AS INTEGER) IN (" + strings.Join(marks, ",") + ")"
		}
	}
	projection := `json_extract(value,'$.user')`
	if !page.Editor {
		projection = `json_object('id',json_extract(value,'$.user.id'),'username',json_extract(value,'$.user.username'),'role',json_extract(value,'$.user.role'),'radio_id',json_extract(value,'$.user.radio_id'),'enabled',json_extract(value,'$.user.enabled'),'profile',json_object('name',json_extract(value,'$.user.profile.name'),'preferred_position_id',json_extract(value,'$.user.profile.preferred_position_id'),'selected_talkgroup',json_extract(value,'$.user.profile.selected_talkgroup')))`
	}
	statement := `SELECT CAST(key AS INTEGER),` + projection + ` FROM json_each((SELECT document FROM framework_documents WHERE name='users'))`
	if where != "" {
		statement += " WHERE " + where
	}
	if page.Editor {
		statement += " LIMIT 1"
	} else if where == "" {
		statement += fmt.Sprintf(" LIMIT %d OFFSET %d", adminChoicePageSize, page.Offset)
	}
	rows, err := s.Store.db.QueryContext(r.Context(), statement, args...)
	if err != nil {
		return nil, page, err
	}
	defer rows.Close()
	result := []any{}
	indices := []int{}
	for rows.Next() {
		var index int
		var raw string
		if err = rows.Scan(&index, &raw); err != nil {
			return nil, page, err
		}
		record, err := webConfig([]byte(raw))
		if err != nil {
			return nil, page, err
		}
		if !page.Editor {
			record["enabled"] = webText(record["enabled"]) == "1"
		}
		result = append(result, record)
		indices = append(indices, index)
	}
	page.Indices = indices
	return result, page, rows.Err()
}

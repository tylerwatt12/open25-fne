package framework

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const adminChoicePageSize = 50

type adminChoicePage struct {
	Notice string        `json:"notice,omitempty"`
	Items  []adminChoice `json:"items"`
	Total  int           `json:"total"`
	Offset int           `json:"offset"`
	Limit  int           `json:"limit"`
}

// Explicit administration searches only. Project two public identity fields from
// owned/accepted records; never return credentials, policy or key material.
func (s *Service) webChooser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	kind, q := r.URL.Query().Get("kind"), strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if r.URL.Query().Get("offset") == "" {
		offset = 0
		err = nil
	}
	if err != nil || offset < 0 {
		http.Error(w, "Could not read result page.", 400)
		return
	}
	page := adminChoicePage{Items: []adminChoice{}, Offset: offset, Limit: adminChoicePageSize}
	addChoice := func(choice adminChoice) {
		id, name := choice.ID, choice.Name
		if r.URL.Query().Has("identity") && id != r.URL.Query().Get("identity") {
			return
		}
		if q != "" && !strings.Contains(strings.ToLower(id), q) && !strings.Contains(strings.ToLower(name), q) {
			return
		}
		if page.Total >= offset && len(page.Items) < adminChoicePageSize {
			page.Items = append(page.Items, choice)
		}
		page.Total++
	}
	add := func(id, name string) { addChoice(adminChoice{ID: id, Name: name}) }
	view := s.view()
	switch kind {
	case "radios-groups", "talkgroups-groups":
		groups := map[string]bool{}
		if kind == "radios-groups" {
			for _, v := range view.System.Radios {
				if v.Group != "" {
					groups[v.Group] = true
				}
			}
		} else {
			for _, v := range view.System.Talkgroups {
				if v.Group != "" {
					groups[v.Group] = true
				}
			}
		}
		ordered := make([]string, 0, len(groups))
		for name := range groups {
			ordered = append(ordered, name)
		}
		sort.Strings(ordered)
		for _, name := range ordered {
			add(name, name)
		}
	case "instances":
		for _, v := range s.adminInstances() {
			if v.URL != "" {
				add(v.URL, adminTier(v.Role)+": "+v.Name)
			}
		}
	case "usb":
		if s.Config().Role != RoleSite {
			http.NotFound(w, r)
			return
		}
		inventory := s.hardwareInventory()
		page.Notice = inventory.Error
		for _, v := range webUSBChoices(inventory) {
			add(v.ID, v.Name)
		}
	case "keys":
		if s.Config().Role != RoleSystem {
			http.NotFound(w, r)
			return
		}
		keys, err := s.readTrafficKeys()
		if err != nil {
			http.Error(w, "Could not read key identities.", 500)
			return
		}
		defer func() {
			for i := range keys {
				keys[i].Material = ""
			}
		}()
		for _, v := range keys {
			add(fmt.Sprintf("%d/%d", v.AlgorithmID, v.KeyID), v.Name)
		}
	case "radios":
		for _, v := range view.System.Radios {
			add(strconv.Itoa(v.RID), v.Alias)
		}
	case "talkgroups":
		for _, v := range view.System.Talkgroups {
			add(strconv.Itoa(v.TGID), v.Alias)
		}
	case "positions":
		for _, v := range view.System.ConsolePositions {
			add(v.ID, v.Name)
		}
	case "systems":
		add(view.System.ID, view.System.Name)
	case "sites", "bands":
		if zone := s.webKnownZone(); zone != nil {
			if kind == "sites" {
				for _, v := range zone.Sites {
					add(v.ID, v.Name)
				}
			} else {
				for _, v := range zone.ChannelIdentifiers {
					addChoice(adminChoice{ID: strconv.Itoa(v.Identifier), Name: fmt.Sprintf("%s MHz · %s kHz", webUnitText("base_frequency_hz", v.BaseHz), webUnitText("channel_spacing_hz", v.SpacingHz)), BaseHz: strconv.FormatInt(v.BaseHz, 10), SpacingHz: strconv.FormatInt(v.SpacingHz, 10), OffsetHz: strconv.FormatInt(v.OffsetHz, 10)})
				}
			}
		}
	case "modems", "channels":
		if s.Config().Role != RoleSite {
			http.NotFound(w, r)
			return
		}
		var site SiteConfig
		if err := s.owned(&site); err != nil {
			http.Error(w, "Could not read Site records.", 500)
			return
		}
		if kind == "modems" {
			for _, v := range site.Modems {
				add(v.ID, v.Name)
			}
		} else {
			for _, v := range site.Channels {
				add(v.ID, v.Name)
			}
		}
	case "users":
		if s.Config().Role != RoleSystem {
			http.NotFound(w, r)
			return
		}
		// SQLite projects public fields without decoding password hashes or profiles.
		rows, err := s.Store.db.QueryContext(r.Context(), `SELECT json_extract(value,'$.user.id'),json_extract(value,'$.user.username') FROM json_each((SELECT document FROM framework_documents WHERE name='users'))`)
		if err != nil {
			http.Error(w, "Could not read login identities.", 500)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id, name string
			if rows.Scan(&id, &name) != nil {
				http.Error(w, "Could not read login identity.", 500)
				return
			}
			add(id, name)
		}
		if rows.Err() != nil {
			http.Error(w, "Could not read login identities.", 500)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	respond(w, http.StatusOK, page)
}

func webChooserButton(w io.Writer, kind, target string, multiple bool, labels ...string) {
	label := "Choose"
	if len(labels) > 0 {
		label = labels[0]
	}
	fmt.Fprintf(w, `<button type=button data-chooser-kind="%s" data-chooser-target="%s" data-chooser-multiple="%t">%s</button>`, esc(kind), esc(target), multiple, esc(label))
}

// Lists and choosers share bounded pages; submitted records retain their own identity.
func webRecordPage(w io.Writer, r *http.Request, path string, rows []any, fields []webField, csrf string, windows ...*adminRecordWindow) {
	selectable := path == "radios" || path == "talkgroups"
	if selectable {
		fmt.Fprintf(w, `<form id=bulk-record-selection method=post action="/%s/bulk" class="ts-panel ts-bulkbar">`, esc(path))
		webHidden(w, csrf)
		fmt.Fprint(w, `<input type=hidden name=selection data-bulk-selection>`)
		webChooserButton(w, path, "selection", true, "Select records")
		fmt.Fprint(w, `<span class=ts-caption data-bulk-count>0 selected</span><button name=action value=edit>Edit selected</button></form>`)
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	indices := []int{}
	total := 0
	var window *adminRecordWindow
	if len(windows) > 0 {
		window = windows[0]
	}
	if window != nil {
		total = window.Total
		offset = window.Offset
		for i := range rows {
			indices = append(indices, i)
		}
	} else {
		for i, item := range rows {
			object, _ := item.(map[string]any)
			match := q == ""
			for _, f := range fields {
				if strings.Contains(strings.ToLower(webDisplay(f, webValue(object, f.Name))), q) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			if total >= offset && len(indices) < adminChoicePageSize {
				indices = append(indices, i)
			}
			total++
		}
	}
	fmt.Fprintf(w, `<section class="ts-panel ts-records"><form method=get class=ts-toolbar><span class=ts-caption>%d records</span><input type=search name=q value="%s" aria-label="Search records" placeholder="Search name or identity…"><button>Search</button></form><div class=ts-tablescroll tabindex=0 aria-label="Scrollable records"><table><thead><tr>`, total, esc(r.URL.Query().Get("q")))
	if selectable {
		fmt.Fprint(w, `<th scope=col><input type=checkbox data-bulk-page aria-label="Select displayed records"></th>`)
	}
	for _, f := range fields {
		fmt.Fprintf(w, `<th scope=col>%s</th>`, esc(f.Label))
	}
	fmt.Fprint(w, `<th scope=col>Actions</th></tr></thead><tbody>`)
	for _, i := range indices {
		object, _ := rows[i].(map[string]any)
		fmt.Fprint(w, `<tr>`)
		if selectable {
			fmt.Fprintf(w, `<td><input type=checkbox data-bulk-record value="%s" aria-label="Select %s"></td>`, esc(webText(webValue(object, fields[0].Name))), esc(webText(webValue(object, fields[0].Name))))
		}
		for _, f := range fields {
			fmt.Fprintf(w, `<td>%s</td>`, esc(webDisplay(f, webValue(object, f.Name))))
		}
		rowIndex := i
		if window != nil {
			rowIndex = window.Indices[i]
		}
		fmt.Fprintf(w, `<td><div class=ts-actions><a class=ts-link href=/%s/edit?row=%d>Edit</a>`, esc(path), rowIndex)
		if selectable {
			fmt.Fprintf(w, `<a class=ts-link href=/%s/edit?clone=%d>Clone</a>`, esc(path), i)
		}
		fmt.Fprint(w, `</div></td></tr>`)
	}
	if len(indices) == 0 {
		fmt.Fprintf(w, `<tr><td colspan=%d>No matching records.</td></tr>`, len(fields)+1+map[bool]int{true: 1}[selectable])
	}
	fmt.Fprint(w, `</tbody></table></div><nav class=ts-actions aria-label="Record pages">`)
	for _, step := range []int{-adminChoicePageSize, adminChoicePageSize} {
		next := offset + step
		if next < 0 || (step > 0 && next >= total) {
			continue
		}
		query := r.URL.Query()
		query.Set("offset", strconv.Itoa(next))
		label := "Next"
		if step < 0 {
			label = "Previous"
		}
		fmt.Fprintf(w, `<a class=ts-button href="?%s">%s</a>`, esc(query.Encode()), label)
	}
	first := 0
	if len(indices) > 0 {
		first = offset + 1
	}
	last := 0
	if len(indices) > 0 {
		last = offset + len(indices)
	}
	fmt.Fprintf(w, `<span>%d–%d of %d</span></nav></section>`, first, last, total)
}

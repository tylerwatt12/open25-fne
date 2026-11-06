package framework

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func webSelection(text string) map[string]bool {
	selected := map[string]bool{}
	for _, id := range strings.FieldsFunc(text, func(c rune) bool { return c == ',' || c == ' ' || c == '\n' || c == '\r' || c == '\t' }) {
		selected[id] = true
	}
	return selected
}

// Bulk edits are one explicit owner Save. Only checked fields are changed.
func (s *Service) webBulkRecords(w http.ResponseWriter, r *http.Request, session Session, section webSection) {
	if s.Config().Role != RoleSystem || (section.Path != "radios" && section.Path != "talkgroups") {
		http.NotFound(w, r)
		return
	}
	raw, err := s.Store.Config()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	config, err := webConfig(raw)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	rows, _ := config[section.Collection].([]any)
	selection := r.FormValue("selection")
	selected := webSelection(selection)
	identity := section.Fields[0].Name
	record := map[string]any{}
	count := 0
	for _, item := range rows {
		v, _ := item.(map[string]any)
		if selected[webText(v[identity])] {
			if count == 0 {
				record = v
			}
			count++
		}
	}
	apply := map[string]bool{}
	for _, name := range r.PostForm["apply"] {
		apply[name] = true
	}
	if r.Method == http.MethodPost && r.FormValue("action") == "save" {
		fields := []webField{}
		for _, f := range section.Fields {
			if apply[f.Name] {
				fields = append(fields, f)
			}
		}
		if len(fields) == 0 {
			count = 0
		}
		patch := map[string]any{}
		err = webReadFields(r, fields, patch)
		if err == nil {
			for _, item := range rows {
				v, _ := item.(map[string]any)
				if selected[webText(v[identity])] {
					for _, f := range fields {
						v[f.Name] = patch[f.Name]
					}
				}
			}
			var encoded []byte
			encoded, err = json.Marshal(config)
			if err == nil {
				err = s.saveOwned(encoded)
			}
			if err == nil {
				s.refreshDirectory()
				http.Redirect(w, r, fmt.Sprintf("/%s?saved=1&updated=%d", section.Path, count), http.StatusSeeOther)
				return
			}
		}
	}
	s.webHeader(w, r, "Edit selected "+section.Path, session, false)
	defer webFooter(w)
	if err != nil {
		webError(w, err)
	}
	fmt.Fprintf(w, `<p class=ts-reference>%d selected records. Check Apply beside each setting to change.</p><div class=ts-editor><form method=post data-config-form action="/%s/bulk">`, count, section.Path)
	webHidden(w, session.CSRF)
	fmt.Fprintf(w, `<div class=ts-field><label for=bulk-selection>Selected identities</label><div class=ts-relation><input id=bulk-selection name=selection value="%s" data-bulk-selection>`, esc(selection))
	webChooserButton(w, section.Path, "selection", true)
	fmt.Fprint(w, `</div></div>`)
	fields := append([]webField(nil), section.Fields...)
	for i := range fields {
		fields[i].BulkApply = true
		fields[i].ApplyChecked = apply[fields[i].Name]
	}
	if r.Method == http.MethodPost && r.FormValue("action") == "save" {
		s.adminEditorFields(w, section.Path, fields, record, r.PostForm)
	} else {
		s.adminEditorFields(w, section.Path, fields, record)
	}
	fmt.Fprintf(w, `<div class=ts-savebar><span data-admin-unsaved-label></span><div class=ts-actions><a class=ts-button data-admin-cancel href="/%s">Cancel</a><button class=ts-primary name=action value=save formnovalidate>Save selected</button></div></div></form></div>`, section.Path)
}

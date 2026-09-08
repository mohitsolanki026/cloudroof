package api

import (
	"net/http"
	"regexp"
	"strings"

	"cloudroof/internal/actions"
	"cloudroof/internal/store"
)

type customActionInput struct {
	Label    string                    `json:"label"`
	Category string                    `json:"category"`
	Command  string                    `json:"command"`
	Params   []store.CustomActionParam `json:"params"`
	Requires []string                  `json:"requires"`
	Sudo     string                    `json:"sudo"`
	Danger   int                       `json:"danger"`
}

var reParamName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func (s *Server) listCustomActions(w http.ResponseWriter, r *http.Request) {
	as, err := s.store.ListCustomActions()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, as)
}

func (s *Server) createCustomAction(w http.ResponseWriter, r *http.Request) {
	var in customActionInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	in.Label = strings.TrimSpace(in.Label)
	in.Command = strings.TrimSpace(in.Command)
	if in.Label == "" || in.Command == "" {
		badRequest(w, "label and command are required")
		return
	}
	// The gate refuses Tier 3 anyway, but reject it at the door so a Tier-3
	// button can never even be saved. Read/reversible/disruptive only.
	if in.Danger < actions.DangerRead || in.Danger > actions.DangerDisruptive {
		badRequest(w, "danger must be 0 (read), 1 (confirm), or 2 (type-to-confirm)")
		return
	}
	switch in.Sudo {
	case "", string(actions.SudoPreferred), string(actions.SudoRequired):
	default:
		badRequest(w, "sudo must be empty, preferred, or required")
		return
	}
	for _, p := range in.Params {
		if !reParamName.MatchString(p.Name) {
			badRequest(w, "param names must be lowercase letters/digits/underscore, starting with a letter: "+p.Name)
			return
		}
		if !strings.Contains(in.Command, "{{"+p.Name+"}}") {
			badRequest(w, "param "+p.Name+" is declared but not used as {{"+p.Name+"}} in the command")
			return
		}
	}

	id := "custom." + slug(in.Label)
	if _, err := s.store.GetCustomAction(id); err == nil {
		badRequest(w, "a custom action with that name already exists")
		return
	}
	if _, ok := actions.Lookup(id); ok {
		badRequest(w, "that name collides with a built-in action")
		return
	}
	category := strings.TrimSpace(in.Category)
	if category == "" {
		category = "custom"
	}
	a, err := s.store.CreateCustomAction(store.CustomAction{
		ID: id, Label: in.Label, Category: category, Command: in.Command,
		Params: in.Params, Requires: cleanTags(in.Requires), Sudo: in.Sudo, Danger: in.Danger,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, a)
}

func (s *Server) deleteCustomAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteCustomAction(id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

// slug turns a label into a URL- and shell-safe id fragment.
func slug(label string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

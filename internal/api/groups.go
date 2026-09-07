package api

import (
	"net/http"
	"strings"

	"bosun/internal/store"
)

// A group is a named set of tags; its members are the machines carrying ALL of
// them, resolved live. Groups exist to target bulk actions.

type groupInput struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListGroups()
	if err != nil {
		fail(w, err)
		return
	}
	machines, err := s.store.ListMachines()
	if err != nil {
		fail(w, err)
		return
	}
	for i := range groups {
		groups[i].Members = countMatching(machines, groups[i].Tags)
	}
	writeJSON(w, 200, groups)
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	var in groupInput
	if err := readJSON(w, r, &in); err != nil {
		badRequest(w, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	tags := cleanTags(in.Tags)
	if in.Name == "" {
		badRequest(w, "name is required")
		return
	}
	if len(tags) == 0 {
		// A tagless group would match nothing (by design), so it is useless;
		// reject it rather than create a dead group.
		badRequest(w, "a group needs at least one tag")
		return
	}
	g, err := s.store.CreateGroup(store.Group{Name: in.Name, Tags: tags})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, g)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	if err := s.store.DeleteGroup(id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) groupMachines(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	members, err := s.store.GroupMembers(id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, s.withFacts(members))
}

// withFacts decorates machines with their host facts for the fleet views.
func (s *Server) withFacts(machines []store.Machine) []machineView {
	out := make([]machineView, 0, len(machines))
	for _, m := range machines {
		v := machineView{Machine: m}
		if f, err := s.store.GetFacts(m.ID); err == nil {
			v.Facts = &f
		}
		out = append(out, v)
	}
	return out
}

func cleanTags(in []string) []string {
	out := []string{}
	for _, t := range in {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func countMatching(machines []store.Machine, want []string) int {
	if len(want) == 0 {
		return 0
	}
	n := 0
	for _, m := range machines {
		have := make(map[string]bool, len(m.Tags))
		for _, t := range m.Tags {
			have[t] = true
		}
		ok := true
		for _, wtag := range want {
			if !have[wtag] {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}

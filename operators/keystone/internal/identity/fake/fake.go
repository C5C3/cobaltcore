// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package fake provides an httptest-backed in-memory double of the Keystone
// identity API surface the operator's minimal identity client consumes: the
// password-method token endpoint and domain CRUD. It records every request so
// tests can assert interaction contracts — "adopt never mutates" and
// "disable before delete" — from the request log rather than from
// implementation internals. This is the "identity-API test double via
// httptest; no ORC fixtures needed" seam shared by the identity client's unit
// tests, the controller unit tests, and envtest integration.
package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Token is the static token value the fake issues on successful
// authentication.
const Token = "fake-identity-token"

// Domain mirrors identity.Domain without importing it, keeping the fake
// import-cycle-free for the identity package's own tests.
type Domain struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// Server is the in-memory identity API double.
type Server struct {
	mu sync.Mutex

	// Password is the admin password the token endpoint accepts; any other
	// password yields HTTP 401.
	Password string

	domains map[string]*Domain // keyed by ID
	nextID  int

	// requests records "METHOD /path" for every call, in order.
	requests []string

	httpServer *httptest.Server
}

// NewServer starts the fake identity API accepting the given admin password.
// Callers must Close() it (t.Cleanup-friendly).
func NewServer(password string) *Server {
	s := &Server{
		Password: password,
		domains:  make(map[string]*Domain),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/auth/tokens", s.handleAuth)
	mux.HandleFunc("/v3/domains", s.handleDomains)
	mux.HandleFunc("/v3/domains/", s.handleDomainByID)
	s.httpServer = httptest.NewServer(mux)
	return s
}

// Close shuts the underlying httptest server down.
func (s *Server) Close() { s.httpServer.Close() }

// Endpoint returns the /v3 base URL clients authenticate against.
func (s *Server) Endpoint() string { return s.httpServer.URL + "/v3" }

// Requests returns a copy of the recorded "METHOD /path" log.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.requests))
	copy(out, s.requests)
	return out
}

// MutatingRequests returns the subset of recorded requests that mutate
// domain state (POST/PATCH/DELETE on /v3/domains*), for adopt-never-mutates
// assertions.
func (s *Server) MutatingRequests() []string {
	var out []string
	for _, r := range s.Requests() {
		if strings.HasPrefix(r, "GET ") || strings.HasPrefix(r, "POST /v3/auth/tokens") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// SeedDomain pre-creates a domain (for adopt / already-exists scenarios) and
// returns its assigned ID.
func (s *Server) SeedDomain(name, description string, enabled bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.allocateIDLocked()
	s.domains[id] = &Domain{ID: id, Name: name, Description: description, Enabled: enabled}
	return id
}

// GetDomain returns a copy of the domain with the given ID, or nil.
func (s *Server) GetDomain(id string) *Domain {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.domains[id]
	if !ok {
		return nil
	}
	out := *d
	return &out
}

// GetDomainByName returns a copy of the domain with the given name, or nil.
func (s *Server) GetDomainByName(name string) *Domain {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.domains {
		if d.Name == name {
			out := *d
			return &out
		}
	}
	return nil
}

func (s *Server) allocateIDLocked() string {
	s.nextID++
	return fmt.Sprintf("domain-%04d", s.nextID)
}

func (s *Server) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Auth struct {
			Identity struct {
				Password struct {
					User struct {
						Name     string `json:"name"`
						Password string `json:"password"`
					} `json:"user"`
				} `json:"password"`
			} `json:"identity"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if body.Auth.Identity.Password.User.Password != s.Password {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("X-Subject-Token", Token)
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"token":{}}`))
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Auth-Token") != Token {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if !s.authorized(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		name := r.URL.Query().Get("name")
		s.mu.Lock()
		var matches []Domain
		for _, d := range s.domains {
			if name == "" || d.Name == name {
				matches = append(matches, *d)
			}
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"domains": matches})
	case http.MethodPost:
		var body struct {
			Domain struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Enabled     *bool  `json:"enabled"`
			} `json:"domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		for _, d := range s.domains {
			if d.Name == body.Domain.Name {
				s.mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		id := s.allocateIDLocked()
		enabled := body.Domain.Enabled == nil || *body.Domain.Enabled
		d := &Domain{ID: id, Name: body.Domain.Name, Description: body.Domain.Description, Enabled: enabled}
		s.domains[id] = d
		out := *d
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"domain": out})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleDomainByID(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if !s.authorized(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v3/domains/")
	s.mu.Lock()
	d, ok := s.domains[id]
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"domain": *d})
	case http.MethodPatch:
		var body struct {
			Domain struct {
				Enabled     *bool   `json:"enabled"`
				Description *string `json:"description"`
			} `json:"domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		if body.Domain.Enabled != nil {
			d.Enabled = *body.Domain.Enabled
		}
		if body.Domain.Description != nil {
			d.Description = *body.Domain.Description
		}
		out := *d
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"domain": out})
	case http.MethodDelete:
		// Real Keystone forbids deleting an enabled domain — the fake keeps
		// that contract so the disable-before-delete order is observable.
		s.mu.Lock()
		if d.Enabled {
			s.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			return
		}
		delete(s.domains, id)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

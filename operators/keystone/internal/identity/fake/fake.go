// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package fake provides an httptest-backed in-memory double of the Keystone
// identity API surface the operator's minimal identity client consumes: the
// password-method token endpoint, domain CRUD, and the federation-object
// surface (identity providers, protocols, mappings, groups, roles, role
// assignments). It records every request so tests can assert interaction
// contracts — "adopt never mutates", "disable before delete", "drift-only
// writes", and "teardown order" — from the request log rather than from
// implementation internals. InjectFault arms a Fault that answers matching
// requests with an error status or a closed connection, so tests also reach
// the error paths a correct answer never takes. This is the "identity-API
// test double via httptest; no ORC fixtures needed" seam shared by the
// identity client's unit tests, the controller unit tests, and envtest
// integration.
package fake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
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

// IdentityProvider mirrors identity.IdentityProvider (import-cycle-free).
type IdentityProvider struct {
	ID          string   `json:"id"`
	DomainID    string   `json:"domain_id"`
	Description string   `json:"description,omitempty"`
	Enabled     bool     `json:"enabled"`
	RemoteIDs   []string `json:"remote_ids"`
}

// Protocol mirrors identity.Protocol (import-cycle-free).
type Protocol struct {
	ID        string `json:"id"`
	MappingID string `json:"mapping_id"`
}

// Mapping stores the mapping rules as raw JSON so the fake needs no
// gophercloud import: the client sends {"mapping": {"rules": [...]}} and the
// fake round-trips the rules array verbatim.
type Mapping struct {
	ID    string          `json:"id"`
	Rules json.RawMessage `json:"rules"`
}

// Group mirrors identity.Group (import-cycle-free).
type Group struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DomainID    string `json:"domain_id"`
	Description string `json:"description,omitempty"`
}

// Role mirrors identity.Role (import-cycle-free).
type Role struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Project mirrors identity.Project (import-cycle-free).
type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	DomainID string `json:"domain_id"`
}

// Server is the in-memory identity API double.
type Server struct {
	mu sync.Mutex

	// Password is the admin password the token endpoint accepts; any other
	// password yields HTTP 401.
	Password string

	domains map[string]*Domain // keyed by ID
	nextID  int

	idps      map[string]*IdentityProvider    // keyed by ID
	protocols map[string]map[string]*Protocol // idp ID -> protocol ID
	mappings  map[string]*Mapping             // keyed by ID
	groups    map[string]*Group               // keyed by ID
	roles     map[string]*Role                // keyed by ID
	projects  map[string]*Project             // keyed by ID
	// roleAssignments records "domain|project/<scopeID>/group/<gID>/role/<rID>".
	roleAssignments map[string]struct{}

	// requests records "METHOD /path" for every call, in order.
	requests []string
	// faults holds the armed faults in registration order.
	faults []Fault

	mux        *http.ServeMux
	httpServer *httptest.Server
}

// Fault is one injected failure. The fake answers a matching request with
// Status and Body, or hijacks and closes the connection when CloseConnection
// is set, instead of running the handler.
//
// net/http replays a GET or HEAD once on a new connection when a reused
// keep-alive connection dies before any response byte arrives. A
// CloseConnection fault with Times: 1 on a GET is therefore spent on the
// first attempt, and the client sees the replay succeed. A connection-close
// fault that must reach the caller targets a request net/http does not
// replay (POST /v3/auth/tokens, which precedes every API call, or any PUT,
// PATCH or DELETE) or arms Times: 2. Keep-alives stay enabled because the
// envtest integration suite runs against this transport.
type Fault struct {
	// Method matches r.Method exactly; "" matches every method.
	Method string
	// Path matches r.URL.Path (the query string is not part of it). "" matches
	// every path. A Path ending in "/" matches the subtree below it, like an
	// http.ServeMux pattern: "/v3/domains/" matches "/v3/domains/domain-0001"
	// and "/v3/domains/domain-0001/groups/g/roles/r".
	Path string
	// Query matches r.URL.RawQuery exactly; "" matches every query string.
	// It tells apart lookups that share a path, such as "name=corp" and
	// "name=shared" on GET /v3/domains.
	Query string
	// Status is the HTTP status written when CloseConnection is false, and
	// required then: a status outside 200-999 answers 500 with a "fake:"
	// error body instead.
	Status int
	// Body is the optional response body written with Status.
	Body string
	// CloseConnection hijacks the TCP connection and closes it without
	// writing a response. The client sees io.EOF.
	CloseConnection bool
	// Times is how many matching requests the fault fires for; 0 fires
	// until ClearFaults. Each handler invocation counts once.
	Times int
}

// matches reports whether r falls under the fault's method, query and path.
func (f Fault) matches(r *http.Request) bool {
	if f.Method != "" && f.Method != r.Method {
		return false
	}
	if f.Query != "" && f.Query != r.URL.RawQuery {
		return false
	}
	if f.Path == "" || f.Path == r.URL.Path {
		return true
	}
	return strings.HasSuffix(f.Path, "/") && strings.HasPrefix(r.URL.Path, f.Path)
}

// NewServer starts the fake identity API accepting the given admin password.
// Callers must Close() it (t.Cleanup-friendly).
func NewServer(password string) *Server {
	s := &Server{
		Password:        password,
		domains:         make(map[string]*Domain),
		idps:            make(map[string]*IdentityProvider),
		protocols:       make(map[string]map[string]*Protocol),
		mappings:        make(map[string]*Mapping),
		groups:          make(map[string]*Group),
		roles:           make(map[string]*Role),
		projects:        make(map[string]*Project),
		roleAssignments: make(map[string]struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/auth/tokens", s.handleAuth)
	mux.HandleFunc("/v3/domains", s.handleDomains)
	mux.HandleFunc("/v3/domains/", s.handleDomainByID)
	mux.HandleFunc("/v3/OS-FEDERATION/identity_providers/", s.handleIdentityProviders)
	mux.HandleFunc("/v3/OS-FEDERATION/mappings/", s.handleMappings)
	mux.HandleFunc("/v3/groups", s.handleGroups)
	mux.HandleFunc("/v3/roles", s.handleRoles)
	mux.HandleFunc("/v3/projects", s.handleProjects)
	mux.HandleFunc("/v3/projects/", s.handleProjectSubpath)
	s.mux = mux
	s.httpServer = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
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

// MutatingRequests returns the subset of recorded requests that mutate server
// state (everything except GETs and the token endpoint), for
// adopt-never-mutates and drift-only-writes assertions.
func (s *Server) MutatingRequests() []string {
	var out []string
	for _, r := range s.Requests() {
		if strings.HasPrefix(r, "GET ") || strings.HasPrefix(r, "HEAD ") || strings.HasPrefix(r, "POST /v3/auth/tokens") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// InjectFault arms f. Faults are matched in registration order; the first
// match fires. A fault armed with a positive Times is removed once it has
// fired that many times; Times 0 keeps it armed until ClearFaults.
func (s *Server) InjectFault(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, f)
}

// ClearFaults disarms every fault.
func (s *Server) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
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

// serveHTTP records every request once, answers it from the first matching
// fault, and routes it to the API handlers otherwise.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if f, ok := s.takeFault(r); ok {
		serveFault(w, r, f)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// takeFault returns the first armed fault matching r and spends one of its
// Times, removing the fault when it reaches zero.
func (s *Server) takeFault(r *http.Request) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.faults {
		if !f.matches(r) {
			continue
		}
		if f.Times > 0 {
			s.faults[i].Times--
			if s.faults[i].Times == 0 {
				s.faults = slices.Delete(s.faults, i, i+1)
			}
		}
		return f, true
	}
	return Fault{}, false
}

// serveFault answers r with f: the fault's status and body, or a connection
// closed without a response.
func serveFault(w http.ResponseWriter, r *http.Request, f Fault) {
	if !f.CloseConnection {
		if f.Status < 200 || f.Status > 999 {
			// WriteHeader panics below 100 or above 999, and the server drops
			// the connection, which reads like a CloseConnection fault. A 1xx
			// status is informational: the response would still finish as
			// 200. Answer instead so the test that armed the fault fails on
			// an unexpected status that names the mistake.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, "fake: fault for %s %s has no valid Status (%d)", f.Method, f.Path, f.Status)
			return
		}
		if f.Body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(f.Status)
		_, _ = io.WriteString(w, f.Body)
		return
	}
	// Read the request body to its end first: closing a socket with unread
	// data resets the connection, and the client would see ECONNRESET
	// instead of io.EOF.
	_, _ = io.Copy(io.Discard, r.Body)
	var conn net.Conn
	err := errors.New("the response writer does not implement http.Hijacker")
	if hj, ok := w.(http.Hijacker); ok {
		conn, _, err = hj.Hijack()
	}
	if err != nil {
		// Answer instead of hanging so the test that armed the fault fails
		// on an unexpected status.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "fake: cannot close the connection: "+err.Error())
		return
	}
	_ = conn.Close()
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
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
	if !s.authorized(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v3/domains/")
	// Route domain-scoped role assignments
	// (PUT /v3/domains/{d}/groups/{g}/roles/{r}) before the plain domain CRUD.
	if strings.Contains(id, "/") {
		s.handleRoleAssignment(w, r, "domain", id)
		return
	}
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

// handleIdentityProviders serves the identity-provider CRUD and the nested
// protocol CRUD (/v3/OS-FEDERATION/identity_providers/{id}[/protocols/{pid}]).
func (s *Server) handleIdentityProviders(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v3/OS-FEDERATION/identity_providers/")
	if idpID, pid, ok := strings.Cut(rest, "/protocols/"); ok {
		s.handleProtocol(w, r, idpID, pid)
		return
	}
	id := rest

	switch r.Method {
	case http.MethodPut:
		var body struct {
			IdentityProvider IdentityProvider `json:"identity_provider"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		if _, exists := s.idps[id]; exists {
			s.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			return
		}
		idp := body.IdentityProvider
		idp.ID = id
		s.idps[id] = &idp
		out := idp
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"identity_provider": out})
	case http.MethodGet:
		s.mu.Lock()
		idp, ok := s.idps[id]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"identity_provider": *idp})
	case http.MethodPatch:
		var body struct {
			IdentityProvider struct {
				Enabled     *bool    `json:"enabled"`
				Description *string  `json:"description"`
				RemoteIDs   []string `json:"remote_ids"`
			} `json:"identity_provider"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		idp, ok := s.idps[id]
		if !ok {
			s.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if body.IdentityProvider.Enabled != nil {
			idp.Enabled = *body.IdentityProvider.Enabled
		}
		if body.IdentityProvider.Description != nil {
			idp.Description = *body.IdentityProvider.Description
		}
		if body.IdentityProvider.RemoteIDs != nil {
			idp.RemoteIDs = body.IdentityProvider.RemoteIDs
		}
		out := *idp
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"identity_provider": out})
	case http.MethodDelete:
		s.mu.Lock()
		if _, ok := s.idps[id]; !ok {
			s.mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(s.idps, id)
		delete(s.protocols, id)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleProtocol serves the nested protocol CRUD of one identity provider.
func (s *Server) handleProtocol(w http.ResponseWriter, r *http.Request, idpID, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.idps[idpID]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Protocol Protocol `json:"protocol"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if s.protocols[idpID] == nil {
			s.protocols[idpID] = make(map[string]*Protocol)
		}
		if _, exists := s.protocols[idpID][id]; exists {
			w.WriteHeader(http.StatusConflict)
			return
		}
		p := &Protocol{ID: id, MappingID: body.Protocol.MappingID}
		s.protocols[idpID][id] = p
		writeJSON(w, http.StatusCreated, map[string]any{"protocol": *p})
	case http.MethodGet:
		p, ok := s.protocols[idpID][id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"protocol": *p})
	case http.MethodPatch:
		p, ok := s.protocols[idpID][id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Protocol Protocol `json:"protocol"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.MappingID = body.Protocol.MappingID
		writeJSON(w, http.StatusOK, map[string]any{"protocol": *p})
	case http.MethodDelete:
		if _, ok := s.protocols[idpID][id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(s.protocols[idpID], id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleMappings serves the federation mapping CRUD
// (/v3/OS-FEDERATION/mappings/{id}).
func (s *Server) handleMappings(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v3/OS-FEDERATION/mappings/")

	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Mapping struct {
				Rules json.RawMessage `json:"rules"`
			} `json:"mapping"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, exists := s.mappings[id]; exists {
			w.WriteHeader(http.StatusConflict)
			return
		}
		m := &Mapping{ID: id, Rules: body.Mapping.Rules}
		s.mappings[id] = m
		writeJSON(w, http.StatusCreated, map[string]any{"mapping": *m})
	case http.MethodGet:
		m, ok := s.mappings[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mapping": *m})
	case http.MethodPatch:
		m, ok := s.mappings[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Mapping struct {
				Rules json.RawMessage `json:"rules"`
			} `json:"mapping"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.Rules = body.Mapping.Rules
		writeJSON(w, http.StatusOK, map[string]any{"mapping": *m})
	case http.MethodDelete:
		if _, ok := s.mappings[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(s.mappings, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleGroups serves GET /v3/groups?name=&domain_id= and POST /v3/groups.
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		name := r.URL.Query().Get("name")
		domainID := r.URL.Query().Get("domain_id")
		s.mu.Lock()
		matches := []Group{}
		for _, g := range s.groups {
			if (name == "" || g.Name == name) && (domainID == "" || g.DomainID == domainID) {
				matches = append(matches, *g)
			}
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"groups": matches})
	case http.MethodPost:
		var body struct {
			Group Group `json:"group"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		for _, g := range s.groups {
			if g.Name == body.Group.Name && g.DomainID == body.Group.DomainID {
				s.mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		s.nextID++
		g := body.Group
		g.ID = fmt.Sprintf("group-%04d", s.nextID)
		s.groups[g.ID] = &g
		out := g
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"group": out})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleRoles serves GET /v3/roles?name=.
func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("name")
	s.mu.Lock()
	matches := []Role{}
	for _, role := range s.roles {
		if name == "" || role.Name == name {
			matches = append(matches, *role)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"roles": matches})
}

// handleProjects serves GET /v3/projects?name=&domain_id=.
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("name")
	domainID := r.URL.Query().Get("domain_id")
	s.mu.Lock()
	matches := []Project{}
	for _, p := range s.projects {
		if (name == "" || p.Name == name) && (domainID == "" || p.DomainID == domainID) {
			matches = append(matches, *p)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"projects": matches})
}

// handleProjectSubpath serves project-scoped role assignments
// (PUT /v3/projects/{p}/groups/{g}/roles/{r}).
func (s *Server) handleProjectSubpath(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v3/projects/")
	if !strings.Contains(rest, "/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.handleRoleAssignment(w, r, "project", rest)
}

// handleRoleAssignment serves PUT (grant, idempotent like real keystone) and
// HEAD (existence probe: 204 when the assignment exists, 404 otherwise) on
// {scope}/{scopeID}/groups/{g}/roles/{r}.
func (s *Server) handleRoleAssignment(w http.ResponseWriter, r *http.Request, scope, rest string) {
	parts := strings.Split(rest, "/")
	// Expect {scopeID}/groups/{g}/roles/{r}.
	if len(parts) != 5 || parts[1] != "groups" || parts[3] != "roles" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	key := fmt.Sprintf("%s/%s/group/%s/role/%s", scope, parts[0], parts[2], parts[4])
	switch r.Method {
	case http.MethodPut:
		s.mu.Lock()
		s.roleAssignments[key] = struct{}{}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case http.MethodHead:
		s.mu.Lock()
		_, ok := s.roleAssignments[key]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// SeedRole pre-creates a role and returns its assigned ID.
func (s *Server) SeedRole(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("role-%04d", s.nextID)
	s.roles[id] = &Role{ID: id, Name: name}
	return id
}

// SeedProject pre-creates a project in a domain and returns its assigned ID.
func (s *Server) SeedProject(name, domainID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("project-%04d", s.nextID)
	s.projects[id] = &Project{ID: id, Name: name, DomainID: domainID}
	return id
}

// SeedGroup pre-creates a group in a domain and returns its assigned ID.
func (s *Server) SeedGroup(name, domainID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("group-%04d", s.nextID)
	s.groups[id] = &Group{ID: id, Name: name, DomainID: domainID}
	return id
}

// IdentityProvider returns a copy of the identity provider with the given ID,
// or nil.
func (s *Server) IdentityProvider(id string) *IdentityProvider {
	s.mu.Lock()
	defer s.mu.Unlock()
	idp, ok := s.idps[id]
	if !ok {
		return nil
	}
	out := *idp
	return &out
}

// Protocol returns a copy of the protocol with the given IDs, or nil.
func (s *Server) Protocol(idpID, id string) *Protocol {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.protocols[idpID][id]
	if !ok {
		return nil
	}
	out := *p
	return &out
}

// Mapping returns a copy of the mapping with the given ID, or nil.
func (s *Server) Mapping(id string) *Mapping {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.mappings[id]
	if !ok {
		return nil
	}
	out := *m
	return &out
}

// GroupByName returns a copy of the group with the given name in the given
// domain, or nil.
func (s *Server) GroupByName(name, domainID string) *Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.groups {
		if g.Name == name && g.DomainID == domainID {
			out := *g
			return &out
		}
	}
	return nil
}

// RoleAssignments returns the recorded role assignments as sorted
// "{domain|project}/{scopeID}/group/{groupID}/role/{roleID}" strings.
func (s *Server) RoleAssignments() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.roleAssignments))
	for a := range s.roleAssignments {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

// Package computeapitest is an in-memory Keystone and Nova compute API for the
// tests of the computeapi client and the NovaCompute reconciler. It keeps
// compute services, host aggregates and a server count per host, answers the
// requests the client sends at microversions 2.53 and 2.69, and records every
// call.
//
// Fake is both an http.Handler and a computeapi.Doer. Used as a Doer it serves
// the request in process, so any URL reaches it and no listener is needed.
package computeapitest

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// token is the token the fake issues and requires on compute requests.
const token = "fake-token"

// Call is one request the fake received.
type Call struct {
	Method string
	// Path is the URL path, without the query.
	Path string
	// Query is the raw URL query.
	Query string
	Body  string
}

// Service is one compute service the fake keeps.
type Service struct {
	ID             string
	Host           string
	Status         string
	State          string
	DisabledReason string
}

// Aggregate is one host aggregate the fake keeps.
type Aggregate struct {
	ID               int
	Name             string
	AvailabilityZone string
	Hosts            []string
	Metadata         map[string]string
}

// failure is an injected answer for the next request that matches.
type failure struct {
	method, pathPrefix string
	status             int
}

// Fake is an in-memory Keystone and Nova. The zero value is not usable; call
// New.
type Fake struct {
	mu sync.Mutex

	// RejectAuth answers every token request with 401.
	RejectAuth bool

	services   []*Service
	aggregates []*Aggregate
	servers    map[string]int
	downCells  map[string]bool
	failures   []failure
	calls      []Call
	nextSvc    int
	nextAgg    int
}

// New returns an empty fake.
func New() *Fake {
	return &Fake{servers: map[string]int{}, downCells: map[string]bool{}}
}

// Do serves req in process.
func (f *Fake) Do(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// AddService registers a nova-compute service for host and returns its id.
func (f *Fake) AddService(host, status, state string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSvc++
	id := fmt.Sprintf("00000000-0000-0000-0000-%012d", f.nextSvc)
	f.services = append(f.services, &Service{ID: id, Host: host, Status: status, State: state})
	return id
}

// SetServers sets how many servers the fake reports on host.
func (f *Fake) SetServers(host string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers[host] = n
}

// SetCellDown puts host in a cell Nova cannot reach. Below microversion 2.69
// the host's service is left out of the service list, and at 2.69 it is listed
// as a record of status UNKNOWN; the servers list skips the host's servers at
// any microversion.
func (f *Fake) SetCellDown(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downCells[host] = true
}

// AddAggregate creates an aggregate and returns its id. zone "" creates one
// without an availability zone.
func (f *Fake) AddAggregate(name, zone string, hosts []string, metadata map[string]string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addAggregate(name, zone, hosts, metadata).ID
}

func (f *Fake) addAggregate(name, zone string, hosts []string, metadata map[string]string) *Aggregate {
	f.nextAgg++
	md := maps.Clone(metadata)
	if md == nil {
		md = map[string]string{}
	}
	if zone != "" {
		md["availability_zone"] = zone
	}
	agg := &Aggregate{ID: f.nextAgg, Name: name, AvailabilityZone: zone, Hosts: slices.Clone(hosts), Metadata: md}
	f.aggregates = append(f.aggregates, agg)
	return agg
}

// FailNext makes the next request whose method matches and whose path starts
// with pathPrefix answer status instead.
func (f *Fake) FailNext(method, pathPrefix string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, failure{method: method, pathPrefix: pathPrefix, status: status})
}

// Calls returns every request received so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallsTo returns the requests with the given method whose path starts with
// pathPrefix.
func (f *Fake) CallsTo(method, pathPrefix string) []Call {
	var out []Call
	for _, c := range f.Calls() {
		if c.Method == method && strings.HasPrefix(c.Path, pathPrefix) {
			out = append(out, c)
		}
	}
	return out
}

// Services returns a copy of the services.
func (f *Fake) Services() []Service {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Service, 0, len(f.services))
	for _, s := range f.services {
		out = append(out, *s)
	}
	return out
}

// Aggregates returns a copy of the aggregates.
func (f *Fake) Aggregates() []Aggregate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Aggregate, 0, len(f.aggregates))
	for _, a := range f.aggregates {
		cp := *a
		cp.Hosts = slices.Clone(a.Hosts)
		cp.Metadata = maps.Clone(a.Metadata)
		out = append(out, cp)
	}
	return out
}

// Aggregate returns the aggregate of the given name.
func (f *Fake) Aggregate(name string) (Aggregate, bool) {
	for _, a := range f.Aggregates() {
		if a.Name == name {
			return a, true
		}
	}
	return Aggregate{}, false
}

// ServeHTTP answers one Keystone or Nova request.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)})

	if status, ok := f.takeFailure(r); ok {
		writeJSON(w, status, map[string]string{"message": fmt.Sprintf("injected HTTP %d", status)})
		return
	}

	if strings.HasSuffix(r.URL.Path, "/auth/tokens") && r.Method == http.MethodPost {
		f.serveToken(w)
		return
	}

	if r.Header.Get("X-Auth-Token") != token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "the request you have made requires authentication"})
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) >= 2 && parts[0] == "v2.1" && parts[1] == "os-services":
		f.serveServices(w, r, parts[2:], body)
	case len(parts) == 2 && parts[0] == "v2.1" && parts[1] == "servers" && r.Method == http.MethodGet:
		f.serveServers(w, r)
	case len(parts) >= 2 && parts[0] == "v2.1" && parts[1] == "os-aggregates":
		f.serveAggregates(w, r, parts[2:], body)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such route"})
	}
}

func (f *Fake) takeFailure(r *http.Request) (int, bool) {
	for i, fl := range f.failures {
		if fl.method == r.Method && strings.HasPrefix(r.URL.Path, fl.pathPrefix) {
			f.failures = slices.Delete(f.failures, i, i+1)
			return fl.status, true
		}
	}
	return 0, false
}

func (f *Fake) serveToken(w http.ResponseWriter) {
	if f.RejectAuth {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "the request you have made requires authentication"})
		return
	}
	w.Header().Set("X-Subject-Token", token)
	writeJSON(w, http.StatusCreated, map[string]any{"token": map[string]any{"methods": []string{"password"}}})
}

func (f *Fake) serveServices(w http.ResponseWriter, r *http.Request, rest []string, body []byte) {
	if len(rest) == 0 && r.Method == http.MethodGet {
		services := make([]map[string]any, 0, len(f.services))
		for _, s := range f.services {
			if f.downCells[s.Host] {
				if microversion(r) >= 69 {
					services = append(services, map[string]any{
						"binary": "nova-compute", "host": s.Host, "status": "UNKNOWN", "zone": "nova",
					})
				}
				continue
			}
			var reason any
			if s.DisabledReason != "" {
				reason = s.DisabledReason
			}
			services = append(services, map[string]any{
				"id": s.ID, "binary": "nova-compute", "host": s.Host, "status": s.Status,
				"state": s.State, "disabled_reason": reason, "zone": "nova",
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"services": services})
		return
	}
	if len(rest) != 1 {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such route"})
		return
	}

	idx := slices.IndexFunc(f.services, func(s *Service) bool { return s.ID == rest[0] })
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
		return
	}
	svc := f.services[idx]

	switch r.Method {
	case http.MethodPut:
		var update struct {
			Status         string `json:"status"`
			DisabledReason string `json:"disabled_reason"`
		}
		if err := json.Unmarshal(body, &update); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		svc.Status = update.Status
		svc.DisabledReason = update.DisabledReason
		if update.Status == "enabled" {
			svc.DisabledReason = ""
		}
		writeJSON(w, http.StatusOK, map[string]any{"service": map[string]any{"id": svc.ID, "status": svc.Status}})
	case http.MethodDelete:
		// Nova 32.0.0 refuses to delete a compute service whose host still has
		// instances, and otherwise takes the host out of every aggregate.
		if f.servers[svc.Host] > 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "unable to delete compute service that is hosting instances"})
			return
		}
		for _, a := range f.aggregates {
			a.Hosts = slices.DeleteFunc(a.Hosts, func(h string) bool { return h == svc.Host })
		}
		f.services = slices.Delete(f.services, idx, idx+1)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "method not allowed"})
	}
}

func (f *Fake) serveServers(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	n := f.servers[host]
	if f.downCells[host] {
		n = 0
	}
	servers := make([]map[string]string, 0, n)
	for i := range n {
		servers = append(servers, map[string]string{"id": strconv.Itoa(i + 1)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

func (f *Fake) serveAggregates(w http.ResponseWriter, r *http.Request, rest []string, body []byte) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			aggregates := make([]map[string]any, 0, len(f.aggregates))
			for _, a := range f.aggregates {
				aggregates = append(aggregates, aggregateJSON(a))
			}
			writeJSON(w, http.StatusOK, map[string]any{"aggregates": aggregates})
		case http.MethodPost:
			var create struct {
				Aggregate struct {
					Name             string `json:"name"`
					AvailabilityZone string `json:"availability_zone"`
				} `json:"aggregate"`
			}
			if err := json.Unmarshal(body, &create); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
				return
			}
			if slices.ContainsFunc(f.aggregates, func(a *Aggregate) bool { return a.Name == create.Aggregate.Name }) {
				writeJSON(w, http.StatusConflict, map[string]string{"message": "aggregate already exists"})
				return
			}
			agg := f.addAggregate(create.Aggregate.Name, create.Aggregate.AvailabilityZone, nil, nil)
			writeJSON(w, http.StatusOK, map[string]any{"aggregate": aggregateJSON(agg)})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "method not allowed"})
		}
		return
	}

	id, err := strconv.Atoi(rest[0])
	idx := slices.IndexFunc(f.aggregates, func(a *Aggregate) bool { return a.ID == id })
	if err != nil || idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "aggregate not found"})
		return
	}
	agg := f.aggregates[idx]

	switch {
	case len(rest) == 2 && rest[1] == "action" && r.Method == http.MethodPost:
		var action struct {
			SetMetadata *struct {
				Metadata map[string]*string `json:"metadata"`
			} `json:"set_metadata"`
		}
		if err := json.Unmarshal(body, &action); err != nil || action.SetMetadata == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "unsupported action"})
			return
		}
		for k, v := range action.SetMetadata.Metadata {
			if v == nil {
				delete(agg.Metadata, k)
				continue
			}
			agg.Metadata[k] = *v
		}
		writeJSON(w, http.StatusOK, map[string]any{"aggregate": aggregateJSON(agg)})
	case len(rest) == 1 && r.Method == http.MethodDelete:
		// Nova refuses to delete an aggregate that still has hosts.
		if len(agg.Hosts) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "aggregate is not empty"})
			return
		}
		f.aggregates = slices.Delete(f.aggregates, idx, idx+1)
		w.WriteHeader(http.StatusOK)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such route"})
	}
}

// microversion is the minor compute API microversion r asks for, 0 when it
// names none.
func microversion(r *http.Request) int {
	_, minor, _ := strings.Cut(r.Header.Get("X-OpenStack-Nova-API-Version"), ".")
	n, _ := strconv.Atoi(minor)
	return n
}

func aggregateJSON(a *Aggregate) map[string]any {
	var zone any
	if a.AvailabilityZone != "" {
		zone = a.AvailabilityZone
	}
	hosts := a.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	return map[string]any{
		"id": a.ID, "name": a.Name, "availability_zone": zone, "hosts": hosts, "metadata": a.Metadata,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

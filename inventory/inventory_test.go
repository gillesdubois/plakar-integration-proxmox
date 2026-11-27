/*
 * Copyright (c) 2026 Julien Castets
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package inventory_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	integration "github.com/PlakarKorp/integration-sdk"
	"github.com/gillesdubois/plakar-integration-proxmox/inventory"
)

const (
	tokenID     = "plakar@pve!inventory"
	tokenSecret = "5f0c3e0e-0000-4000-8000-000000000000"
)

const (
	statusPath    = "/api2/json/cluster/status"
	resourcesPath = "/api2/json/cluster/resources?type=vm"
)

const clusterStatus = `[
	{"type":"cluster","id":"cluster","name":"prod","nodes":3,"quorate":1},
	{"type":"node","id":"node/pve1","name":"pve1","ip":"10.0.0.1","online":1},
	{"type":"node","id":"node/pve2","name":"pve2","ip":"10.0.0.2","online":0},
	{"type":"node","id":"node/pve3","name":"pve3","online":1}
]`

const clusterResources = `[
	{"id":"qemu/101","type":"qemu","vmid":101,"name":"web","node":"pve1","status":"running","pool":"prod","tags":"db;eu","template":0,"maxmem":2147483648},
	{"id":"lxc/102","type":"lxc","vmid":102,"name":"dns","node":"pve2","status":"unknown"},
	{"id":"qemu/9000","type":"qemu","vmid":9000,"name":"debian-tpl","node":"pve1","status":"stopped","template":1},
	{"id":"qemu/103","type":"qemu","vmid":103,"node":"pve3"}
]`

// fakeAPI serves the Proxmox API from routes, a map of path to the JSON of
// the data field. It answers 401 to any request without the test token.
func fakeAPI(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken="+tokenID+"="+tokenSecret {
			http.Error(w, "authentication failure", http.StatusUnauthorized)
			return
		}
		data, ok := routes[r.URL.RequestURI()]
		if !ok {
			http.Error(w, "no such route", http.StatusNotImplemented)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":` + data + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func defaultRoutes() map[string]string {
	return map[string]string{statusPath: clusterStatus, resourcesPath: clusterResources}
}

func baseConfig(location string) map[string]string {
	return map[string]string{
		"location":     location,
		"token_id":     tokenID,
		"token_secret": tokenSecret,
	}
}

// params answers the config for srv, whose certificate is self-signed.
func params(srv *httptest.Server) map[string]string {
	p := baseConfig(srv.URL)
	p["tls_insecure"] = "true"
	return p
}

func list(t *testing.T, config map[string]string) ([]*integration.InventoryEntry, error) {
	t.Helper()
	inv, err := inventory.NewInventory(context.Background(), config)
	if err != nil {
		t.Fatalf("NewInventory: %v", err)
	}
	t.Cleanup(func() { _ = inv.Close(context.Background()) })

	ch := make(chan *integration.InventoryEntry)
	errc := make(chan error, 1)
	go func() { errc <- inv.List(context.Background(), ch) }()

	var entries []*integration.InventoryEntry
	for e := range ch {
		entries = append(entries, e)
	}
	return entries, <-errc
}

func TestListCluster(t *testing.T) {
	srv := fakeAPI(t, defaultRoutes())

	entries, err := list(t, params(srv))
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	byURN := make(map[string]*integration.InventoryEntry)
	for _, e := range entries {
		byURN[e.URN] = e
	}
	if len(byURN) != 3 {
		t.Fatalf("got %d entries, want 3 (the template is skipped): %v", len(entries), byURN)
	}
	if _, ok := byURN["urn:proxmox:prod:qemu:9000"]; ok {
		t.Errorf("template listed")
	}

	tests := []struct {
		urn      string
		name     string
		region   string
		service  string
		tags     []string
		endpoint string
	}{
		{
			urn:      "urn:proxmox:prod:qemu:101",
			name:     "web",
			region:   "pve1",
			service:  "qemu",
			tags:     []string{"db", "eu", "pool=prod", "type=qemu", "status=running"},
			endpoint: "10.0.0.1",
		},
		{
			urn:      "urn:proxmox:prod:lxc:102",
			name:     "dns",
			region:   "pve2",
			service:  "lxc",
			tags:     []string{"type=lxc", "status=unknown"},
			endpoint: "10.0.0.2",
		},
		{
			urn:      "urn:proxmox:prod:qemu:103",
			name:     "103",
			region:   "pve3",
			service:  "qemu",
			tags:     []string{"type=qemu"},
			endpoint: "pve3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.urn, func(t *testing.T) {
			e, ok := byURN[tt.urn]
			if !ok {
				t.Fatalf("missing entry")
			}
			if e.Class != integration.ResourceClassCompute || e.SubClass != integration.ResourceSubClassUndefined {
				t.Errorf("class = %s/%s, want compute with no subclass", e.Class, e.SubClass)
			}
			if e.Name != tt.name {
				t.Errorf("name = %q, want %q", e.Name, tt.name)
			}
			if e.Region != tt.region {
				t.Errorf("region = %q, want %q", e.Region, tt.region)
			}
			if e.Service != tt.service || e.Resource != "proxmox:"+tt.service {
				t.Errorf("service, resource = %q, %q", e.Service, e.Resource)
			}
			if !reflect.DeepEqual(e.Tags, tt.tags) {
				t.Errorf("tags = %q, want %q", e.Tags, tt.tags)
			}

			vmid := tt.urn[strings.LastIndex(tt.urn, ":")+1:]
			want := []integration.HostEndpoint{{
				Type:       integration.EndpointIdentifier,
				Endpoint:   tt.endpoint,
				Attributes: map[string]string{"vmid": vmid, "node": tt.region},
			}}
			if !reflect.DeepEqual(e.Endpoints, want) {
				t.Errorf("endpoints = %+v, want %+v", e.Endpoints, want)
			}

			var details struct {
				VMID int `json:"vmid"`
			}
			if err := json.Unmarshal(e.Details, &details); err != nil || details.VMID == 0 {
				t.Errorf("details = %s, want the guest's resource JSON", e.Details)
			}
		})
	}
}

func TestListPlakarTags(t *testing.T) {
	srv := fakeAPI(t, map[string]string{
		statusPath:    clusterStatus,
		resourcesPath: `[{"type":"qemu","vmid":101,"node":"pve1","status":"running","tags":"plakar-env-pre-prod;plakar-environment-prod;plakar-data-class-pii;plakar-dataclass-hr;plakar-class-logs;plakar-env-;db"}]`,
	})

	entries, err := list(t, params(srv))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	want := []string{
		"plakar:environment:pre-prod",
		"plakar:environment:prod",
		"plakar:data-class:pii",
		"plakar:data-class:hr",
		"plakar:data-class:logs",
		"plakar-env-",
		"db",
		"type=qemu",
		"status=running",
	}
	if got := entries[0].Tags; !reflect.DeepEqual(got, want) {
		t.Errorf("tags = %q, want %q", got, want)
	}
}

func TestListStandaloneNode(t *testing.T) {
	srv := fakeAPI(t, map[string]string{
		statusPath:    `[{"type":"node","id":"node/pve","name":"pve","ip":"192.168.1.10","online":1,"local":1}]`,
		resourcesPath: `[{"type":"lxc","vmid":200,"name":"ct","node":"pve","status":"running"}]`,
	})

	entries, err := list(t, params(srv))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if got := entries[0].URN; got != "urn:proxmox:pve:lxc:200" {
		t.Errorf("URN = %q, want the node name as scope", got)
	}
}

func TestListFails(t *testing.T) {
	tests := []struct {
		name   string
		routes map[string]string
		mutate func(map[string]string)
	}{
		{
			name:   "cluster status unavailable",
			routes: map[string]string{resourcesPath: clusterResources},
		},
		{
			name:   "resources unavailable",
			routes: map[string]string{statusPath: clusterStatus},
		},
		{
			name: "undecodable guest after valid ones",
			routes: map[string]string{
				statusPath:    clusterStatus,
				resourcesPath: `[{"type":"qemu","vmid":101,"node":"pve1"},{"type":"qemu","vmid":"oops","node":"pve1"}]`,
			},
		},
		{
			name: "no cluster name and several nodes",
			routes: map[string]string{
				statusPath:    `[{"type":"node","name":"a"},{"type":"node","name":"b"}]`,
				resourcesPath: clusterResources,
			},
		},
		{
			name:   "wrong token",
			routes: defaultRoutes(),
			mutate: func(p map[string]string) { p["token_secret"] = "wrong" },
		},
		{
			name:   "self-signed certificate without tls_insecure",
			routes: defaultRoutes(),
			mutate: func(p map[string]string) { delete(p, "tls_insecure") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakeAPI(t, tt.routes)
			config := params(srv)
			if tt.mutate != nil {
				tt.mutate(config)
			}
			entries, err := list(t, config)
			if err == nil {
				t.Errorf("List succeeded, want an error")
			}
			if len(entries) != 0 {
				t.Errorf("got %d entries, want none", len(entries))
			}
		})
	}
}

func TestLocation(t *testing.T) {
	srv := fakeAPI(t, defaultRoutes())
	hostport := strings.TrimPrefix(srv.URL, "https://")

	for _, location := range []string{srv.URL, hostport, "proxmox-inventory://" + hostport} {
		t.Run(location, func(t *testing.T) {
			p := params(srv)
			p["location"] = location
			if _, err := list(t, p); err != nil {
				t.Errorf("List: %v", err)
			}
		})
	}
}

func TestLocationPathPrefix(t *testing.T) {
	routes := make(map[string]string)
	for path, data := range defaultRoutes() {
		routes["/pve"+path] = data
	}
	srv := fakeAPI(t, routes)

	for _, location := range []string{srv.URL + "/pve", srv.URL + "/pve/"} {
		t.Run(location, func(t *testing.T) {
			p := params(srv)
			p["location"] = location
			if _, err := list(t, p); err != nil {
				t.Errorf("List: %v", err)
			}
		})
	}
}

// TestLocationDefaultPort checks which address a location without a port
// reaches, from the URL in the connection error.
func TestLocationDefaultPort(t *testing.T) {
	tests := []struct {
		location string
		want     string
	}{
		{"127.0.0.1", "https://127.0.0.1:8006/"},
		{"https://127.0.0.1", "https://127.0.0.1:443/"},
		{"https://127.0.0.1:", "https://127.0.0.1:443/"},
		{"proxmox-inventory://127.0.0.1", "https://127.0.0.1:8006/"},
		{"[::1]", "https://[::1]:8006/"},
	}
	for _, tt := range tests {
		t.Run(tt.location, func(t *testing.T) {
			_, err := list(t, baseConfig(tt.location))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("List error = %v, want a request to %s", err, tt.want)
			}
		})
	}
}

func TestNewInventoryInvalidConfig(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"missing location", "location", ""},
		{"http location", "location", "http://pve.example.com"},
		{"location with userinfo", "location", "https://root@pve.example.com"},
		{"location with a query", "location", "https://pve.example.com?node=pve1"},
		{"path without a scheme", "location", "pve.example.com/pve"},
		{"missing token_id", "token_id", ""},
		{"missing token_secret", "token_secret", ""},
		{"invalid tls_insecure", "tls_insecure", "maybe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := baseConfig("https://pve.example.com")
			config[tt.key] = tt.value
			if _, err := inventory.NewInventory(context.Background(), config); err == nil {
				t.Errorf("NewInventory succeeded, want an error")
			}
		})
	}
}

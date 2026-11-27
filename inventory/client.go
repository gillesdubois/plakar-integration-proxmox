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

package inventory

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultPort = "8006"

// maxResponseSize caps an API response. /cluster/resources of a large cluster
// is a few MiB at most.
const maxResponseSize = 64 << 20

const requestTimeout = 2 * time.Minute

type config struct {
	baseURL     string
	tokenID     string
	tokenSecret string
	tlsInsecure bool
}

func parseConfig(params map[string]string) (*config, error) {
	baseURL, err := parseLocation(params["location"])
	if err != nil {
		return nil, err
	}

	cfg := &config{
		baseURL:     baseURL,
		tokenID:     strings.TrimSpace(params["token_id"]),
		tokenSecret: strings.TrimSpace(params["token_secret"]),
	}
	if cfg.tokenID == "" {
		return nil, fmt.Errorf("missing token_id")
	}
	if cfg.tokenSecret == "" {
		return nil, fmt.Errorf("missing token_secret")
	}

	if v := strings.TrimSpace(params["tls_insecure"]); v != "" {
		cfg.tlsInsecure, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid tls_insecure value: %s", v)
		}
	}
	return cfg, nil
}

// parseLocation answers the API base URL. The location is https://host[:port],
// proxmox-inventory://host[:port] or a bare host[:port]. The URL forms may
// carry a path, the prefix of a reverse proxy in front of the API. The API is
// always served over HTTPS. An https URL follows the usual rules, port 443 by
// default; the other forms name a Proxmox host, whose API is on port 8006.
func parseLocation(location string) (string, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", fmt.Errorf("missing location")
	}

	host, port, prefix := location, defaultPort, ""
	if strings.Contains(location, "://") {
		u, err := url.Parse(location)
		if err != nil {
			return "", fmt.Errorf("invalid location: %w", err)
		}
		if u.Scheme != "https" && u.Scheme != "proxmox-inventory" {
			return "", fmt.Errorf("invalid location scheme: %s", u.Scheme)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("invalid location: only scheme, host, port and path are allowed")
		}
		host = u.Host
		prefix = strings.TrimRight(u.Path, "/")
		if u.Scheme == "https" {
			port = "443"
		}
	} else if strings.Contains(location, "/") {
		return "", fmt.Errorf("invalid location: a path needs a scheme, as in https://%s", location)
	}
	if host == "" {
		return "", fmt.Errorf("missing host in location")
	}

	if h, p, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(strings.Trim(host, "[]"), port)
	} else if p == "" {
		host = net.JoinHostPort(h, port)
	}
	return "https://" + host + prefix + "/api2/json", nil
}

type client struct {
	baseURL string
	auth    string
	http    *http.Client
}

func newClient(cfg *config) *client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.tlsInsecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	// The caller's context may have no deadline: a hung pveproxy must not
	// block the sync forever. The timeout covers reading the body too.
	return &client{
		baseURL: cfg.baseURL,
		auth:    "PVEAPIToken=" + cfg.tokenID + "=" + cfg.tokenSecret,
		http:    &http.Client{Transport: transport, Timeout: requestTimeout},
	}
}

// clusterStatus is an entry of /cluster/status: the cluster, or one node.
type clusterStatus struct {
	Type string `json:"type"`
	Name string `json:"name"`
	IP   string `json:"ip"`
}

func (c *client) clusterStatus(ctx context.Context) ([]clusterStatus, error) {
	var status []clusterStatus
	if err := c.get(ctx, "/cluster/status", &status); err != nil {
		return nil, err
	}
	return status, nil
}

// guest is a QEMU VM or LXC container of /cluster/resources.
type guest struct {
	Type     string `json:"type"`
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Node     string `json:"node"`
	Status   string `json:"status"`
	Pool     string `json:"pool"`
	Tags     string `json:"tags"`
	Template int    `json:"template"`

	// raw is the resource as the API returned it.
	raw json.RawMessage
}

func (c *client) guests(ctx context.Context) ([]guest, error) {
	var resources []json.RawMessage
	if err := c.get(ctx, "/cluster/resources?type=vm", &resources); err != nil {
		return nil, err
	}
	guests := make([]guest, len(resources))
	for i, raw := range resources {
		if err := json.Unmarshal(raw, &guests[i]); err != nil {
			return nil, fmt.Errorf("decode guest: %w", err)
		}
		guests[i].raw = raw
	}
	return guests, nil
}

// get decodes the data field of the API response to path into out.
func (c *client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("get %s: %w", path, err)
	}
	req.Header.Set("Authorization", c.auth)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", path, err)
	}
	defer resp.Body.Close()

	body := io.LimitReader(resp.Body, maxResponseSize)
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(body, 512))
		return fmt.Errorf("get %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}

	envelope := struct {
		Data any `json:"data"`
	}{Data: out}
	if err := json.NewDecoder(body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

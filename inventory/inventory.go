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

// Package inventory lists the guests of a Proxmox host or cluster through the
// Proxmox HTTP API, so each can be backed up with the proxmox+backup importer.
package inventory

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	integration "github.com/PlakarKorp/integration-sdk"
)

// ProxmoxInventory lists the QEMU VMs and LXC containers of a Proxmox host or
// cluster.
type ProxmoxInventory struct {
	client *client
}

// NewInventory returns an inventory for the Proxmox API described by config.
func NewInventory(ctx context.Context, config map[string]string) (integration.Inventory, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	return &ProxmoxInventory{client: newClient(cfg)}, nil
}

// List sends one entry per guest. Every entry is built before anything is
// sent, so a failure never leaves a partial inventory.
func (inv *ProxmoxInventory) List(ctx context.Context, entries chan<- *integration.InventoryEntry) error {
	defer close(entries)

	status, err := inv.client.clusterStatus(ctx)
	if err != nil {
		return err
	}
	scope, nodeIPs, err := parseClusterStatus(status)
	if err != nil {
		return err
	}

	guests, err := inv.client.guests(ctx)
	if err != nil {
		return err
	}

	var list []*integration.InventoryEntry
	for _, g := range guests {
		if (g.Type != "qemu" && g.Type != "lxc") || g.Template == 1 {
			continue
		}
		list = append(list, newEntry(scope, nodeIPs, g))
	}

	for _, e := range list {
		select {
		case entries <- e:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Close releases the idle connections to the API.
func (inv *ProxmoxInventory) Close(ctx context.Context) error {
	inv.client.http.CloseIdleConnections()
	return nil
}

// parseClusterStatus answers the name that scopes the URNs, the cluster's or
// the standalone node's, and the IP of each node.
func parseClusterStatus(status []clusterStatus) (string, map[string]string, error) {
	var cluster string
	nodeIPs := make(map[string]string)
	for _, s := range status {
		switch s.Type {
		case "cluster":
			cluster = s.Name
		case "node":
			nodeIPs[s.Name] = s.IP
		}
	}

	if cluster != "" {
		return cluster, nodeIPs, nil
	}
	if len(nodeIPs) == 1 {
		for node := range nodeIPs {
			if node != "" {
				return node, nodeIPs, nil
			}
		}
	}
	return "", nil, fmt.Errorf("cluster status: no cluster name and %d nodes", len(nodeIPs))
}

func newEntry(scope string, nodeIPs map[string]string, g guest) *integration.InventoryEntry {
	vmid := strconv.Itoa(g.VMID)

	name := g.Name
	if name == "" {
		name = vmid
	}

	// The endpoint becomes the importer's location, and vzdump only backs up
	// the guests of the node it runs on. The endpoint is mandatory: without an
	// IP, the node name stands in, and only works if it resolves.
	endpoint := nodeIPs[g.Node]
	if endpoint == "" {
		endpoint = g.Node
	}

	tags := strings.FieldsFunc(g.Tags, func(r rune) bool {
		return r == ';' || r == ',' || r == ' '
	})
	for i, tag := range tags {
		tags[i] = plakarTag(tag)
	}
	if g.Pool != "" {
		tags = append(tags, "pool="+g.Pool)
	}
	tags = append(tags, "type="+g.Type)
	if g.Status != "" {
		tags = append(tags, "status="+g.Status)
	}

	return &integration.InventoryEntry{
		Class:    integration.ResourceClassCompute,
		Tags:     tags,
		URN:      fmt.Sprintf("urn:proxmox:%s:%s:%s", scope, g.Type, vmid),
		Name:     name,
		Region:   g.Node,
		Service:  g.Type,
		Resource: "proxmox:" + g.Type,
		Details:  g.raw,
		Endpoints: []integration.HostEndpoint{{
			Type:     integration.EndpointIdentifier,
			Endpoint: endpoint,
			Attributes: map[string]string{
				"vmid": vmid,
				"node": g.Node,
			},
		}},
	}
}

// plakarPrefixes maps the Proxmox spelling of the Plakar tag prefixes to the
// one Plakar matches policies on. Proxmox tags cannot hold ':' or '='.
var plakarPrefixes = []struct{ proxmox, plakar string }{
	{"plakar-environment-", "plakar:environment:"},
	{"plakar-env-", "plakar:environment:"},
	{"plakar-data-class-", "plakar:data-class:"},
	{"plakar-dataclass-", "plakar:data-class:"},
	{"plakar-class-", "plakar:data-class:"},
}

// plakarTag answers tag with a Plakar prefix spelled the Proxmox way, such as
// plakar-env-prod, rewritten to plakar:environment:prod. Other tags are
// returned as is.
func plakarTag(tag string) string {
	for _, p := range plakarPrefixes {
		if name, ok := strings.CutPrefix(tag, p.proxmox); ok && name != "" {
			return p.plakar + name
		}
	}
	return tag
}

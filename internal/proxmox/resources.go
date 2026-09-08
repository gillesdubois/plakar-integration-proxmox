/*
 * Copyright (c) 2026 Gilles DUBOIS
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

package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const resourceCacheTTL = 15 * time.Second

type vmResource struct {
	VMID   int    `json:"vmid"`
	Type   string `json:"type"`
	Node   string `json:"node"`
	Name   string `json:"name,omitempty"`
	Pool   string `json:"pool,omitempty"`
	Status string `json:"status,omitempty"`
}

type poolResponse struct {
	Members []vmResource `json:"members"`
}

func (c *Client) ListAllVMIDs(ctx context.Context) ([]int, error) {
	resources, err := c.listResources(ctx)
	if err != nil {
		return nil, err
	}
	vmids, skipped := filterVMIDs(resources, c.cfg.Node)
	warnSkippedResources(skipped)
	return vmids, nil
}

func (c *Client) VMType(ctx context.Context, vmid int) (string, error) {
	res, err := c.vmResourceByID(ctx, vmid)
	if err != nil {
		return "", err
	}
	return res.Type, nil
}

func (c *Client) VMPool(ctx context.Context, vmid int) (string, error) {
	res, err := c.vmResourceByID(ctx, vmid)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Pool), nil
}

func (c *Client) VMName(ctx context.Context, vmid int) (string, error) {
	res, err := c.vmResourceByID(ctx, vmid)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Name), nil
}

func (c *Client) PoolExists(ctx context.Context, pool string) (bool, error) {
	pool = strings.TrimSpace(pool)
	if pool == "" {
		return false, nil
	}

	_, err := c.runPvesh(ctx, "pvesh get pool failed", "get", "/pools/"+pool, "--output-format", "json")
	if err != nil {
		if isMissingPoolError(err.Error()) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func isMissingPoolError(output string) bool {
	normalized := strings.ToLower(strings.TrimSpace(output))
	if normalized == "" {
		return false
	}
	return strings.Contains(normalized, "does not exist") ||
		strings.Contains(normalized, "not found") ||
		strings.Contains(normalized, "no such")
}

func (c *Client) ListPoolVMIDs(ctx context.Context, pool string) ([]int, error) {
	stdout, err := c.runPvesh(ctx, "pvesh get pool failed", "get", "/pools/"+pool, "--output-format", "json")
	if err != nil {
		return nil, err
	}

	var response poolResponse
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		return nil, fmt.Errorf("failed to parse pool response: %w", err)
	}
	vmids, skipped := filterVMIDs(response.Members, c.cfg.Node)
	warnSkippedResources(skipped)
	return vmids, nil
}

// filterVMIDs selects the backup-able VM/CT of an inventory, and returns the
// ones deliberately left out so the caller can report them.
//
// A resource whose status is "unknown" lives on a node the cluster cannot reach:
// dumping it is a guaranteed failure, so it is skipped rather than allowed to
// break the run. Templates are kept, vzdump handles them fine.
func filterVMIDs(resources []vmResource, node string) ([]int, []vmResource) {
	set := make(map[int]struct{})
	skipped := make([]vmResource, 0)

	for _, item := range resources {
		if item.Type != "qemu" && item.Type != "lxc" {
			continue
		}
		if node != "" && item.Node != node {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(item.Status), "unknown") {
			skipped = append(skipped, item)
			continue
		}
		set[item.VMID] = struct{}{}
	}

	vmids := make([]int, 0, len(set))
	for vmid := range set {
		vmids = append(vmids, vmid)
	}
	sort.Ints(vmids)
	return vmids, skipped
}

func warnSkippedResources(skipped []vmResource) {
	for _, item := range skipped {
		Warnf("skipping %s %d on node %q: the cluster reports its status as unknown (node unreachable?)",
			item.Type, item.VMID, item.Node)
	}
}

func (c *Client) vmResourceByID(ctx context.Context, vmid int) (vmResource, error) {
	resources, err := c.listResources(ctx)
	if err != nil {
		return vmResource{}, err
	}

	for _, res := range resources {
		if res.VMID != vmid {
			continue
		}
		if c.cfg.Node != "" && res.Node != c.cfg.Node {
			continue
		}
		if res.Type == "qemu" || res.Type == "lxc" {
			return res, nil
		}
	}

	return vmResource{}, fmt.Errorf("unable to determine VM resource for vmid %d", vmid)
}

func (c *Client) listResources(ctx context.Context) ([]vmResource, error) {
	if cached, ok := c.cachedResources(); ok {
		return cached, nil
	}

	stdout, err := c.runPvesh(ctx, "pvesh get cluster resources failed", "get", "/cluster/resources", "--type", "vm", "--output-format", "json")
	if err != nil {
		return nil, err
	}

	var resources []vmResource
	if err := json.Unmarshal([]byte(stdout), &resources); err != nil {
		return nil, fmt.Errorf("failed to parse cluster resources: %w", err)
	}

	c.setResourceCache(resources)
	return resources, nil
}

func (c *Client) cachedResources() ([]vmResource, bool) {
	c.resourceCacheMu.Lock()
	defer c.resourceCacheMu.Unlock()

	if len(c.resourceCache) == 0 {
		return nil, false
	}
	if time.Since(c.resourceCacheAt) > resourceCacheTTL {
		return nil, false
	}
	cached := make([]vmResource, len(c.resourceCache))
	copy(cached, c.resourceCache)
	return cached, true
}

func (c *Client) setResourceCache(resources []vmResource) {
	c.resourceCacheMu.Lock()
	c.resourceCache = append([]vmResource(nil), resources...)
	c.resourceCacheAt = time.Now()
	c.resourceCacheMu.Unlock()
}

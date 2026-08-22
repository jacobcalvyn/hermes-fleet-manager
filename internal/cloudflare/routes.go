package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
	"github.com/jacobcalvyn/hermes-fleet-manager/internal/netpolicy"
)

const (
	RouteProviderReady      = "ready_to_publish"
	RouteProviderPublishing = "publishing"
	RouteProviderPublished  = "published"
	RouteProviderMismatch   = "configuration_mismatch"
	RouteProviderFailed     = "failed"

	ResourcePending  = "pending"
	ResourceReady    = "ready"
	ResourceConflict = "conflict"
	ResourceFailed   = "failed"

	EndpointUnchecked       = "unchecked"
	EndpointChecking        = "checking"
	EndpointPropagating     = "propagating"
	EndpointReachable       = "reachable"
	EndpointAccessProtected = "access_protected"
	EndpointUnavailable     = "unavailable"
)

func routeAutomationConfigured(config Config) bool {
	automation := config.RouteAutomation
	return automation.AccountID != "" && automation.ZoneID != "" &&
		automation.TunnelID != "" && automation.APIToken != ""
}

func pendingRouteObservations(routes map[string]string, publishing bool) map[string]RouteObservation {
	state := RouteProviderReady
	detail := "Instance publishing is not connected"
	if publishing {
		state = RouteProviderPublishing
		detail = "Waiting for Cloudflare verification"
	}
	observations := make(map[string]RouteObservation, len(routes))
	for hostname, origin := range routes {
		observations[hostname] = RouteObservation{
			Hostname: hostname, OriginService: origin,
			ProviderState: state, ProviderDetail: detail,
			DNSState: ResourcePending, IngressState: ResourcePending,
			EndpointState: EndpointUnchecked,
		}
	}
	return observations
}

func revalidatingRouteObservations(routes map[string]string, previous map[string]RouteObservation) map[string]RouteObservation {
	observations := pendingRouteObservations(routes, true)
	for hostname, origin := range routes {
		observation, exists := previous[hostname]
		if !exists || observation.OriginService != origin ||
			observation.ProviderState != RouteProviderPublished ||
			observation.DNSState != ResourceReady || observation.IngressState != ResourceReady ||
			observation.EndpointState != EndpointReachable {
			continue
		}
		observation.ProviderState = RouteProviderPublishing
		observation.ProviderDetail = "Published route is being revalidated"
		observation.Revalidating = true
		observations[hostname] = observation
	}
	return observations
}

func (manager *Manager) reconcileAutomatedInstanceRoutes(ctx context.Context, config Config, desired map[string]string) (map[string]string, error) {
	if manager.ownership == nil {
		manager.failAutomatedRoutes(desired, "Fleet resource ownership store is unavailable")
		return nil, fmt.Errorf("Fleet resource ownership store is unavailable")
	}
	automation := config.RouteAutomation
	apiConfig := config
	apiConfig.AccountID = automation.AccountID
	apiConfig.ZoneID = automation.ZoneID

	manager.mu.Lock()
	manager.observations = revalidatingRouteObservations(desired, manager.observations)
	manager.mu.Unlock()

	instanceIDs, err := manager.instanceIDsByHostname(ctx, desired)
	if err != nil {
		manager.failAutomatedRoutes(desired, err.Error())
		return nil, err
	}
	zoneName, err := manager.verifyPublishingZone(ctx, apiConfig, automation)
	if err != nil {
		manager.failAutomatedRoutes(desired, "Cloudflare zone could not be verified: "+err.Error())
		return nil, fmt.Errorf("verify Cloudflare zone: %w", err)
	}
	for hostname := range desired {
		if hostname != zoneName && !strings.HasSuffix(hostname, "."+zoneName) {
			detail := fmt.Sprintf("Public hostname must belong to the configured zone %s", zoneName)
			manager.failAutomatedRoute(hostname, desired[hostname], detail)
			return nil, fmt.Errorf("%s: %s", hostname, detail)
		}
		if hostname == config.AdminHostname {
			detail := "Public hostname conflicts with the Fleet Manager admin hostname"
			manager.failAutomatedRoute(hostname, desired[hostname], detail)
			return nil, fmt.Errorf("%s: %s", hostname, detail)
		}
	}

	owned, err := manager.ownership.ListRemoteAccessResources(ctx)
	if err != nil {
		manager.failAutomatedRoutes(desired, "Fleet resource ownership could not be loaded: "+err.Error())
		return nil, fmt.Errorf("load Fleet resource ownership: %w", err)
	}

	dnsConflicts, removedDNS, err := manager.ensureOwnedDNS(ctx, apiConfig, automation, desired, instanceIDs, owned)
	if err != nil {
		manager.failAutomatedRoutes(desired, "Cloudflare DNS could not be reconciled: "+err.Error())
		return nil, fmt.Errorf("reconcile Cloudflare instance DNS: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	manager.mu.Lock()
	for hostname, observation := range manager.observations {
		observation.DNSCheckedAt = now
		if conflict := dnsConflicts[hostname]; conflict != "" {
			observation.DNSState = ResourceConflict
			observation.DNSDetail = conflict
			observation.Revalidating = false
			observation.ProviderState, observation.ProviderDetail = providerState(observation)
		} else {
			observation.DNSState = ResourceReady
			observation.DNSDetail = "Cloudflare DNS CNAME is owned by Fleet and targets this tunnel"
		}
		manager.observations[hostname] = observation
	}
	manager.mu.Unlock()

	current, err := manager.readAutomatedTunnelConfiguration(ctx, apiConfig, automation)
	if err != nil {
		manager.failIngress(desired, "Cloudflare tunnel configuration could not be read: "+err.Error())
		return nil, fmt.Errorf("read Cloudflare instance routes: %w", err)
	}
	if current.Source != "cloudflare" {
		detail := fmt.Sprintf("Cloudflare tunnel is %q managed; Fleet requires a remotely managed tunnel", current.Source)
		manager.failIngress(desired, detail)
		return nil, fmt.Errorf("%s", detail)
	}
	mergedIngress, ingressConflicts, applied, removed, err := mergeOwnedIngress(current.Config.Ingress, desired, instanceIDs, owned)
	if err != nil {
		manager.failIngress(desired, err.Error())
		return nil, err
	}
	if !sameJSON(current.Config.Ingress, mergedIngress) {
		path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", url.PathEscape(automation.AccountID), url.PathEscape(automation.TunnelID))
		if current.Version > 0 {
			path += "?version=" + fmt.Sprint(current.Version)
		}
		body := map[string]any{"config": map[string]any{
			"ingress": mergedIngress, "originRequest": current.Config.OriginRequest,
		}}
		if err := manager.apiWithConfig(ctx, apiConfig, automation.APIToken, http.MethodPut, path, body, nil); err != nil {
			writeErr := cloudflareTunnelConfigurationWriteError(err)
			manager.failIngress(desired, writeErr.Error())
			return nil, fmt.Errorf("update Cloudflare instance routes: %w", writeErr)
		}
	}
	resourceTime := time.Now().UTC()
	for hostname := range applied {
		if err := manager.ownership.PutRemoteAccessResource(ctx, domain.RemoteAccessResource{
			InstanceID: instanceIDs[hostname], Kind: "ingress", Hostname: hostname,
			TunnelID: automation.TunnelID, ZoneID: automation.ZoneID, OriginService: desired[hostname],
			CreatedAt: resourceTime, UpdatedAt: resourceTime,
		}); err != nil {
			return nil, fmt.Errorf("record Fleet ingress ownership: %w", err)
		}
	}
	verifiedConfig, err := manager.readAutomatedTunnelConfiguration(ctx, apiConfig, automation)
	if err != nil {
		manager.failIngress(desired, "Cloudflare route verification failed: "+err.Error())
		return nil, fmt.Errorf("verify Cloudflare instance routes: %w", err)
	}
	dnsRecords, err := manager.listAutomatedDNS(ctx, apiConfig, automation)
	if err != nil {
		manager.failAutomatedRoutes(desired, "Cloudflare DNS verification failed: "+err.Error())
		return nil, fmt.Errorf("verify Cloudflare instance DNS: %w", err)
	}
	for _, resource := range removedDNS {
		if dnsResourcePresent(dnsRecords, resource) {
			return nil, fmt.Errorf("verify stale Fleet DNS removal: %s is still present", resource.Hostname)
		}
		if err := manager.ownership.DeleteRemoteAccessResource(ctx, resource.InstanceID, resource.Kind, resource.Hostname); err != nil {
			return nil, fmt.Errorf("remove stale Fleet DNS ownership: %w", err)
		}
	}
	for _, resource := range removed {
		if ingressHostnamePresent(verifiedConfig.Config.Ingress, resource.Hostname) {
			return nil, fmt.Errorf("verify stale Fleet ingress removal: %s is still present", resource.Hostname)
		}
		if err := manager.ownership.DeleteRemoteAccessResource(ctx, resource.InstanceID, resource.Kind, resource.Hostname); err != nil {
			return nil, fmt.Errorf("remove stale Fleet ingress ownership: %w", err)
		}
	}

	now = time.Now().UTC().Format(time.RFC3339)
	target := normalizeHostname(automation.TunnelID + ".cfargotunnel.com")
	published := make(map[string]string)
	endpointCandidates := make(map[string]struct{}, len(desired))
	manager.mu.Lock()
	for hostname, origin := range desired {
		observation := manager.observations[hostname]
		observation.IngressCheckedAt = now
		if conflict := ingressConflicts[hostname]; conflict != "" {
			observation.IngressState = ResourceConflict
			observation.IngressDetail = conflict
		} else if !ingressContains(verifiedConfig.Config.Ingress, hostname, origin) {
			observation.IngressState = ResourceFailed
			observation.IngressDetail = "Cloudflare ingress does not match the Fleet service URL"
		} else {
			observation.IngressState = ResourceReady
			observation.IngressDetail = "Cloudflare ingress is owned by Fleet and matches the service URL"
		}
		if observation.DNSState == ResourceReady && !dnsContains(dnsRecords, hostname, target) {
			observation.DNSState = ResourceFailed
			observation.DNSDetail = "Cloudflare DNS verification did not find the expected CNAME"
		}
		if observation.DNSState == ResourceReady && observation.IngressState == ResourceReady {
			endpointCandidates[hostname] = struct{}{}
			if !observation.Revalidating {
				observation.EndpointState = EndpointChecking
			}
		} else {
			observation.Revalidating = false
		}
		observation.ProviderCheckedAt = now
		if observation.Revalidating {
			observation.ProviderState = RouteProviderPublishing
			observation.ProviderDetail = "Published route is being revalidated"
		} else {
			observation.ProviderState, observation.ProviderDetail = providerState(observation)
		}
		manager.observations[hostname] = observation
	}
	manager.mu.Unlock()

	for hostname, origin := range desired {
		if _, shouldCheck := endpointCandidates[hostname]; !shouldCheck {
			continue
		}
		state, detail := manager.checkPublicEndpoint(ctx, hostname)
		manager.mu.Lock()
		observation := manager.observations[hostname]
		observation.EndpointState = state
		observation.EndpointDetail = detail
		observation.EndpointCheckedAt = time.Now().UTC().Format(time.RFC3339)
		observation.Revalidating = false
		observation.ProviderState, observation.ProviderDetail = providerState(observation)
		manager.observations[hostname] = observation
		manager.mu.Unlock()
		if state == EndpointReachable {
			published[hostname] = origin
		}
	}
	return published, nil
}

func providerState(observation RouteObservation) (string, string) {
	if observation.DNSState == ResourceReady && observation.IngressState == ResourceReady && observation.EndpointState == EndpointReachable {
		return RouteProviderPublished, "DNS, tunnel ingress, and public endpoint are verified"
	}
	if observation.DNSState == ResourceConflict || observation.IngressState == ResourceConflict {
		return RouteProviderMismatch, "Cloudflare configuration conflicts with a resource not owned by Fleet"
	}
	if observation.DNSState == ResourceFailed || observation.IngressState == ResourceFailed || observation.EndpointState == EndpointUnavailable {
		return RouteProviderFailed, "One or more publication checks failed"
	}
	if observation.EndpointState == EndpointAccessProtected {
		return RouteProviderReady, "Cloudflare Access is reachable, but the dashboard origin is not verified"
	}
	return RouteProviderPublishing, "Publication verification is still in progress"
}

func (manager *Manager) instanceIDsByHostname(ctx context.Context, desired map[string]string) (map[string]string, error) {
	instances, err := manager.source(ctx)
	if err != nil {
		return nil, fmt.Errorf("list managed instances: %w", err)
	}
	result := make(map[string]string, len(desired))
	for _, instance := range instances {
		hostname, err := NormalizePublicHostname(instance.PublicHostname)
		if err != nil || hostname == "" {
			continue
		}
		if _, wanted := desired[hostname]; wanted {
			if previous := result[hostname]; previous != "" && previous != instance.ID {
				return nil, fmt.Errorf("public hostname %s is assigned to multiple instances", hostname)
			}
			result[hostname] = instance.ID
		}
	}
	for hostname := range desired {
		if result[hostname] == "" {
			return nil, fmt.Errorf("public hostname %s has no active Fleet instance owner", hostname)
		}
	}
	return result, nil
}

func (manager *Manager) verifyPublishingZone(ctx context.Context, config Config, automation RouteAutomationConfig) (string, error) {
	var zone struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := manager.apiWithConfig(ctx, config, automation.APIToken, http.MethodGet,
		"/zones/"+url.PathEscape(automation.ZoneID), nil, &zone); err != nil {
		return "", err
	}
	if zone.ID == "" || (zone.Account.ID != "" && zone.Account.ID != automation.AccountID) {
		return "", fmt.Errorf("configured zone does not belong to the configured account")
	}
	zoneName := normalizeHostname(zone.Name)
	if !validHostname(zoneName) {
		return "", fmt.Errorf("Cloudflare returned an invalid zone name")
	}
	manager.mu.Lock()
	if manager.config.RouteAutomation.AccountID == automation.AccountID && manager.config.RouteAutomation.ZoneID == automation.ZoneID {
		manager.config.RouteAutomation.ZoneName = zoneName
	}
	manager.mu.Unlock()
	return zoneName, nil
}

func indexOwnedResources(resources []domain.RemoteAccessResource, kind string) map[string]domain.RemoteAccessResource {
	indexed := make(map[string]domain.RemoteAccessResource)
	for _, resource := range resources {
		if resource.Kind == kind {
			indexed[normalizeHostname(resource.Hostname)] = resource
		}
	}
	return indexed
}

func (manager *Manager) ensureOwnedDNS(ctx context.Context, config Config, automation RouteAutomationConfig, desired map[string]string, instanceIDs map[string]string, owned []domain.RemoteAccessResource) (map[string]string, []domain.RemoteAccessResource, error) {
	records, err := manager.listAutomatedDNS(ctx, config, automation)
	if err != nil {
		return nil, nil, err
	}
	byName := make(map[string][]dnsRecord)
	byID := make(map[string]dnsRecord)
	for _, record := range records {
		byName[normalizeHostname(record.Name)] = append(byName[normalizeHostname(record.Name)], record)
		byID[record.ID] = record
	}
	target := normalizeHostname(automation.TunnelID + ".cfargotunnel.com")
	ownedByHostname := indexOwnedResources(owned, "dns")
	conflicts := make(map[string]string)
	removed := make([]domain.RemoteAccessResource, 0)
	hostnames := sortedRouteHostnames(desired)
	for _, hostname := range hostnames {
		existing := byName[hostname]
		ownership, isOwned := ownedByHostname[hostname]
		if len(existing) > 1 {
			conflicts[hostname] = "Cloudflare has multiple CNAME records for this hostname"
			continue
		}
		if len(existing) == 1 {
			record := existing[0]
			if !isOwned || ownership.InstanceID != instanceIDs[hostname] || ownership.ResourceID != record.ID {
				conflicts[hostname] = "An existing DNS record is not owned by this Fleet instance"
				continue
			}
			if normalizeHostname(record.Content) != target || !record.Proxied || record.Comment != managedDNSComment {
				conflicts[hostname] = "The Fleet-owned DNS record was changed outside Fleet"
			}
			continue
		}
		var created dnsRecord
		body := map[string]any{"type": "CNAME", "name": hostname, "content": target, "proxied": true, "ttl": 1, "comment": managedDNSComment}
		path := fmt.Sprintf("/zones/%s/dns_records", url.PathEscape(automation.ZoneID))
		if err := manager.apiWithConfig(ctx, config, automation.APIToken, http.MethodPost, path, body, &created); err != nil {
			conflicts[hostname] = "Cloudflare DNS record creation failed: " + err.Error()
			continue
		}
		if created.ID == "" {
			return conflicts, nil, fmt.Errorf("Cloudflare did not return the created DNS record identity for %s", hostname)
		}
		now := time.Now().UTC()
		if err := manager.ownership.PutRemoteAccessResource(ctx, domain.RemoteAccessResource{
			InstanceID: instanceIDs[hostname], Kind: "dns", ResourceID: created.ID, Hostname: hostname,
			TunnelID: automation.TunnelID, ZoneID: automation.ZoneID, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return conflicts, nil, err
		}
	}
	for _, resource := range owned {
		if resource.Kind != "dns" {
			continue
		}
		if _, wanted := desired[normalizeHostname(resource.Hostname)]; wanted {
			continue
		}
		record, exists := byID[resource.ResourceID]
		if exists {
			if normalizeHostname(record.Name) != normalizeHostname(resource.Hostname) || normalizeHostname(record.Content) != target || record.Comment != managedDNSComment {
				return conflicts, nil, fmt.Errorf("refusing to delete changed DNS record %s", resource.Hostname)
			}
			path := fmt.Sprintf("/zones/%s/dns_records/%s", url.PathEscape(automation.ZoneID), url.PathEscape(record.ID))
			if err := manager.apiWithConfig(ctx, config, automation.APIToken, http.MethodDelete, path, nil, nil); err != nil {
				return conflicts, nil, fmt.Errorf("delete stale Fleet DNS record %s: %w", resource.Hostname, err)
			}
		}
		removed = append(removed, resource)
	}
	return conflicts, removed, nil
}

func dnsResourcePresent(records []dnsRecord, resource domain.RemoteAccessResource) bool {
	hostname := normalizeHostname(resource.Hostname)
	for _, record := range records {
		if resource.ResourceID != "" && record.ID == resource.ResourceID {
			return true
		}
		if normalizeHostname(record.Name) == hostname && record.Comment == managedDNSComment {
			return true
		}
	}
	return false
}

func ingressHostnamePresent(entries []map[string]any, hostname string) bool {
	hostname = normalizeHostname(hostname)
	for _, entry := range entries {
		if normalizeHostname(fmt.Sprint(entry["hostname"])) == hostname {
			return true
		}
	}
	return false
}

func mergeOwnedIngress(current []map[string]any, desired map[string]string, instanceIDs map[string]string, owned []domain.RemoteAccessResource) ([]map[string]any, map[string]string, map[string]struct{}, []domain.RemoteAccessResource, error) {
	ownedByHostname := indexOwnedResources(owned, "ingress")
	result := make([]map[string]any, 0, len(current)+len(desired)+1)
	conflicts := make(map[string]string)
	applied := make(map[string]struct{})
	seen := make(map[string]struct{})
	var fallback map[string]any
	for _, entry := range current {
		hostname, _ := entry["hostname"].(string)
		hostname = normalizeHostname(hostname)
		if hostname == "" {
			fallback = cloneIngressEntry(entry)
			continue
		}
		wantedService, wanted := desired[hostname]
		ownership, isOwned := ownedByHostname[hostname]
		entryService, _ := entry["service"].(string)
		entryService = strings.TrimSpace(entryService)
		if wanted {
			seen[hostname] = struct{}{}
			if !isOwned || ownership.InstanceID != instanceIDs[hostname] {
				conflicts[hostname] = "An existing tunnel ingress rule is not owned by this Fleet instance"
				result = append(result, cloneIngressEntry(entry))
				continue
			}
			if entryService != ownership.OriginService {
				conflicts[hostname] = "The Fleet-owned tunnel ingress rule was changed outside Fleet"
				result = append(result, cloneIngressEntry(entry))
				continue
			}
			updated := cloneIngressEntry(entry)
			updated["hostname"] = hostname
			updated["service"] = wantedService
			result = append(result, updated)
			applied[hostname] = struct{}{}
			continue
		}
		if isOwned {
			if entryService != ownership.OriginService {
				return nil, conflicts, nil, nil, fmt.Errorf("refusing to remove changed ingress rule %s", hostname)
			}
			continue
		}
		result = append(result, cloneIngressEntry(entry))
	}
	for _, hostname := range sortedRouteHostnames(desired) {
		if _, exists := seen[hostname]; exists {
			continue
		}
		result = append(result, map[string]any{"hostname": hostname, "service": desired[hostname]})
		applied[hostname] = struct{}{}
	}
	if fallback == nil {
		fallback = map[string]any{"service": "http_status:404"}
	}
	result = append(result, fallback)
	var removed []domain.RemoteAccessResource
	for _, resource := range owned {
		if resource.Kind != "ingress" {
			continue
		}
		if _, wanted := desired[normalizeHostname(resource.Hostname)]; !wanted {
			removed = append(removed, resource)
		}
	}
	return result, conflicts, applied, removed, nil
}

// mergeManagedIngress remains as a small compatibility helper for local tests.
// Production reconciliation uses mergeOwnedIngress and never infers ownership
// from a hostname or service-name pattern.
func mergeManagedIngress(current []map[string]any, desired map[string]string) []map[string]any {
	result := make([]map[string]any, 0, len(current)+len(desired)+1)
	seen := make(map[string]struct{})
	var fallback map[string]any
	for _, entry := range current {
		hostname := normalizeHostname(fmt.Sprint(entry["hostname"]))
		if hostname == "" {
			fallback = cloneIngressEntry(entry)
			continue
		}
		if service, wanted := desired[hostname]; wanted {
			updated := cloneIngressEntry(entry)
			updated["service"] = service
			result = append(result, updated)
			seen[hostname] = struct{}{}
			continue
		}
		result = append(result, cloneIngressEntry(entry))
	}
	for _, hostname := range sortedRouteHostnames(desired) {
		if _, exists := seen[hostname]; !exists {
			result = append(result, map[string]any{"hostname": hostname, "service": desired[hostname]})
		}
	}
	if fallback == nil {
		fallback = map[string]any{"service": "http_status:404"}
	}
	return append(result, fallback)
}

func sortedRouteHostnames(routes map[string]string) []string {
	hostnames := make([]string, 0, len(routes))
	for hostname := range routes {
		hostnames = append(hostnames, hostname)
	}
	sort.Strings(hostnames)
	return hostnames
}

func cloneIngressEntry(entry map[string]any) map[string]any {
	copyEntry := make(map[string]any, len(entry))
	for key, value := range entry {
		copyEntry[key] = value
	}
	return copyEntry
}

func sameJSON(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func ingressContains(ingress []map[string]any, hostname, service string) bool {
	for _, entry := range ingress {
		entryHostname, _ := entry["hostname"].(string)
		entryService, _ := entry["service"].(string)
		if normalizeHostname(entryHostname) == hostname && strings.TrimSpace(entryService) == service {
			return true
		}
	}
	return false
}

func (manager *Manager) readAutomatedTunnelConfiguration(ctx context.Context, config Config, automation RouteAutomationConfig) (tunnelConfiguration, error) {
	var current tunnelConfiguration
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", url.PathEscape(automation.AccountID), url.PathEscape(automation.TunnelID))
	err := manager.apiWithConfig(ctx, config, automation.APIToken, http.MethodGet, path, nil, &current)
	return current, err
}

func (manager *Manager) listAutomatedDNS(ctx context.Context, config Config, automation RouteAutomationConfig) ([]dnsRecord, error) {
	path := fmt.Sprintf("/zones/%s/dns_records?type=CNAME&per_page=5000", url.PathEscape(automation.ZoneID))
	var records []dnsRecord
	err := manager.apiWithConfig(ctx, config, automation.APIToken, http.MethodGet, path, nil, &records)
	return records, err
}

func dnsContains(records []dnsRecord, hostname, target string) bool {
	matches := 0
	for _, record := range records {
		if normalizeHostname(record.Name) != hostname {
			continue
		}
		matches++
		if normalizeHostname(record.Content) != target || !record.Proxied || record.Comment != managedDNSComment {
			return false
		}
	}
	return matches == 1
}

func (manager *Manager) failAutomatedRoute(hostname, origin, detail string) {
	now := time.Now().UTC().Format(time.RFC3339)
	manager.mu.Lock()
	manager.observations[hostname] = RouteObservation{
		Hostname: hostname, OriginService: origin, ProviderState: RouteProviderFailed,
		ProviderDetail: detail, ProviderCheckedAt: now, DNSState: ResourceFailed,
		IngressState: ResourcePending, EndpointState: EndpointUnchecked,
	}
	manager.mu.Unlock()
}

func (manager *Manager) failAutomatedRoutes(routes map[string]string, detail string) {
	now := time.Now().UTC().Format(time.RFC3339)
	observations := make(map[string]RouteObservation, len(routes))
	for hostname, origin := range routes {
		observations[hostname] = RouteObservation{
			Hostname: hostname, OriginService: origin,
			ProviderState: RouteProviderFailed, ProviderDetail: detail, ProviderCheckedAt: now,
			DNSState: ResourceFailed, IngressState: ResourcePending, EndpointState: EndpointUnchecked,
		}
	}
	manager.mu.Lock()
	manager.observations = observations
	manager.mu.Unlock()
}

func (manager *Manager) failIngress(routes map[string]string, detail string) {
	now := time.Now().UTC().Format(time.RFC3339)
	manager.mu.Lock()
	for hostname, origin := range routes {
		observation := manager.observations[hostname]
		observation.Hostname = hostname
		observation.OriginService = origin
		observation.ProviderState = RouteProviderFailed
		observation.ProviderDetail = detail
		observation.ProviderCheckedAt = now
		observation.IngressState = ResourceFailed
		observation.IngressDetail = detail
		observation.IngressCheckedAt = now
		observation.EndpointState = EndpointUnchecked
		observation.Revalidating = false
		manager.observations[hostname] = observation
	}
	manager.mu.Unlock()
}

func (manager *Manager) checkPublicEndpoint(ctx context.Context, hostname string) (string, string) {
	return manager.checkPublicEndpointWithPolicy(ctx, hostname, 30*time.Second, time.Second)
}

func (manager *Manager) checkPublicEndpointWithPolicy(ctx context.Context, hostname string, timeout, retryInterval time.Duration) (string, string) {
	verificationContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lastState := EndpointUnavailable
	lastDetail := "Public endpoint verification timed out"
	for {
		state, detail, retry := manager.checkPublicEndpointOnce(verificationContext, hostname)
		lastState, lastDetail = state, detail
		if !retry {
			return state, detail
		}
		select {
		case <-verificationContext.Done():
			return lastState, lastDetail
		case <-time.After(retryInterval):
		}
	}
}

func (manager *Manager) checkPublicEndpointOnce(ctx context.Context, hostname string) (string, string, bool) {
	endpointContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	configuredClient := manager.endpointClient
	if manager.endpointClientFor != nil {
		var err error
		configuredClient, err = manager.endpointClientFor(endpointContext, hostname)
		if err != nil {
			return classifyPublicEndpointRequestError(ctx, err)
		}
	} else if configuredClient == nil {
		configuredClient = manager.client
	}
	client := *configuredClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := requestPublicEndpoint(endpointContext, &client, http.MethodHead, hostname)
	if err != nil {
		return classifyPublicEndpointRequestError(ctx, err)
	}
	if response.StatusCode == http.StatusMethodNotAllowed {
		response.Body.Close()
		response, err = requestPublicEndpoint(endpointContext, &client, http.MethodGet, hostname)
		if err != nil {
			return classifyPublicEndpointRequestError(ctx, err)
		}
	}
	response.Body.Close()
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return EndpointReachable, fmt.Sprintf("Public endpoint returned HTTP %d", response.StatusCode), false
	case response.StatusCode >= 300 && response.StatusCode < 400:
		location := strings.ToLower(response.Header.Get("Location"))
		if strings.Contains(location, ".cloudflareaccess.com") || strings.Contains(location, "/cdn-cgi/access") {
			return EndpointAccessProtected, "Cloudflare Access is reachable; origin was not verified", false
		}
		return EndpointReachable, fmt.Sprintf("Public endpoint returned redirect HTTP %d", response.StatusCode), false
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return EndpointAccessProtected, fmt.Sprintf("Protected endpoint returned HTTP %d; origin was not verified", response.StatusCode), false
	case response.StatusCode >= 500:
		return EndpointUnavailable, fmt.Sprintf("Public endpoint returned HTTP %d", response.StatusCode), true
	default:
		return EndpointUnavailable, fmt.Sprintf("Public endpoint returned HTTP %d", response.StatusCode), false
	}
}

func classifyPublicEndpointRequestError(ctx context.Context, err error) (string, string, bool) {
	if errors.Is(err, netpolicy.ErrUnsafeAddress) || errors.Is(err, netpolicy.ErrInvalidEndpoint) {
		return EndpointUnavailable, err.Error(), false
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		if dnsError.IsNotFound {
			return EndpointPropagating, "Public DNS is still propagating; Fleet will verify it automatically", ctx.Err() == nil
		}
		return EndpointUnavailable, "Public DNS verification could not resolve the hostname", ctx.Err() == nil
	}
	return EndpointUnavailable, err.Error(), ctx.Err() == nil
}

func requestPublicEndpoint(ctx context.Context, client *http.Client, method, hostname string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, "https://"+hostname, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

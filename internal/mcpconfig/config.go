package mcpconfig

import (
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/jacobcalvyn/hermes-fleet-manager/internal/domain"
)

const (
	MaximumServers        = 20
	MaximumToolsPerServer = 100
)

var (
	serverNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)
	toolNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

// NormalizeAndValidate deliberately accepts only remote MCP servers. Arbitrary
// stdio commands are equivalent to remote code execution and require a
// separately reviewed executable/package policy before Fleet may expose them.
func NormalizeAndValidate(config domain.MCPConfiguration) (domain.MCPConfiguration, error) {
	if len(config.Servers) > MaximumServers {
		return domain.MCPConfiguration{}, errors.New("at most 20 MCP servers can be configured per instance")
	}
	seen := make(map[string]struct{}, len(config.Servers))
	servers := make([]domain.MCPServerConfiguration, 0, len(config.Servers))
	for _, candidate := range config.Servers {
		server := candidate
		server.Name = strings.ToLower(strings.TrimSpace(server.Name))
		server.Source = strings.ToLower(strings.TrimSpace(server.Source))
		server.URL = strings.TrimSpace(server.URL)
		server.AuthType = strings.ToLower(strings.TrimSpace(server.AuthType))
		server.BearerToken = strings.TrimSpace(server.BearerToken)
		if !serverNamePattern.MatchString(server.Name) {
			return domain.MCPConfiguration{}, errors.New("MCP server name must use 2-32 lowercase letters, numbers, underscores, or hyphens")
		}
		if _, exists := seen[server.Name]; exists {
			return domain.MCPConfiguration{}, errors.New("MCP server names must be unique")
		}
		seen[server.Name] = struct{}{}
		if server.Source == "" {
			server.Source = "remote"
		}
		if server.Source != "remote" {
			return domain.MCPConfiguration{}, errors.New("only remote MCP servers are supported by this Fleet release")
		}
		parsed, err := url.Parse(server.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return domain.MCPConfiguration{}, errors.New("remote MCP URL must be an https URL without embedded credentials or a fragment")
		}
		if server.AuthType == "" {
			server.AuthType = "none"
		}
		switch server.AuthType {
		case "none":
			server.BearerToken = ""
		case "bearer":
			if server.Enabled && server.BearerToken == "" {
				return domain.MCPConfiguration{}, errors.New("enabled bearer-authenticated MCP servers require a token")
			}
			if len(server.BearerToken) > 4096 {
				return domain.MCPConfiguration{}, errors.New("MCP bearer token is too long")
			}
			if strings.IndexFunc(server.BearerToken, func(value rune) bool { return value < 0x20 || value == 0x7f }) >= 0 {
				return domain.MCPConfiguration{}, errors.New("MCP bearer token cannot contain control characters")
			}
		default:
			return domain.MCPConfiguration{}, errors.New("MCP authentication must be none or bearer")
		}
		if len(server.Tools) > MaximumToolsPerServer {
			return domain.MCPConfiguration{}, errors.New("an MCP server can allow at most 100 tools")
		}
		if server.Enabled && len(server.Tools) == 0 {
			return domain.MCPConfiguration{}, errors.New("enabled MCP servers require an explicit tool allowlist")
		}
		toolSet := make(map[string]struct{}, len(server.Tools))
		tools := make([]string, 0, len(server.Tools))
		for _, candidateTool := range server.Tools {
			tool := strings.TrimSpace(candidateTool)
			if !toolNamePattern.MatchString(tool) {
				return domain.MCPConfiguration{}, errors.New("MCP tool names contain unsupported characters or length")
			}
			if _, exists := toolSet[tool]; exists {
				continue
			}
			toolSet[tool] = struct{}{}
			tools = append(tools, tool)
		}
		sort.Strings(tools)
		server.Tools = tools
		servers = append(servers, server)
	}
	sort.Slice(servers, func(left, right int) bool { return servers[left].Name < servers[right].Name })
	return domain.MCPConfiguration{Servers: servers}, nil
}

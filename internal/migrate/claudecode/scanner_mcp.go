package claudecode

import (
	"encoding/json"
	"os"
	"sort"
)

// rawMCPServer is the on-the-wire shape. We decode into this, extract the
// key names from env, then **discard** the env map before returning. Values
// must not flow out of this function.
type rawMCPServer struct {
	Command  string                     `json:"command"`
	Args     []string                   `json:"args"`
	Type     string                     `json:"type"`
	URL      string                     `json:"url"`
	Env      map[string]json.RawMessage `json:"env"`
	Disabled bool                       `json:"disabled"`
	// Capture additional fields we don't model so we can flag them.
	Extra map[string]json.RawMessage `json:"-"`
}

func scanMCP(claudeUserConfig string) ([]ScannedMCPServer, []Warning, error) {
	info, err := os.Lstat(claudeUserConfig)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, []Warning{{Kind: "symlink_escape", Path: "~/.claude.json"}}, nil
	}
	data, err := os.ReadFile(claudeUserConfig)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	// Parse top-level into a generic map so we can detect unknown fields per server.
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, []Warning{{Kind: "parse_failed", Path: "~/.claude.json"}}, nil
	}
	raw, ok := root["mcpServers"]
	if !ok {
		return nil, nil, nil
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, []Warning{{Kind: "parse_failed", Path: "~/.claude.json"}}, nil
	}

	knownKeys := map[string]bool{
		"command": true, "args": true, "type": true, "url": true,
		"env": true, "disabled": true,
	}

	var out []ScannedMCPServer
	for name, srvRaw := range servers {
		var perServer map[string]json.RawMessage
		if err := json.Unmarshal(srvRaw, &perServer); err != nil {
			out = append(out, ScannedMCPServer{Name: name, Status: "error", ErrorReason: "parse_failed"})
			continue
		}

		s := ScannedMCPServer{Name: name, Status: "ok"}

		// Map known fields.
		if v, ok := perServer["command"]; ok {
			_ = json.Unmarshal(v, &s.Command)
		}
		if v, ok := perServer["args"]; ok {
			_ = json.Unmarshal(v, &s.Args)
		}
		if v, ok := perServer["type"]; ok {
			_ = json.Unmarshal(v, &s.Transport)
		}
		if s.Transport == "" {
			s.Transport = "stdio"
		}
		if s.Transport == "sse" {
			s.Transport = "http" // treat sse like http for our schema
		}
		if v, ok := perServer["url"]; ok {
			_ = json.Unmarshal(v, &s.URL)
		}
		if v, ok := perServer["disabled"]; ok {
			_ = json.Unmarshal(v, &s.Disabled)
		}

		// env: extract key names, discard values.
		if v, ok := perServer["env"]; ok {
			var envMap map[string]json.RawMessage
			if err := json.Unmarshal(v, &envMap); err == nil {
				for k := range envMap {
					s.EnvKeys = append(s.EnvKeys, k)
				}
				sort.Strings(s.EnvKeys)
				// Now the local variable envMap goes out of scope. No further reference.
			}
		}

		// Surface anything else (e.g., headers) as unsupported.
		for k := range perServer {
			if !knownKeys[k] {
				s.UnsupportedFields = append(s.UnsupportedFields, k)
			}
		}
		sort.Strings(s.UnsupportedFields)

		// Reject any transport we don't model. Per spec §10.4, only stdio,
		// http, and sse (normalized to http above) are supported in v1.
		// An unknown transport stays in the response with status=error so
		// the planner skips it; the result page lists it under skipped.
		switch s.Transport {
		case "stdio":
			if s.Command == "" {
				s.Status = "error"
				s.ErrorReason = "missing_command"
			}
		case "http":
			if s.URL == "" {
				s.Status = "error"
				s.ErrorReason = "missing_url"
			}
		default:
			s.Status = "error"
			s.ErrorReason = "unsupported_transport"
		}

		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil, nil
}

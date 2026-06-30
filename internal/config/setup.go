package config

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"

	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
)

const DefaultEndpoint = "https://api-dev.shannon.run"

const (
	hintCloud = "Get your API key at https://shannon.run"
	hintLocal = "Running locally? See https://github.com/Kocoro-lab/Shannon for self-hosting docs."
)

// keychainStoreOpener returns the credential store used by setup-time
// codepaths (Load hydration, setupGateway api_key persist). dir is the
// daemon's shannon dir (config.ShannonDir()) — used by the Linux file
// backend; ignored by the macOS Keychain / Windows Credential Manager
// backends. Tests replace this with an in-memory backend to avoid polluting
// the developer's real credential store.
//
// Returns nil-Store + non-nil-err on platforms without a credential store
// (ErrUnsupportedPlatform) so callers can short-circuit cleanly.
var keychainStoreOpener = func(dir string) (*keychain.Store, error) {
	return keychain.NewOSStoreAt(dir, nil)
}

// NeedsSetup returns true if the config has no API key and the endpoint
// is not a local address (localhost/127.0.0.1 bypass auth).
// Ollama provider never needs gateway setup. Load hydrates cfg.APIKey from
// Keychain on macOS before callers reach this check.
func NeedsSetup(cfg *Config) bool {
	if cfg.Provider == "ollama" {
		return cfg.Ollama.Model == "" // model required for ollama to be usable
	}
	if cfg.APIKey != "" {
		return false
	}
	return !isLocalEndpoint(cfg.Endpoint)
}

func hydrateAPIKeyFromKeychain(cfg *Config, shannonDir string) {
	if cfg == nil || cfg.APIKey != "" || !keychain.Supported() {
		return
	}
	if testing.Testing() && os.Getenv("KOCORO_FORCE_KEYCHAIN_HYDRATE") != "1" {
		return
	}
	store, err := keychainStoreOpener(shannonDir)
	if err != nil {
		return
	}
	apiKey, err := store.GetAPIKey()
	if err != nil {
		return
	}
	cfg.APIKey = strings.TrimSpace(apiKey)
	cfg.apiKeyFromKeychain = cfg.APIKey != ""
}

// RunSetup runs the interactive setup flow, prompting the user for
// provider selection (Shannon Cloud or Ollama) and provider-specific config.
func RunSetup(cfg *Config, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)

	fmt.Fprintln(out, "Kocoro CLI Setup")
	fmt.Fprintln(out)

	// Provider selection
	fmt.Fprintln(out, "Choose your LLM provider:")
	fmt.Fprintln(out, "  1) Shannon Cloud")
	fmt.Fprintln(out, "  2) Local model (Ollama)")
	fmt.Fprint(out, "Choice [1]: ")
	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)

	switch choice {
	case "2":
		if err := setupOllama(cfg, reader, out); err != nil {
			return err
		}
	default:
		if err := setupGateway(cfg, in, reader, out); err != nil {
			return err
		}
	}

	return saveSetup(cfg, out)
}

// setupGateway runs the gateway (Shannon Cloud) setup flow.
func setupGateway(cfg *Config, in io.Reader, reader *bufio.Reader, out io.Writer) error {
	cfg.Provider = "gateway"

	// Endpoint
	defaultEP := cfg.Endpoint
	if defaultEP == "" {
		defaultEP = DefaultEndpoint
	}
	fmt.Fprintf(out, "API endpoint [%s]: ", defaultEP)
	epInput, _ := reader.ReadString('\n')
	epInput = strings.TrimSpace(epInput)
	if epInput != "" {
		cfg.Endpoint = epInput
	} else {
		cfg.Endpoint = defaultEP
	}

	// Contextual hint
	if isLocalEndpoint(cfg.Endpoint) {
		fmt.Fprintln(out, hintLocal)
	} else {
		fmt.Fprintln(out, hintCloud)
	}
	fmt.Fprintln(out)

	// API key + health check with retry (max 3 attempts)
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Prompt for key
		if isLocalEndpoint(cfg.Endpoint) {
			fmt.Fprint(out, "API key (optional for local, Enter to skip): ")
		} else {
			fmt.Fprint(out, "API key: ")
		}

		if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			keyBytes, err := term.ReadPassword(int(f.Fd()))
			fmt.Fprintln(out) // newline after masked input
			if err != nil {
				fmt.Fprintf(out, "Error reading key: %v\n", err)
				continue
			}
			cfg.APIKey = strings.TrimSpace(string(keyBytes))
		} else {
			keyInput, _ := reader.ReadString('\n')
			cfg.APIKey = strings.TrimSpace(keyInput)
		}

		// Health check
		fmt.Fprint(out, "Testing connection... ")
		if err := checkEndpointHealth(cfg.Endpoint, cfg.APIKey); err != nil {
			fmt.Fprintf(out, "FAILED (%v)\n", err)

			if attempt == maxAttempts-1 {
				fmt.Fprintln(out, "Config saved anyway. Re-run 'shan --setup' to fix.")
				break
			}
			fmt.Fprint(out, "Re-enter credentials? [Y/n]: ")
			ans, _ := reader.ReadString('\n')
			ans = strings.TrimSpace(strings.ToLower(ans))
			if ans == "n" || ans == "no" {
				fmt.Fprintln(out, "Config saved anyway. Re-run 'shan --setup' to fix.")
				break
			}
			continue
		}

		fmt.Fprintln(out, "OK")
		break
	}

	// On platforms with a credential store (macOS Keychain / Windows
	// Credential Manager / Linux file store), route the pasted api_key into
	// the "legacy" account so AuthManager.Bootstrap can adopt it (resolve
	// user_id via /auth/me, rename the entry). Clear cfg.APIKey so it does not
	// end up in yaml. Other platforms continue to persist cfg.APIKey to yaml
	// (legacy path).
	if keychain.Supported() && cfg.APIKey != "" {
		if store, err := keychainStoreOpener(ShannonDir()); err == nil {
			if err := store.Write(keychain.ServiceDaemonAPIKey, keychain.AccountLegacy, cfg.APIKey); err == nil {
				_ = store.Write(keychain.ServiceDaemonState, keychain.AccountCurrentUser, keychain.AccountLegacy)
				cfg.APIKey = ""
				fmt.Fprintln(out, "API key stored in the OS credential store (ai.kocoro.daemon.api_key).")
			} else {
				fmt.Fprintf(out, "Warning: could not write credential store (%v); falling back to yaml plaintext.\n", err)
			}
		}
	}

	return nil
}

// setupOllama runs the Ollama local model setup flow.
func setupOllama(cfg *Config, reader *bufio.Reader, out io.Writer) error {
	cfg.Provider = "ollama"

	// Endpoint
	defaultEP := cfg.Ollama.Endpoint
	if defaultEP == "" {
		defaultEP = "http://localhost:11434"
	}
	fmt.Fprintf(out, "Ollama endpoint [%s]: ", defaultEP)
	epInput, _ := reader.ReadString('\n')
	epInput = strings.TrimSpace(epInput)
	if epInput != "" {
		cfg.Ollama.Endpoint = epInput
	} else {
		cfg.Ollama.Endpoint = defaultEP
	}

	// Health check
	fmt.Fprint(out, "Checking Ollama... ")
	if err := checkOllamaHealth(cfg.Ollama.Endpoint); err != nil {
		fmt.Fprintf(out, "FAILED (%v)\n", err)
		fmt.Fprintln(out, "Config saved anyway. Re-run 'shan --setup' to fix.")
		return nil
	}
	fmt.Fprintln(out, "OK")

	// Fetch and list models
	models, err := fetchOllamaModels(cfg.Ollama.Endpoint)
	if err != nil {
		fmt.Fprintf(out, "Could not list models: %v\n", err)
		fmt.Fprint(out, "Enter model name manually: ")
		name, _ := reader.ReadString('\n')
		cfg.Ollama.Model = strings.TrimSpace(name)
		return nil
	}

	if len(models) == 0 {
		fmt.Fprintln(out, "No models found. Pull a model first: ollama pull <model>")
		fmt.Fprint(out, "Enter model name manually: ")
		name, _ := reader.ReadString('\n')
		cfg.Ollama.Model = strings.TrimSpace(name)
		return nil
	}

	fmt.Fprintln(out, "Available models:")
	for i, m := range models {
		sizeGB := float64(m.Size) / 1e9
		paramSize := m.Details.ParameterSize
		if paramSize != "" {
			fmt.Fprintf(out, "  %d) %s (%s, %.1f GB)\n", i+1, m.Name, paramSize, sizeGB)
		} else {
			fmt.Fprintf(out, "  %d) %s (%.1f GB)\n", i+1, m.Name, sizeGB)
		}
	}
	fmt.Fprint(out, "Choose model [1]: ")
	modelChoice, _ := reader.ReadString('\n')
	modelChoice = strings.TrimSpace(modelChoice)

	idx := 0 // default to first
	if modelChoice != "" {
		fmt.Sscanf(modelChoice, "%d", &idx)
		idx-- // 1-based → 0-based
	}
	if idx < 0 || idx >= len(models) {
		idx = 0
	}
	cfg.Ollama.Model = models[idx].Name

	return nil
}

// ollamaModelInfo represents a model entry from the Ollama /api/tags response.
type ollamaModelInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Details struct {
		ParameterSize string `json:"parameter_size"`
	} `json:"details"`
}

// checkOllamaHealth verifies that an Ollama server is reachable.
func checkOllamaHealth(endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	base := strings.TrimSuffix(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// fetchOllamaModels retrieves the list of locally available models from Ollama.
func fetchOllamaModels(endpoint string) ([]ollamaModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	base := strings.TrimSuffix(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unreachable")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result struct {
		Models []ollamaModelInfo `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}
	return result.Models, nil
}

// saveSetup persists the config to disk. Tolerates Save errors (e.g. in tests
// where no real shannon dir exists) by printing a warning instead of failing.
func saveSetup(cfg *Config, out io.Writer) error {
	if err := Save(cfg); err != nil {
		// Save may fail in test environments (no real config dir) or due to
		// permission issues. Print a warning so the user knows, but don't
		// block setup — the in-memory config is still correct for this session.
		fmt.Fprintf(out, "Warning: could not save config: %v\n", err)
	} else if dir := ShannonDir(); dir != "" {
		fmt.Fprintf(out, "Config saved to %s/config.yaml\n", dir)
	}
	fmt.Fprintln(out)
	return nil
}

func isLocalEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0"
}

func checkEndpointHealth(endpoint, apiKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	base := strings.TrimSuffix(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return err
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

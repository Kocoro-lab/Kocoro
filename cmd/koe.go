package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// koeConfig holds the resolved settings for one `shan koe` voice session.
type koeConfig struct {
	openAIKey string // DEV-KEY: replaced by the deferred daemon mint relay (→ Plan D Cloud mint)
	daemonURL string
	agent     string
	model     string
	language  string
}

func defaultKoeConfig() koeConfig {
	return koeConfig{
		daemonURL: "http://127.0.0.1:7533", // must match the daemon's listen addr; Desktop (Plan E) passes the real one
		model:     "gpt-realtime-mini-2025-12-15",
	}
}

var koeCmd = &cobra.Command{
	Use:   "koe",
	Short: "Voice front-brain: a realtime voice agent that delegates to the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := defaultKoeConfig()
		if v, _ := cmd.Flags().GetString("openai-key"); v != "" {
			cfg.openAIKey = v
		}
		if cfg.openAIKey == "" {
			cfg.openAIKey = os.Getenv("OPENAI_API_KEY") // DEV-KEY: replaced by the deferred daemon mint relay (→ Plan D Cloud mint)
		}
		if v, _ := cmd.Flags().GetString("daemon-url"); v != "" {
			cfg.daemonURL = v
		}
		cfg.agent, _ = cmd.Flags().GetString("agent")
		if v, _ := cmd.Flags().GetString("model"); v != "" {
			cfg.model = v
		}
		cfg.language, _ = cmd.Flags().GetString("language")

		if cfg.openAIKey == "" {
			return fmt.Errorf("no OpenAI key: set OPENAI_API_KEY or --openai-key (C-minimal dev key; the deferred daemon mint relay replaces this in prod)")
		}
		return runKoeCall(cmd.Context(), cfg) // implemented in Task 5
	},
}

func init() {
	koeCmd.Flags().String("openai-key", "", "OpenAI API key (dev; C-minimal only)")
	koeCmd.Flags().String("daemon-url", "", "daemon base URL (default http://127.0.0.1:7533)")
	koeCmd.Flags().String("agent", "", "bound back-brain agent slug (empty = daemon default)")
	koeCmd.Flags().String("model", "", "realtime model (default gpt-realtime-mini-2025-12-15)")
	koeCmd.Flags().String("language", "", "conversation language hint")
	rootCmd.AddCommand(koeCmd)
}

// stub — replaced in Task 5
func runKoeCall(ctx context.Context, cfg koeConfig) error { return fmt.Errorf("not implemented") }

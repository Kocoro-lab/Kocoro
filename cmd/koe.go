package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Kocoro-lab/ShanClaw/internal/koe"
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

		// No key check: with no --openai-key/OPENAI_API_KEY, runKoeCall mints via
		// the daemon (production path — Koe holds no credential). A dev key, if set,
		// takes the direct mint path instead.
		return runKoeCall(cmd.Context(), cfg)
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

const koePersona = "You are Kocoro, a calm, professional voice assistant. Speak in the first person as Kocoro. " +
	"When the user asks for real work, call do_task and then say the result in one or two short spoken sentences. " +
	"Never read markdown, code, JSON, URLs, or file paths aloud. Confirm irreversible actions by restating them and waiting for a clear yes. " +
	"Never narrate that you are delegating — just do it and report back as yourself."

func newBurstID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "burst-" + hex.EncodeToString(b[:])
}

func runKoeCall(ctx context.Context, cfg koeConfig) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Plan B wiring: link + resolver + dispatcher + per-call state.
	client := koe.NewDaemonClient(cfg.daemonURL)
	agents, err := client.ListAgents(ctx)
	if err != nil {
		log.Printf("koe: list agents failed (continuing with empty registry): %v", err)
	}
	resolver := koe.NewAgentResolver(agents, koe.NoopSemanticMatcher{})
	state := koe.NewCallState(newBurstID(), cfg.agent)
	disp := koe.NewDispatcher(client, resolver, state, nil) // controlApp nil in C-minimal (no Desktop)

	// Audio + WebRTC.
	audio, err := koe.NewAudioIO()
	if err != nil {
		return fmt.Errorf("audio init: %v", err)
	}
	if err := audio.Start(); err != nil {
		return fmt.Errorf("audio start: %v", err)
	}
	defer audio.Stop()

	// Mint the ephemeral secret. Production path is the via-daemon relay (Koe
	// never holds a long-lived credential). A dev key (--openai-key/OPENAI_API_KEY)
	// takes the direct mint instead — the C-minimal escape hatch for running
	// without a signed-in daemon.
	var ek string
	if cfg.openAIKey != "" {
		ek, err = koe.MintEphemeral(ctx, cfg.openAIKey, cfg.model) // DEV-KEY: direct dev mint
	} else {
		ek, err = client.MintViaDaemon(ctx, cfg.model) // via daemon → Cloud
	}
	if err != nil {
		return fmt.Errorf("mint: %v", err)
	}
	conn, err := koe.Connect(ctx, audio, ek, koePersona, state, disp)
	if err != nil {
		return fmt.Errorf("connect: %v", err)
	}
	defer conn.Close()

	fmt.Println("Kocoro is listening. Speak; Ctrl-C to end.")
	<-ctx.Done()
	return nil
}

package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kocoro-lab/ShanClaw/internal/koe"
)

// koeConfig holds the resolved settings for one `shan koe` voice session.
type koeConfig struct {
	openAIKey   string // DEV-KEY: replaced by the deferred daemon mint relay (→ Plan D Cloud mint)
	daemonURL   string
	agent       string
	model       string
	language    string
	controlPort string // Desktop↔Koe control server port (Kocoro Desktop passes it); empty = no control channel
	aec         string // echo control: "" / "gate" = v1 half-duplex gate (default), "vpio" = Apple VoiceProcessingIO full-duplex AEC
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
		cfg.controlPort, _ = cmd.Flags().GetString("control-port")
		cfg.aec, _ = cmd.Flags().GetString("aec")

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
	koeCmd.Flags().String("control-port", "", "Desktop↔Koe control server port (Kocoro Desktop passes it)")
	koeCmd.Flags().String("aec", "", "echo control: gate (default, half-duplex) | vpio (Apple VoiceProcessingIO full-duplex AEC)")
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

	// G2: Kocoro Desktop control channel. When Desktop spawns `shan koe
	// --control-port N`, stand up the control server so it receives voice_state /
	// control_app SSE and the control_app tool routes to the Desktop window. No
	// port = standalone CLI (no control channel).
	var onVoiceState func(string)
	var controlApp koe.ControlAppFunc
	if cfg.controlPort != "" {
		ctrl := koe.NewControlServer(nil, nil)
		onVoiceState = ctrl.EmitVoiceState
		controlApp = func(_ context.Context, action string) error {
			ctrl.EmitControlApp(action)
			return nil
		}
		go func() {
			addr := "127.0.0.1:" + cfg.controlPort
			if err := http.ListenAndServe(addr, ctrl.Handler()); err != nil {
				log.Printf("koe: control server on %s exited: %v", addr, err)
			}
		}()
	}
	disp := koe.NewDispatcher(client, resolver, state, controlApp)

	// Audio + WebRTC.
	audio, err := koe.NewAudioIO()
	if err != nil {
		return fmt.Errorf("audio init: %v", err)
	}
	startAudio := audio.Start
	if cfg.aec == "vpio" {
		startAudio = audio.StartVPIO // Apple VoiceProcessingIO full-duplex AEC (terminal); default is the v1 half-duplex gate
	}
	if err := startAudio(); err != nil {
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
	// G3: relay each turn's token usage via the daemon to Cloud for server-side
	// cost + quota (fire-and-forget; a usage failure never interrupts the call,
	// and Koe never sees pricing). Active whenever the daemon is reachable.
	onUsage := func(usage json.RawMessage) {
		go func() {
			if err := client.SendRealtimeUsage(context.Background(), usage); err != nil {
				log.Printf("koe: usage relay failed: %v", err)
			}
		}()
	}
	// persona-profile: append the daemon's small-tier-distilled user context (who
	// the user is, how to address them) to the base persona, so Kocoro greets the
	// user as themselves. Bounded to 3s + best-effort — a slow/failed distill must
	// not delay or block the call; Koe then speaks with its base persona only.
	persona := koePersona
	pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
	if extra, perr := client.FetchPersona(pctx); perr == nil && extra != "" {
		persona = koePersona + " " + extra
	}
	pcancel()

	conn, err := koe.Connect(ctx, audio, ek, persona, state, disp, koe.ConnectOptions{
		OnVoiceState: onVoiceState,
		Model:        cfg.model,
		OnUsage:      onUsage,
	})
	if err != nil {
		return fmt.Errorf("connect: %v", err)
	}
	defer conn.Close()

	fmt.Println("Kocoro is listening. Speak; Ctrl-C to end.")
	<-ctx.Done()
	return nil
}

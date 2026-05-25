package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
	"github.com/spf13/cobra"
)

// parseStatefulFlag converts the --stateful CLI string into the *bool shape
// schedule.UpdateOpts expects. Empty input means "no change"; any non-empty
// value MUST parse as a bool or we error out — silently coercing "maybe" /
// "yes" / "" to false would let a typo flip a schedule's behaviour without
// the user realising.
func parseStatefulFlag(raw string) (*bool, error) {
	if raw == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, fmt.Errorf("--stateful must be true or false, got %q", raw)
	}
	return &b, nil
}

func newScheduleManager() *schedule.Manager {
	dir := config.ShannonDir()
	return schedule.NewManager(filepath.Join(dir, "schedules.json"))
}

var scheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Manage local scheduled tasks",
}

var scheduleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all scheduled tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr := newScheduleManager()
		list, err := mgr.List()
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("No scheduled tasks.")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tAGENT\tCRON\tENABLED\tSYNC\tPROMPT")
		for _, s := range list {
			prompt := s.Prompt
			if len([]rune(prompt)) > 50 {
				prompt = string([]rune(prompt)[:50]) + "..."
			}
			agent := s.Agent
			if agent == "" {
				agent = "(default)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\t%s\n", s.ID, agent, s.Cron, s.Enabled, s.SyncStatus, prompt)
		}
		w.Flush()
		return nil
	},
}

var (
	schedCreateAgent    string
	schedCreateCron     string
	schedCreatePrompt   string
	schedCreateStateful bool
)

var scheduleCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new scheduled task",
	RunE: func(cmd *cobra.Command, args []string) error {
		if schedCreateCron == "" || schedCreatePrompt == "" {
			return fmt.Errorf("--cron and --prompt are required")
		}
		mgr := newScheduleManager()
		id, err := mgr.Create(schedCreateAgent, schedCreateCron, schedCreatePrompt, schedCreateStateful)
		if err != nil {
			return err
		}
		fmt.Printf("Created schedule %s\n", id)
		return nil
	},
}

var (
	schedUpdateCron     string
	schedUpdatePrompt   string
	schedUpdateStateful string // "" | parseable bool
)

var scheduleUpdateCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Update a scheduled task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		statefulPtr, err := parseStatefulFlag(schedUpdateStateful)
		if err != nil {
			return err
		}
		if schedUpdateCron == "" && schedUpdatePrompt == "" && statefulPtr == nil {
			return fmt.Errorf("at least one of --cron, --prompt, --stateful is required")
		}
		mgr := newScheduleManager()
		opts := &schedule.UpdateOpts{Stateful: statefulPtr}
		if schedUpdateCron != "" {
			opts.Cron = &schedUpdateCron
		}
		if schedUpdatePrompt != "" {
			opts.Prompt = &schedUpdatePrompt
		}
		if err := mgr.Update(args[0], opts); err != nil {
			return err
		}
		fmt.Printf("Updated schedule %s\n", args[0])
		return nil
	},
}

var scheduleRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Remove a scheduled task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr := newScheduleManager()
		if err := mgr.Remove(args[0]); err != nil {
			return err
		}
		fmt.Printf("Removed schedule %s\n", args[0])
		return nil
	},
}

var scheduleEnableCmd = &cobra.Command{
	Use:   "enable <id>",
	Short: "Enable a scheduled task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr := newScheduleManager()
		enabled := true
		if err := mgr.Update(args[0], &schedule.UpdateOpts{Enabled: &enabled}); err != nil {
			return err
		}
		fmt.Printf("Enabled schedule %s\n", args[0])
		return nil
	},
}

var scheduleDisableCmd = &cobra.Command{
	Use:   "disable <id>",
	Short: "Disable a scheduled task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mgr := newScheduleManager()
		disabled := false
		if err := mgr.Update(args[0], &schedule.UpdateOpts{Enabled: &disabled}); err != nil {
			return err
		}
		fmt.Printf("Disabled schedule %s\n", args[0])
		return nil
	},
}

func init() {
	scheduleCreateCmd.Flags().StringVar(&schedCreateAgent, "agent", "", "Agent to run (empty for default)")
	scheduleCreateCmd.Flags().StringVar(&schedCreateCron, "cron", "", "Cron expression (5-field, supports ranges/steps/lists)")
	scheduleCreateCmd.Flags().StringVar(&schedCreatePrompt, "prompt", "", "Prompt to send")
	scheduleCreateCmd.Flags().BoolVar(&schedCreateStateful, "stateful", false,
		"Preserve LLM conversation history across runs (default false: each run starts with empty context). "+
			"Set --stateful for tasks that genuinely need cross-run memory.")

	scheduleUpdateCmd.Flags().StringVar(&schedUpdateCron, "cron", "", "New cron expression")
	scheduleUpdateCmd.Flags().StringVar(&schedUpdatePrompt, "prompt", "", "New prompt")
	scheduleUpdateCmd.Flags().StringVar(&schedUpdateStateful, "stateful", "",
		"Change history-preservation behaviour: 'true', 'false', or omit to leave unchanged.")

	scheduleCmd.AddCommand(scheduleListCmd)
	scheduleCmd.AddCommand(scheduleCreateCmd)
	scheduleCmd.AddCommand(scheduleUpdateCmd)
	scheduleCmd.AddCommand(scheduleRemoveCmd)
	scheduleCmd.AddCommand(scheduleEnableCmd)
	scheduleCmd.AddCommand(scheduleDisableCmd)
	rootCmd.AddCommand(scheduleCmd)
}

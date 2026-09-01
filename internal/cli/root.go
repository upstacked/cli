package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/api"
	"github.com/upstacked/cli/internal/config"
	"github.com/upstacked/cli/internal/errs"
)

// NewRoot builds the command tree. Everything hangs off one App so that tests
// can drive the whole surface with injected streams and a stub server.
func NewRoot(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "ups",
		Short: "Operate Upstacked infrastructure from the terminal",
		Long: `ups manages monitored network infrastructure: devices, monitoring,
credentials, IPAM, changes, runbooks and tickets.

Most commands act on a live environment. Run 'ups doctor' if anything
looks wrong, and 'ups context show' to see which server and infrastructure
are active before you write.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return app.Load()
		},
	}

	f := root.PersistentFlags()
	f.StringVar(&app.ConfigDir, "config-dir", "", "override the config directory")
	f.StringVar(&app.ProfileArg, "profile", "", "profile to use")
	f.StringVar(&app.APIURLArg, "api-url", "", "Upstacked server URL (or an alias: "+joinAliases()+")")
	f.StringVar(&app.InfraArg, "infra", "", "infrastructure id to act on")
	f.StringVar(&app.CustArg, "customer", "", "customer id to act on")
	f.BoolVar(&app.AsJSON, "json", false, "emit JSON instead of a table")
	f.BoolVar(&app.IDOnly, "id-only", false, "emit only ids, one per line")
	f.DurationVar(&app.Timeout, "timeout", api.DefaultTimeout, "per-request timeout")
	f.IntVar(&app.Limit, "limit", 100, "maximum results to return (0 for no client-side cap)")
	f.BoolVar(&app.AssumeYes, "yes", false, "do not prompt for confirmation")
	f.BoolVar(&app.DryRun, "dry-run", false, "show what would happen without doing it")
	f.BoolVar(&app.Debug, "debug", false, "log requests to stderr")

	root.AddCommand(
		newInitCmd(app),
		newDoctorCmd(app),
		newSkillCmd(app),
		newLoginCmd(app),
		newLogoutCmd(app),
		newWhoamiCmd(app),
		newContextCmd(app),
		newProfileCmd(app),
		newInfraCmd(app),
		newHostCmd(app),
		newMonitoringCmd(app),
		newEventCmd(app),
		newMaintenanceCmd(app),
		newChangeCmd(app),
		newCredentialCmd(app),
		newIPAMCmd(app),
		newAssetCmd(app),
		newRunbookCmd(app),
		newTicketCmd(app),
		newReportCmd(app),
		newLogsCmd(app),
		newDiscoveryCmd(app),
		newExportCmd(app),
		newDiffCmd(app),
		newApplyCmd(app),
		newCompletionCmd(),
		newVersionCmd(app),
	)
	return root
}

func joinAliases() string {
	names := config.AliasNames()
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// Execute runs the CLI and maps errors onto the exit-code contract.
func Execute(version string) int {
	Version = version
	api.Version = version

	app := &App{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	root := NewRoot(app)
	root.Version = version

	err := root.Execute()
	if err == nil {
		return errs.CodeOK
	}
	return reportError(app, err)
}

// reportError prints the failure and its remedy, then returns the exit code.
func reportError(app *App, err error) int {
	theme := app.Theme()
	fmt.Fprintf(app.Stderr, "%s %s\n", theme.Red.Apply("error:"), err.Error())
	if hint := errs.HintOf(err); hint != "" {
		fmt.Fprintf(app.Stderr, "  %s %s\n", theme.Dim.Apply("hint:"), hint)
	}
	return errs.CodeOf(err)
}

func newVersionCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		RunE: func(cmd *cobra.Command, args []string) error {
			if app.AsJSON {
				return app.Printer.JSON(map[string]string{"version": Version})
			}
			app.Printer.Printf("ups %s", Version)
			return nil
		},
	}
}

func newCompletionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate a shell completion script",
		Long: `Generate a shell completion script.

  zsh:   ups completion zsh  > "${fpath[1]}/_ups"
  bash:  ups completion bash > /usr/local/etc/bash_completion.d/ups
  fish:  ups completion fish > ~/.config/fish/completions/ups.fish`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return cmd.Root().GenZshCompletion(os.Stdout)
			case "fish":
				return cmd.Root().GenFishCompletion(os.Stdout, true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
			}
			return errs.Usage("unknown shell %q", args[0])
		},
	}
	return c
}

var _ = time.Second

package cmd

import (
	"fmt"
	"strconv"

	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish]",
	Short: "Generate shell completion scripts",
	Long: `Generate shell completion scripts for sonar.

To load completions:

Bash:
  $ source <(sonar completion bash)

  # To load completions for each session, execute once:
  # Linux:
  $ sonar completion bash > /etc/bash_completion.d/sonar
  # macOS:
  $ sonar completion bash > $(brew --prefix)/etc/bash_completion.d/sonar

Zsh:
  # If shell completion is not already enabled in your environment,
  # you will need to enable it. You can execute the following once:
  $ echo "autoload -U compinit; compinit" >> ~/.zshrc

  # To load completions for each session, execute once:
  $ sonar completion zsh > "${fpath[1]}/_sonar"

  # You will need to start a new shell for this setup to take effect.

Fish:
  $ sonar completion fish | source

  # To load completions for each session, execute once:
  $ sonar completion fish > ~/.config/fish/completions/sonar.fish
`,
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{"bash", "zsh", "fish"},
	Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return cmd.Root().GenBashCompletion(cmd.OutOrStdout())
		case "zsh":
			return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
		case "fish":
			return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
		default:
			return fmt.Errorf("unsupported shell: %s", args[0])
		}
	},
}

func init() {
	rootCmd.AddCommand(completionCmd)
}

// completePort provides shell completions for commands that take a <port> argument.
// It scans listening ports, enriches them with Docker and process info, and returns
// completions formatted as "port\tdescription".
func completePort(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	pp, err := ports.Scan()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}

	docker.EnrichPorts(pp)
	ports.Enrich(pp)

	var completions []string
	for _, p := range pp {
		portStr := strconv.Itoa(p.Port)
		completions = append(completions, fmt.Sprintf("%s\t%s", portStr, p.DisplayName()))
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}

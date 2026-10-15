package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/selfupdate"
	"github.com/spf13/cobra"
)

var (
	remoteAddName     string
	remoteAddPort     int
	remoteAddIdentity string
	remoteAddSSHArgs  []string
	remoteAddBinary   string
	remoteAddJSON     bool
)

var remoteAddCmd = &cobra.Command{
	Use:   "add [name] <user@host>",
	Short: "Register a host and start bridging to it",
	Long: `Register an SSH host. The daemon connects to it immediately and keeps the
connection up, reconnecting with backoff for as long as the host stays
registered.

The name comes from the first of two arguments, from --name, or — with neither
— from the host part of the target. The target itself is handed to ssh
unchanged, so jump hosts, aliases and identities from your ~/.ssh/config apply
exactly as they do from a shell. sonar stores no password and no key.

Examples:
  sonar remote add deploy@203.0.113.7
  sonar remote add hetzner deploy@203.0.113.7
  sonar remote add deploy@203.0.113.7 --name hetzner
  sonar remote add box --ssh-arg -J --ssh-arg bastion
  sonar remote add deploy@box --identity ~/.ssh/id_ed25519 --port 2222`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runRemoteAdd,
}

func init() {
	remoteAddCmd.Flags().StringVar(&remoteAddName, "name", "",
		"Name to address the host by (default: the host part of the target)")
	remoteAddCmd.Flags().IntVar(&remoteAddPort, "port", 0, "SSH port")
	remoteAddCmd.Flags().StringVar(&remoteAddIdentity, "identity", "", "SSH identity file")
	remoteAddCmd.Flags().StringArrayVar(&remoteAddSSHArgs, "ssh-arg", nil,
		"Extra argument for ssh; repeat for each one")
	remoteAddCmd.Flags().StringVar(&remoteAddBinary, "remote-bin", "",
		"Path to sonar on the remote host (default: ~/.local/bin/sonar, then sonar on its PATH; run through sh, so a Windows host needs this set)")
	remoteAddCmd.Flags().BoolVar(&remoteAddJSON, "json", false, "Output as JSON")
	remoteCmd.AddCommand(remoteAddCmd)
}

func runRemoteAdd(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true

	// Two arguments are `<name> <target>`, the form the spec's CLI table and
	// `sonar remote install` both print. One argument is the target alone,
	// with --name as the optional override.
	name, target := remoteAddName, args[0]
	if len(args) == 2 {
		if remoteAddName != "" && remoteAddName != args[0] {
			return fmt.Errorf("the name is given twice: %q as an argument and %q as --name",
				args[0], remoteAddName)
		}
		name, target = args[0], args[1]
	}

	c, err := connectDaemonForRemote(cmd.Context())
	if err != nil {
		return err
	}
	defer c.Close()

	var res rpc.RemoteAddResult
	err = c.Call(cmd.Context(), "remote.add", rpc.RemoteAddParams{
		Target:    target,
		Name:      name,
		SSHArgs:   remoteAddSSHArgs,
		Identity:  remoteAddIdentity,
		Port:      remoteAddPort,
		RemoteBin: remoteAddBinary,
	}, &res)
	if err != nil {
		return cliError(err)
	}

	if remoteAddJSON {
		return writeJSON(res)
	}
	fmt.Fprintf(os.Stdout, "Registered %s as %s.\n",
		res.Host.Address, display.Cyan(res.Host.Name))
	fmt.Fprintf(os.Stdout, "Connecting; `sonar remote list` shows the status.\n")
	return nil
}

// connectDaemonForRemote dials the daemon, starting one if needed. Every
// `sonar remote` subcommand needs it: the bridges live in the daemon, so there
// is nothing a daemon-less path could report.
func connectDaemonForRemote(ctx context.Context) (*client.Client, error) {
	c, err := client.Connect(ctx, client.ClientInfo{Name: "cli", Version: selfupdate.Version})
	if err != nil {
		return nil, fmt.Errorf("remote hosts need a running daemon: %w", err)
	}
	return c, nil
}

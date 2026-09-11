package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/spf13/cobra"
)

// `sonar groups add|rename|remove` are the CLI half of `groups.config.set`
// (contract §13.2, step 5A.4). The daemon owns every write to a `sonar.yaml`,
// so these commands resolve the group to a path and send the edit; nothing here
// touches the file itself. That is the whole point: the desktop app, the MCP
// server and the CLI all go through the same handler, so comments and ordering
// survive an edit whoever made it.
//
// Naming these as subcommands of `sonar groups` costs the three names as
// literal group arguments: `sonar groups add` is the subcommand, so a group
// actually called `add` is inspected with `sonar groups --json` instead.

var (
	groupsAddPort        int
	groupsAddCmdFlag     string
	groupsAddCwd         string
	groupsAddHealth      string
	groupsAddIcon        string
	groupsAddColor       string
	groupsAddDescription string
	groupsAddDependsOn   []string
	groupsEditJSON       bool
)

var groupsAddCmd = &cobra.Command{
	Use:   "add <group> <name>",
	Short: "Add a service to a group's " + groups.ConfigName,
	Long: "Appends a service to the group's " + groups.ConfigName + ". The daemon writes\n" +
		"the file, so its comments and key order survive. A name or a port the\n" +
		"file already declares is refused.",
	Args: cobra.ExactArgs(2),
	RunE: groupsAddRun,
}

var groupsRenameCmd = &cobra.Command{
	Use:   "rename <group> <old> <new>",
	Short: "Rename a service in a group's " + groups.ConfigName,
	Long: "Renames the service everywhere in the file, depends_on references\n" +
		"included, so the file stays valid across the rename.",
	Args: cobra.ExactArgs(3),
	RunE: groupsRenameRun,
}

var groupsRemoveCmd = &cobra.Command{
	Use:     "remove <group> <name>",
	Aliases: []string{"rm"},
	Short:   "Remove a service from a group's " + groups.ConfigName,
	Long: "Removes the service and drops it from every other service's\n" +
		"depends_on, so the file stays valid.",
	Args: cobra.ExactArgs(2),
	RunE: groupsRemoveRun,
}

func init() {
	groupsAddCmd.Flags().IntVar(&groupsAddPort, "port", 0, "The `port` the service listens on")
	groupsAddCmd.Flags().StringVar(&groupsAddCmdFlag, "cmd", "", "The `command` that starts it, for `sonar up`")
	groupsAddCmd.Flags().StringVar(&groupsAddCwd, "cwd", "", "Working `directory` for the command, relative to the file")
	groupsAddCmd.Flags().StringVar(&groupsAddHealth, "health", "", "HTTP `path` the daemon polls while it is up")
	groupsAddCmd.Flags().StringVar(&groupsAddIcon, "icon", "", "Free-form `icon` name for the desktop app")
	groupsAddCmd.Flags().StringVar(&groupsAddColor, "color", "", "Free-form `color` for the desktop app")
	groupsAddCmd.Flags().StringVar(&groupsAddDescription, "description", "", "One-line `description` of the service")
	groupsAddCmd.Flags().StringArrayVar(&groupsAddDependsOn, "depends-on", nil, "A `service` that must start first (repeatable)")
	if err := groupsAddCmd.MarkFlagRequired("port"); err != nil {
		panic(err)
	}

	for _, c := range []*cobra.Command{groupsAddCmd, groupsRenameCmd, groupsRemoveCmd} {
		c.Flags().BoolVar(&groupsEditJSON, "json", false, "Output as JSON")
		groupsCmd.AddCommand(c)
	}
}

func groupsAddRun(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[1])
	if name == "" {
		return fmt.Errorf("the service name is empty")
	}
	return editConfig(cmd, args[0], groups.ConfigEdit{Add: []groups.ServiceAdd{{
		Name:        name,
		Port:        groupsAddPort,
		Cmd:         strings.TrimSpace(groupsAddCmdFlag),
		Cwd:         strings.TrimSpace(groupsAddCwd),
		Health:      strings.TrimSpace(groupsAddHealth),
		Description: strings.TrimSpace(groupsAddDescription),
		Icon:        strings.TrimSpace(groupsAddIcon),
		Color:       strings.TrimSpace(groupsAddColor),
		DependsOn:   groupsAddDependsOn,
	}}}, fmt.Sprintf("added %s to %s", display.Bold(name), args[0]))
}

func groupsRenameRun(cmd *cobra.Command, args []string) error {
	from, to := strings.TrimSpace(args[1]), strings.TrimSpace(args[2])
	if from == "" || to == "" {
		return fmt.Errorf("both the old and the new service name are required")
	}
	return editConfig(cmd, args[0], groups.ConfigEdit{Rename: []groups.ServiceRename{{From: from, To: to}}},
		fmt.Sprintf("%s is now %s in %s", from, display.Bold(to), args[0]))
}

func groupsRemoveRun(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[1])
	if name == "" {
		return fmt.Errorf("the service name is empty")
	}
	return editConfig(cmd, args[0], groups.ConfigEdit{Remove: []string{name}},
		fmt.Sprintf("removed %s from %s", display.Bold(name), args[0]))
}

// editConfig resolves the group to a config path and sends one edit.
func editConfig(cmd *cobra.Command, group string, edit groups.ConfigEdit, done string) error {
	name := strings.TrimSpace(group)
	if name == "" {
		return fmt.Errorf("a group name is required (`sonar groups` lists them)")
	}
	c, err := connectForWrite(cmd.Context())
	if err != nil {
		return err
	}
	defer c.Close()

	path, err := configPathForGroup(cmd.Context(), c, name)
	if err != nil {
		return err
	}

	var res rpc.GroupsConfigSetResult
	if err := c.Call(cmd.Context(), "groups.config.set", rpc.GroupsConfigSetParams{
		Path:     path,
		Add:      edit.Add,
		Rename:   edit.Rename,
		Remove:   edit.Remove,
		Services: edit.Services,
	}, &res); err != nil {
		return daemonError(err)
	}
	if groupsEditJSON {
		return writeJSON(res)
	}
	fmt.Printf("%s (%s)\n", done, shortPath(res.Path))
	return nil
}

// configPathForGroup is the `sonar.yaml` a group name stands for. The daemon
// answers first, because its index is the authority on which files exist. A
// project it has never seen a process in is not in that index, so the working
// directory's own config is the fallback — and only when it carries the name
// that was asked for, so a mistyped group never silently edits the file you
// happen to be standing in.
func configPathForGroup(ctx context.Context, c *client.Client, name string) (string, error) {
	var got rpc.GroupsConfigGetResult
	err := c.Call(ctx, "groups.config.get", rpc.GroupsConfigGetParams{Name: &name}, &got)
	if err == nil {
		return got.Path, nil
	}
	if cfg := localConfig(); cfg != nil && cfg.Name == name {
		return cfg.Path, nil
	}
	return "", daemonError(err)
}

// localConfig is the `sonar.yaml` at or above the working directory, or nil.
func localConfig() *groups.Config {
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}
	index := groups.NewIndex()
	index.Observe(wd)
	return index.Nearest(wd)
}

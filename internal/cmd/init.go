package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

var (
	initForceFlag   bool
	initDryRunFlag  bool
	initMergeFlag   bool
	initServiceFlag []string
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Write a sonar.yaml for this project from what is running now",
	Long: "Proposes a group name and one service per listening port whose process\n" +
		"works inside this repository, and writes it next to .git. Edit and\n" +
		"commit the result, or name the services yourself with --service.",
	Args: cobra.NoArgs,
	RunE: initRun,
}

func init() {
	initCmd.Flags().BoolVar(&initForceFlag, "force", false, "Overwrite an existing "+groups.ConfigName)
	initCmd.Flags().BoolVar(&initMergeFlag, "merge", false, "Append to an existing "+groups.ConfigName+" instead of refusing")
	initCmd.Flags().BoolVar(&initDryRunFlag, "dry-run", false, "Print the proposed file instead of writing it")
	initCmd.Flags().StringArrayVar(&initServiceFlag, "service", nil,
		"Write this service instead of the proposal, as `name:port[:health]` (repeatable)")
	rootCmd.AddCommand(initCmd)
}

func initRun(cmd *cobra.Command, args []string) error {
	if initForceFlag && initMergeFlag {
		return fmt.Errorf("--force and --merge cannot both be given: --force replaces the file, --merge appends to it")
	}
	curated, err := parseInitServices(initServiceFlag)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, _, ok := groups.Find(cwd)
	if !ok {
		root = cwd
		fmt.Fprintf(os.Stderr, "note: %s is not inside a git repository; using it as the project root\n", cwd)
	}
	// An existing config is written in place whatever its spelling, so --merge
	// and --force never leave a second file next to a `.sonar.yaml`.
	target := groups.TargetIn(root)
	_, statErr := os.Lstat(target)
	exists := statErr == nil

	if exists && !initForceFlag && !initMergeFlag && !initDryRunFlag {
		return fmt.Errorf("%s already exists; pass --force to overwrite it or --merge to append to it", target)
	}

	cfg, err := proposeConfig(root, curated)
	if err != nil {
		return err
	}

	if initMergeFlag && exists {
		return initMerge(target, cfg)
	}

	data, err := groups.Marshal(cfg)
	if err != nil {
		return err
	}
	// A curated list is the user's, so it is checked before it is written
	// rather than discovered by the next scan. The pure proposal cannot fail.
	if _, err := groups.Parse(target, data); err != nil {
		return err
	}

	if initDryRunFlag {
		fmt.Print(string(data))
		return nil
	}
	if err := groups.WriteConfigFile(target, data); err != nil {
		return err
	}
	fmt.Printf("wrote %s: group %s with %d %s\n",
		target, cfg.Name, len(cfg.Services), pluralWord(len(cfg.Services), "service"))
	if len(cfg.Services) == 0 {
		fmt.Fprintln(os.Stderr, "note: nothing was listening inside this project, so no services were proposed")
	}
	return nil
}

// proposeConfig is the file this run would write: the proposal built from what
// is listening, with the caller's own service list put in its place when
// --service was given.
func proposeConfig(root string, curated []groups.ServiceAdd) (*groups.Config, error) {
	results, err := ports.Scan()
	if err != nil {
		return nil, err
	}
	docker.EnrichPorts(results)
	ports.Enrich(results)
	results = excludeApps(results)

	_, index := groups.Attribute(results)
	cfg := groups.Propose(root, results, index)
	if len(curated) > 0 {
		cfg = groups.Curate(cfg, curated)
	}
	return cfg, nil
}

// initMerge appends the proposal into a file that is already there. The append
// goes through the node-level edit the daemon uses, so the existing file's
// comments and key order survive and a name or port it already declares is
// refused rather than duplicated.
func initMerge(target string, cfg *groups.Config) error {
	adds := groups.AddsFrom(cfg)
	if len(adds) == 0 {
		return fmt.Errorf("nothing to merge into %s: no services were proposed", target)
	}
	data, merged, err := groups.RenderEdit(target, groups.ConfigEdit{Add: adds})
	if err != nil {
		return err
	}
	if initDryRunFlag {
		fmt.Print(string(data))
		return nil
	}
	if err := groups.WriteConfigFile(merged.Path, data); err != nil {
		return err
	}
	fmt.Printf("merged %d %s into %s: group %s\n",
		len(adds), pluralWord(len(adds), "service"), target, merged.Name)
	return nil
}

// parseInitServices reads the repeatable --service flag. The form is
// `name:port[:health]`: the two things a service needs to be found again, plus
// the health path when there is one. A command and the display metadata are
// added afterwards, with `sonar groups add` or the desktop app.
func parseInitServices(values []string) ([]groups.ServiceAdd, error) {
	out := make([]groups.ServiceAdd, 0, len(values))
	for _, raw := range values {
		// SplitN with 3: a health path may itself contain a colon, and it is
		// the last field, so it keeps whatever is left.
		parts := strings.SplitN(strings.TrimSpace(raw), ":", 3)
		name := strings.TrimSpace(parts[0])
		if name == "" || len(parts) < 2 {
			return nil, fmt.Errorf("--service %q: the form is name:port[:health], for example api:8000:/healthz", raw)
		}
		port, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("--service %q: %q is not a port between 1 and 65535", raw, parts[1])
		}
		svc := groups.ServiceAdd{Name: name, Port: port}
		if len(parts) == 3 {
			svc.Health = strings.TrimSpace(parts[2])
		}
		out = append(out, svc)
	}
	return out, nil
}

func pluralWord(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

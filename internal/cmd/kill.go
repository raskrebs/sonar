package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/killer"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/spf13/cobra"
)

var (
	killPIDFlag        []int
	killGroupFlag      string
	killSessionFlag    string
	killAllFlag        bool
	killFilterFlag     string
	killProjectFlag    string
	killTreeFlag       bool
	forceFlag          bool
	killGraceFlag      time.Duration
	killNoEscalateFlag bool
	killYesFlag        bool
	killDryRunFlag     bool
	killJSONFlag       bool
)

var killCmd = &cobra.Command{
	Use:   "kill [port|pid ...]",
	Short: "Stop what is listening on a port, a pid, a group, or everything",
	Long: `Stop listeners. Docker-published ports are stopped with docker stop; a
listener sonar started is stopped together with its whole process tree.

Examples:
  sonar kill 3000                      # SIGTERM the listener on port 3000
  sonar kill 3000 5432 --force         # SIGKILL both
  sonar kill --pid 12345 --tree        # a process and everything below it
  sonar kill -g sonar                  # a whole group, confirms unless --yes
  sonar kill --session current -y      # everything this agent session started
  sonar kill --all --filter docker -y  # every container publishing a port
  sonar kill --all --project my-app    # one Docker Compose project
  sonar kill 3000 --ip 127.0.0.1       # disambiguate a multi-bind port
  sonar kill 3000 --tree --dry-run     # show the tree, change nothing
  sonar kill 3000 --host hetzner       # on a registered remote host

A positional argument is read as a port; a number no one is listening on that
matches a running process is read as a pid.`,
	ValidArgsFunction: completePort,
	RunE:              runKill,
}

func init() {
	killCmd.Flags().IntSliceVar(&killPIDFlag, "pid", nil, "Kill by process id (repeatable)")
	killCmd.Flags().StringVarP(&killGroupFlag, "group", "g", "", "Kill every port in a group")
	killCmd.Flags().StringVar(&killSessionFlag, "session", "",
		"Stop everything an agent `session` started (`current` is this shell's own)")
	killCmd.Flags().BoolVar(&killAllFlag, "all", false, "Kill every listening port")
	killCmd.Flags().StringVar(&killFilterFlag, "filter", "", "With --all: keep only docker, user or system ports")
	killCmd.Flags().StringVar(&killProjectFlag, "project", "", "With --all: keep only one Docker Compose project")
	killCmd.Flags().BoolVar(&killTreeFlag, "tree", false, "Kill the target's whole process tree, children first")
	killCmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Send SIGKILL instead of SIGTERM")
	killCmd.Flags().DurationVar(&killGraceFlag, "grace", killer.DefaultGrace,
		"How long to wait after SIGTERM before escalating (e.g. 10s)")
	killCmd.Flags().BoolVar(&killNoEscalateFlag, "no-escalate", false, "Never escalate to SIGKILL")
	killCmd.Flags().BoolVarP(&killYesFlag, "yes", "y", false, "Skip the confirmation prompt")
	killCmd.Flags().BoolVar(&killDryRunFlag, "dry-run", false, "Show what would be killed and do nothing")
	killCmd.Flags().String("ip", "", "Bind address, when a port is bound to several")
	killCmd.Flags().BoolVar(&killJSONFlag, "json", false, "Print the result list as JSON")
	addHostFlag(killCmd, "Kill on a registered remote `host` instead of this machine")
	killCmd.MarkFlagsMutuallyExclusive("group", "all")
	killCmd.MarkFlagsMutuallyExclusive("session", "all")
	killCmd.MarkFlagsMutuallyExclusive("session", "group")
	rootCmd.AddCommand(killCmd)
}

func runKill(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	bindIP, _ := cmd.Flags().GetString("ip")

	if killSessionFlag != "" {
		if len(args) > 0 || len(killPIDFlag) > 0 {
			return fmt.Errorf("--session cannot be combined with ports or pids")
		}
		if onRemoteHost() {
			// A session is local daemon state: the agent that started the run
			// is on this machine, and the remote daemon has never heard of it.
			return fmt.Errorf("--session and --host cannot be combined: a session is local daemon state")
		}
		return killSession(cmd.Context())
	}

	// A kill on another machine has no direct path: the bridge to that host is
	// in the daemon, so the daemon does the killing there as well as here.
	if onRemoteHost() {
		c, err := connectForHostWrite(cmd.Context())
		if err != nil {
			return err
		}
		defer c.Close()
		return killThroughDaemon(cmd.Context(), c, args, bindIP)
	}

	// A reachable daemon does the killing, so it rescans straight afterwards
	// and the history ring sees the port go down (contract §22). Without this
	// the daemon's own cache could still show the port for up to CacheTTL after
	// `sonar kill` returned. The connect is dial-only, like the read commands
	// in §20: killing is not a reason to start a daemon.
	if c := daemonClient(cmd.Context()); c != nil {
		defer c.Close()
		return killThroughDaemon(cmd.Context(), c, args, bindIP)
	}
	return killDirect(cmd.Context(), args, bindIP)
}

// killDirect is the no-daemon path: scan here, kill here. It is also what runs
// under --no-daemon and whenever the daemon is down, which is why it stays a
// complete implementation rather than a degraded one.
func killDirect(ctx context.Context, args []string, bindIP string) error {
	snapshot := scanForKill()
	targets, confirm, err := killTargets(args, snapshot, bindIP)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		return reportKill(os.Stdout, nil, snapshot, killJSONFlag, killDryRunFlag)
	}

	opts := killOptions()
	opts.Ports = snapshot

	return killRun(ctx, targets, snapshot, opts,
		confirm && !killYesFlag && !killDryRunFlag, killJSONFlag)
}

// killOptions is the killer configuration the flags describe, shared by both
// paths so the daemon is asked for exactly what the direct path would do.
func killOptions() killer.Options {
	opts := killer.Options{
		Tree:   killTreeFlag,
		Force:  forceFlag,
		Grace:  killGraceFlag,
		DryRun: killDryRunFlag,
	}
	if killNoEscalateFlag {
		off := false
		opts.Escalate = &off
	}
	return opts
}

// killRun is the body every kill-shaped command shares: confirm the plan when
// asked to, run it through the killer, print the outcome. `kill-all` and
// `down` are nothing but different ways of choosing targets for it.
func killRun(ctx context.Context, targets []killer.Target, snapshot []ports.ListeningPort,
	opts killer.Options, confirm, asJSON bool) error {
	// The confirmation lists the plan the killer actually produced, so the
	// prompt shows exactly what will happen — including the whole tree.
	if confirm {
		plan := opts
		plan.DryRun = true
		if !confirmPlan(killer.KillPorts(ctx, targets, plan), snapshot) {
			fmt.Println("Aborted.")
			return nil
		}
	}
	results := killer.KillPorts(ctx, targets, opts)
	return reportKill(os.Stdout, results, snapshot, asJSON, opts.DryRun)
}

// scanForKill takes the enriched scan every selector is resolved against, and
// which the killer reuses instead of scanning a second time. Group attribution
// is part of that enrichment: without it `-g` would only ever see a Compose
// project or a run tag, never a `sonar.yaml` name or a git root.
func scanForKill() []ports.ListeningPort {
	found, err := ports.Scan()
	if err != nil {
		return nil
	}
	docker.EnrichPorts(found)
	ports.Enrich(found)
	groups.Attribute(found)
	return found
}

// killTargets turns the command line into killer targets. The bool reports
// whether the selection is broad enough to confirm before acting.
func killTargets(args []string, snapshot []ports.ListeningPort, bindIP string) ([]killer.Target, bool, error) {
	var targets []killer.Target
	for _, pid := range killPIDFlag {
		targets = append(targets, killer.Target{PID: pid})
	}
	for _, arg := range args {
		n, err := strconv.Atoi(arg)
		if err != nil || n <= 0 {
			return nil, false, fmt.Errorf("invalid port or pid: %s", arg)
		}
		targets = append(targets, positionalTarget(n, snapshot, bindIP))
	}

	switch {
	case killGroupFlag != "":
		if len(targets) > 0 {
			return nil, false, fmt.Errorf("--group cannot be combined with ports or pids")
		}
		members := groupMembers(snapshot, killGroupFlag)
		if len(members) == 0 {
			return nil, false, fmt.Errorf("no listening port belongs to group %q", killGroupFlag)
		}
		return members, true, nil

	case killAllFlag, killFilterFlag != "" || killProjectFlag != "":
		if len(targets) > 0 {
			return nil, false, fmt.Errorf("--all cannot be combined with ports or pids")
		}
		// An empty sweep is not a failure: there was simply nothing to stop.
		members, err := sweepTargets(snapshot, killFilterFlag, killProjectFlag)
		if err != nil {
			return nil, false, err
		}
		return members, true, nil
	}

	if len(targets) == 0 {
		return nil, false, fmt.Errorf("nothing to kill: give a port or pid, or use --pid, --group or --all")
	}
	return targets, false, nil
}

// positionalTarget reads a bare number as a port, falling back to a pid when
// nothing is listening on it but a process with that id exists.
func positionalTarget(n int, snapshot []ports.ListeningPort, bindIP string) killer.Target {
	for _, p := range snapshot {
		if p.Port == n {
			return killer.Target{Port: n, BindAddress: bindIP}
		}
	}
	if n > 65535 || killer.Alive(n) {
		return killer.Target{PID: n}
	}
	return killer.Target{Port: n, BindAddress: bindIP}
}

// groupMembers resolves a group name against the scan. The scanner's `group`
// field is the primary signal; a `sonar run` tag or id and a Docker Compose
// project name are accepted too, so the flag works for everything that is
// grouped today.
func groupMembers(snapshot []ports.ListeningPort, name string) []killer.Target {
	var out []killer.Target
	for _, p := range snapshot {
		if inGroup(p, name) {
			out = append(out, killer.Target{Port: p.Port, BindAddress: p.BindAddress})
		}
	}
	return out
}

func inGroup(p ports.ListeningPort, name string) bool {
	for _, candidate := range []string{p.Group, p.Tag, p.RunID, p.DockerComposeProject} {
		if candidate != "" && strings.EqualFold(candidate, name) {
			return true
		}
	}
	return false
}

// sweepTargets is `--all` with its optional narrowing, shared with the
// `kill-all` alias. Desktop apps are never swept up: they are hidden from
// `sonar list` for the same reason.
func sweepTargets(snapshot []ports.ListeningPort, filter, project string) ([]killer.Target, error) {
	selected := excludeApps(snapshot)
	if filter != "" {
		if err := validateKillFilter(filter); err != nil {
			return nil, err
		}
		selected = display.FilterPorts(selected, filter)
	}
	if project != "" {
		selected = filterByProject(selected, project)
	}
	var out []killer.Target
	for _, p := range selected {
		out = append(out, killer.Target{Port: p.Port, BindAddress: p.BindAddress})
	}
	return out, nil
}

func validateKillFilter(filter string) error {
	switch strings.ToLower(filter) {
	case "docker", "user", "system":
		return nil
	}
	return fmt.Errorf("unknown filter %q (available: docker, user, system)", filter)
}

// filterByProject keeps the ports of one Docker Compose project.
func filterByProject(pp []ports.ListeningPort, project string) []ports.ListeningPort {
	var result []ports.ListeningPort
	for _, p := range pp {
		if strings.EqualFold(p.DockerComposeProject, project) {
			result = append(result, p)
		}
	}
	return result
}

// confirmPlan prints the planned actions and asks before taking them.
func confirmPlan(plan []killer.Result, snapshot []ports.ListeningPort) bool {
	fmt.Printf("Will stop %d process(es):\n", len(plan))
	for _, r := range plan {
		fmt.Printf("  - %s\n", describeResult(r, snapshot))
	}
	fmt.Print("\nProceed? [y/N] ")
	var answer string
	fmt.Scanln(&answer)
	return strings.EqualFold(strings.TrimSpace(answer), "y")
}

// reportKill prints the outcome and returns a non-nil error when any target
// failed, so the command exits 1 (daemon spec, "Error handling").
func reportKill(w io.Writer, results []killer.Result, snapshot []ports.ListeningPort, asJSON, dryRun bool) error {
	failed := 0
	for _, r := range results {
		if !r.OK {
			failed++
		}
	}

	if asJSON {
		if err := writeKillJSON(w, results); err != nil {
			return err
		}
		if failed > 0 {
			return errSilent
		}
		return nil
	}

	if len(results) == 0 {
		fmt.Fprintln(w, "Nothing to stop.")
		return nil
	}
	if dryRun {
		fmt.Fprintf(w, "Dry run: %d action(s), children first.\n", len(results))
	}
	for _, r := range results {
		if r.OK {
			fmt.Fprintf(w, "%s %s\n", killVerb(r, dryRun), describeResult(r, snapshot))
			continue
		}
		fmt.Fprintf(w, "error: %s\n", r.Error)
	}
	if !dryRun {
		for _, url := range freedURLs(results, snapshot) {
			fmt.Fprintf(w, "Freed %s\n", display.Underline(url))
		}
		fmt.Fprintf(w, "\n%d/%d stopped.\n", len(results)-failed, len(results))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d targets failed", failed, len(results))
	}
	return nil
}

// killVerb is the leading word of a result line: what was done, or — in a dry
// run — what would be.
func killVerb(r killer.Result, dryRun bool) string {
	if dryRun {
		return "would " + string(r.Method)
	}
	return string(r.Method)
}

// describeResult names the process or container a row acted on.
func describeResult(r killer.Result, snapshot []ports.ListeningPort) string {
	name := r.Name
	if name == "" {
		name = "unknown"
	}
	var b strings.Builder
	b.WriteString(display.Bold(name))
	if r.PID > 0 {
		fmt.Fprintf(&b, " (PID %d)", r.PID)
	}
	if r.Port > 0 {
		fmt.Fprintf(&b, " on port %d", r.Port)
		if bindAmbiguous(snapshot, r.Port) && r.BindAddress != "" {
			fmt.Fprintf(&b, " [%s]", r.BindAddress)
		}
	}
	if r.Method == state.MethodDockerStop {
		b.WriteString(" (container)")
	}
	return b.String()
}

// bindAmbiguous reports whether a port number appears more than once in the
// scan, in which case the bind address is worth printing.
func bindAmbiguous(snapshot []ports.ListeningPort, port int) bool {
	seen := 0
	for _, p := range snapshot {
		if p.Port == port {
			seen++
		}
	}
	return seen > 1
}

// freedURLs lists the distinct URLs of the ports that were successfully acted
// on, in the order they appear in the results.
func freedURLs(results []killer.Result, snapshot []ports.ListeningPort) []string {
	byKey := map[string]string{}
	for i := range snapshot {
		byKey[snapshot[i].PortKey()] = snapshot[i].URL()
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range results {
		if !r.OK || r.Port == 0 {
			continue
		}
		key := r.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		if url, ok := byKey[key]; ok {
			out = append(out, url)
		}
	}
	return out
}

// writeKillJSON prints the contract §3 result list.
func writeKillJSON(w io.Writer, results []killer.Result) error {
	if results == nil {
		results = []killer.Result{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

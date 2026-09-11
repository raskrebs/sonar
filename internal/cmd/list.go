package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/groups"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/spf13/cobra"
)

var (
	jsonFlag       bool
	filterFlag     string
	sortFlag       string
	allFlag        bool
	columnsFlag    string
	allColumnsFlag bool
	healthFlag     bool
	hostFlag       string
	statsFlag      bool
	ipv4Flag       bool
	ipv6Flag       bool
	groupFlag      string
	sessionFlag    string
	tagFlag        string
	treeFlag       bool
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all processes listening on localhost TCP ports",
	RunE:  listRun,
}

func init() {
	listCmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	listCmd.Flags().StringVar(&filterFlag, "filter", "", "Filter by type: docker, user, system")
	listCmd.Flags().StringVar(&sortFlag, "sort", "port", "Sort by: port, pid, name, type")
	listCmd.Flags().BoolVarP(&allFlag, "all", "a", false, "Include desktop apps (hidden by default)")
	listCmd.Flags().StringVarP(&columnsFlag, "columns", "c", "",
		"Columns to display (comma-separated: "+strings.Join(display.AllColumns, ", ")+")")
	listCmd.Flags().BoolVar(&allColumnsFlag, "all-columns", false, "Display all available columns")
	listCmd.Flags().BoolVar(&healthFlag, "health", false, "Run HTTP health checks on each port")
	listCmd.Flags().BoolVar(&statsFlag, "stats", false, "Include resource stats (CPU, memory, threads, uptime, state)")
	listCmd.Flags().StringVar(&hostFlag, "host", "",
		"Read a registered `host` through the daemon, \"*\" for every host, or scan any user@hostname over SSH")
	_ = listCmd.RegisterFlagCompletionFunc("host",
		func(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			// Only `list` reads every host at once, so only `list` offers it.
			return append([]string{allHosts}, remoteHostNames(cmd.Context())...),
				cobra.ShellCompDirectiveNoFileComp
		})
	listCmd.Flags().BoolVarP(&ipv4Flag, "ipv4", "4", false, "Show only IPv4 ports")
	listCmd.Flags().BoolVarP(&ipv6Flag, "ipv6", "6", false, "Show only IPv6 ports")
	listCmd.Flags().StringVar(&groupFlag, "group", "", "Show only ports in this `group`")
	listCmd.Flags().StringVar(&sessionFlag, "session", "",
		"Show only the ports an agent `session` started (`current` is this shell's own)")
	listCmd.Flags().StringVar(&tagFlag, "tag", "", "Alias of --group, kept for one release")
	listCmd.Flags().BoolVar(&treeFlag, "tree", false, "Group the ports into a tree instead of a table")
	listCmd.MarkFlagsMutuallyExclusive("ipv4", "ipv6")
	rootCmd.AddCommand(listCmd)
}

func listRun(cmd *cobra.Command, args []string) error {
	// Resolve row-affecting settings (config fills in where no flag was passed).
	cfg := loadedConfig
	showApps := effectiveBool(cmd.Flags().Changed("all"), allFlag, cfg.List.All)
	activeFilter := effectiveString(cmd.Flags().Changed("filter"), filterFlag, cfg.List.Filter)
	group := groupFlag
	if group == "" {
		group = tagFlag
	}
	if tagFlag != "" {
		Hint(cmd, HintTagToGroup("list", tagFlag))
	}
	ipVersion := ""
	if ipv4Flag {
		ipVersion = "IPv4"
	} else if ipv6Flag {
		ipVersion = "IPv6"
	}

	session, err := currentSession(sessionFlag)
	if err != nil {
		return err
	}

	results, index, err := listPorts(cmd.Context(), listQuery{
		showApps:  showApps,
		filter:    activeFilter,
		group:     group,
		session:   session,
		ipVersion: ipVersion,
	})
	if err != nil {
		return err
	}

	if jsonFlag {
		return display.RenderJSON(os.Stdout, results)
	}

	if treeFlag {
		if index == nil {
			index = observeConfigs(results)
		}
		display.RenderTree(os.Stdout, results, groups.Groups(state.FromListeningAll(results), index))
		return nil
	}

	sortBy := effectiveString(cmd.Flags().Changed("sort"), sortFlag, cfg.List.Sort)
	columns := effectiveColumns(allColumnsFlag, columnsFlag, statsFlag, cfg.List.Columns)

	display.RenderTable(os.Stdout, results, display.TableOptions{
		SortBy:  sortBy,
		Columns: columns,
	})

	if hostFlag == "" && hasHiddenProcesses(results) {
		fmt.Fprintln(os.Stderr, "\nnote: some processes are hidden — re-run with sudo for full visibility")
	}
	return nil
}

// errSessionNeedsDaemon is what --session says when nothing is listening on
// the daemon socket.
var errSessionNeedsDaemon = errors.New(
	"--session needs a running daemon; start one with `sonar serve --detach`")

// hasHiddenProcesses reports whether the scan ran unprivileged and at least one
// listening socket came back without a resolvable process name — the signature
// of the OS withholding process info for sockets owned by other users.
func hasHiddenProcesses(pp []ports.ListeningPort) bool {
	// Geteuid returns -1 on Windows; the >0 check naturally excludes it and root.
	if os.Geteuid() <= 0 {
		return false
	}
	for _, p := range pp {
		if p.DisplayName() == "" {
			return true
		}
	}
	return false
}

func parseColumns(s string) []string {
	parts := strings.Split(s, ",")
	var cols []string
	for _, p := range parts {
		c := strings.TrimSpace(strings.ToLower(p))
		if c != "" {
			cols = append(cols, c)
		}
	}
	return cols
}

// effectiveString applies precedence: explicit flag > config value > flag default.
func effectiveString(flagChanged bool, flagVal, cfgVal string) string {
	if !flagChanged && cfgVal != "" {
		return cfgVal
	}
	return flagVal
}

// effectiveBool applies precedence: explicit flag > config value > flag default.
func effectiveBool(flagChanged bool, flagVal bool, cfgVal *bool) bool {
	if !flagChanged && cfgVal != nil {
		return *cfgVal
	}
	return flagVal
}

// effectiveColumns resolves the column list. Returns nil when no flag, config,
// or stats override applies, so RenderTable falls back to its built-in defaults
// (preserving current behavior when no config is present).
func effectiveColumns(allCols bool, columnsFlag string, stats bool, cfgCols []string) []string {
	switch {
	case allCols:
		return display.AllColumns
	case columnsFlag != "":
		return parseColumns(columnsFlag)
	}
	if stats {
		base := display.DefaultColumns
		if len(cfgCols) > 0 {
			base = cfgCols
		}
		out := append([]string{}, base...)
		return append(out, "cpu", "mem", "state", "uptime", "connections")
	}
	// nil when no config columns → RenderTable falls back to its defaults.
	return cfgCols
}

func excludeApps(pp []ports.ListeningPort) []ports.ListeningPort {
	var result []ports.ListeningPort
	for _, p := range pp {
		if !p.IsApp {
			result = append(result, p)
		}
	}
	return result
}

// filterByGroup keeps only ports in one group. A `sonar run` tag and a run id
// still match, so the old --tag spelling keeps working while it is an alias.
func filterByGroup(pp []ports.ListeningPort, group string) []ports.ListeningPort {
	var out []ports.ListeningPort
	for _, p := range pp {
		if p.Group == group || p.Tag == group || p.RunID == group {
			out = append(out, p)
		}
	}
	return out
}

func filterByIPVersion(pp []ports.ListeningPort, ver string) []ports.ListeningPort {
	var out []ports.ListeningPort
	for _, p := range pp {
		if p.IPVersion == ver {
			out = append(out, p)
		}
	}
	return out
}

// ValidateColumns checks that all column names are valid.
func ValidateColumns(cols []string) error {
	valid := make(map[string]bool)
	for _, c := range display.AllColumns {
		valid[c] = true
	}
	for _, c := range cols {
		if !valid[c] {
			return fmt.Errorf("unknown column %q (available: %s)", c, strings.Join(display.AllColumns, ", "))
		}
	}
	return nil
}

// listQuery is what `sonar list` asks for, already resolved from flags and
// config, so the daemon and the direct path filter by the same values.
type listQuery struct {
	showApps  bool
	filter    string
	group     string
	session   string
	ipVersion string
}

// listPorts returns the rows to render. It reads through the daemon when one is
// reachable (autostarting it, contract §7) and falls back to a direct scan with
// a one-line note otherwise. The group index is only built on the direct path;
// through the daemon the tree view derives what it needs from the rows.
func listPorts(ctx context.Context, q listQuery) ([]ports.ListeningPort, *groups.Index, error) {
	if hostFlag != "" && q.session != "" {
		return nil, nil, errors.New("--session and --host cannot be combined: a session is local daemon state")
	}
	if hostFlag == allHosts {
		rows, err := listEveryHost(ctx)
		if err != nil {
			return nil, nil, err
		}
		return applyListFilters(rows, q), nil, nil
	}
	if hostFlag != "" {
		// A registered host has a daemon and a bridge, so its ports come back
		// through the local daemon already attributed, named and grouped. The
		// SSH scan below is the fallback for a host that has neither.
		if c := routeRemote(ctx, hostFlag); c != nil {
			defer c.Close()
			// `host` on the method itself: the local daemon forwards the read
			// over that host's bridge and hands the rows back tagged with it
			// (contract §43).
			rows, err := daemonList(ctx, c, rpc.PortsListParams{
				HostParams: rpc.HostParams{Host: hostFlag},
				Group:      strPtrOrNil(q.group),
				Filter:     strPtrOrNil(q.filter),
				All:        q.showApps,
				IPVersion:  strPtrOrNil(q.ipVersion),
				Include:    listInclude(statsFlag, healthFlag),
			})
			if err != nil {
				return nil, nil, cliError(err)
			}
			return rows, nil, nil
		}

		// A remote host with no daemon is scanned over SSH; the local daemon
		// knows nothing about it.
		results, err := ports.ScanRemote(hostFlag)
		if err != nil {
			return nil, nil, err
		}
		for i := range results {
			results[i].Type = ports.ClassifyPort(results[i].Port)
		}
		return applyListFilters(results, q), nil, nil
	}

	if c := daemonClient(ctx); c != nil {
		defer c.Close()
		rows, err := daemonList(ctx, c, rpc.PortsListParams{
			Group:     strPtrOrNil(q.group),
			Session:   strPtrOrNil(q.session),
			Filter:    strPtrOrNil(q.filter),
			All:       q.showApps,
			IPVersion: strPtrOrNil(q.ipVersion),
			Include:   listInclude(statsFlag, healthFlag),
		})
		if err == nil {
			return rows, nil, nil
		}
		// A daemon that answered with an error is a real failure, not a
		// reason to scan twice.
		return nil, nil, err
	}

	if q.session != "" {
		// A session is daemon state: it lives in the run registry and the
		// database, and a direct scan has no way to know one existed. Saying
		// so beats printing an empty table (contract §20).
		return nil, nil, errSessionNeedsDaemon
	}

	results, err := ports.Scan()
	if err != nil {
		return nil, nil, err
	}
	docker.EnrichPorts(results)
	ports.Enrich(results)
	if statsFlag {
		ports.EnrichStats(results, docker.AllContainerStatsAsEntries())
	}
	if healthFlag {
		ports.EnrichHealth(results, 2*time.Second, 0)
	}
	// Resolve every port's group: pin > run > sonar.yaml > Compose > git root.
	// This is the no-daemon path, so it happens per command.
	_, index := groups.Attribute(results)
	return applyListFilters(results, q), index, nil
}

// allHosts is what `--host "*"` spells: every machine sonar knows about, this
// one included.
const allHosts = "*"

// listEveryHost reads the whole multiplexed table. It goes through
// `state.snapshot {hosts: ["*"]}` rather than a read per host because that is
// the one call that already means "every machine": the daemon has the rows
// from every bridge in hand, tagged and keyed, and does not have to go back
// over the network for them.
func listEveryHost(ctx context.Context) ([]ports.ListeningPort, error) {
	if noDaemonFlag {
		return nil, errors.New(`--host "*" needs the daemon: the other hosts are connected from there`)
	}
	c, err := dialDaemon(ctx)
	if err != nil {
		return nil, fmt.Errorf(`--host "*" needs the daemon: %w`, err)
	}
	defer c.Close()

	var snap state.Snapshot
	if err := c.Call(ctx, "state.snapshot", rpc.StateSnapshotParams{
		Hosts:   []string{allHosts},
		Include: listInclude(statsFlag, healthFlag),
	}, &snap); err != nil {
		return nil, cliError(err)
	}
	return state.ToListeningAll(snap.Ports), nil
}

// applyListFilters is the direct path's copy of what the daemon does to
// ports.list params. The two must agree: spec integration test 6 compares them.
func applyListFilters(results []ports.ListeningPort, q listQuery) []ports.ListeningPort {
	if !q.showApps {
		results = excludeApps(results)
	}
	if q.filter != "" {
		results = display.FilterPorts(results, q.filter)
	}
	if q.ipVersion != "" {
		results = filterByIPVersion(results, q.ipVersion)
	}
	if q.group != "" {
		results = filterByGroup(results, q.group)
	}
	return results
}

// observeConfigs builds the group index the tree view needs from rows the
// daemon resolved. It only looks for `sonar.yaml` files — the group each port
// belongs to already came off the wire — so the tree shows the same service
// names and roots either way.
func observeConfigs(rows []ports.ListeningPort) *groups.Index {
	index := groups.NewIndex()
	if wd, err := os.Getwd(); err == nil {
		index.Observe(wd)
	}
	for i := range rows {
		index.Observe(rows[i].Cwd)
	}
	return index
}

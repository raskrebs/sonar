package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
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
	listCmd.Flags().StringVar(&hostFlag, "host", "", "Scan a remote host via SSH (e.g. user@hostname)")
	listCmd.Flags().BoolVarP(&ipv4Flag, "ipv4", "4", false, "Show only IPv4 ports")
	listCmd.Flags().BoolVarP(&ipv6Flag, "ipv6", "6", false, "Show only IPv6 ports")
	listCmd.MarkFlagsMutuallyExclusive("ipv4", "ipv6")
	rootCmd.AddCommand(listCmd)
}

func listRun(cmd *cobra.Command, args []string) error {
	var results []ports.ListeningPort
	var err error

	if hostFlag != "" {
		results, err = ports.ScanRemote(hostFlag)
		if err != nil {
			return err
		}
		// Classify port types only; Docker and process stats are not available over SSH
		for i := range results {
			results[i].Type = ports.ClassifyPort(results[i].Port)
		}
	} else {
		results, err = ports.Scan()
		if err != nil {
			return err
		}
		docker.EnrichPorts(results)
		ports.Enrich(results)
		if statsFlag {
			ports.EnrichStats(results, docker.AllContainerStatsAsEntries())
		}
		if healthFlag {
			ports.EnrichHealth(results, 2*time.Second)
		}
	}

	// Resolve row-affecting settings (config fills in where no flag was passed).
	cfg := loadedConfig
	showApps := effectiveBool(cmd.Flags().Changed("all"), allFlag, cfg.List.All)
	activeFilter := effectiveString(cmd.Flags().Changed("filter"), filterFlag, cfg.List.Filter)

	if !showApps {
		results = excludeApps(results)
	}
	if activeFilter != "" {
		results = display.FilterPorts(results, activeFilter)
	}

	if ipv4Flag {
		results = filterByIPVersion(results, "IPv4")
	} else if ipv6Flag {
		results = filterByIPVersion(results, "IPv6")
	}

	if jsonFlag {
		return display.RenderJSON(os.Stdout, results)
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

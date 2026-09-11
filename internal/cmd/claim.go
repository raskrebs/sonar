package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/raskrebs/sonar/internal/claims"
	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/spf13/cobra"
)

var (
	claimProject  string
	claimWorktree string
	claimCount    int
	claimTTLFlag  string
	claimRelease  bool
	claimListFlag bool
	claimJSON     bool
	claimsJSON    bool
)

var claimCmd = &cobra.Command{
	Use:   "claim",
	Short: "Reserve ports for this project and worktree",
	Long: `Reserve ports for this project and worktree.

The same worktree gets the same ports every time: the ports are derived from
the project and worktree names, then probed upward past anything that is
listening or claimed by someone else. Parallel agents in sibling worktrees
therefore never pick the same port, and ` + "`sonar next`" + ` steps over every claim
but your own.

Project and worktree default to the git checkout containing the current
directory. A claim is sonar's book-keeping, not an OS-level bind: a process
outside sonar can still take the port.

Examples:
  sonar claim                 # one port for this worktree
  sonar claim --count 3       # three
  sonar claim --ttl 2h        # expire sooner than the 24h default
  sonar claim --release       # give this worktree's ports back
  sonar claim --list --json   # every live claim on this machine`,
	Args: cobra.NoArgs,
	RunE: claimRun,
}

// `sonar claims` is the read-only half, as named in spec 2 §4.
var claimsCmd = &cobra.Command{
	Use:   "claims",
	Short: "List live port claims",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := connectForWrite(cmd.Context())
		if err != nil {
			return err
		}
		defer c.Close()
		return listClaims(cmd.Context(), c, claimsJSON)
	},
}

func init() {
	claimCmd.Flags().StringVar(&claimProject, "project", "", "Project name (default: the git checkout's name)")
	claimCmd.Flags().StringVar(&claimWorktree, "worktree", "", "Worktree name (default: this checkout's, or main)")
	claimCmd.Flags().IntVarP(&claimCount, "count", "n", 0, "How many ports to claim (default: the project's worktree_ports from .sonar.yaml, else 1)")
	claimCmd.Flags().StringVar(&claimTTLFlag, "ttl", "24h", "How long the claim lives (e.g. 2h, 30m)")
	claimCmd.Flags().BoolVar(&claimRelease, "release", false, "Release this key's ports instead of claiming")
	claimCmd.Flags().BoolVar(&claimListFlag, "list", false, "List live claims instead of claiming")
	claimCmd.Flags().BoolVar(&claimJSON, "json", false, "Output as JSON")
	claimsCmd.Flags().BoolVar(&claimsJSON, "json", false, "Output as JSON")
	rootCmd.AddCommand(claimCmd)
	rootCmd.AddCommand(claimsCmd)
}

func claimRun(cmd *cobra.Command, _ []string) error {
	ttl, err := claimTTL()
	if err != nil {
		return err
	}
	// An unset --count sends no count, so the daemon can apply the project's
	// worktree_ports; only a count the user typed is checked here.
	if cmd.Flags().Changed("count") && claimCount < 1 {
		return fmt.Errorf("--count must be at least 1")
	}

	c, err := connectForWrite(cmd.Context())
	if err != nil {
		return err
	}
	defer c.Close()

	if claimListFlag {
		return listClaims(cmd.Context(), c, claimJSON)
	}

	project, worktree := claimIdentity()
	key := claims.Key(project, worktree)

	if claimRelease {
		var res rpc.ClaimsReleaseResult
		if err := c.Call(cmd.Context(), "claims.release",
			rpc.ClaimsReleaseParams{Key: key}, &res); err != nil {
			return daemonError(err)
		}
		if claimJSON {
			return writeJSON(struct {
				Key      string `json:"key"`
				Released int    `json:"released"`
			}{key, res.Released})
		}
		fmt.Printf("released %d port(s) held by %s\n", res.Released, key)
		return nil
	}

	res, err := acquireClaim(cmd.Context(), c, rpc.ClaimsAcquireParams{
		Project:    project,
		Worktree:   worktree,
		Count:      claimCount,
		TTLSeconds: int64(ttl / time.Second),
	})
	if err != nil {
		return err
	}
	return printClaim(project, worktree, res, claimJSON)
}

// acquireClaim is the one call `sonar claim` and `sonar next --claim` share.
func acquireClaim(ctx context.Context, c *client.Client, params rpc.ClaimsAcquireParams) (rpc.ClaimsAcquireResult, error) {
	var res rpc.ClaimsAcquireResult
	if err := c.Call(ctx, "claims.acquire", params, &res); err != nil {
		return res, daemonError(err)
	}
	return res, nil
}

func printClaim(project, worktree string, res rpc.ClaimsAcquireResult, asJSON bool) error {
	if asJSON {
		return writeJSON(struct {
			Key       string `json:"key"`
			Project   string `json:"project"`
			Worktree  string `json:"worktree"`
			Ports     []int  `json:"ports"`
			ExpiresAt string `json:"expires_at"`
		}{res.Key, project, worktree, res.Ports, res.ExpiresAt})
	}
	for _, p := range res.Ports {
		fmt.Println(p)
	}
	return nil
}

func listClaims(ctx context.Context, c *client.Client, asJSON bool) error {
	var res rpc.ClaimsListResult
	if err := c.Call(ctx, "claims.list", struct{}{}, &res); err != nil {
		return daemonError(err)
	}
	if asJSON {
		if res.Claims == nil {
			res.Claims = []state.Claim{}
		}
		return writeJSON(res)
	}
	if len(res.Claims) == 0 {
		fmt.Println("no ports are claimed")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "%s\t%s\t%s\n", display.Dim("KEY"), display.Dim("PORTS"), display.Dim("EXPIRES"))
	for _, cl := range res.Claims {
		fmt.Fprintf(w, "%s\t%s\t%s\n", cl.Key, joinPorts(cl.Ports), expiresIn(cl.ExpiresAt))
	}
	return w.Flush()
}

func joinPorts(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprint(p))
	}
	return strings.Join(parts, ",")
}

// expiresIn prints the remaining life of a claim, which is what a human is
// actually asking when they list claims.
func expiresIn(at string) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return at
	}
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	return "in " + d.Round(time.Minute).String()
}

func claimTTL() (time.Duration, error) {
	ttl, err := time.ParseDuration(strings.TrimSpace(claimTTLFlag))
	if err != nil {
		return 0, fmt.Errorf("--ttl %q is not a duration (try 24h, 90m)", claimTTLFlag)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("--ttl must be positive")
	}
	return ttl, nil
}

// claimIdentity derives the project and worktree from the current directory:
// the git checkout's name, and the worktree name for a linked worktree (spec 2
// §4). The flags win over both. The derivation itself lives in internal/claims
// so `sonar claim` and the `claim_port` MCP tool compute the same key.
func claimIdentity() (project, worktree string) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	return claims.Identity(cwd, claimProject, claimWorktree)
}

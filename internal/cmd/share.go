package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/raskrebs/sonar/internal/daemon"
	"github.com/raskrebs/sonar/internal/daemon/client"
	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/selfupdate"
	"github.com/raskrebs/sonar/internal/state"
	"github.com/spf13/cobra"

	// The share namespace registers its handlers from its own init(), the way
	// every other namespace is pulled in by the command that fronts it.
	_ "github.com/raskrebs/sonar/internal/share"
)

// `sonar share` — the whole of the feature's CLI surface.
//
// One verb, and the reach is always written down. `sonar share 3000` with no
// reach is an error and not a default: "everyone on this wifi" and "the entire
// internet" are far enough apart that nobody should arrive at the second by
// forgetting a flag, and no wording of a default makes that safe
// (sonar-relay/docs/SHARE.md).
//
// This replaces the never-shipped `expose`. It is not a fifty-first command:
// the daemon owns the share, so there is no `sonar unshare`, no `sonar shares`
// and — the one that matters most — no `sonar login`. Signing in is a step of
// the thing you asked for, which is the only way in on a headless box reached
// over SSH.

var (
	sharePublic  bool
	shareLAN     bool
	shareTTL     string
	shareReplace bool
	shareStop    bool
	shareList    bool
	shareJSON    bool
)

var shareCmd = &cobra.Command{
	Use:   "share [port]",
	Short: "Make a local service reachable from somewhere else",
	Long: "Publishes a service running on this machine at a URL you can send someone.\n\n" +
		"The reach is always explicit — there is no default:\n" +
		"  --public   anyone with the link, over the internet\n" +
		"  --lan      everyone on this network (not built yet)\n\n" +
		"A public share needs a sonar account. If this machine is not signed in,\n" +
		"sonar starts the sign-in here and finishes the share afterwards; it works\n" +
		"over SSH, and there is no separate login command.\n\n" +
		"The daemon holds the share, so this command prints the URL and exits.\n" +
		"The share ends when the service stops, or when the TTL runs out.",
	Args: cobra.MaximumNArgs(1),
	// The messages here are the feature: the reach refusal and the limit offer
	// are both several lines meant to be read. Cobra printing them a second
	// time above the usage block would bury them.
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runShare,
}

func init() {
	shareCmd.Flags().BoolVar(&sharePublic, "public", false,
		"Reachable by anyone with the link, over the internet")
	shareCmd.Flags().BoolVar(&shareLAN, "lan", false,
		"Reachable by everyone on this network (not built yet)")
	shareCmd.Flags().StringVar(&shareTTL, "ttl", "",
		`How long the share lives: "while-it-runs" (default), "1h" or "24h"`)
	shareCmd.Flags().BoolVar(&shareReplace, "replace", false,
		"Stop whatever is already shared and share this instead")
	shareCmd.Flags().BoolVar(&shareStop, "stop", false,
		"Stop the share on this port, or every share with no port")
	shareCmd.Flags().BoolVar(&shareList, "list", false,
		"List the shares this machine is holding")
	shareCmd.Flags().BoolVar(&shareJSON, "json", false, "Output JSON")
	rootCmd.AddCommand(shareCmd)
}

func runShare(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	switch {
	case shareList:
		return shareListRun(ctx)
	case shareStop:
		return shareStopRun(ctx, args)
	}

	if len(args) == 0 {
		return errors.New("which port? try `sonar share 3000 --public`")
	}
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%q is not a port", args[0])
	}

	reach, err := reachFrom()
	if err != nil {
		return err
	}
	ttl, err := ttlFrom(shareTTL)
	if err != nil {
		return err
	}

	c, err := shareDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	params := rpc.ShareCreateParams{
		Target:  rpc.Selector{Port: &port},
		Reach:   reach,
		Replace: shareReplace,
	}
	if ttl != "" {
		params.TTL = &ttl
	}

	// Offer the whole project before anything is published. Asking first is
	// the point: publishing the one service and then offering to replace it
	// would spend an address on a share nobody asked for.
	if wantsProject(ctx, c, port) {
		params.Project = true
	}

	res, err := shareCreate(ctx, c, params)
	if err != nil {
		return err
	}
	if shareJSON {
		printJSON(res)
		return nil
	}
	printShare(res.Share, port)
	printNotes(res.Notes)
	return nil
}

// reachFrom is the whole of the "no default" rule.
func reachFrom() (string, error) {
	switch {
	case sharePublic && shareLAN:
		return "", errors.New("--public and --lan are two different things; pick one")
	case sharePublic:
		return "public", nil
	case shareLAN:
		return "lan", nil
	}
	return "", errors.New(
		"say how far this should reach:\n" +
			"  sonar share <port> --lan      everyone on this wifi\n" +
			"  sonar share <port> --public   anyone with the link\n\n" +
			"there is no default — the two are too far apart to pick by accident")
}

func ttlFrom(raw string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "":
		return "", nil
	case "while-it-runs", "while_it_runs", "run", "running":
		return "while_it_runs", nil
	case "1h", "1hour", "60m":
		return "1h", nil
	case "24h", "1d", "24hour":
		return "24h", nil
	}
	return "", fmt.Errorf("%q is not a ttl; use while-it-runs, 1h or 24h", raw)
}

// shareCreate is `share.create` plus the two answers that are not failures:
// a machine that is not signed in, and an account already sharing something
// else.
func shareCreate(ctx context.Context, c *client.Client, params rpc.ShareCreateParams) (rpc.ShareCreateResult, error) {
	var res rpc.ShareCreateResult
	err := c.Call(ctx, "share.create", params, &res)
	if err == nil {
		return res, nil
	}

	switch codeOf(err) {
	case rpc.CodeNotSignedIn:
		// Sign-in is a step of the thing you asked for, not a command of its
		// own. This is the only way in on a headless box.
		if err := signInHere(ctx, c); err != nil {
			return res, err
		}
		if err := c.Call(ctx, "share.create", params, &res); err != nil {
			return res, err
		}
		return res, nil

	case rpc.CodeShareLimitReached:
		agreed, err := offerToMove(err)
		if err != nil {
			return res, err
		}
		if !agreed {
			return res, errSilent
		}
		params.Replace = true
		if err := c.Call(ctx, "share.create", params, &res); err != nil {
			return res, err
		}
		return res, nil
	}
	return res, err
}

// offerToMove turns `share_limit_reached` into the question SHARE.md insists it
// should be. A hard failure on the only interesting action in the product is
// where people leave.
func offerToMove(err error) (bool, error) {
	live := shareIn(err)
	where := "your other share"
	if live != nil {
		where = describeShare(*live)
	}
	fmt.Fprintf(os.Stderr, "You're already sharing %s.\n", where)

	if shareReplace {
		// --replace was already asked for and the relay still refused, so
		// there is nothing left to offer.
		return false, err
	}
	if !stdinIsTerminal() {
		fmt.Fprintln(os.Stderr, "Re-run with --replace to stop that one and share this instead.")
		return false, errSilent
	}
	fmt.Fprint(os.Stderr, "Stop that and share this instead? [y/N] ")
	line, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
	if readErr != nil {
		return false, errSilent
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, errSilent
}

func describeShare(s state.Share) string {
	what := ""
	if s.TargetService != nil {
		what = *s.TargetService
	}
	if what == "" && s.TargetGroup != nil {
		what = *s.TargetGroup
	}
	if what == "" && s.TargetPort > 0 {
		// A share on the fallback key has no service and no group to name, so
		// the port is what the person will recognise. "a service" tells them
		// nothing about which of their shares is about to be moved.
		what = fmt.Sprintf("localhost:%d", s.TargetPort)
	}
	if what == "" {
		what = "a service"
	}
	if s.Repo != "" && s.Repo != what {
		what += " (" + s.Repo + ")"
	}
	if s.URL != "" {
		what += " at " + s.URL
	}
	return what
}

// signInHere drives the daemon's device flow from this terminal.
//
// The daemon runs the flow; this prints the code, waits, and says who signed
// in. The device code itself never crosses the socket in this direction, and
// the session token never crosses it at all — the daemon stores it, which is
// what makes `share.create` work on the next invocation without asking again.
func signInHere(ctx context.Context, c *client.Client) error {
	var started rpc.SessionStartResult
	if err := c.Call(ctx, "session.start", nil, &started); err != nil {
		return err
	}

	where := started.VerificationURI
	if where == "" {
		where = started.VerificationURIComplete
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Sharing publicly needs a sonar account.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  Open  %s\n", display.BoldCyan(where))
	fmt.Fprintf(os.Stderr, "  Code  %s\n", display.Bold(started.UserCode))
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, display.Dim("  Waiting for you to approve it…"))

	interval := started.Interval
	if interval <= 0 {
		interval = 5
	}
	deadline := time.Now().Add(15 * time.Minute)
	if started.ExpiresIn > 0 {
		deadline = time.Now().Add(time.Duration(started.ExpiresIn) * time.Second)
	}

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			// The flow is this terminal's; walking away should not leave a
			// code on the relay that the daemon still thinks is being polled.
			return ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}

		var got rpc.SessionPollResult
		if err := c.Call(ctx, "session.poll",
			rpc.SessionPollParams{FlowID: started.FlowID}, &got); err != nil {
			return err
		}
		switch got.State {
		case rpc.SessionPending:
			if got.Interval > 0 {
				interval = got.Interval
			}
		case rpc.SessionSlowDown:
			if got.Interval > 0 {
				interval = got.Interval
			}
		case rpc.SessionSignedIn:
			who := "this machine"
			if got.Account != nil {
				who = accountName(*got.Account)
			}
			fmt.Fprintf(os.Stderr, "  %s Signed in as %s.\n\n", display.Green("✓"), who)
			return nil
		case rpc.SessionDenied:
			return errors.New("the sign-in was declined")
		case rpc.SessionExpired:
			detail := got.Detail
			if detail == "" {
				detail = "that code is no longer valid"
			}
			return errors.New(detail)
		}
	}
	return errors.New("the code expired before it was approved")
}

func accountName(a rpc.SessionAccount) string {
	if a.Email != "" {
		return a.Email
	}
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.ID
}

func shareListRun(ctx context.Context) error {
	c, err := shareDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	var res rpc.ShareListResult
	if err := c.Call(ctx, "share.list", nil, &res); err != nil {
		return err
	}
	if shareJSON {
		printJSON(res)
		return nil
	}
	if len(res.Shares) == 0 {
		fmt.Println(display.Dim("Nothing is shared from this machine."))
		return nil
	}
	for _, s := range res.Shares {
		status := s.Status
		if s.StatusReason != "" {
			status += " — " + s.StatusReason
		}
		fmt.Printf("%s %s %s  %s\n",
			display.BoldCyan(s.URL),
			display.Dim("->"),
			fmt.Sprintf("localhost:%d", s.TargetPort),
			display.Dim(status))
	}
	return nil
}

func shareStopRun(ctx context.Context, args []string) error {
	c, err := shareDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	params := rpc.ShareStopParams{}
	if len(args) == 0 {
		params.All = true
	} else {
		port, err := strconv.Atoi(args[0])
		if err != nil {
			id := args[0]
			params.ID = &id
		} else {
			params.Target = &rpc.Selector{Port: &port}
		}
	}

	var res rpc.ShareStopResult
	if err := c.Call(ctx, "share.stop", params, &res); err != nil {
		return err
	}
	if shareJSON {
		printJSON(res)
		return nil
	}
	switch len(res.Stopped) {
	case 0:
		fmt.Println(display.Dim("Nothing to stop."))
	case 1:
		fmt.Println("Share stopped. The URL is still reserved to you; share it again to get it back.")
	default:
		fmt.Printf("%d shares stopped. Their URLs are still reserved to you.\n", len(res.Stopped))
	}
	return nil
}

// printShare is what a person reads when it worked.
// printNotes puts the daemon's sentences under the URL. They are advice about
// the share that was just made, never failures, so they are dim and they come
// after the link rather than in front of it.
func printNotes(notes []string) {
	for _, n := range notes {
		if strings.TrimSpace(n) == "" {
			continue
		}
		fmt.Println()
		// A note that already has line breaks is a table, and wrapping it
		// would lose the shape that makes it readable.
		if strings.Contains(n, "\n") {
			for _, line := range strings.Split(n, "\n") {
				fmt.Println(display.Dim("  " + line))
			}
			continue
		}
		for _, line := range wrapNote(n) {
			fmt.Println(display.Dim("  " + line))
		}
	}
}

// wrapNote breaks a sentence at 72 columns, which is what the rest of this
// output is written to.
func wrapNote(s string) []string {
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= 72:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

func printShare(s state.Share, port int) {
	fmt.Println()
	fmt.Printf("  %s %s %s\n",
		display.BoldCyan(s.URL),
		display.Dim("->"),
		display.Bold(fmt.Sprintf("localhost:%d", port)))
	fmt.Println()

	// The URL is the capability, and it is said out loud, once, next to the
	// link — not buried in a settings page.
	fmt.Println(display.Dim("  Anyone with this link can reach the service while the share is live."))
	fmt.Println(display.Dim("  " + expiryLine(s)))

	if s.Repo == "" {
		// The fallback key. Say plainly that it is tied to this machine and
		// this directory; the honest nudge is `sonar init`.
		fmt.Println()
		fmt.Println(display.Dim("  This project has no sonar.yaml, so the URL is tied to this machine and"))
		fmt.Println(display.Dim("  this directory. `sonar init` and a committed sonar.yaml make it follow"))
		fmt.Println(display.Dim("  the project instead."))
	}
	if s.Status == "connecting" {
		fmt.Println()
		fmt.Println(display.Yellow("  Still connecting — the link will start working in a moment."))
	}

	fmt.Println()
	fmt.Printf("  %s\n", display.Dim(fmt.Sprintf("sonar share %d --stop   to end it", port)))
	fmt.Println()
}

func expiryLine(s state.Share) string {
	if s.ExpiresAt == nil || *s.ExpiresAt == "" {
		return "It ends when the service stops."
	}
	at, err := time.Parse(time.RFC3339, *s.ExpiresAt)
	if err != nil {
		return "It ends when the service stops."
	}
	left := time.Until(at).Round(time.Minute)
	if left <= 0 {
		return "It ends when the service stops."
	}
	return fmt.Sprintf("It ends when the service stops, or in %s.", humanDuration(left))
}

func humanDuration(d time.Duration) string {
	if d >= time.Hour {
		hours := int(d.Hours())
		mins := int(d.Minutes()) % 60
		if mins == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// shareDaemon connects, starting the daemon if it is not running.
//
// Unlike the read commands, this one genuinely needs it: the daemon is what
// holds the tunnel, and a share that died when the terminal closed would be
// useless on the remote box this feature is aimed at.
func shareDaemon(ctx context.Context) (*client.Client, error) {
	c, err := dialDaemon(ctx)
	if err == nil {
		return c, nil
	}
	socket := daemon.SocketPath()
	if startErr := client.Autostart(ctx, "", socket); startErr != nil {
		return nil, fmt.Errorf("sharing needs the sonar daemon, and it could not be started: %w", startErr)
	}
	return client.Dial(ctx, client.ClientInfo{Name: "cli", Version: selfupdate.Version})
}

func codeOf(err error) int {
	var e *rpc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// shareIn is the live share a `share_limit_reached` carries, or nil.
func shareIn(err error) *state.Share {
	var e *rpc.Error
	if errors.As(err, &e) {
		return e.Data.Share
	}
	return nil
}

// stdinIsTerminal is whether there is someone there to answer a question.
//
// It is a real terminal check and not "is stdin a character device", which is
// the cheap version used elsewhere in this package: /dev/null is a character
// device too, so the cheap version prompts into a void whenever a share runs
// from a script or a CI job, reads EOF, and calls that a no. Here the answer
// decides whether someone's live share gets moved, so it is worth a dependency
// that is already in the module graph.
//
// It is a variable so a test can be both a terminal and not one.
var stdinIsTerminal = func() bool { return isatty.IsTerminal(os.Stdin.Fd()) }

// wantsProject asks the daemon whether this port is part of a project worth
// sharing whole, and if so asks the person.
//
// Decision 4: the existing command offers rather than a new one existing.
// Somebody sharing a frontend usually wants the API behind it too, and
// discovering that by having the login button do nothing is the report this
// feature exists to prevent.
func wantsProject(ctx context.Context, c *client.Client, port int) bool {
	if shareJSON || !stdinIsTerminal() {
		// A script asks for what it wants: `sonar share <project> --public`.
		// Never a prompt, and never a silent upgrade to something broader
		// than the command said.
		return false
	}
	var p rpc.ShareProjectResult
	if err := c.Call(ctx, "share.project",
		rpc.ShareProjectParams{Target: rpc.Selector{Port: &port}}, &p); err != nil {
		// A daemon too old to answer, or anything else: share the one service,
		// which is exactly what was typed.
		return false
	}
	if !p.Available {
		return false
	}

	fmt.Fprintf(os.Stderr, "\n%s is part of %s, which also runs:\n",
		display.Bold(fmt.Sprintf("localhost:%d", port)), display.Bold(p.Group))
	for _, line := range p.Services {
		fmt.Fprintf(os.Stderr, "  %s\n", display.Dim(line))
	}
	fmt.Fprint(os.Stderr, "\nShare the whole project? [Y/n] ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

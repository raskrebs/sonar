package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/spf13/cobra"
)

var (
	graphJSONFlag bool
	graphDotFlag  bool
)

var graphCmd = &cobra.Command{
	Use:   "graph",
	Short: "Show dependency graph of connected services",
	Long:  "Discover which listening services connect to each other via established TCP connections.",
	RunE:  graphRun,
}

func init() {
	graphCmd.Flags().BoolVar(&graphJSONFlag, "json", false, "Output as JSON")
	graphCmd.Flags().BoolVar(&graphDotFlag, "dot", false, "Output in Graphviz DOT format")
	rootCmd.AddCommand(graphCmd)
}

func graphRun(cmd *cobra.Command, args []string) error {
	results, err := ports.Scan()
	if err != nil {
		return err
	}

	docker.EnrichPorts(results)
	ports.Enrich(results)

	connections, err := ports.BuildGraph(results)
	if err != nil {
		return err
	}

	if len(connections) == 0 {
		fmt.Println("No inter-service connections detected.")
		return nil
	}

	if graphJSONFlag {
		return renderGraphJSON(connections)
	}

	if graphDotFlag {
		return renderGraphDot(connections)
	}

	return renderGraphASCII(connections)
}

func renderGraphASCII(connections []ports.Connection) error {
	for _, c := range connections {
		fmt.Printf("%s (%d) \u2192 %s (%d)\n", c.FromProcess, c.FromPort, c.ToProcess, c.ToPort)
	}
	return nil
}

func renderGraphJSON(connections []ports.Connection) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(connections)
}

func renderGraphDot(connections []ports.Connection) error {
	fmt.Println("digraph sonar {")
	fmt.Println("  rankdir=LR;")
	fmt.Println("  node [shape=box, style=rounded];")

	// Collect unique nodes
	nodes := make(map[string]bool)
	for _, c := range connections {
		from := fmt.Sprintf("%s:%d", c.FromProcess, c.FromPort)
		to := fmt.Sprintf("%s:%d", c.ToProcess, c.ToPort)
		nodes[from] = true
		nodes[to] = true
	}

	// Declare nodes
	for n := range nodes {
		fmt.Printf("  %q;\n", n)
	}

	fmt.Println()

	// Declare edges
	for _, c := range connections {
		from := fmt.Sprintf("%s:%d", c.FromProcess, c.FromPort)
		to := fmt.Sprintf("%s:%d", c.ToProcess, c.ToPort)
		fmt.Printf("  %q -> %q;\n", from, to)
	}

	fmt.Println("}")
	return nil
}

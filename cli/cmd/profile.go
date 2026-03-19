package cmd

import (
	"fmt"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/profile"
	"github.com/spf13/cobra"
)

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage port profiles for projects",
}

var profileListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all saved profiles",
	RunE: func(cmd *cobra.Command, args []string) error {
		names, err := profile.List()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("No profiles found.")
			fmt.Printf("Create one with: %s\n", display.Bold("sonar profile create <name>"))
			return nil
		}
		fmt.Println(display.Bold("Saved profiles:"))
		for _, name := range names {
			fmt.Printf("  - %s\n", name)
		}
		return nil
	},
}

var profileShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show details of a profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := profile.Load(args[0])
		if err != nil {
			return err
		}

		fmt.Printf("%s\n", display.Bold(p.Name))
		if p.Description != "" {
			fmt.Printf("  %s\n", display.Dim(p.Description))
		}
		fmt.Println()
		fmt.Printf("  %-8s %-20s %s\n",
			display.Dim("PORT"), display.Dim("NAME"), display.Dim("HEALTH"))
		for _, entry := range p.Ports {
			health := "no"
			if entry.Health {
				health = "yes"
				if entry.HealthPath != "" {
					health = entry.HealthPath
				}
			}
			fmt.Printf("  %-8d %-20s %s\n", entry.Port, entry.Name, health)
		}
		return nil
	},
}

var profileCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Snapshot current listening ports into a new profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		results, err := ports.Scan()
		if err != nil {
			return err
		}
		docker.EnrichPorts(results)
		ports.Enrich(results)

		// Exclude desktop apps
		var filtered []ports.ListeningPort
		for _, p := range results {
			if !p.IsApp {
				filtered = append(filtered, p)
			}
		}

		if len(filtered) == 0 {
			return fmt.Errorf("no listening ports found to snapshot")
		}

		var entries []profile.PortEntry
		for _, p := range filtered {
			entries = append(entries, profile.PortEntry{
				Port: p.Port,
				Name: p.DisplayName(),
			})
		}

		prof := &profile.Profile{
			Name:  name,
			Ports: entries,
		}

		if err := profile.Save(prof); err != nil {
			return err
		}

		fmt.Printf("Profile %s created with %d port(s).\n",
			display.Bold(name), len(entries))
		for _, e := range entries {
			fmt.Printf("  %d  %s\n", e.Port, display.Dim(e.Name))
		}
		return nil
	},
}

var profileDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a saved profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := profile.Delete(name); err != nil {
			return err
		}
		fmt.Printf("Profile %s deleted.\n", display.Bold(name))
		return nil
	},
}

func init() {
	profileCmd.AddCommand(profileListCmd)
	profileCmd.AddCommand(profileShowCmd)
	profileCmd.AddCommand(profileCreateCmd)
	profileCmd.AddCommand(profileDeleteCmd)
	rootCmd.AddCommand(profileCmd)
}

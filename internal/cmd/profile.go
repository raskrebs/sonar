package cmd

import (
	"fmt"
	"strings"

	"github.com/raskrebs/sonar/internal/display"
	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/groups"
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
		Hint(cmd, HintProfileToConfig(""))
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
		Hint(cmd, HintProfileToConfig(args[0]))
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
		Hint(cmd, HintProfileToConfig(name))

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
		Hint(cmd, HintProfileToConfig(name))
		if err := profile.Delete(name); err != nil {
			return err
		}
		fmt.Printf("Profile %s deleted.\n", display.Bold(name))
		return nil
	},
}

// profileExportCmd is the migration path off profiles: it prints the
// `sonar.yaml` a profile would become, and never writes it. A profile is a
// per-machine snapshot of ports; `sonar.yaml` is committed with the project,
// so which repository it belongs in is the user's call, not ours (daemon spec,
// "Migration and deprecation").
var profileExportCmd = &cobra.Command{
	Use:   "export <name>",
	Short: "Print the " + groups.ConfigName + " a profile would become",
	Long: "Convert a saved profile into a " + groups.ConfigName + " proposal on stdout.\n\n" +
		"Nothing is written: redirect it to the file yourself once you have\n" +
		"reviewed it, at the root of the repository the services belong to.\n\n" +
		"  sonar profile export my-app > " + groups.ConfigName,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		prof, err := profile.Load(args[0])
		if err != nil {
			return err
		}
		out, err := groups.Marshal(profileConfig(prof))
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "# Converted from the sonar profile %q by `sonar profile export`.\n", prof.Name)
		fmt.Fprint(cmd.OutOrStdout(), string(out))
		return nil
	},
}

// profileConfig converts a profile into a group config. A profile only knows
// ports, names and health paths — it never recorded how a service is started —
// so the proposal carries no `cmd:` and the user fills those in. Health comes
// across because that is the one field the two formats share.
func profileConfig(p *profile.Profile) *groups.Config {
	cfg := &groups.Config{Name: configName(p.Name)}
	used := map[string]bool{}
	for _, entry := range p.Ports {
		svc := groups.Service{
			Name: uniqueServiceName(entry.Name, entry.Port, used),
			Port: entry.Port,
		}
		switch {
		case entry.HealthPath != "":
			svc.Health = entry.HealthPath
		case entry.Health:
			svc.Health = "/"
		}
		cfg.Services = append(cfg.Services, svc)
	}
	return cfg
}

// configName makes a profile name usable as a group name: the validator
// rejects slashes and whitespace.
func configName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ' ' || r == '\t' || r == '\n' {
			return '-'
		}
		return r
	}, name)
	if name == "" {
		return "project"
	}
	return name
}

// uniqueServiceName sanitises a profile entry's display name into something the
// config validator accepts, and keeps it unique within the file.
func uniqueServiceName(candidate string, port int, used map[string]bool) string {
	var b strings.Builder
	for _, r := range strings.ToLower(candidate) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-._")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	if name == "" {
		name = fmt.Sprintf("service-%d", port)
	}
	base := name
	for i := 2; used[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used[name] = true
	return name
}

func init() {
	profileCmd.AddCommand(profileExportCmd)
	profileCmd.AddCommand(profileListCmd)
	profileCmd.AddCommand(profileShowCmd)
	profileCmd.AddCommand(profileCreateCmd)
	profileCmd.AddCommand(profileDeleteCmd)
	rootCmd.AddCommand(profileCmd)
}

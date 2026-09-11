package display

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/state"
)

// RenderGroups writes the `sonar groups` table: NAME SOURCE ROOT PORTS STATUS.
func RenderGroups(w io.Writer, gg []state.Group) {
	if len(gg) == 0 {
		fmt.Fprintln(w, "No groups found.")
		return
	}

	headers := []string{"NAME", "SOURCE", "ROOT", "PORTS", "STATUS"}
	rows := make([][]string, len(gg))
	for i, g := range gg {
		root := ""
		if g.RootDir != nil {
			root = shortenHome(*g.RootDir)
		}
		rows[i] = []string{
			Bold(g.Name),
			string(g.Source),
			Dim(root),
			portList(g.Members),
			colorStatus(g.Status),
		}
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = visibleLen(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if vw := visibleLen(cell); vw > widths[i] {
				widths[i] = vw
			}
		}
	}

	const gap = 3
	line := func(cells []string) {
		var b strings.Builder
		for i, cell := range cells {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", gap))
			}
			b.WriteString(padRight(cell, widths[i]))
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}

	bold := make([]string, len(headers))
	for i, h := range headers {
		bold[i] = Bold(h)
	}
	line(bold)
	for _, row := range rows {
		line(row)
	}

	fmt.Fprintf(w, "\n%s\n", Dim(fmt.Sprintf("%d %s", len(gg), plural(len(gg), "group"))))
}

// RenderGroup writes the detail view for one group: where it comes from, the
// ports resolved to it, and the services its config declares.
func RenderGroup(w io.Writer, g state.Group, pp []ports.ListeningPort) {
	fmt.Fprintf(w, "%s  (source: %s, status: %s)\n", Bold(g.Name), string(g.Source), colorStatus(g.Status))
	if g.RootDir != nil {
		fmt.Fprintf(w, "root:    %s\n", Dim(shortenHome(*g.RootDir)))
	}
	if g.ConfigPath != nil {
		fmt.Fprintf(w, "config:  %s\n", Dim(shortenHome(*g.ConfigPath)))
	}

	members := make([]ports.ListeningPort, 0, len(pp))
	for _, p := range pp {
		if p.Group == g.Name {
			members = append(members, p)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Port < members[j].Port })

	fmt.Fprintf(w, "\n%s\n", Bold("PORTS"))
	if len(members) == 0 {
		fmt.Fprintln(w, "  none listening")
	}
	for _, p := range members {
		name := p.DisplayName()
		if name == "" {
			name = p.Process
		}
		fmt.Fprintf(w, "  %s  %s  %s\n",
			padRight(BoldCyan(strconv.Itoa(p.Port)), 5), padRight(colorName(p, name), 20), Underline(p.URL()))
	}

	if len(g.Services) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", Bold("SERVICES"))
	for _, s := range g.Services {
		want := ""
		if s.Port != nil {
			want = strconv.Itoa(*s.Port)
		}
		status := Dim("stopped")
		if s.Running {
			status = Green("running")
			if s.PortActual != nil && (s.Port == nil || *s.PortActual != *s.Port) {
				status = Green("running") + Dim(" on "+strconv.Itoa(*s.PortActual))
			}
		}
		extra := ""
		if len(s.DependsOn) > 0 {
			extra = Dim("  after " + strings.Join(s.DependsOn, ", "))
		}
		fmt.Fprintf(w, "  %s%s  %s  %s%s\n",
			serviceIcon(s), padRight(s.Name, 16), padRight(want, 5), status, extra)
		if meta := serviceMeta(s); meta != "" {
			fmt.Fprintf(w, "  %s\n", Dim(meta))
		}
	}
}

// serviceIcon prefixes a service with the icon its `sonar.yaml` gave it.
// The daemon never interprets the string; the terminal prints whatever the
// author wrote, which for a one-rune emoji is exactly what they meant.
func serviceIcon(s state.Service) string {
	if s.Icon == nil || *s.Icon == "" {
		return ""
	}
	return *s.Icon + " "
}

// serviceMeta is the description and colour line under a service, printed only
// when the config actually carries them.
func serviceMeta(s state.Service) string {
	var parts []string
	if s.Description != nil && *s.Description != "" {
		parts = append(parts, *s.Description)
	}
	if s.Color != nil && *s.Color != "" {
		parts = append(parts, "color "+*s.Color)
	}
	if len(parts) == 0 {
		return ""
	}
	return "  " + strings.Join(parts, "  ")
}

func portList(members []int) string {
	if len(members) == 0 {
		return Dim("-")
	}
	const showAt = 5
	parts := make([]string, 0, showAt+1)
	for i, p := range members {
		if i == showAt {
			parts = append(parts, fmt.Sprintf("+%d", len(members)-showAt))
			break
		}
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ",")
}

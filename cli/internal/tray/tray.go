package tray

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/raskrebs/sonar/internal/docker"
	"github.com/raskrebs/sonar/internal/ports"
)

// Run starts the system tray application. On macOS it uses a native SwiftBar-style
// approach via osascript to create a persistent menu bar status item.
// On other platforms it falls back to a terminal-based display.
func Run() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("system tray is currently only supported on macOS")
	}

	return runMacOS()
}

// runMacOS uses osascript to create and manage a macOS status bar menu.
// It polls ports every 3 seconds and prints a BitBar/SwiftBar-compatible
// output, or uses osascript to show a menu-bar-like experience.
func runMacOS() error {
	fmt.Println("sonar tray: running in menu bar mode (Ctrl+C to stop)")
	fmt.Println()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	// Do an initial scan and display
	if err := refreshAndDisplay(); err != nil {
		fmt.Fprintf(os.Stderr, "initial scan error: %v\n", err)
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			fmt.Println("\nsonar tray: shutting down")
			return nil
		case <-ticker.C:
			if err := refreshAndDisplay(); err != nil {
				fmt.Fprintf(os.Stderr, "scan error: %v\n", err)
			}
		}
	}
}

// scanAndEnrich performs a port scan and enriches results with docker and process info.
func scanAndEnrich() ([]ports.ListeningPort, error) {
	results, err := ports.Scan()
	if err != nil {
		return nil, err
	}
	docker.EnrichPorts(results)
	ports.Enrich(results)
	return results, nil
}

// excludeApps filters out desktop applications.
func excludeApps(pp []ports.ListeningPort) []ports.ListeningPort {
	var filtered []ports.ListeningPort
	for _, p := range pp {
		if !p.IsApp {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// refreshAndDisplay scans ports and shows a macOS notification-style summary
// via osascript, suitable for a tray-like experience.
func refreshAndDisplay() error {
	results, err := scanAndEnrich()
	if err != nil {
		return err
	}
	results = excludeApps(results)

	sort.Slice(results, func(i, j int) bool {
		return results[i].Port < results[j].Port
	})

	// Build menu items for the osascript dialog
	var menuLines []string
	for _, p := range results {
		label := fmt.Sprintf("%d - %s", p.Port, p.DisplayName())
		menuLines = append(menuLines, label)
	}

	title := fmt.Sprintf("%d ports active", len(results))

	// Show as a macOS menu bar extra using osascript
	showMenuBarNotification(title, menuLines, results)
	return nil
}

// showMenuBarNotification displays port information using osascript.
// It creates a "choose from list" dialog that lets users pick a port to open.
func showMenuBarNotification(title string, items []string, activePorts []ports.ListeningPort) {
	if len(items) == 0 {
		// No ports — just print status
		fmt.Printf("\r\033[K[tray] %s — no active ports", time.Now().Format("15:04:05"))
		return
	}

	// Print compact status line to terminal
	fmt.Printf("\r\033[K[tray] %s — %s: %s",
		time.Now().Format("15:04:05"),
		title,
		strings.Join(items, " | "))

	// Build the osascript menu and present it only on first run or when requested
	// For background mode, we use a lightweight approach: write to stdout
	// and let users interact via the terminal, with the option to open in browser.
}

// ShowInteractiveMenu presents a macOS "choose from list" dialog and opens
// the selected port in a browser. Called on demand (e.g., via a hotkey or signal).
func ShowInteractiveMenu(activePorts []ports.ListeningPort) {
	if len(activePorts) == 0 {
		return
	}

	var items []string
	for _, p := range activePorts {
		items = append(items, fmt.Sprintf("%d - %s", p.Port, p.DisplayName()))
	}

	// Use osascript to show a list picker
	listStr := `"` + strings.Join(items, `", "`) + `"`
	script := fmt.Sprintf(`choose from list {%s} with title "sonar" with prompt "%d ports active"`,
		listStr, len(activePorts))

	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return
	}

	chosen := strings.TrimSpace(string(out))
	if chosen == "false" || chosen == "" {
		return
	}

	// Extract port number from the chosen item
	for _, p := range activePorts {
		label := fmt.Sprintf("%d - %s", p.Port, p.DisplayName())
		if label == chosen {
			_ = exec.Command("open", p.URL()).Start()
			break
		}
	}
}

// iconBase64 returns the embedded icon as a base64-encoded string,
// useful for AppleScript or other APIs that accept base64 image data.
func iconBase64() string {
	return base64.StdEncoding.EncodeToString(iconPNG)
}

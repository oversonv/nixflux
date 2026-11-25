package nixflux

import (
	"fmt"
	"os"
	"time"
)

const accentColor = "\033[38;2;28;72;166m"
const resetColor = "\033[0m"

func paint(value string) string {
	if os.Getenv("NO_COLOR") != "" {
		return value
	}
	if info, err := os.Stdout.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		return accentColor + value + resetColor
	}
	return value
}
func renderInstallSuccess(name, version string, started time.Time, dependencyCount int) {
	fmt.Println("Checking store catalogs...")
	fmt.Println("Catalogs have been prepared")
	fmt.Println()
	fmt.Printf("NixFlux: Installing %s\n\n", paint(name))
	fmt.Println("Resolving dependencies...")
	if dependencyCount > 0 {
		fmt.Printf("Resolved %d dependencies.\n", dependencyCount)
	}
	fmt.Printf("Downloading %s %s...\n", name, paint(version))
	fmt.Println("Verifying package integrity...")
	fmt.Println("Extracting package...")
	fmt.Println("Installing files...")
	fmt.Println("Updating package database...")
	fmt.Printf("\n%s %s installed successfully.\n\n", paint(name), paint(version))
	fmt.Printf("Completed in %.2fs\n", time.Since(started).Seconds())
}
func renderSimpleSuccess(action, name, version string) {
	fmt.Printf("NixFlux: %s %s\n", action, paint(name))
	if version != "" {
		fmt.Printf("Version: %s\n", paint(version))
	}
	fmt.Println("Updating package database...")
	fmt.Printf("%s completed successfully.\n", action)
}
func renderChannelFetch(entry Channel, status ChannelStatus) {
	fmt.Printf("channel:  %s\nurl:      %s\nstatus:   %d %s\ntype:     %s\npackages: %d\ntime:     %d ms\n", entry.Name, entry.URL, status.Status, status.Reason, status.ContentType, status.PackageCount, status.ElapsedMS)
	if status.FinalURL != "" {
		fmt.Printf("release:  %s\n", status.FinalURL)
	}
}

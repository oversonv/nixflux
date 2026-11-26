package nixflux

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const accentColor = "\033[1;38;2;79;156;255m"
const resetColor = "\033[0m"

func paint(value string) string {
	if os.Getenv("NO_COLOR") != "" {
		return value
	}
	return paintTransaction(value)
}

func paintTransaction(value string) string {
	if os.Getenv("NO_COLOR") != "" {
		return value
	}
	if info, err := os.Stdout.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		return accentColor + value + resetColor
	}
	return value
}

type operationProgress struct {
	stop chan struct{}
	done chan struct{}
}

func startOperationProgress(label string) *operationProgress {
	info, err := os.Stdout.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	progress := &operationProgress{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(progress.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		colorEnabled := os.Getenv("NO_COLOR") == ""
		render := func(frame int) {
			spinner := frames[frame%len(frames)]
			if colorEnabled {
				spinner = accentColor + spinner + resetColor
			}
			fmt.Fprintf(os.Stdout, "\r%s %s\033[K", spinner, label)
		}
		for frame := 0; ; frame++ {
			render(frame)
			select {
			case <-progress.stop:
				fmt.Fprint(os.Stdout, "\r\033[2K")
				return
			case <-ticker.C:
			}
		}
	}()
	return progress
}

func (progress *operationProgress) finish() {
	if progress == nil {
		return
	}
	close(progress.stop)
	<-progress.done
}

type PackageTransactionItem struct {
	Package       InstalledPackage
	DownloadSize  int64
	InstalledSize int64
}

func printPacmanPackageList(packages []PackageTransactionItem, showSizes bool) {
	if len(packages) == 0 {
		fmt.Println("there is nothing to do")
		return
	}
	ordered := append([]PackageTransactionItem(nil), packages...)
	sort.Slice(ordered, func(i, j int) bool {
		return strings.ToLower(ordered[i].Package.Name) < strings.ToLower(ordered[j].Package.Name)
	})
	fmt.Printf("\n%s", paintTransaction(fmt.Sprintf("Packages (%d)", len(ordered))))
	var downloadSize, installedSize int64
	for _, item := range ordered {
		fmt.Printf(" %s-%s", item.Package.Name, item.Package.Version)
		downloadSize += item.DownloadSize
		installedSize += item.InstalledSize
	}
	fmt.Print("\n")
	if showSizes {
		fmt.Printf("%s    %s\n", paintTransaction("Total Download Size:"), formatPackageSize(downloadSize))
		fmt.Printf("%s   %s\n", paintTransaction("Total Installed Size:"), formatPackageSize(installedSize))
	}
	fmt.Print("\n")
}

func formatPackageSize(size int64) string {
	const kib = 1 << 10
	const mib = 1 << 20
	switch {
	case size >= mib:
		return fmt.Sprintf("%.2f MiB", float64(size)/mib)
	case size >= kib:
		return fmt.Sprintf("%.2f KiB", float64(size)/kib)
	default:
		return fmt.Sprintf("%d B", size)
	}
}

func renderInstallSteps(packages []PackageTransactionItem, rootName string) {
	ordered := append([]PackageTransactionItem(nil), packages...)
	rootKey := normalizePackageName(rootName)
	sort.Slice(ordered, func(i, j int) bool {
		iRoot := normalizePackageName(ordered[i].Package.Name) == rootKey
		jRoot := normalizePackageName(ordered[j].Package.Name) == rootKey
		if iRoot != jRoot {
			return !iRoot
		}
		return strings.ToLower(ordered[i].Package.Name) < strings.ToLower(ordered[j].Package.Name)
	})
	for i, pkg := range ordered {
		fmt.Printf("(%d/%d) %s %s (%s)\n", i+1, len(ordered), paintTransaction("installing"), pkg.Package.Name, pkg.Package.Version)
	}
}

func renderRemoveSteps(packages []PackageTransactionItem, rootName string) {
	ordered := append([]PackageTransactionItem(nil), packages...)
	rootKey := normalizePackageName(rootName)
	sort.Slice(ordered, func(i, j int) bool {
		iRoot := normalizePackageName(ordered[i].Package.Name) == rootKey
		jRoot := normalizePackageName(ordered[j].Package.Name) == rootKey
		if iRoot != jRoot {
			return iRoot
		}
		return strings.ToLower(ordered[i].Package.Name) > strings.ToLower(ordered[j].Package.Name)
	})
	for i, pkg := range ordered {
		fmt.Printf("(%d/%d) %s %s (%s)\n", i+1, len(ordered), paintTransaction("removing"), pkg.Package.Name, pkg.Package.Version)
	}
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

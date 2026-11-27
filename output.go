package nixnet

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const resetColor = "\033[0m"

func uvColorsEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func uvDim(value string) string {
	if uvColorsEnabled() {
		return "\033[2m" + value + resetColor
	}
	return value
}

func uvPackageRef(name, version string) string {
	return fmt.Sprintf("%s==%s", uvEmphasis(name), uvDim(version))
}

func uvPackageChange(action string, pkg InstalledPackage) string {
	marker, color := "+", "38;2;79;156;255"
	switch action {
	case "Update", "Reinstall":
		marker, color = "~", "36"
	case "Downgrade":
		marker, color = "~", "33"
	case "Remove":
		marker, color = "-", "37"
	}
	if uvColorsEnabled() {
		marker = fmt.Sprintf("\033[1;%sm%s%s", color, marker, resetColor)
		return fmt.Sprintf(" %s %s", marker, uvPackageRef(pkg.Name, pkg.Version))
	}
	return fmt.Sprintf(" %s %s==%s", marker, pkg.Name, pkg.Version)
}

func formatOperationDuration(duration time.Duration) string {
	if duration < time.Second {
		return fmt.Sprintf("%dms", duration.Round(time.Millisecond).Milliseconds())
	}
	return fmt.Sprintf("%.2fs", duration.Seconds())
}

func packageCountLabel(count int) string {
	if count == 1 {
		return "1 package"
	}
	return fmt.Sprintf("%d packages", count)
}

func uvEmphasis(value string) string {
	if uvColorsEnabled() {
		return "\033[1m" + value + resetColor
	}
	return value
}

func uvAccent(value string) string {
	if uvColorsEnabled() {
		return "\033[38;2;79;156;255m" + value + resetColor
	}
	return value
}

type operationProgress struct {
	stop   chan bool
	done   chan struct{}
	rows   int
	pinned bool
}

type terminalWindowSize struct {
	rows, columns, xpixel, ypixel uint16
}

func terminalRows() int {
	var size terminalWindowSize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
	if errno != 0 || size.rows < 3 {
		return 0
	}
	return int(size.rows)
}

func startOperationProgress(label string) *operationProgress {
	info, err := os.Stdout.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	progress := &operationProgress{stop: make(chan bool, 1), done: make(chan struct{}), rows: terminalRows()}
	progress.pinned = progress.rows > 0
	if progress.pinned {
		fmt.Fprintf(os.Stdout, "\0337\033[1;%dr\033[%d;1H\033[?25l", progress.rows-1, progress.rows)
	}
	go func() {
		defer close(progress.done)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		colorEnabled := uvColorsEnabled()
		const width = 18
		render := func(frame int, complete bool) {
			filled := 0
			if complete {
				filled = width
			}
			var bar strings.Builder
			for i := 0; i < width; i++ {
				if i < filled {
					if colorEnabled {
						bar.WriteString("\033[37m#\033[0m")
					} else {
						bar.WriteByte('#')
					}
				} else if colorEnabled {
					bar.WriteString("\033[2m─\033[0m")
				} else {
					bar.WriteRune('─')
				}
			}
			spinner := frames[frame%len(frames)]
			labelText := label
			if colorEnabled {
				spinner = "\033[37m" + spinner + resetColor
				labelText = uvDim(label)
			}
			if progress.pinned {
				fmt.Fprintf(os.Stdout, "\033[%d;1H", progress.rows)
			}
			fmt.Fprintf(os.Stdout, "\r  %s %s  [%s]\033[K", spinner, labelText, bar.String())
		}
		for frame := 0; ; frame++ {
			render(frame, false)
			select {
			case success := <-progress.stop:
				render(frame, success)
				if success {
					time.Sleep(80 * time.Millisecond)
				}
				if progress.pinned {
					fmt.Fprintf(os.Stdout, "\033[%d;1H\033[2K\033[r\0338\033[?25h", progress.rows)
				} else {
					fmt.Fprint(os.Stdout, "\r\033[2K")
				}
				return
			case <-ticker.C:
			}
		}
	}()
	return progress
}

func (progress *operationProgress) finish() {
	progress.stopWithResult(false)
}

func (progress *operationProgress) complete() {
	progress.stopWithResult(true)
}

func (progress *operationProgress) stopWithResult(success bool) {
	if progress == nil {
		return
	}
	progress.stop <- success
	<-progress.done
}

type PackageTransactionItem struct {
	Package       InstalledPackage
	DownloadSize  int64
	InstalledSize int64
	Action        string
}

func renderResolved(count int, elapsed time.Duration) {
	if count == 0 {
		fmt.Println(uvDim("No packages to resolve."))
		return
	}
	fmt.Printf("%s %s in %s\n", uvDim("Resolved"), uvEmphasis(packageCountLabel(count)), uvDim(formatOperationDuration(elapsed)))
}

func renderPackageChanges(packages []PackageTransactionItem, elapsed time.Duration) {
	if len(packages) == 0 {
		return
	}
	ordered := append([]PackageTransactionItem(nil), packages...)
	sort.Slice(ordered, func(i, j int) bool {
		return strings.ToLower(ordered[i].Package.Name) < strings.ToLower(ordered[j].Package.Name)
	})
	verb := map[string]string{"Install": "Installed", "Update": "Updated", "Downgrade": "Downgraded", "Reinstall": "Reinstalled", "Remove": "Uninstalled"}[ordered[0].Action]
	if verb == "" {
		verb = "Installed"
	}
	for _, item := range ordered[1:] {
		if item.Action != ordered[0].Action {
			verb = "Applied changes to"
			break
		}
	}
	fmt.Printf("%s %s in %s\n", uvDim(verb), uvEmphasis(packageCountLabel(len(ordered))), uvDim(formatOperationDuration(elapsed)))
	for _, item := range ordered {
		fmt.Println(uvPackageChange(item.Action, item.Package))
	}
}

func renderInstallNoop(plan packageInstallPlan) {
	if plan.HadPrevious && plan.Root.Dir == plan.Previous.Dir && !plan.ExplicitlyAdded {
		fmt.Printf("%s %s\n", uvDim("Already up to date:"), uvPackageRef(plan.Root.Name, plan.Root.Version))
		return
	}
	if plan.HadPrevious && plan.Root.Dir == plan.Previous.Dir {
		fmt.Printf("%s %s to the profile; package files were already available.\n", uvDim("Added"), uvPackageRef(plan.Root.Name, plan.Root.Version))
		return
	}
	fmt.Printf("%s %s; no download was needed.\n", uvDim("Using stored files for"), uvPackageRef(plan.Root.Name, plan.Root.Version))
}

func renderUpdateResult(plan packageUpdatePlan, elapsed time.Duration) {
	if len(plan.Changes) == 0 {
		if plan.ResolvedCount == 0 {
			fmt.Println("No packages to update.")
			return
		}
		fmt.Println("No updates available.")
		return
	}
	renderPackageChanges(plan.Changes, elapsed)
}

func renderEnvironmentUpdateResult(plan envUpdatePlan, elapsed time.Duration) {
	profileLabel := "current profile"
	if plan.Profile != "" {
		profileLabel = "profile " + plan.Profile
	}
	if plan.TargetCount == 0 {
		fmt.Printf("%s %s in %s.\n", uvDim("No enabled packages from channel"), uvEmphasis(plan.Channel), profileLabel)
	} else if len(plan.Changes) > 0 {
		renderPackageChanges(plan.Changes, elapsed)
	} else if len(plan.Frozen) > 0 || len(plan.Unavailable) > 0 {
		fmt.Println(uvDim("No package updates were applied."))
	} else {
		fmt.Printf("%s\n", uvDim("No updates available in "+profileLabel+"."))
	}
	for _, pkg := range plan.Frozen {
		fmt.Printf("%s %s\n", uvDim("Skipped frozen:"), uvPackageRef(pkg.Name, pkg.Version))
	}
	for _, pkg := range plan.Unavailable {
		fmt.Printf("%s %s\n", uvDim("Unavailable in channel:"), uvPackageRef(pkg.Name, pkg.Version))
	}
}

func renderSimpleSuccess(action, name, version string) {
	item := uvEmphasis(name)
	if version != "" {
		item = uvPackageRef(name, version)
	}
	fmt.Printf("%s %s\n", uvDim(action), item)
}

func renderActionSuccess(action, subject, detail string) {
	if detail == "" {
		fmt.Printf("%s %s\n", uvDim(action), uvEmphasis(subject))
		return
	}
	fmt.Printf("%s %s %s\n", uvDim(action), uvEmphasis(subject), uvDim(detail))
}

func renderPackageListing(lines []string) {
	for _, line := range lines {
		parts := strings.SplitN(line, "==", 2)
		if len(parts) != 2 {
			fmt.Println(line)
			continue
		}
		name := parts[0]
		rest := strings.Fields(parts[1])
		if len(rest) == 0 {
			fmt.Println(uvPackageRef(name, parts[1]))
			continue
		}
		fmt.Printf("%s==%s", uvEmphasis(name), uvDim(rest[0]))
		if len(rest) > 1 {
			fmt.Printf(" %s", uvDim(strings.Join(rest[1:], " ")))
		}
		fmt.Println()
	}
}

func renderChannelFetch(entry Channel, status ChannelStatus) {
	finalURL := status.FinalURL
	if finalURL == "" {
		finalURL = entry.URL
	}
	fmt.Printf("%s %s\n", uvDim("Channel"), uvEmphasis(entry.Name))
	fmt.Printf("%s %s\n", uvDim("URL"), entry.URL)
	fmt.Printf("%s %s %s\n", uvDim("Status"), uvAccent(fmt.Sprintf("%d", status.Status)), status.Reason)
	fmt.Printf("%s %s\n", uvDim("Type"), status.ContentType)
	fmt.Printf("%s %s\n", uvDim("Packages"), uvEmphasis(fmt.Sprintf("%d", status.PackageCount)))
	fmt.Printf("%s %s\n", uvDim("Fetched in"), formatOperationDuration(time.Duration(status.ElapsedMS)*time.Millisecond))
	if finalURL != entry.URL {
		fmt.Printf("%s %s\n", uvDim("Final URL"), finalURL)
	}
}

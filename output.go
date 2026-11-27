package nixnet

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

const resetColor = "\033[0m"

const (
	colorProgressWhite   = "37"
	progressTickInterval = 200 * time.Millisecond
)

var progressFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func uvColorsEnabled() bool {
	return terminalColorsEnabled(os.Stdout)
}

func terminalColorsEnabled(file *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	info, err := file.Stat()
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
	marker, color := "+", "37"
	switch action {
	case "Update", "Reinstall":
		marker, color = "~", "38;2;129;199;132"
	case "Downgrade":
		marker, color = "~", "33"
	case "Remove":
		marker, color = "-", "37"
	}
	if uvColorsEnabled() {
		marker = fmt.Sprintf("\033[%sm%s%s", color, marker, resetColor)
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
	return value
}

func uvErrorLabel(value string) string {
	if uvColorsEnabled() {
		return "\033[31m" + value + resetColor
	}
	return value
}

func uvAction(value string) string {
	return value
}

func uvStatusLabel(value string) string {
	if uvColorsEnabled() {
		return "\033[32m" + value + resetColor
	}
	return value
}

func uvProgressColor(position, width int) string {
	if width < 2 {
		return "38;2;124;34;206"
	}
	ratio := float64(position) / float64(width-1)
	red := 124 + int(84*ratio)
	green := 34 + int(4*ratio)
	blue := 206 + int(22*ratio)
	return fmt.Sprintf("38;2;%d;%d;%d", red, green, blue)
}

type operationProgress struct {
	label   string
	started time.Time
	stop    chan progressResult
	done    chan struct{}
}

type progressResult struct {
	success bool
	output  string
}

func progressCompletionLabel(label string) string {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "install":
		return "Installation"
	case "remove":
		return "Removal"
	case "updates":
		return "Update"
	case "checking channel":
		return "Channel Check"
	default:
		return label
	}
}

func progressCompletionText(label string, success bool) string {
	text := progressCompletionLabel(label)
	if success {
		return text + " Complete"
	}
	return text + " Failed"
}

func progressActiveLabel(label string) string {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "install":
		return "Installing"
	case "remove":
		return "Removing"
	case "updates":
		return "Updating"
	default:
		return label
	}
}

func progressLine(marker, message string) string {
	if marker == "" {
		return message
	}
	return marker + " " + message
}

func startOperationProgress(label string) *operationProgress {
	info, err := os.Stdout.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	progress := &operationProgress{label: label, started: time.Now(), stop: make(chan progressResult, 1), done: make(chan struct{})}
	go func() {
		defer close(progress.done)
		ticker := time.NewTicker(progressTickInterval)
		defer ticker.Stop()
		colorEnabled := uvColorsEnabled()
		render := func(frame int) {
			spinner := progressFrames[frame%len(progressFrames)]
			if colorEnabled {
				spinner = "\033[" + colorProgressWhite + "m" + spinner + resetColor
			}
			message := progressActiveLabel(label)
			if colorEnabled {
				message = "\033[2m" + message + resetColor
			}
			fmt.Fprintf(os.Stdout, "\r\033[K%s", progressLine(spinner, message))
		}
		for frame := 0; ; frame++ {
			render(frame)
			select {
			case result := <-progress.stop:
				if remaining := progressTickInterval - time.Since(progress.started); remaining > 0 {
					time.Sleep(remaining)
				}
				fmt.Fprint(os.Stdout, "\r\033[K")
				if result.output != "" {
					fmt.Fprint(os.Stdout, result.output)
					if !strings.HasSuffix(result.output, "\n") {
						fmt.Fprintln(os.Stdout)
					}
				}
				if currentCLIContext().Err() == nil {
					message := progressCompletionText(progress.label, result.success)
					if colorEnabled && !result.success {
						message = uvErrorLabel(message)
					}
					fmt.Fprintln(os.Stdout, message)
				}
				return
			case <-ticker.C:
			}
		}
	}()
	return progress
}

func (progress *operationProgress) finish() {
	progress.stopWithResult(progressResult{})
}

func (progress *operationProgress) complete() {
	progress.stopWithResult(progressResult{success: true})
}

func (progress *operationProgress) completeWithOutput(output string) {
	progress.stopWithResult(progressResult{success: true, output: output})
}

func (progress *operationProgress) stopWithResult(result progressResult) {
	if progress == nil {
		return
	}
	progress.stop <- result
	<-progress.done
}

type PackageTransactionItem struct {
	Package       InstalledPackage
	DownloadSize  int64
	InstalledSize int64
	Action        string
}

func renderResolved(count int, elapsed time.Duration) {
	renderResolvedTo(os.Stdout, count, elapsed)
}

func renderResolvedTo(writer io.Writer, count int, elapsed time.Duration) {
	if count == 0 {
		fmt.Fprintln(writer, uvDim("No packages to resolve."))
		return
	}
	fmt.Fprintf(writer, "%s %s %s %s\n", uvAction("Resolved"), uvEmphasis(packageCountLabel(count)), uvDim("in"), uvDim(formatOperationDuration(elapsed)))
}

func renderPackageChanges(packages []PackageTransactionItem, elapsed time.Duration) {
	renderPackageChangesTo(os.Stdout, packages, elapsed)
}

func renderPackageChangesTo(writer io.Writer, packages []PackageTransactionItem, elapsed time.Duration) {
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
	fmt.Fprintf(writer, "%s %s %s %s\n", uvAction(verb), uvEmphasis(packageCountLabel(len(ordered))), uvDim("in"), uvDim(formatOperationDuration(elapsed)))
	for _, item := range ordered {
		fmt.Fprintln(writer, uvPackageChange(item.Action, item.Package))
	}
}

func renderInstallNoop(plan packageInstallPlan) {
	renderInstallNoopTo(os.Stdout, plan)
}

func renderInstallNoopTo(writer io.Writer, plan packageInstallPlan) {
	if len(plan.Roots) > 1 {
		for _, root := range plan.Roots {
			switch {
			case root.HadPrevious && root.Root.Dir == root.Previous.Dir && !root.ExplicitlyAdded:
				fmt.Fprintf(writer, "%s %s\n", uvAction("Already up to date:"), uvPackageRef(root.Root.Name, root.Root.Version))
			case root.HadPrevious && root.Root.Dir == root.Previous.Dir:
				fmt.Fprintf(writer, "%s %s to the profile; package files were already available.\n", uvAction("Added"), uvPackageRef(root.Root.Name, root.Root.Version))
			default:
				fmt.Fprintf(writer, "%s %s; no download was needed.\n", uvDim("Using stored files for"), uvPackageRef(root.Root.Name, root.Root.Version))
			}
		}
		return
	}
	if plan.HadPrevious && plan.Root.Dir == plan.Previous.Dir && !plan.ExplicitlyAdded {
		fmt.Fprintf(writer, "%s %s\n", uvAction("Already up to date:"), uvPackageRef(plan.Root.Name, plan.Root.Version))
		return
	}
	if plan.HadPrevious && plan.Root.Dir == plan.Previous.Dir {
		fmt.Fprintf(writer, "%s %s to the profile; package files were already available.\n", uvAction("Added"), uvPackageRef(plan.Root.Name, plan.Root.Version))
		return
	}
	fmt.Fprintf(writer, "%s %s; no download was needed.\n", uvDim("Using stored files for"), uvPackageRef(plan.Root.Name, plan.Root.Version))
}

func renderUpdateResult(plan packageUpdatePlan, elapsed time.Duration) {
	if len(plan.Changes) == 0 {
		if plan.ResolvedCount == 0 {
			fmt.Println(uvDim("No packages to update."))
			return
		}
		fmt.Println(uvDim("No updates available."))
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
	fmt.Printf("%s %s\n", uvAction(action), item)
}

func renderActionSuccess(action, subject, detail string) {
	if detail == "" {
		fmt.Printf("%s %s\n", uvAction(action), uvEmphasis(subject))
		return
	}
	fmt.Printf("%s %s %s\n", uvAction(action), uvEmphasis(subject), uvDim(detail))
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
	statusCode := fmt.Sprintf("%d", status.Status)
	if status.Status >= 200 && status.Status < 400 {
		statusCode = uvStatusLabel(statusCode)
	} else {
		statusCode = uvErrorLabel(statusCode)
	}
	fmt.Printf("%s %s %s\n", uvDim("Status"), statusCode, status.Reason)
	fmt.Printf("%s %s\n", uvDim("Type"), status.ContentType)
	fmt.Printf("%s %s\n", uvDim("Packages"), uvEmphasis(fmt.Sprintf("%d", status.PackageCount)))
	fmt.Printf("%s %s\n", uvDim("Fetched in"), formatOperationDuration(time.Duration(status.ElapsedMS)*time.Millisecond))
	if finalURL != entry.URL {
		fmt.Printf("%s %s\n", uvDim("Final URL"), finalURL)
	}
}

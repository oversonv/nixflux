package nixflux

import (
	"fmt"
	"os"
	"strings"
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
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		palette := [][3]int{{138, 110, 255}, {105, 130, 255}, {85, 165, 255}, {160, 185, 220}}
		colorEnabled := os.Getenv("NO_COLOR") == ""
		const width = 18
		for frame := 0; ; frame++ {
			start := frame%(width+4) - 4
			var bar strings.Builder
			for i := 0; i < width; i++ {
				if i >= start && i < start+4 {
					if colorEnabled {
						color := palette[(frame+i)%len(palette)]
						fmt.Fprintf(&bar, "\033[38;2;%d;%d;%dm▰\033[0m", color[0], color[1], color[2])
					} else {
						bar.WriteRune('▰')
					}
				} else if colorEnabled {
					bar.WriteString("\033[38;2;65;70;85m─\033[0m")
				} else {
					bar.WriteRune('─')
				}
			}
			spinner := frames[frame%len(frames)]
			labelText := label
			if colorEnabled {
				color := palette[frame%len(palette)]
				spinner = fmt.Sprintf("\033[38;2;%d;%d;%dm%s\033[0m", color[0], color[1], color[2], spinner)
				labelText = progressGradient(label, frame, palette)
			}
			fmt.Fprintf(os.Stdout, "\r  %s %s · [%s]\033[K", spinner, labelText, bar.String())
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

func progressGradient(value string, offset int, palette [][3]int) string {
	var result strings.Builder
	for i, char := range value {
		color := palette[(i/3+offset)%len(palette)]
		fmt.Fprintf(&result, "\033[38;2;%d;%d;%dm%c\033[0m", color[0], color[1], color[2], char)
	}
	return result.String()
}

func (progress *operationProgress) finish() {
	if progress == nil {
		return
	}
	close(progress.stop)
	<-progress.done
}

func renderInstallSuccess(name, version string, started time.Time, dependencyCount int) {
	fmt.Printf("%s %s installed successfully.\n", paint(name), paint(version))
	if dependencyCount > 0 {
		fmt.Printf("Installed %d dependencies.\n", dependencyCount)
	}
	fmt.Printf("Completed in %.2fs\n", time.Since(started).Seconds())
}

func renderRemoveSuccess(name, version string, started time.Time) {
	fmt.Printf("%s %s removed successfully.\n", paint(name), paint(version))
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

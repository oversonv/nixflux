package nixnet

import (
	"strings"
	"testing"
	"time"
)

func TestProgressInstallLabel(t *testing.T) {
	if got := progressCompletionLabel("Install"); got != "Installation" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressRemoveLabel(t *testing.T) {
	if got := progressCompletionLabel("Remove"); got != "Removal" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressUpdateLabel(t *testing.T) {
	if got := progressCompletionLabel("Updates"); got != "Update" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressChannelLabel(t *testing.T) {
	if got := progressCompletionLabel("Checking channel"); got != "Channel Check" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressSuccessText(t *testing.T) {
	if got := progressCompletionText("Install", true); got != "Installation Complete" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressFailureText(t *testing.T) {
	if got := progressCompletionText("Install", false); got != "Installation Failed" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressLineShowsSpinnerBeforeAction(t *testing.T) {
	if got := progressLine("⠋", progressActiveLabel("Install")); got != "⠋ Installing" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressRemoveUsesRemovingLabel(t *testing.T) {
	if got := progressLine("⠋", progressActiveLabel("Remove")); got != "⠋ Removing" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressUpdatesUsesUpdatingLabel(t *testing.T) {
	if got := progressLine("⠋", progressActiveLabel("Updates")); got != "⠋ Updating" {
		t.Fatalf("got %q", got)
	}
}

func TestProgressSpinnerUsesUVWhiteAccent(t *testing.T) {
	if colorProgressWhite != "37" {
		t.Fatalf("unexpected spinner color: %q", colorProgressWhite)
	}
}

func TestProgressSpinnerMatchesUVBrailleFramesAndRate(t *testing.T) {
	want := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	if len(progressFrames) != len(want) {
		t.Fatalf("got %d frames, want %d", len(progressFrames), len(want))
	}
	for i := range want {
		if progressFrames[i] != want[i] {
			t.Fatalf("frame %d = %q, want %q", i, progressFrames[i], want[i])
		}
	}
	if progressTickInterval != 200*time.Millisecond {
		t.Fatalf("tick interval = %s, want 200ms", progressTickInterval)
	}
}

func TestProgressActionLineHasNoIndent(t *testing.T) {
	got := progressLine("⠋", progressActiveLabel("Install"))
	if strings.HasPrefix(got, " ") {
		t.Fatalf("got %q", got)
	}
}

func TestProgressCompletionHasNoSpinnerOrCheckmark(t *testing.T) {
	got := progressCompletionText("Install", true)
	if strings.ContainsAny(got, "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✓✔") {
		t.Fatalf("got %q", got)
	}
}

func TestProgressCompletionHasNoIndent(t *testing.T) {
	if got := progressCompletionText("Install", true); strings.HasPrefix(got, " ") {
		t.Fatalf("got %q", got)
	}
}

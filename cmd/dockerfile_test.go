package main

import (
	"os"
	"regexp"
	"testing"
)

// TestDockerfilePlaywrightVersion guards against go.mod bumps of playwright-go
// that leave the driver baked into the Docker image at the old version, which
// makes playwright.Install() fail inside the container.
func TestDockerfilePlaywrightVersion(t *testing.T) {
	t.Parallel()

	goMod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}

	dockerfile, err := os.ReadFile("../Dockerfile.goreleaser")
	if err != nil {
		t.Fatal(err)
	}

	want := regexp.MustCompile(`github\.com/mxschmitt/playwright-go (v\S+)`).
		FindSubmatch(goMod)
	if want == nil {
		t.Fatal("playwright-go not found in go.mod")
	}

	got := regexp.MustCompile(`playwright-go/cmd/playwright@(v\S+)`).
		FindSubmatch(dockerfile)
	if got == nil {
		t.Fatal("playwright install not found in Dockerfile.goreleaser")
	}

	if string(got[1]) != string(want[1]) {
		t.Errorf(
			"Dockerfile.goreleaser installs playwright-go %s, go.mod has %s",
			got[1], want[1],
		)
	}
}

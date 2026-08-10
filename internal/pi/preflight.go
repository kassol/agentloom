// Package pi provides the process boundary for the Pi Agent Runtime.
package pi

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// MinimumVersion is the oldest Pi RPC version supported by CodexLoom.
const MinimumVersion = "0.84.1"

var minimumVersion = [3]int{0, 84, 1}

// Check verifies that the selected Pi executable satisfies the startup prerequisite.
func Check(bin string) error {
	version, err := Version(bin)
	if err != nil {
		return fmt.Errorf("Pi %s or newer is required: %w", MinimumVersion, err)
	}
	parsed, err := parseVersion(version)
	if err != nil {
		return fmt.Errorf("Pi %s or newer is required: cannot parse `pi --version` output %q", MinimumVersion, version)
	}
	if compareVersion(parsed, minimumVersion) < 0 {
		return fmt.Errorf("Pi %s is too old; Pi %s or newer is required (set PI_BIN to a supported executable or update Pi)", version, MinimumVersion)
	}
	return nil
}

// ResolveBin selects an explicit path, PI_BIN, or pi from PATH, in that order.
func ResolveBin(bin string) (string, error) {
	piBin := strings.TrimSpace(bin)
	if piBin == "" {
		piBin = strings.TrimSpace(os.Getenv("PI_BIN"))
	}
	if piBin != "" {
		return piBin, nil
	}
	resolved, err := exec.LookPath("pi")
	if err != nil {
		return "", fmt.Errorf("pi not found in PATH; set PI_BIN or install @earendil-works/pi-coding-agent globally")
	}
	return resolved, nil
}

// Version returns the selected executable's reported version.
func Version(bin string) (string, error) {
	piBin, err := ResolveBin(bin)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, piBin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("read Pi version from %s: %w", piBin, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func parseVersion(version string) ([3]int, error) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return [3]int{}, fmt.Errorf("invalid version")
	}
	var parsed [3]int
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return [3]int{}, fmt.Errorf("invalid version")
		}
		parsed[i] = value
	}
	return parsed, nil
}

func compareVersion(left, right [3]int) int {
	for i := range left {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	return 0
}

package piopenshell_test

import (
	"os"
	"strings"
	"testing"
)

func TestPrototypePolicyDeclaresNoHostMountsOrNetworkDestinations(t *testing.T) {
	contents, err := os.ReadFile("policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy := string(contents)
	for _, required := range []string{
		"version: 1",
		"read_only:",
		"- /opt/codex-loom",
		"read_write:",
		"- /sandbox",
		"- /tmp",
		"network_policies: {}",
		"run_as_user: sandbox",
	} {
		if !strings.Contains(policy, required) {
			t.Errorf("policy is missing %q", required)
		}
	}
	for _, forbidden := range []string{"/Users", "/home", "~", "host.docker.internal", "0.0.0.0/0"} {
		if strings.Contains(policy, forbidden) {
			t.Errorf("policy exposes forbidden host surface %q", forbidden)
		}
	}
}

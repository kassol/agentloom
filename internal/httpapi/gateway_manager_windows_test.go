//go:build windows

package httpapi

import (
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/hub"
)

func TestWindowsManagedGatewaysAreExplicitlyUnsupported(t *testing.T) {
	var server *Server
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "Feishu", run: func() error {
			_, err := server.installFeishuGateway(hub.PlatformConnection{}, hub.AgentAddress{}, "", "")
			return err
		}},
		{name: "Slack", run: func() error {
			_, err := server.installSlackGateway(hub.PlatformConnection{}, hub.AgentAddress{}, "", "", "", "")
			return err
		}},
		{name: "Parall", run: func() error {
			_, err := server.installParallGateway(hub.PlatformConnection{}, hub.AgentAddress{}, "", "", "")
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unsupported on windows") {
				t.Fatalf("managed %s gateway error = %v", test.name, err)
			}
		})
	}
}

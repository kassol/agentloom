package probe

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
)

type Options struct {
	OpenShellBin string
}

type Report struct {
	State             string `json:"state"`
	Reason            string `json:"reason,omitempty"`
	Detail            string `json:"detail,omitempty"`
	OpenShellPath     string `json:"openshellPath,omitempty"`
	OpenShellVersion  string `json:"openshellVersion,omitempty"`
	HostOS            string `json:"hostOS"`
	HostArch          string `json:"hostArch"`
	HostFallback      string `json:"hostFallback"`
	IsolationEvidence string `json:"isolationEvidence"`
}

func Inspect(ctx context.Context, options Options) Report {
	report := Report{
		HostOS:            runtime.GOOS,
		HostArch:          runtime.GOARCH,
		HostFallback:      "not_implemented",
		IsolationEvidence: "unverified",
	}
	bin := options.OpenShellBin
	if bin == "" {
		bin = "openshell"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		report.State = "unavailable"
		report.Reason = "openshell executable was not found"
		return report
	}
	report.OpenShellPath = path
	version, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		report.State = "unavailable"
		report.Reason = "openshell version check failed"
		report.Detail = strings.TrimSpace(string(version))
		return report
	}
	report.OpenShellVersion = strings.TrimSpace(string(version))
	status, err := exec.CommandContext(ctx, path, "status").CombinedOutput()
	if err != nil {
		report.State = "unavailable"
		report.Reason = "openshell gateway is not reachable"
		report.Detail = strings.TrimSpace(string(status))
		return report
	}
	report.State = "available"
	report.Detail = strings.TrimSpace(string(status))
	return report
}

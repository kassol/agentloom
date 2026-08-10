package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"time"

	"github.com/yan5xu/codex-loom/prototype/pi-openshell/probe"
)

func main() {
	openShellBin := flag.String("openshell-bin", "openshell", "OpenShell CLI executable")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	os.Exit(run(ctx, os.Stdout, *openShellBin))
}

func run(ctx context.Context, output io.Writer, openShellBin string) int {
	report := probe.Inspect(ctx, probe.Options{OpenShellBin: openShellBin})
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return 2
	}
	if report.State != "available" {
		return 1
	}
	return 0
}

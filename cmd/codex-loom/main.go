// codex-loom is the local CodexLoom service. It governs durable Agents whose
// execution history lives in Codex Threads.
//
//	codex-loom [-port 4870] [-data ~/.codex-loom]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/claudegen"
	"github.com/yan5xu/codex-loom/internal/httpapi"
	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/platform"
	"github.com/yan5xu/codex-loom/internal/processlifecycle"
	"github.com/yan5xu/codex-loom/internal/store"
	"github.com/yan5xu/codex-loom/internal/webui"
)

func main() {
	startedAt := time.Now()
	defaultPort := 4870
	p := os.Getenv("CODEX_LOOM_PORT")
	if p == "" {
		p = os.Getenv("CODEX_HUB_PORT")
	}
	if p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			defaultPort = v
		}
	}
	port := flag.Int("port", defaultPort, "listen port")
	dataDir := flag.String("data", store.DefaultDir(), "data directory")
	canary := flag.Bool("canary", false, "run a passive, read-only development canary")
	openBrowser := flag.Bool("open", false, "open the WebUI in the default browser")
	flag.Parse()

	if err := hub.PreflightPiRuntime(context.Background()); err != nil {
		log.Fatal(err)
	}
	st, err := store.OpenWithOptions(*dataDir, store.OpenOptions{ReadOnly: *canary})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	claudeGenerations := claudegen.Default()
	h, err := hub.OpenWithOptions(st, hub.OpenOptions{Passive: *canary, RuntimeAPIURL: fmt.Sprintf("http://127.0.0.1:%d", *port), ClaudeGenerations: claudeGenerations})
	if err != nil {
		_ = st.Close()
		log.Fatalf("open Hub state: %v", err)
	}
	var startup sync.WaitGroup
	mode := "normal"
	if *canary {
		mode = "canary"
	}
	srv := httpapi.NewWithOptions(h, st, webui.FS(), httpapi.Options{StartedAt: startedAt, Mode: mode, ReadOnly: *canary, ClaudeGenerations: claudeGenerations})
	if !*canary {
		startup.Add(3)
		go func() {
			defer startup.Done()
			if err := h.SyncThreadNames(); err != nil {
				log.Printf("[codex-loom] sync Codex Thread names: %v", err)
			}
		}()
		go func() {
			defer startup.Done()
			srv.RestartManagedGateways()
		}()
		go func() {
			defer startup.Done()
			srv.ResumeRestartPausedGoals()
		}()
	}

	listenAddress := fmt.Sprintf(":%d", *port)
	if *canary {
		listenAddress = fmt.Sprintf("127.0.0.1:%d", *port)
	}
	httpServer := &http.Server{
		Addr:    listenAddress,
		Handler: srv.Handler(),
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		reason := processlifecycle.WaitForShutdown()
		log.Printf("[codex-loom] %s — shutting down", reason)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = httpServer.Shutdown(ctx)
		cancel()
		_ = httpServer.Close() // Close long-lived SSE streams after the request grace window.
		srv.StopRuntimeGenerationOperations()
		startup.Wait()
		h.Shutdown()
		if err := st.Close(); err != nil {
			log.Printf("[codex-loom] close Store: %v", err)
		}
	}()

	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		log.Fatal(err)
	}
	webURL := fmt.Sprintf("http://localhost:%d", *port)
	log.Printf("[codex-loom] listening on %s (mode: %s, data: %s)", webURL, mode, *dataDir)
	if *openBrowser {
		if err := platform.OpenBrowser(webURL); err != nil {
			log.Printf("[codex-loom] open browser: %v", err)
		}
	}
	if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-shutdownDone
}

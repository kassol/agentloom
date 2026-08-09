// loom-feishu-gateway connects one Feishu application identity to a
// CodexLoom Connection without requiring lark-cli at runtime.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yan5xu/codex-loom/internal/credentials"
	"github.com/yan5xu/codex-loom/internal/feishu"
	"github.com/yan5xu/codex-loom/internal/feishugw"
	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func main() {
	hubURL := flag.String("hub", envFirst("CODEX_LOOM_URL", "CHUB_URL", "http://127.0.0.1:4870"), "CodexLoom base URL")
	connectionID := flag.String("connection", envFirst("CODEX_LOOM_CONNECTION_ID", "CHUB_CONNECTION_ID"), "integration connection ID")
	addressID := flag.String("address", envFirst("CODEX_LOOM_ADDRESS_ID", "CHUB_ADDRESS_ID"), "Agent address ID")
	appID := flag.String("app-id", os.Getenv("FEISHU_APP_ID"), "Feishu App ID")
	stateFile := flag.String("state-file", "", "gateway state file")
	flag.Parse()

	processProof := gatewayProcessProofFromEnv()
	managedRef := strings.TrimSpace(os.Getenv("CODEX_LOOM_MANAGED_CREDENTIAL_REF"))
	secret := ""
	if managedRef != "" {
		if !credentials.IsManagedRef(managedRef) {
			log.Fatalf("invalid managed credential reference")
		}
		resolved, err := resolveManagedSecret(dataDir(), credentials.Ref(managedRef))
		if err != nil {
			log.Fatalf("resolve managed credential: %v", err)
		}
		secret = string(resolved)
	} else {
		secret = strings.TrimSpace(os.Getenv("FEISHU_APP_SECRET"))
		if secret == "" {
			if inherited, ok := readInheritedCredentialFD(); ok {
				secret = strings.TrimSpace(string(inherited))
			}
		}
		if secret == "" && strings.TrimSpace(*appID) != "" {
			var err error
			secret, err = feishu.LoadAppSecret(*appID)
			if err != nil {
				log.Fatalf("read Feishu App Secret from keychain: %v", err)
			}
		}
	}
	if *stateFile == "" && *connectionID != "" {
		*stateFile = filepath.Join(dataDir(), "gateway", "feishu-"+*connectionID+".json")
	}
	gateway, err := feishugw.New(feishugw.Config{
		HubURL: *hubURL, ConnectionID: *connectionID, AddressID: *addressID,
		AppID: *appID, AppSecret: secret, ConnectorToken: os.Getenv("CODEX_LOOM_CONNECTOR_TOKEN"),
		StateFile: *stateFile, ProcessProof: processProof,
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := gateway.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

// gatewayProcessProofFromEnv reads the private R1 attempt identity that the
// launch plan froze into the unit environment. All four values must appear
// together and be bounded, secret-free strings; the exact proof is returned to
// the Hub only after the provider socket opens.
func gatewayProcessProofFromEnv() *hub.GatewayProcessHeartbeatParams {
	attemptID := strings.TrimSpace(os.Getenv("CODEX_LOOM_GATEWAY_ATTEMPT_ID"))
	generation := strings.TrimSpace(os.Getenv("CODEX_LOOM_GATEWAY_GENERATION"))
	build := strings.TrimSpace(os.Getenv("CODEX_LOOM_GATEWAY_BUILD"))
	digest := strings.TrimSpace(os.Getenv("CODEX_LOOM_GATEWAY_EXECUTABLE_DIGEST"))
	if attemptID == "" && generation == "" && build == "" && digest == "" {
		return nil
	}
	if attemptID == "" || generation == "" || build == "" || digest == "" {
		log.Fatalf("gateway attempt proof identity is incomplete")
	}
	for _, value := range []string{attemptID, generation, build, digest} {
		if len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
			log.Fatalf("gateway attempt proof identity is unbounded or invalid")
		}
	}
	return &hub.GatewayProcessHeartbeatParams{
		AttemptID: attemptID, Generation: generation, Build: build, ExecutableDigest: digest,
	}
}

// resolveManagedSecret opens the C-v1 stable data directory read-only and
// resolves one canonical managed reference. It never writes and never falls
// back to another credential source.
func resolveManagedSecret(dataDir string, ref credentials.Ref) ([]byte, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("managed credential resolution requires a data directory")
	}
	st, err := store.OpenWithOptions(dataDir, store.OpenOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return credentials.ResolveReadOnly(st, ref)
}

func envFirst(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func dataDir() string {
	if value := envFirst("CODEX_LOOM_DATA", "CODEX_HUB_DATA"); value != "" {
		return value
	}
	home, _ := os.UserHomeDir()
	current := filepath.Join(home, ".codex-loom")
	if _, err := os.Stat(current); err == nil {
		return current
	}
	return filepath.Join(home, ".codex-hub")
}

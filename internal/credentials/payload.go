package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/yan5xu/codex-loom/internal/store"
)

const payloadVersion = 1

var ErrConnectionNotManaged = errors.New("Connection does not use a managed credential")

// Payload is the provider-neutral on-disk representation of one managed
// credential. It is only serialized inside the owner-only credential store.
type Payload struct {
	Version  int               `json:"version"`
	Provider string            `json:"provider"`
	Values   map[string]string `json:"values"`
}

// EncodePayload validates and encodes credential material for the managed
// store. Callers must never log or return the resulting bytes.
func EncodePayload(provider string, values map[string]string) ([]byte, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil, fmt.Errorf("credential provider is required")
	}
	clean := make(map[string]string, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			return nil, fmt.Errorf("credential payload contains an empty field")
		}
		clean[key] = value
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("credential payload is empty")
	}
	return json.Marshal(Payload{Version: payloadVersion, Provider: provider, Values: clean})
}

// DecodePayload validates managed credential bytes for one provider.
func DecodePayload(data []byte, provider string) (map[string]string, error) {
	var payload Payload
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("invalid managed credential payload")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if payload.Version != payloadVersion || payload.Provider != provider || len(payload.Values) == 0 {
		return nil, fmt.Errorf("managed credential payload does not match provider %s", provider)
	}
	out := make(map[string]string, len(payload.Values))
	for key, value := range payload.Values {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			return nil, fmt.Errorf("managed credential payload contains an empty field")
		}
		out[key] = value
	}
	return out, nil
}

// ResolveConnectionPayload is the narrow read-only gateway bootstrap path. It
// finds the Connection's canonical managed reference in durable state, verifies
// the owner-only credential file, and returns decoded bytes to the wrapper,
// which must pass them onward through an anonymous descriptor.
func ResolveConnectionPayload(dataDir, connectionID, provider string) (map[string]string, error) {
	st, err := store.OpenWithOptions(dataDir, store.OpenOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer st.Close()
	var config struct {
		Connections map[string]*struct {
			Provider      string `json:"provider"`
			CredentialRef string `json:"credentialRef"`
			Enabled       bool   `json:"enabled"`
			ArchivedAt    string `json:"archivedAt"`
		} `json:"connections"`
	}
	if err := st.LoadIntegrations(&config); err != nil {
		return nil, err
	}
	connection := config.Connections[strings.TrimSpace(connectionID)]
	if connection == nil || !connection.Enabled || connection.ArchivedAt != "" {
		return nil, fmt.Errorf("enabled Connection not found")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	connectionProvider := strings.ToLower(strings.TrimSpace(connection.Provider))
	if !(provider == "lark" && connectionProvider == "feishu") && connectionProvider != provider {
		return nil, fmt.Errorf("Connection provider does not match %s", provider)
	}
	if !IsManagedRef(connection.CredentialRef) {
		return nil, ErrConnectionNotManaged
	}
	data, err := ResolveReadOnly(st, Ref(connection.CredentialRef))
	if err != nil {
		return nil, err
	}
	return DecodePayload(data, provider)
}

package httpapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const onboardingCredentialIndexPath = "credential-onboarding/index.json"

type onboardingCredentialIndex struct {
	Version int               `json:"version"`
	Refs    map[string]string `json:"refs"`
}

func onboardingCredentialKey(provider string, parts ...string) string {
	values := []string{strings.ToLower(strings.TrimSpace(provider))}
	for _, part := range parts {
		values = append(values, strings.TrimSpace(part))
	}
	return strings.Join(values, "\x00")
}

func (s *Server) storeOnboardingCredential(provider, key string, values map[string]string) (string, error) {
	ref, err := s.hub.PutManagedCredential(provider, values)
	if err != nil {
		return "", err
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	index, err := s.readOnboardingCredentialIndex()
	if err != nil {
		_ = s.hub.DeleteManagedCredential(ref)
		return "", err
	}
	index.Refs[key] = ref
	if err := s.writeOnboardingCredentialIndex(index); err != nil {
		_ = s.hub.DeleteManagedCredential(ref)
		return "", err
	}
	return ref, nil
}

func (s *Server) restoreOnboardingCredential(key, previousRef, replacementRef string) error {
	s.credentialMu.Lock()
	index, err := s.readOnboardingCredentialIndex()
	if err == nil {
		if previousRef == "" {
			delete(index.Refs, key)
		} else {
			index.Refs[key] = previousRef
		}
		err = s.writeOnboardingCredentialIndex(index)
	}
	s.credentialMu.Unlock()
	if err != nil {
		return err
	}
	if replacementRef != "" && replacementRef != previousRef && !s.connectionUsesManagedRef(replacementRef) {
		return s.hub.DeleteManagedCredential(replacementRef)
	}
	return nil
}

func (s *Server) loadOnboardingCredential(provider, key string) (string, map[string]string, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	index, err := s.readOnboardingCredentialIndex()
	if err != nil {
		return "", nil, err
	}
	ref := strings.TrimSpace(index.Refs[key])
	if ref == "" {
		return "", nil, nil
	}
	values, err := s.hub.ResolveManagedCredential(ref, provider)
	if err != nil {
		return "", nil, err
	}
	return ref, values, nil
}

func (s *Server) connectionUsesManagedRef(ref string) bool {
	for _, connection := range s.hub.ListConnections() {
		if connection.CredentialRef == ref {
			return true
		}
	}
	return false
}

func (s *Server) readOnboardingCredentialIndex() (onboardingCredentialIndex, error) {
	index := onboardingCredentialIndex{Version: 1, Refs: map[string]string{}}
	data, err := s.st.ReadStableFile(onboardingCredentialIndexPath)
	if os.IsNotExist(err) {
		return index, nil
	}
	if err != nil {
		return index, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&index); err != nil || index.Version != 1 || index.Refs == nil {
		return onboardingCredentialIndex{}, fmt.Errorf("managed onboarding credential index is invalid")
	}
	return index, nil
}

func (s *Server) writeOnboardingCredentialIndex(index onboardingCredentialIndex) error {
	payload, err := json.Marshal(index)
	if err != nil {
		return err
	}
	return s.st.WithStableWriteRoot(func(root *os.Root) error {
		if err := root.MkdirAll(filepath.Dir(onboardingCredentialIndexPath), 0o700); err != nil {
			return err
		}
		temporary := filepath.Join(filepath.Dir(onboardingCredentialIndexPath), ".index.tmp")
		file, err := root.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := file.Write(payload); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := root.Rename(temporary, onboardingCredentialIndexPath); err != nil {
			return err
		}
		directory, err := root.Open(filepath.Dir(onboardingCredentialIndexPath))
		if err != nil {
			return err
		}
		defer directory.Close()
		return directory.Sync()
	})
}

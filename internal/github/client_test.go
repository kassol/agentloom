package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestCredentialRoundTrip(t *testing.T) {
	keyring.MockInit()
	ref, err := SaveToken("Yan5xu", "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "keychain:com.codexloom.github.yan5xu" {
		t.Fatalf("ref = %q", ref)
	}
	token, err := LoadCredential(ref)
	if err != nil || token != "secret-token" {
		t.Fatalf("LoadCredential = %q, %v", token, err)
	}
}

func TestScopedCredentialRoundTrip(t *testing.T) {
	keyring.MockInit()
	firstRef, err := SaveScopedToken("Yan5xu", "parall-hq", "parall-token")
	if err != nil {
		t.Fatal(err)
	}
	secondRef, err := SaveScopedToken("Yan5xu", "yan5xu", "personal-token")
	if err != nil {
		t.Fatal(err)
	}
	if firstRef != "keychain:com.codexloom.github.yan5xu.parall-hq" || secondRef != "keychain:com.codexloom.github.yan5xu.yan5xu" {
		t.Fatalf("refs = %q, %q", firstRef, secondRef)
	}
	first, _ := LoadCredential(firstRef)
	second, _ := LoadCredential(secondRef)
	if first != "parall-token" || second != "personal-token" {
		t.Fatalf("tokens = %q, %q", first, second)
	}
}

func TestClientUsesDirectGitHubAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Fatalf("request = %s headers=%v", r.URL.Path, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"yan5xu","id":7}`))
	}))
	defer server.Close()
	client := NewClient("test-token")
	client.APIURL = server.URL
	user, err := client.GetUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if user.Login != "yan5xu" {
		t.Fatalf("user = %#v", user)
	}
}

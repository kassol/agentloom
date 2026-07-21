package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	githubapi "github.com/yan5xu/codex-loom/internal/github"
)

func TestObserveGitHubPullRequestMatchesHeadChange(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/pulls/12" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":12,"state":"open","merged":false,"updated_at":"2026-07-19T01:02:03Z","head":{"sha":"def456"}}`))
	}))
	defer provider.Close()
	client := githubapi.NewClient("token")
	client.APIURL = provider.URL

	observation, err := observeGitHubTrigger(context.Background(), client, Trigger{
		Provider: "github", ResourceKind: "pull-request",
		Subject:    map[string]string{"owner": "acme", "repo": "widget", "number": "12", "expectedHead": "abc123"},
		Conditions: []TriggerCondition{{Event: "merged"}, {Event: "head-changed"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Matched || observation.Event.Event != "head-changed" || observation.Event.EventKey != "github:pr:acme/widget#12:head:def456" {
		t.Fatalf("pull request observation = %#v", observation)
	}
	if observation.Event.Snapshot["headSha"] != "def456" || observation.Event.OccurredAt != "2026-07-19T01:02:03Z" {
		t.Fatalf("pull request evidence = %#v", observation.Event)
	}
}

func TestObserveGitHubPullRequestTerminalEvents(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		condition  string
		eventKey   string
		occurredAt string
	}{
		{
			name: "merged", condition: "merged",
			body:       `{"number":12,"state":"closed","merged":true,"merged_at":"2026-07-19T01:02:03Z","merge_commit_sha":"merge123","head":{"sha":"abc123"}}`,
			eventKey:   "github:pr:acme/widget#12:merged:merge123",
			occurredAt: "2026-07-19T01:02:03Z",
		},
		{
			name: "closed without merge", condition: "closed",
			body:       `{"number":12,"state":"closed","merged":false,"closed_at":"2026-07-19T01:02:03Z","updated_at":"2026-07-19T01:02:04Z","head":{"sha":"abc123"}}`,
			eventKey:   "github:pr:acme/widget#12:closed:2026-07-19T01:02:04Z",
			occurredAt: "2026-07-19T01:02:03Z",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer provider.Close()
			client := githubapi.NewClient("token")
			client.APIURL = provider.URL
			observation, err := observeGitHubTrigger(context.Background(), client, Trigger{
				Provider: "github", ResourceKind: "pull-request",
				Subject:    map[string]string{"owner": "acme", "repo": "widget", "number": "12"},
				Conditions: []TriggerCondition{{Event: test.condition}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !observation.Matched || observation.Event.Event != test.condition || observation.Event.EventKey != test.eventKey || observation.Event.OccurredAt != test.occurredAt {
				t.Fatalf("terminal pull request observation = %#v", observation)
			}
		})
	}
}

func TestObserveGitHubWorkflowMatchesSuccessfulCompletion(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/actions/runs/42" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"name":"verify","status":"completed","conclusion":"success","head_sha":"abc123","updated_at":"2026-07-19T01:02:03Z"}`))
	}))
	defer provider.Close()
	client := githubapi.NewClient("token")
	client.APIURL = provider.URL

	observation, err := observeGitHubTrigger(context.Background(), client, Trigger{
		Provider: "github", ResourceKind: "workflow-run",
		Subject:    map[string]string{"owner": "acme", "repo": "widget", "runId": "42"},
		Conditions: []TriggerCondition{{Event: "success"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Matched || observation.Event.Event != "success" || observation.Event.EventKey != "github:workflow:42:completed:success" {
		t.Fatalf("workflow observation = %#v", observation)
	}
}

func TestObserveGitHubTriggerTreatsNotFoundAsPermanent(t *testing.T) {
	provider := httptest.NewServer(http.NotFoundHandler())
	defer provider.Close()
	client := githubapi.NewClient("token")
	client.APIURL = provider.URL

	observation, err := observeGitHubTrigger(context.Background(), client, Trigger{
		Provider: "github", ResourceKind: "pull-request",
		Subject:    map[string]string{"owner": "acme", "repo": "missing", "number": "12"},
		Conditions: []TriggerCondition{{Event: "merged"}},
	})
	if err == nil || !observation.Permanent {
		t.Fatalf("not-found observation = %#v, err = %v", observation, err)
	}
}

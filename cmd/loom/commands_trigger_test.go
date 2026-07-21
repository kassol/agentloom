package main

import "testing"

func TestParseGitHubTriggerSubjects(t *testing.T) {
	pull, err := parseTriggerSubject("github", "pull-request", "owner/repo#1970", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if pull["owner"] != "owner" || pull["repo"] != "repo" || pull["number"] != "1970" || pull["expectedHead"] != "abc123" {
		t.Fatalf("pull subject = %#v", pull)
	}
	run, err := parseTriggerSubject("github", "workflow-run", "owner/repo/12345", "")
	if err != nil {
		t.Fatal(err)
	}
	if run["owner"] != "owner" || run["repo"] != "repo" || run["runId"] != "12345" {
		t.Fatalf("workflow subject = %#v", run)
	}
	for _, value := range []string{"owner/repo", "owner/repo#0", "owner/#12"} {
		if _, err := parseTriggerSubject("github", "pull-request", value, ""); err == nil {
			t.Fatalf("accepted invalid pull request %q", value)
		}
	}
	for _, test := range []struct {
		kind  string
		value string
	}{
		{kind: "pull-request", value: "https://github.com/owner/repo/pull/12"},
		{kind: "workflow-run", value: "https://github.com/owner/repo/actions/runs/123"},
		{kind: "workflow-run", value: "owner/repo/123/extra"},
	} {
		if _, err := parseTriggerSubject("github", test.kind, test.value, ""); err == nil {
			t.Fatalf("accepted invalid %s subject %q", test.kind, test.value)
		}
	}
}

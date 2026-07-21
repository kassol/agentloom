package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPIURL    = "https://api.github.com"
	defaultDeviceURL = "https://github.com/login/device/code"
	defaultTokenURL  = "https://github.com/login/oauth/access_token"
)

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Status == 0 {
		return e.Message
	}
	return fmt.Sprintf("GitHub API %d: %s", e.Status, e.Message)
}

type Client struct {
	HTTPClient *http.Client
	APIURL     string
	Token      string
}

type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Name  string `json:"name"`
}

type PullRequest struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergedAt       string `json:"merged_at"`
	ClosedAt       string `json:"closed_at"`
	UpdatedAt      string `json:"updated_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadSHA    string `json:"head_sha"`
	Event      string `json:"event"`
	UpdatedAt  string `json:"updated_at"`
}

func NewClient(token string) *Client {
	return &Client{HTTPClient: &http.Client{Timeout: 20 * time.Second}, APIURL: defaultAPIURL, Token: strings.TrimSpace(token)}
}

func (c *Client) GetUser(ctx context.Context) (User, error) {
	var user User
	err := c.get(ctx, "/user", &user)
	if err == nil && strings.TrimSpace(user.Login) == "" {
		err = fmt.Errorf("GitHub returned an empty login")
	}
	return user, err
}

func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	var pull PullRequest
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls/" + strconv.Itoa(number)
	return pull, c.get(ctx, path, &pull)
}

func (c *Client) GetWorkflowRun(ctx context.Context, owner, repo string, runID int64) (WorkflowRun, error) {
	var run WorkflowRun
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/actions/runs/" + strconv.FormatInt(runID, 10)
	return run, c.get(ctx, path, &run)
}

func (c *Client) get(ctx context.Context, path string, target any) error {
	base := strings.TrimRight(strings.TrimSpace(c.APIURL), "/")
	if base == "" {
		base = defaultAPIURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "CodexLoom")
	if token := strings.TrimSpace(c.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &body)
		if body.Message == "" {
			body.Message = strings.TrimSpace(string(data))
		}
		return &APIError{Status: resp.StatusCode, Message: body.Message}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}

type DeviceAuthorization struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type DeviceTokenResult struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func StartDeviceFlow(ctx context.Context, httpClient *http.Client, clientID, scope string) (DeviceAuthorization, error) {
	values := url.Values{"client_id": {strings.TrimSpace(clientID)}}
	if scope = strings.TrimSpace(scope); scope != "" {
		values.Set("scope", scope)
	}
	var result DeviceAuthorization
	if err := oauthPost(ctx, httpClient, deviceEndpoint(), values, &result); err != nil {
		return result, err
	}
	if result.DeviceCode == "" || result.UserCode == "" || result.VerificationURI == "" {
		return result, fmt.Errorf("GitHub returned an incomplete device authorization")
	}
	if result.Interval < 5 {
		result.Interval = 5
	}
	return result, nil
}

func PollDeviceFlow(ctx context.Context, httpClient *http.Client, clientID, deviceCode string) (DeviceTokenResult, error) {
	values := url.Values{
		"client_id":   {strings.TrimSpace(clientID)},
		"device_code": {strings.TrimSpace(deviceCode)},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	var result DeviceTokenResult
	err := oauthPost(ctx, httpClient, tokenEndpoint(), values, &result)
	return result, err
}

func oauthPost(ctx context.Context, httpClient *http.Client, endpoint string, values url.Values, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "CodexLoom")
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode GitHub OAuth response: %w", err)
	}
	return nil
}

func deviceEndpoint() string {
	if value := strings.TrimSpace(getenv("CODEX_LOOM_GITHUB_DEVICE_URL")); value != "" {
		return value
	}
	return defaultDeviceURL
}

func tokenEndpoint() string {
	if value := strings.TrimSpace(getenv("CODEX_LOOM_GITHUB_TOKEN_URL")); value != "" {
		return value
	}
	return defaultTokenURL
}

var getenv = os.Getenv

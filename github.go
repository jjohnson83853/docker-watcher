package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxGitHubResponseBytes = 8 << 20

type RepositoryOwner struct {
	Login string `json:"login"`
}
type Repository struct {
	Name          string          `json:"name"`
	FullName      string          `json:"full_name"`
	CloneURL      string          `json:"clone_url"`
	DefaultBranch string          `json:"default_branch"`
	PushedAt      string          `json:"pushed_at"`
	Owner         RepositoryOwner `json:"owner"`
}

type RepositoryClient interface {
	ListOwnerRepos(context.Context, string) ([]Repository, error)
	HasDockerfile(context.Context, string, string, string) (bool, error)
	LatestCommitSHA(context.Context, string, string, string) (string, error)
}

type GitHubClient struct {
	baseURL string
	token   string
	http    *http.Client
	logs    io.Writer
}

func NewGitHubClient(baseURL, token string, client *http.Client, logs io.Writer) *GitHubClient {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitHubClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: client, logs: logs}
}

func (c *GitHubClient) get(ctx context.Context, endpoint string, quiet404 bool, target any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("github request failed")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("github request failed")
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	if status != http.StatusOK {
		if c.logs != nil && !(quiet404 && status == http.StatusNotFound) {
			WriteLog(c.logs, "error", "github_api_error", map[string]any{"status": status, "url": req.URL.String()})
		}
		return status, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubResponseBytes+1))
	if err != nil || len(body) > maxGitHubResponseBytes {
		return status, fmt.Errorf("github response invalid")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return status, fmt.Errorf("github response invalid")
	}
	return status, nil
}

func (c *GitHubClient) ListOwnerRepos(ctx context.Context, username string) ([]Repository, error) {
	var repos []Repository
	for page := 1; ; page++ {
		var batch []Repository
		path := fmt.Sprintf("/user/repos?affiliation=owner&per_page=100&page=%d", page)
		status, err := c.get(ctx, path, false, &batch)
		if err != nil {
			return nil, err
		}
		if len(batch) > 100 {
			return nil, fmt.Errorf("github response invalid")
		}
		if status != http.StatusOK || len(batch) == 0 {
			break
		}
		repos = append(repos, batch...)
		if len(batch) < 100 {
			break
		}
	}
	owned := make([]Repository, 0, len(repos))
	for _, repo := range repos {
		if strings.EqualFold(repo.Owner.Login, username) {
			owned = append(owned, repo)
		}
	}
	if c.logs != nil {
		WriteLog(c.logs, "info", "repos_found", map[string]any{"count": len(owned)})
	}
	return owned, nil
}

func (c *GitHubClient) HasDockerfile(ctx context.Context, owner, repo, branch string) (bool, error) {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contents/Dockerfile?ref=" + url.QueryEscape(branch)
	status, err := c.get(ctx, path, true, &map[string]any{})
	return status == http.StatusOK, err
}

func (c *GitHubClient) LatestCommitSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/commits/" + url.PathEscape(branch)
	var result struct {
		SHA string `json:"sha"`
	}
	status, err := c.get(ctx, path, false, &result)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("github commit request failed")
	}
	if result.SHA == "" {
		return "", fmt.Errorf("github commit response invalid")
	}
	return result.SHA, nil
}

func repositoryStateKey(state State, repo Repository) string {
	if repo.Name != "" {
		if _, ok := state[repo.Name]; ok {
			return repo.Name
		}
	}
	if repo.FullName != "" {
		if _, ok := state[repo.FullName]; ok {
			return repo.FullName
		}
	}
	if repo.Name != "" {
		return repo.Name
	}
	return repo.FullName
}

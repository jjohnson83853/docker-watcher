package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testGitHubResponseLimit = 8 << 20

func TestGitHubResponseRejectsBodyBeyondPerResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "[]", strings.Repeat(" ", testGitHubResponseLimit))
	}))
	defer server.Close()
	client := NewGitHubClient(server.URL, "test-token", server.Client(), nil)
	repos, err := client.ListOwnerRepos(context.Background(), "owner")
	if err == nil || repos != nil {
		t.Fatalf("oversized GitHub response returned repositories=%#v, err=%v; want rejection with no partial result", repos, err)
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Fatalf("GitHub response error disclosed credentials: %v", err)
	}
}

func TestGitHubRepositoryListRejectsOversizedPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(w).Encode([]Repository{})
			return
		}
		batch := make([]Repository, 101)
		for index := range batch {
			batch[index] = Repository{Name: fmt.Sprintf("repo-%03d", index), Owner: RepositoryOwner{Login: "owner"}}
		}
		_ = json.NewEncoder(w).Encode(batch)
	}))
	defer server.Close()
	client := NewGitHubClient(server.URL, "test-token", server.Client(), nil)
	repos, err := client.ListOwnerRepos(context.Background(), "owner")
	if err == nil || repos != nil {
		t.Fatalf("oversized page returned repositories=%d, err=%v; want rejection with no partial result", len(repos), err)
	}
}

func TestGitHubPaginationKeepsFullResultWhenAggregateExceedsPageLimit(t *testing.T) {
	var aggregateResponseBytes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		page := request.URL.Query().Get("page")
		if page < "1" || page > "3" {
			_ = json.NewEncoder(w).Encode([]Repository{})
			return
		}
		batch := make([]map[string]any, 100)
		for index := range batch {
			name := fmt.Sprintf("repo-%s-%03d", page, index)
			batch[index] = map[string]any{
				"name":           name,
				"full_name":      "owner/" + name,
				"clone_url":      "https://github.com/owner/" + name + ".git",
				"default_branch": "main",
				"owner":          map[string]string{"login": "owner"},
				"extra":          strings.Repeat("x", 50_000),
			}
		}
		var body bytes.Buffer
		if err := json.NewEncoder(&body).Encode(batch); err != nil {
			t.Errorf("encode page %s: %v", page, err)
			return
		}
		aggregateResponseBytes += body.Len()
		if _, err := w.Write(body.Bytes()); err != nil {
			t.Errorf("write page %s: %v", page, err)
		}
	}))
	defer server.Close()
	client := NewGitHubClient(server.URL, "test-token", server.Client(), nil)
	repos, err := client.ListOwnerRepos(context.Background(), "owner")
	if err != nil {
		t.Fatalf("complete multi-page response rejected: %v", err)
	}
	if len(repos) != 300 {
		t.Fatalf("complete pagination returned %d repositories; want 300", len(repos))
	}
	if aggregateResponseBytes <= testGitHubResponseLimit {
		t.Fatalf("aggregate response bytes %d did not exceed individual limit %d", aggregateResponseBytes, testGitHubResponseLimit)
	}
}

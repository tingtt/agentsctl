package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGitHubAPI = "https://api.github.com"
	maxResponseBody  = 8 << 20
)

// HTTPDoer is the portion of http.Client used by GitHubStore.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// TokenSource resolves a GitHub credential without exposing it to the domain.
type TokenSource interface {
	Token(context.Context) (string, error)
}

// Repository identifies the GitHub repository used by a Store.
type Repository struct {
	Owner string
	Name  string
}

// GitHubStore provides Issue-level comment persistence and trust facts.
type GitHubStore struct {
	Repository Repository
	HTTP       HTTPDoer
	Tokens     TokenSource
	APIBase    string
}

// ListComments retrieves every current comment for an Issue, following REST
// pagination until no next link remains.
func (s GitHubStore) ListComments(ctx context.Context, issue int64) ([]Comment, error) {
	if issue <= 0 {
		return nil, errors.New("issue number must be positive")
	}
	next := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments?per_page=100", s.apiBase(), url.PathEscape(s.Repository.Owner), url.PathEscape(s.Repository.Name), issue)
	var comments []Comment
	for next != "" {
		var page []githubComment
		response, err := s.request(ctx, http.MethodGet, next, nil, &page)
		if err != nil {
			return nil, fmt.Errorf("list issue comments: %w", err)
		}
		for _, item := range page {
			comments = append(comments, item.domain())
		}
		next = nextLink(response.Header.Get("Link"))
	}
	return comments, nil
}

// CreateComment creates an Issue comment and returns its domain representation.
func (s GitHubStore) CreateComment(ctx context.Context, issue int64, body string) (Comment, error) {
	if issue <= 0 {
		return Comment{}, errors.New("issue number must be positive")
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", s.apiBase(), url.PathEscape(s.Repository.Owner), url.PathEscape(s.Repository.Name), issue)
	var result githubComment
	_, err := s.request(ctx, http.MethodPost, endpoint, map[string]string{"body": body}, &result)
	if err != nil {
		return Comment{}, fmt.Errorf("create issue comment: %w", err)
	}
	return result.domain(), nil
}

// UpdateComment replaces an Issue comment body.
func (s GitHubStore) UpdateComment(ctx context.Context, id CommentID, body string) (Comment, error) {
	if id <= 0 {
		return Comment{}, errors.New("comment ID must be positive")
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d", s.apiBase(), url.PathEscape(s.Repository.Owner), url.PathEscape(s.Repository.Name), id)
	var result githubComment
	_, err := s.request(ctx, http.MethodPatch, endpoint, map[string]string{"body": body}, &result)
	if err != nil {
		return Comment{}, fmt.Errorf("update issue comment: %w", err)
	}
	return result.domain(), nil
}

// AuthenticatedUser returns the identity associated with the current token.
func (s GitHubStore) AuthenticatedUser(ctx context.Context) (User, error) {
	var result githubUser
	_, err := s.request(ctx, http.MethodGet, s.apiBase()+"/user", nil, &result)
	if err != nil {
		return User{}, fmt.Errorf("get authenticated user: %w", err)
	}
	return result.domain(), nil
}

// RepositoryPermission returns a user's effective repository permission.
func (s GitHubStore) RepositoryPermission(ctx context.Context, login string) (Permission, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/collaborators/%s/permission", s.apiBase(), url.PathEscape(s.Repository.Owner), url.PathEscape(s.Repository.Name), url.PathEscape(login))
	var result struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	_, err := s.request(ctx, http.MethodGet, endpoint, nil, &result)
	if err != nil {
		return Permission{}, fmt.Errorf("get repository permission: %w", err)
	}
	return Permission{Permission: result.Permission, RoleName: result.RoleName}, nil
}

// CommentEditor returns the final editor for an edited Issue comment. found is
// false when GitHub has no edit record; callers must fail closed.
func (s GitHubStore) CommentEditor(ctx context.Context, nodeID string) (User, bool, error) {
	query := `query($id: ID!) { node(id: $id) { ... on IssueComment { userContentEdits(last: 1) { nodes { editor { login __typename } } } } } }`
	payload := map[string]any{"query": query, "variables": map[string]string{"id": nodeID}}
	var result struct {
		Data struct {
			Node *struct {
				Edits struct {
					Nodes []struct {
						Editor *struct {
							Login    string `json:"login"`
							TypeName string `json:"__typename"`
						} `json:"editor"`
					} `json:"nodes"`
				} `json:"userContentEdits"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_, err := s.request(ctx, http.MethodPost, s.apiBase()+"/graphql", payload, &result)
	if err != nil {
		return User{}, false, fmt.Errorf("get comment editor: %w", err)
	}
	if len(result.Errors) != 0 {
		return User{}, false, errors.New("GitHub GraphQL returned an error")
	}
	if result.Data.Node == nil || len(result.Data.Node.Edits.Nodes) == 0 {
		return User{}, false, nil
	}
	editor := result.Data.Node.Edits.Nodes[len(result.Data.Node.Edits.Nodes)-1].Editor
	if editor == nil {
		return User{}, false, nil
	}
	return User{Login: editor.Login, Type: editor.TypeName}, true, nil
}

func (s GitHubStore) request(ctx context.Context, method, endpoint string, input, output any) (*http.Response, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	token, err := s.token(ctx)
	if err != nil {
		return nil, errors.New("resolve GitHub credentials")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "agentsctl")
	req.Header.Set("Authorization", "Bearer "+token)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	doer := s.HTTP
	if doer == nil {
		doer = http.DefaultClient
	}
	response, err := doer.Do(req)
	if err != nil {
		return nil, errors.New("GitHub request failed")
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxResponseBody+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("read GitHub response")
	}
	if len(data) > maxResponseBody {
		return nil, errors.New("GitHub response exceeds size limit")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("GitHub request returned %s", response.Status)
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			return nil, errors.New("decode GitHub response")
		}
	}
	return response, nil
}

func (s GitHubStore) token(ctx context.Context) (string, error) {
	if s.Tokens == nil {
		s.Tokens = CredentialSource{}
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return "", errors.New("GitHub credential unavailable")
	}
	return strings.TrimSpace(token), nil
}

func (s GitHubStore) apiBase() string {
	if s.APIBase != "" {
		return strings.TrimRight(s.APIBase, "/")
	}
	return defaultGitHubAPI
}

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		pieces := strings.Split(part, ";")
		if len(pieces) < 2 || !strings.Contains(pieces[1], `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(pieces[0]), "<>")
	}
	return ""
}

type githubUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (u githubUser) domain() User { return User{Login: u.Login, Type: u.Type} }

type githubComment struct {
	ID                int64      `json:"id"`
	NodeID            string     `json:"node_id"`
	Body              string     `json:"body"`
	HTMLURL           string     `json:"html_url"`
	User              githubUser `json:"user"`
	AuthorAssociation string     `json:"author_association"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func (c githubComment) domain() Comment {
	return Comment{ID: CommentID(c.ID), NodeID: c.NodeID, Body: c.Body, HTMLURL: c.HTMLURL, Author: c.User.domain(), AuthorAssociation: c.AuthorAssociation, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// StaticToken is an injectable TokenSource useful for configured credentials
// and tests. Its String form intentionally never reveals the token.
type StaticToken string

// Token returns the configured token.
func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

func (t StaticToken) String() string { return "[redacted GitHub token]" }

// ParseCommentID converts a decimal REST comment ID without assuming 32-bit
// integer width.
func ParseCommentID(value string) (CommentID, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid GitHub comment ID")
	}
	return CommentID(id), nil
}

package work

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestGitHubStorePaginationAndLargeIDs(t *testing.T) {
	const largeID = int64(5_872_981_699)
	requests := 0
	store := testStore(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Header.Get("Authorization") != "Bearer secret-token" {
			t.Fatalf("authorization=%q", req.Header.Get("Authorization"))
		}
		if requests == 1 {
			return response(200, `[{"id":5872981699,"node_id":"node","body":"one","user":{"login":"alice","type":"User"},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`, `<https://api.test/page2>; rel="next"`), nil
		}
		if req.URL.String() != "https://api.test/page2" {
			t.Fatalf("second URL=%s", req.URL)
		}
		return response(200, `[{"id":5872981700,"body":"two","user":{"login":"alice","type":"User"},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`, ""), nil
	}))
	comments, err := store.ListComments(context.Background(), 104)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 || int64(comments[0].ID) != largeID || requests != 2 {
		t.Fatalf("comments=%+v requests=%d", comments, requests)
	}
}

func TestGitHubStoreCreateAndUpdate(t *testing.T) {
	var methods []string
	store := testStore(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		methods = append(methods, req.Method)
		data, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(data), `"body":"visible"`) {
			t.Fatalf("body=%s", data)
		}
		return response(200, `{"id":5872981699,"body":"visible","user":{"login":"alice","type":"User"},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`, ""), nil
	}))
	created, err := store.CreateComment(context.Background(), 104, "visible")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.UpdateComment(context.Background(), created.ID, "visible")
	if err != nil {
		t.Fatal(err)
	}
	if methods[0] != http.MethodPost || methods[1] != http.MethodPatch || updated.ID != created.ID {
		t.Fatalf("methods=%v created=%+v updated=%+v", methods, created, updated)
	}
}

func TestGitHubStoreIdentityPermissionAndEditor(t *testing.T) {
	store := testStore(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Path == "/user":
			return response(200, `{"login":"alice","type":"User"}`, ""), nil
		case strings.HasSuffix(req.URL.Path, "/collaborators/alice/permission"):
			return response(200, `{"permission":"write","role_name":"maintain"}`, ""), nil
		case req.URL.Path == "/graphql":
			data, _ := io.ReadAll(req.Body)
			if !strings.Contains(string(data), `"id":"node-1"`) {
				t.Fatalf("graphql body=%s", data)
			}
			return response(200, `{"data":{"node":{"userContentEdits":{"nodes":[{"editor":{"login":"alice","__typename":"User"}}]}}}}`, ""), nil
		default:
			t.Fatalf("unexpected URL %s", req.URL)
			return nil, nil
		}
	}))
	user, err := store.AuthenticatedUser(context.Background())
	if err != nil || user.Login != "alice" {
		t.Fatalf("user=%+v err=%v", user, err)
	}
	permission, err := store.RepositoryPermission(context.Background(), "alice")
	if err != nil || permission.RoleName != "maintain" {
		t.Fatalf("permission=%+v err=%v", permission, err)
	}
	editor, found, err := store.CommentEditor(context.Background(), "node-1")
	if err != nil || !found || editor.Login != "alice" {
		t.Fatalf("editor=%+v found=%v err=%v", editor, found, err)
	}
}

func TestGitHubStoreFailuresRedactToken(t *testing.T) {
	cases := []struct {
		name string
		do   roundTripFunc
	}{
		{"non-2xx", func(*http.Request) (*http.Response, error) {
			return response(403, `{"message":"secret-token denied"}`, ""), nil
		}},
		{"malformed JSON", func(*http.Request) (*http.Response, error) { return response(200, `{`, ""), nil }},
		{"transport", func(*http.Request) (*http.Response, error) { return nil, errors.New("secret-token transport") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(tc.do)
			_, err := store.ListComments(context.Background(), 1)
			if err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCredentialSourcePrecedenceAndRedactedFailure(t *testing.T) {
	source := CredentialSource{LookupEnv: func(name string) (string, bool) {
		if name == "GH_TOKEN" {
			return " env-token ", true
		}
		return "", false
	}}
	token, err := source.Token(context.Background())
	if err != nil || token != "env-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}

	failing := CredentialSource{LookupEnv: func(string) (string, bool) { return "", false }, Runner: fakeCommandRunner{err: errors.New("sensitive")}}
	_, err = failing.Token(context.Background())
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("error=%v", err)
	}
}

type fakeCommandRunner struct{ err error }

func (f fakeCommandRunner) Output(context.Context, string, ...string) ([]byte, error) {
	return nil, f.err
}

func testStore(doer HTTPDoer) GitHubStore {
	return GitHubStore{Repository: Repository{Owner: "owner", Name: "repo"}, HTTP: doer, Tokens: StaticToken("secret-token"), APIBase: "https://api.test"}
}

func response(status int, body, link string) *http.Response {
	header := make(http.Header)
	if link != "" {
		header.Set("Link", link)
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

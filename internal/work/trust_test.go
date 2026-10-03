package work

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeTrustLookup struct {
	permissions   map[string]Permission
	permissionErr map[string]error
	editor        User
	editorFound   bool
	editorErr     error
}

func (f fakeTrustLookup) RepositoryPermission(_ context.Context, login string) (Permission, error) {
	if err := f.permissionErr[login]; err != nil {
		return Permission{}, err
	}
	return f.permissions[login], nil
}

func (f fakeTrustLookup) CommentEditor(context.Context, string) (User, bool, error) {
	return f.editor, f.editorFound, f.editorErr
}

func TestTrustValidator(t *testing.T) {
	now := time.Now()
	base := Comment{NodeID: "node", Author: User{Login: "alice", Type: "User"}, CreatedAt: now, UpdatedAt: now}
	cases := []struct {
		name       string
		comment    Comment
		lookup     fakeTrustLookup
		want       bool
		wantReason TrustReason
	}{
		{"allowed write", base, trustLookup(Permission{RoleName: "write", Permission: "write"}), true, TrustEstablished},
		{"disallowed login", withAuthor(base, User{Login: "mallory", Type: "User"}), trustLookup(Permission{RoleName: "write"}), false, TrustLoginNotAllowed},
		{"bot", withAuthor(base, User{Login: "alice", Type: "Bot"}), trustLookup(Permission{RoleName: "write"}), false, TrustAuthorNotUser},
		{"read only", base, trustLookup(Permission{RoleName: "read", Permission: "read"}), false, TrustPermissionReadOnly},
		{"maintain", base, trustLookup(Permission{RoleName: "maintain", Permission: "write"}), true, TrustEstablished},
		{"admin", base, trustLookup(Permission{RoleName: "admin", Permission: "admin"}), true, TrustEstablished},
		{"custom role write", base, trustLookup(Permission{RoleName: "release-manager", Permission: "write"}), true, TrustEstablished},
		{"permission failure", base, fakeTrustLookup{permissionErr: map[string]error{"alice": errors.New("offline")}}, false, TrustPermissionUnknown},
		{"trusted editor", edited(base), editedLookup(User{Login: "alice", Type: "User"}, Permission{RoleName: "write"}), true, TrustEstablished},
		{"untrusted editor", edited(base), editedLookup(User{Login: "mallory", Type: "User"}, Permission{RoleName: "write"}), false, TrustEditorLoginDenied},
		{"editor lookup failure", edited(base), fakeTrustLookup{permissions: map[string]Permission{"alice": {RoleName: "write"}}, editorErr: errors.New("offline")}, false, TrustEditorUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			validator := TrustValidator{AllowedLogins: map[string]struct{}{"alice": {}}, Lookup: tc.lookup}
			got := validator.Validate(context.Background(), tc.comment)
			if got.Trusted != tc.want || got.Reason != tc.wantReason {
				t.Fatalf("trust=%+v, want trusted=%v reason=%s", got, tc.want, tc.wantReason)
			}
		})
	}
}

func trustLookup(permission Permission) fakeTrustLookup {
	return fakeTrustLookup{permissions: map[string]Permission{"alice": permission}}
}
func editedLookup(editor User, permission Permission) fakeTrustLookup {
	return fakeTrustLookup{permissions: map[string]Permission{"alice": permission}, editor: editor, editorFound: true}
}
func withAuthor(comment Comment, user User) Comment { comment.Author = user; return comment }
func edited(comment Comment) Comment {
	comment.UpdatedAt = comment.CreatedAt.Add(time.Second)
	return comment
}

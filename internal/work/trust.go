package work

import (
	"context"
	"fmt"
)

// Permission is GitHub's effective repository permission response.
type Permission struct {
	Permission string
	RoleName   string
}

// TrustLookup supplies external facts needed to validate authors and editors.
type TrustLookup interface {
	RepositoryPermission(context.Context, string) (Permission, error)
	CommentEditor(context.Context, string) (User, bool, error)
}

// TrustValidator validates comments independently from protocol syntax.
type TrustValidator struct {
	AllowedLogins map[string]struct{}
	Lookup        TrustLookup
}

// Validate fails closed unless the author and, for edited comments, final
// editor are allowed users with effective write-or-higher permission.
func (v TrustValidator) Validate(ctx context.Context, comment Comment) Trust {
	if comment.Author.Type != "User" {
		return Trust{Reason: TrustAuthorNotUser, Detail: comment.Author.Type}
	}
	if !v.allowed(comment.Author.Login) {
		return Trust{Reason: TrustLoginNotAllowed, Detail: comment.Author.Login}
	}
	permission, err := v.permission(ctx, comment.Author.Login)
	if err != nil {
		return Trust{Reason: TrustPermissionUnknown, Detail: err.Error()}
	}
	if !WriteOrHigher(permission) {
		return Trust{Reason: TrustPermissionReadOnly, Detail: permission.RoleName}
	}
	if comment.UpdatedAt.Equal(comment.CreatedAt) {
		return Trust{Trusted: true, Reason: TrustEstablished}
	}
	if v.Lookup == nil || comment.NodeID == "" {
		return Trust{Reason: TrustEditorUnknown, Detail: "edited comment has no editor lookup"}
	}
	editor, found, err := v.Lookup.CommentEditor(ctx, comment.NodeID)
	if err != nil || !found {
		detail := "editor was not returned"
		if err != nil {
			detail = err.Error()
		}
		return Trust{Reason: TrustEditorUnknown, Detail: detail}
	}
	if editor.Type != "User" {
		return Trust{Reason: TrustEditorNotUser, Detail: editor.Type}
	}
	if !v.allowed(editor.Login) {
		return Trust{Reason: TrustEditorLoginDenied, Detail: editor.Login}
	}
	permission, err = v.permission(ctx, editor.Login)
	if err != nil {
		return Trust{Reason: TrustEditorUnknown, Detail: err.Error()}
	}
	if !WriteOrHigher(permission) {
		return Trust{Reason: TrustEditorReadOnly, Detail: permission.RoleName}
	}
	return Trust{Trusted: true, Reason: TrustEstablished}
}

func (v TrustValidator) allowed(login string) bool {
	_, ok := v.AllowedLogins[login]
	return ok
}

func (v TrustValidator) permission(ctx context.Context, login string) (Permission, error) {
	if v.Lookup == nil {
		return Permission{}, fmt.Errorf("permission lookup unavailable")
	}
	return v.Lookup.RepositoryPermission(ctx, login)
}

// WriteOrHigher applies GitHub's standard-role and custom-role semantics.
func WriteOrHigher(permission Permission) bool {
	switch permission.RoleName {
	case "write", "maintain", "admin":
		return true
	case "read", "triage", "none":
		return false
	default:
		return permission.Permission == "write" || permission.Permission == "admin"
	}
}

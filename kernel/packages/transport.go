package packages

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// GitTransport is an invocation-scoped source. Credentials belong to that
// source, independently of the repository's configured upstream and secret.
type GitTransport struct {
	Source, Username, Secret string
}

type gitTransportKey struct{}

func WithGitTransport(ctx context.Context, route GitTransport) (context.Context, error) {
	source, _, _, err := normalizePackageSource(route.Source)
	if err != nil {
		return ctx, err
	}
	route.Source = source
	return context.WithValue(ctx, gitTransportKey{}, route), nil
}

func (s *Store) transportAuthentication(route GitTransport) ([]string, string, error) {
	route.Secret = strings.TrimSpace(route.Secret)
	if route.Secret == "" && route.Username == "" {
		return nil, "", nil
	}
	if s.secrets == nil || route.Secret == "" {
		return nil, "", fmt.Errorf("package Git credential is unavailable")
	}
	password, err := s.secrets.SecretValue(route.Secret)
	if err != nil {
		return nil, "", err
	}
	target, err := url.Parse(route.Source)
	if err != nil {
		return nil, "", err
	}
	env, err := gitAuthorizationEnvironment(target, password, route.Username)
	return env, password, err
}

func (s *Store) fetchTransport(ctx context.Context, root string, route GitTransport) error {
	env, password, err := s.transportAuthentication(route)
	if err != nil {
		return err
	}
	if err := s.fetchTransportRefs(ctx, root, route.Source, env); err != nil {
		return errors.New(redactGitCredential(err.Error(), password, route.Username))
	}
	return nil
}

func (s *Store) fetchTransportRefs(ctx context.Context, root, source string, env []string) error {
	output, err := s.runGit(ctx, root, env, "fetch", "--quiet", "--prune", "--no-tags", "--", source, "+refs/heads/*:refs/8020/heads/*", "+refs/tags/*:refs/8020/tags/*", "+HEAD:refs/8020/head")
	if err != nil {
		return fmt.Errorf("fetch %s: %w: %s", source, err, cleanGitOutput(output))
	}
	return nil
}

// Read metadata for one exact local commit without refreshing refs or scanning its history.
func (s *Store) DescribePackageCommit(ctx context.Context, id, commit string) (string, error) {
	if !fullCommit(commit) {
		return "", errors.New("a full commit ID is required")
	}
	root, exists, err := s.versionCheckout(id)
	if err != nil || !exists {
		return "", err
	}
	// The commit ID stays immutable across a checkout replacement.
	subject, err := s.gitValue(ctx, root, "show", "-s", "--format=%s", commit+"^{commit}")
	runes := []rune(subject)
	return string(runes[:min(len(runes), 160)]), err
}

// Ask Git about ancestry across the whole graph even when displayed history is bounded.
func (s *Store) updateCommit(ctx context.Context, root, current string, refs ...string) (string, error) {
	output, err := s.gitValue(ctx, root, append([]string{"for-each-ref", "--contains=" + current, "--format=%(refname)", "--sort=-committerdate"}, refs...)...)
	if err != nil {
		return "", err
	}
	for _, ref := range strings.Fields(output) {
		commit, err := s.gitValue(ctx, root, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return "", err
		}
		if commit != current {
			return commit, nil
		}
	}
	return "", nil
}

// Resolve a tag or abbreviated object ID against the advertised source, including commits
// outside the installed checkout and its bounded history.
func (s *Store) ResolveTransportCommit(ctx context.Context, route GitTransport, prefix string) (string, error) {
	ref := prefix
	if tag, ok := strings.CutPrefix(prefix, "tag:"); ok && safeGitTag(tag) {
		ref = "refs/tags/" + tag
	} else if !isCommitID(prefix) {
		return "", errors.New("invalid Git commit ID")
	}
	source, _, _, err := normalizePackageSource(route.Source)
	if err != nil {
		return "", err
	}
	route.Source = source
	root, err := os.MkdirTemp("", "8020-resolve-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(root)
	env, password, err := s.transportAuthentication(route)
	if err != nil {
		return "", err
	}
	if out, err := s.runGit(ctx, "", env, "clone", "--quiet", "--mirror", "--", route.Source, root); err != nil {
		return "", fmt.Errorf("resolve source commit: %s", redactGitCredential(cleanGitOutput(out), password, route.Username))
	}

	return s.gitValue(ctx, root, "rev-parse", "--verify", ref+"^{commit}")
}

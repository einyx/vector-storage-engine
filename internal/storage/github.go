package storage

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"

	"github.com/prysmsh/warp-storage-engine/internal/config"
)

const (
	githubMaxObjectSize = 100 << 20 // GitHub Contents API limit
	githubStorageClass  = "GITHUB"
)

// GitHubBackend implements the Backend interface by mapping buckets to git
// repositories. Objects are files in the repository's configured branch;
// writes are committed through the GitHub Contents API.
type GitHubBackend struct {
	client *github.Client
	cfg    *config.GitHubConfig
}

// NewGitHubBackend creates a new GitHub backend from configuration.
func NewGitHubBackend(cfg *config.GitHubConfig) (*GitHubBackend, error) {
	if cfg == nil {
		return nil, fmt.Errorf("github configuration is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("github token is required")
	}
	if len(cfg.BucketMapping) == 0 {
		return nil, fmt.Errorf("github bucket_mapping must map at least one bucket to 'owner/repo'")
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.Token})
	httpClient := oauth2.NewClient(context.Background(), ts)
	client := github.NewClient(httpClient)
	if cfg.APIURL != "" {
		u := normalizeGitHubBaseURL(cfg.APIURL)
		var err error
		client, err = github.NewClient(httpClient).WithEnterpriseURLs(u, u)
		if err != nil {
			return nil, fmt.Errorf("invalid github api_url '%s': %w", cfg.APIURL, err)
		}
	}

	return &GitHubBackend{client: client, cfg: cfg}, nil
}

func normalizeGitHubBaseURL(raw string) string {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	if !strings.HasSuffix(raw, "/") {
		raw += "/"
	}
	return raw
}

// resolveBucket maps a virtual bucket name to an owner/repo pair.
func (g *GitHubBackend) resolveBucket(bucket string) (string, string, error) {
	repo, ok := g.cfg.BucketMapping[bucket]
	if !ok {
		return "", "", fmt.Errorf("bucket '%s' is not mapped to a GitHub repository", bucket)
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repository mapping for bucket '%s': expected 'owner/repo', got '%s'", bucket, repo)
	}
	return parts[0], parts[1], nil
}

// commitMessage returns the message used for write commits on a key.
func (g *GitHubBackend) commitMessage(key string) string {
	if g.cfg.CommitMessage != "" {
		return fmt.Sprintf("%s: %s", g.cfg.CommitMessage, key)
	}
	return fmt.Sprintf("update %s", key)
}

func commitAuthor(cfg *config.GitHubConfig) *github.CommitAuthor {
	name := cfg.AuthorName
	if name == "" {
		name = "foundation-storage-engine"
	}
	email := cfg.AuthorEmail
	if email == "" {
		email = "storage@foundation.local"
	}
	return &github.CommitAuthor{Name: &name, Email: &email}
}

func treeEntryToObjectInfo(entry *github.TreeEntry) ObjectInfo {
	var lastModified time.Time
	return ObjectInfo{
		Key:          path.Clean(entry.GetPath()),
		Size:         int64(entry.GetSize()),
		ETag:         entry.GetSHA(),
		LastModified: lastModified,
		StorageClass: githubStorageClass,
		ContentType:  guessContentType(entry.GetPath()),
	}
}

func guessContentType(key string) string {
	if ct := mimeByExtension(path.Ext(key)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func mimeByExtension(ext string) string {
	switch strings.ToLower(ext) {
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".html", ".htm":
		return "text/html"
	case ".css":
		return "text/css"
	case ".js":
		return "application/javascript"
	case ".txt":
		return "text/plain"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".md":
		return "text/markdown"
	case ".csv":
		return "text/csv"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	default:
		return ""
	}
}

// ListBuckets returns the configured bucket mappings as buckets.
func (g *GitHubBackend) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	names := make([]string, 0, len(g.cfg.BucketMapping))
	for name := range g.cfg.BucketMapping {
		names = append(names, name)
	}
	sort.Strings(names)
	now := time.Now()
	buckets := make([]BucketInfo, 0, len(names))
	for _, name := range names {
		buckets = append(buckets, BucketInfo{Name: name, CreationDate: now})
	}
	return buckets, nil
}

func (g *GitHubBackend) CreateBucket(ctx context.Context, bucket string) error {
	return fmt.Errorf("creating buckets is not supported by the github backend: add a bucket_mapping entry instead")
}

func (g *GitHubBackend) DeleteBucket(ctx context.Context, bucket string) error {
	return fmt.Errorf("deleting buckets is not supported by the github backend: remove the bucket_mapping entry instead")
}

func (g *GitHubBackend) BucketExists(ctx context.Context, bucket string) (bool, error) {
	owner, repo, err := g.resolveBucket(bucket)
	if err != nil {
		return false, nil
	}
	_, _, err = g.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("checking repository %s/%s: %w", owner, repo, err)
	}
	return true, nil
}

// ListObjects lists files in the repository tree under prefix.
func (g *GitHubBackend) ListObjects(ctx context.Context, bucket, prefix, marker string, maxKeys int) (*ListObjectsResult, error) {
	return g.listTree(ctx, bucket, prefix, marker, "", maxKeys)
}

// ListObjectsWithDelimiter supports virtual directory listing. Common prefixes
// are derived from blob paths grouped by the delimiter.
func (g *GitHubBackend) ListObjectsWithDelimiter(ctx context.Context, bucket, prefix, marker, delimiter string, maxKeys int) (*ListObjectsResult, error) {
	return g.listTree(ctx, bucket, prefix, marker, delimiter, maxKeys)
}

func (g *GitHubBackend) listTree(ctx context.Context, bucket, prefix, marker, delimiter string, maxKeys int) (*ListObjectsResult, error) {
	owner, repo, err := g.resolveBucket(bucket)
	if err != nil {
		return nil, err
	}

	tree, err := g.getRecursiveTree(ctx, owner, repo)
	if err != nil {
		return nil, err
	}

	result := &ListObjectsResult{
		Contents:       []ObjectInfo{},
		CommonPrefixes: []string{},
	}

	seenPrefixes := make(map[string]bool)
	for _, entry := range tree.Entries {
		if entry.GetType() != "blob" {
			continue
		}
		key := entry.GetPath()
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		if marker != "" && key <= marker {
			continue
		}

		if delimiter != "" {
			rest := key[len(prefix):]
			if idx := strings.Index(rest, delimiter); idx >= 0 {
				cp := prefix + rest[:idx+len(delimiter)]
				if seenPrefixes[cp] {
					continue
				}
				seenPrefixes[cp] = true
				result.CommonPrefixes = append(result.CommonPrefixes, cp)
				if len(result.Contents)+len(result.CommonPrefixes) >= maxKeys {
					result.IsTruncated = true
					result.NextMarker = cp
					return result, nil
				}
				continue
			}
		}

		result.Contents = append(result.Contents, treeEntryToObjectInfo(entry))
		if len(result.Contents)+len(result.CommonPrefixes) >= maxKeys {
			result.IsTruncated = true
			result.NextMarker = key
			return result, nil
		}
	}

	sort.Slice(result.Contents, func(i, j int) bool {
		return result.Contents[i].Key < result.Contents[j].Key
	})
	return result, nil
}

func (g *GitHubBackend) getRecursiveTree(ctx context.Context, owner, repo string) (*github.Tree, error) {
	commit, _, err := g.client.Repositories.GetCommit(ctx, owner, repo, g.cfg.Branch, nil)
	if err != nil {
		return nil, fmt.Errorf("getting head commit of %s/%s@%s: %w", owner, repo, g.cfg.Branch, err)
	}
	if commit.Commit == nil || commit.Commit.Tree == nil {
		return nil, fmt.Errorf("commit %s of %s/%s has no tree", commit.GetSHA(), owner, repo)
	}
	tree, _, err := g.client.Git.GetTree(ctx, owner, repo, commit.Commit.Tree.GetSHA(), true)
	if err != nil {
		return nil, fmt.Errorf("getting tree of %s/%s@%s: %w", owner, repo, g.cfg.Branch, err)
	}
	if tree.GetTruncated() {
		return nil, fmt.Errorf("repository tree of %s/%s is too large to list via the github backend", owner, repo)
	}
	return tree, nil
}

// ListDeletedObjects returns an empty result — GitHub has no soft-delete tombstones.
func (g *GitHubBackend) ListDeletedObjects(ctx context.Context, bucket, prefix, marker string, maxKeys int) (*ListObjectsResult, error) {
	return &ListObjectsResult{Contents: []ObjectInfo{}, CommonPrefixes: []string{}}, nil
}

// GetObject fetches a file from the repository.
func (g *GitHubBackend) GetObject(ctx context.Context, bucket, key string) (*Object, error) {
	info, content, err := g.getContents(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	return &Object{
		Body:         io.NopCloser(strings.NewReader(string(content))),
		ContentType:  info.ContentType,
		Size:         info.Size,
		ETag:         info.ETag,
		Metadata:     map[string]string{},
		LastModified: info.LastModified,
	}, nil
}

// getContents fetches file metadata and decoded content for a key.
func (g *GitHubBackend) getContents(ctx context.Context, bucket, key string) (*ObjectInfo, []byte, error) {
	owner, repo, err := g.resolveBucket(bucket)
	if err != nil {
		return nil, nil, err
	}
	key = strings.TrimPrefix(key, "/")
	if key == "" || strings.HasSuffix(key, "/") {
		return nil, nil, fmt.Errorf("invalid object key '%s'", key)
	}

	fileContent, _, _, err := g.client.Repositories.GetContents(ctx, owner, repo, key, &github.RepositoryContentGetOptions{Ref: g.cfg.Branch})
	if err != nil {
		return nil, nil, fmt.Errorf("getting %s from %s/%s@%s: %w", key, owner, repo, g.cfg.Branch, err)
	}
	if fileContent == nil {
		return nil, nil, fmt.Errorf("%s in %s/%s is a directory, not an object", key, owner, repo)
	}

	contentType := fileContent.GetType()
	_ = contentType
	raw, err := fileContent.GetContent()
	if err != nil {
		return nil, nil, fmt.Errorf("decoding content of %s in %s/%s: %w", key, owner, repo, err)
	}

	info := &ObjectInfo{
		Key:          key,
		Size:         int64(fileContent.GetSize()),
		ETag:         fileContent.GetSHA(),
		StorageClass: githubStorageClass,
		ContentType:  guessContentType(key),
		Metadata:     map[string]string{},
	}
	return info, []byte(raw), nil
}

// PutObject creates or updates a file via the Contents API and commits.
func (g *GitHubBackend) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, metadata map[string]string) error {
	owner, repo, err := g.resolveBucket(bucket)
	if err != nil {
		return err
	}
	if size > githubMaxObjectSize {
		return fmt.Errorf("object %s (%d bytes) exceeds the github contents api limit of %d bytes", key, size, githubMaxObjectSize)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("reading object body for %s: %w", key, err)
	}

	key = strings.TrimPrefix(key, "/")
	content := base64.StdEncoding.EncodeToString(data)
	opts := &github.RepositoryContentFileOptions{
		Message: github.String(g.commitMessage(key)),
		Content: []byte(content),
		Branch:  github.String(g.cfg.Branch),
		Author:  commitAuthor(g.cfg),
	}

	// Fetch the existing file SHA so an existing object is updated instead of rejected.
	existing, _, _, err := g.client.Repositories.GetContents(ctx, owner, repo, key, &github.RepositoryContentGetOptions{Ref: g.cfg.Branch})
	if err == nil && existing != nil {
		opts.SHA = github.String(existing.GetSHA())
	} else if err != nil && !isGitHubNotFound(err) {
		return fmt.Errorf("checking existing object %s in %s/%s: %w", key, owner, repo, err)
	}

	_, _, err = g.client.Repositories.CreateFile(ctx, owner, repo, key, opts)
	if err != nil {
		return fmt.Errorf("committing %s to %s/%s@%s: %w", key, owner, repo, g.cfg.Branch, err)
	}
	return nil
}

// DeleteObject deletes a file from the repository via a commit.
func (g *GitHubBackend) DeleteObject(ctx context.Context, bucket, key string) error {
	owner, repo, err := g.resolveBucket(bucket)
	if err != nil {
		return err
	}
	key = strings.TrimPrefix(key, "/")

	existing, _, _, err := g.client.Repositories.GetContents(ctx, owner, repo, key, &github.RepositoryContentGetOptions{Ref: g.cfg.Branch})
	if err != nil {
		if isGitHubNotFound(err) {
			return fmt.Errorf("object %s not found in %s/%s@%s", key, owner, repo, g.cfg.Branch)
		}
		return fmt.Errorf("getting object %s in %s/%s for delete: %w", key, owner, repo, err)
	}
	if existing == nil {
		return fmt.Errorf("%s in %s/%s is a directory, not an object", key, owner, repo)
	}

	opts := &github.RepositoryContentFileOptions{
		Message: github.String(g.commitMessage("delete " + key)),
		SHA:     github.String(existing.GetSHA()),
		Branch:  github.String(g.cfg.Branch),
		Author:  commitAuthor(g.cfg),
	}
	if _, _, err := g.client.Repositories.DeleteFile(ctx, owner, repo, key, opts); err != nil {
		return fmt.Errorf("deleting %s from %s/%s@%s: %w", key, owner, repo, g.cfg.Branch, err)
	}
	return nil
}

// RestoreObject is unsupported — GitHub versioning lives in git history.
func (g *GitHubBackend) RestoreObject(ctx context.Context, bucket, key, versionID string) error {
	return fmt.Errorf("restore is not supported by the github backend")
}

// HeadObject returns file metadata without the body.
func (g *GitHubBackend) HeadObject(ctx context.Context, bucket, key string) (*ObjectInfo, error) {
	info, _, err := g.getContents(ctx, bucket, key)
	return info, err
}

func (g *GitHubBackend) GetObjectACL(ctx context.Context, bucket, key string) (*ACL, error) {
	return nil, fmt.Errorf("acl operations are not supported by the github backend: access is governed by the configured token")
}

func (g *GitHubBackend) PutObjectACL(ctx context.Context, bucket, key string, acl *ACL) error {
	return fmt.Errorf("acl operations are not supported by the github backend: access is governed by the configured token")
}

// Multipart uploads are unsupported: the Contents API commits whole files.
func (g *GitHubBackend) InitiateMultipartUpload(ctx context.Context, bucket, key string, metadata map[string]string) (string, error) {
	return "", fmt.Errorf("multipart uploads are not supported by the github backend")
}

func (g *GitHubBackend) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (string, error) {
	return "", fmt.Errorf("multipart uploads are not supported by the github backend")
}

func (g *GitHubBackend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) error {
	return fmt.Errorf("multipart uploads are not supported by the github backend")
}

func (g *GitHubBackend) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	return fmt.Errorf("multipart uploads are not supported by the github backend")
}

func (g *GitHubBackend) ListParts(ctx context.Context, bucket, key, uploadID string, maxParts int, partNumberMarker int) (*ListPartsResult, error) {
	return nil, fmt.Errorf("multipart uploads are not supported by the github backend")
}

func isGitHubNotFound(err error) bool {
	ghErr, ok := err.(*github.ErrorResponse)
	return ok && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}

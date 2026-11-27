package plugins

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"unicode/utf8"
)

const maxFolderPage = 1000

// folderNames retains the smallest page of eligible names in a max heap.
type folderNames []string

func (h folderNames) Len() int           { return len(h) }
func (h folderNames) Less(i, j int) bool { return h[i] > h[j] }
func (h folderNames) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *folderNames) Push(v any)        { *h = append(*h, v.(string)) }
func (h *folderNames) Pop() any {
	last := len(*h) - 1
	v := (*h)[last]
	*h = (*h)[:last]
	return v
}

func declaresFolderRead(man Manifest) bool {
	return man.Ether != nil && man.RuntimeKind() == "hosted" && normLevel(man.Ether.Permissions.Folders) == "read"
}

// folderRoots returns the stored root strings verbatim, as required by folderCheck.
func folderRoots(man Manifest, g Grants) []string {
	roots := []string{}
	if !declaresFolderRead(man) || g.revoked("folders") {
		return roots
	}
	for _, root := range g.Folders {
		if filepath.IsAbs(root) {
			roots = append(roots, root)
		}
	}
	return roots
}

func (h *host) FoldersRoots(ctx context.Context) ([]string, error) {
	if isClientRequestPath(callPathFromContext(ctx)) {
		return nil, fmt.Errorf("%w: folders.roots requires a non-client route", ErrPermissionDenied)
	}
	r, g, err := h.record(ctx)
	if err != nil {
		return nil, err
	}
	return folderRoots(r.Manifest, g), nil
}

func (h *host) FoldersList(ctx context.Context, root, path string, options ...FolderListOptions) (FolderPage, error) {
	if isClientRequestPath(callPathFromContext(ctx)) {
		return FolderPage{}, fmt.Errorf("%w: folders.list requires a non-client route", ErrPermissionDenied)
	}
	if _, err := h.folderCheck(ctx, root, path); err != nil {
		return FolderPage{}, err
	}
	opts := FolderListOptions{}
	if len(options) != 0 {
		opts = options[0]
	}
	if opts.Limit <= 0 {
		opts.Limit = maxFolderPage
	}
	limit := min(opts.Limit, maxFolderPage)
	fs, err := os.OpenRoot(root)
	if err != nil {
		return FolderPage{}, err
	}
	defer fs.Close()
	dir, err := fs.OpenFile(filepath.Clean(path), folderReadFlags, 0)
	if err != nil {
		return FolderPage{}, err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() {
		return FolderPage{}, fmt.Errorf("folders.list requires a directory")
	}

	names := make(folderNames, 0, limit)
	truncated := false
	for {
		if err := ctx.Err(); err != nil {
			return FolderPage{}, err
		}
		batch, err := dir.ReadDir(256)
		if err != nil && !errors.Is(err, io.EOF) {
			return FolderPage{}, err
		}
		for _, entry := range batch {
			name := entry.Name()
			if name <= opts.Cursor || !utf8.ValidString(name) {
				continue
			}
			// Rooted Stat follows only links whose targets stay in the grant.
			if entry.Type()&os.ModeSymlink != 0 {
				if _, err := fs.Stat(filepath.Join(path, name)); err != nil {
					continue
				}
			}
			if len(names) < limit {
				heap.Push(&names, name)
			} else {
				truncated = true
				if name < names[0] {
					names[0] = name
					heap.Fix(&names, 0)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}

	slices.Sort(names)
	page := FolderPage{Entries: make([]FolderEntry, 0, len(names)), Truncated: truncated}
	if truncated {
		page.NextCursor = names[len(names)-1]
	}
	for _, name := range names {
		rel := filepath.Join(path, name)
		info, err := fs.Stat(rel)
		if err != nil {
			continue // An entry may disappear or become an escaping link during the scan.
		}
		page.Entries = append(page.Entries, FolderEntry{
			Name: name, Path: rel, Dir: info.IsDir(), Size: info.Size(), Mtime: info.ModTime().Unix(),
		})
	}
	if _, err := h.folderCheck(ctx, root, path); err != nil {
		return FolderPage{}, err
	}
	return page, nil
}

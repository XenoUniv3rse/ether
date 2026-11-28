package blobcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ImportLegacy moves indexed whole files from a previous node-local cache.
// Publishing aliases and checking the stored bytes precede unlinking the source.
// An interrupted move is safe to repeat, including when another node has
// already imported the same content with different aliases.
func (c *Cache) ImportLegacy(ctx context.Context, dir string) error {
	st, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("invalid legacy cache directory %q", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var failures []error
	// The destination can be nested under the old directory on custom installs.
	dst, err := os.Stat(c.dir)
	if err != nil {
		return err
	}
	if os.SameFile(st, dst) {
		return nil
	}
	for _, d := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := d.Name()
		if !d.IsDir() || sanitizeNamespaceKey(key) != key {
			continue
		}
		if info, e := d.Info(); e != nil {
			return e
		} else if os.SameFile(info, dst) {
			continue
		}
		data, e := root.ReadFile(filepath.Join(key, "index.json"))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		var doc indexDoc
		if e == nil {
			e = json.Unmarshal(data, &doc)
		}
		if e != nil || doc.Version != 1 {
			failures = append(failures, fmt.Errorf("invalid cache index for %s (version %d, error %v)", key, doc.Version, e))
			continue
		}
		aliases := make(map[string][]string)
		for alias, h := range doc.Keys {
			aliases[h] = append(aliases[h], alias)
		}
		ns := c.Namespace(key, Quota{})
		for h, entry := range doc.Blobs {
			if err := ctx.Err(); err != nil {
				return err
			}
			digest, e := ParseRef("blob:" + h)
			if e != nil || h != digest || entry == nil || entry.Size < 0 {
				failures = append(failures, fmt.Errorf("invalid legacy entry in %s", key))
				continue
			}
			keys := aliases[h]
			for _, k := range entry.Keys {
				if doc.Keys[k] == "" {
					keys = append(keys, k)
				}
			}
			if e := ns.importFile(ctx, root, filepath.Join(key, h[:2], h), h, entry, keys); e != nil {
				failures = append(failures, fmt.Errorf("import %s/%s: %w", key, h, e))
			}
		}
	}
	return errors.Join(failures...)
}

func (n *Namespace) importFile(ctx context.Context, root *os.Root, path, h string, entry *blobEntry, keys []string) error {
	st, err := root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != entry.Size {
		return errors.New("legacy file type or size differs from index")
	}
	ref := Ref("blob:" + h)
	reader, err := n.Open(ref)
	if err == nil {
		// A resumed import or another node may already have published this
		// reference. Verify the old object itself before deleting it.
		src, e := root.Open(path)
		if e != nil {
			reader.Close()
			return e
		}
		e = verifyLegacyBytes(ctx, src, entry.Size, h)
		src.Close()
		if e != nil {
			reader.Close()
			return e
		}
	}
	if errors.Is(err, ErrNotFound) {
		src, e := root.Open(path)
		if e != nil {
			return e
		}
		_, e = n.PutWithExpectedSHA256(ctx, "", src, entry.Size, h, entry.MIME)
		closed := src.Close()
		if e != nil {
			return e
		}
		if closed != nil {
			return closed
		}
		reader, err = n.Open(ref)
	}
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := verifyLegacyBytes(ctx, reader, entry.Size, h); err != nil {
		return err
	}
	if err := n.AddAliases(ref, keys, entry.Name); err != nil {
		return err
	}
	current, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(st, current) || current.Size() != st.Size() || !current.ModTime().Equal(st.ModTime()) {
		return errors.New("source changed during import")
	}
	return root.Remove(path)
}

func verifyLegacyBytes(ctx context.Context, r io.Reader, size int64, digest string) error {
	hash := sha256.New()
	count, err := io.Copy(hash, &ctxReader{ctx, r})
	if err != nil {
		return err
	}
	if count != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("file verification failed")
	}
	return nil
}

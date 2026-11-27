package blobcache

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/lru"
)

// Stat describes the bytes of a blob. Digests are lowercase hexadecimal.
type Stat struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	SHA1   string `json:"sha1"`
	MD5    string `json:"md5"`
	CRC32  string `json:"crc32"`
}

const InteractiveStatBytes = 256 << 10
const MaxStatBytes int64 = 1 << 30

var errStatBusy = errors.New("blobcache: all digest workers are busy")

// Immutable content addresses let staged, saved and cached copies reuse the
// same digests. Callers must verify access before looking up an owned source.
var digestCache = struct {
	sync.Mutex
	entries *lru.Cache[string, Stat]
}{entries: lru.New(lru.Options[string, Stat]{MaxEntries: 4096})}

var digestWorkers = make(chan struct{}, 2)

func copyWithDigests(dst io.Writer, src io.Reader) (Stat, error) {
	sha, shaOne, md, crc := sha256.New(), sha1.New(), md5.New(), crc32.NewIEEE()
	size, err := io.Copy(io.MultiWriter(dst, sha, shaOne, md, crc), src)
	if err != nil {
		return Stat{}, err
	}
	return Stat{
		Size: size, SHA256: hex.EncodeToString(sha.Sum(nil)), SHA1: hex.EncodeToString(shaOne.Sum(nil)),
		MD5: hex.EncodeToString(md.Sum(nil)), CRC32: hex.EncodeToString(crc.Sum(nil)),
	}, nil
}

func rememberStat(stat Stat) {
	digestCache.Lock()
	digestCache.entries.Add(stat.SHA256, stat)
	digestCache.Unlock()
}

type stagedSource struct {
	*fileSource
	stat Stat
}

// StatOwned describes a source already opened through the plugin's ownership
// checks. maxBytes limits cold reads, while staged and cached digests take no
// file reads. The caller closes src.
func StatOwned(ctx context.Context, ref Ref, src io.ReadSeekCloser, maxBytes int64) (Stat, error) {
	if staged, ok := src.(*stagedSource); ok {
		return staged.stat, nil
	}
	var size int64
	if s, ok := src.(interface{ Size() int64 }); ok {
		size = s.Size()
	} else {
		var err error
		size, err = src.Seek(0, io.SeekEnd)
		if err != nil {
			return Stat{}, err
		}
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return Stat{}, err
		}
	}
	hash, err := ref.Hash()
	if strings.HasPrefix(ref.String(), DurableRefPrefix) {
		hash, err = ParseDurableRef(ref)
	}
	if err != nil {
		return Stat{}, err
	}
	digestCache.Lock()
	stat, ok := digestCache.entries.Get(hash)
	digestCache.Unlock()
	if ok && stat.Size == size {
		return stat, nil
	}
	if size < 0 || size > maxBytes {
		return Stat{}, ErrTooLarge
	}
	select {
	case digestWorkers <- struct{}{}:
		defer func() { <-digestWorkers }()
	default:
		return Stat{}, errStatBusy
	}
	stat, err = copyWithDigests(io.Discard, &boundedUploadReader{ctx: ctx, r: src, left: size})
	if err != nil {
		return Stat{}, err
	}
	if stat.Size != size || stat.SHA256 != hash {
		return Stat{}, errors.New("blobcache: bytes do not match the blob reference")
	}
	rememberStat(stat)
	return stat, nil
}

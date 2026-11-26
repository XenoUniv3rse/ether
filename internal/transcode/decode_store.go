package transcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("transcode", 1, `CREATE TABLE transcode_decode_caps (
 build TEXT NOT NULL, source TEXT NOT NULL, supported INTEGER NOT NULL,
 reason TEXT NOT NULL, PRIMARY KEY(build, source));`)
}

// Hash on first demand, never at startup. Build bytes (rather than a version
// string) distinguish differently patched FFmpeg binaries with the same version.
func (c *decodeProbeCache) identifyBuild() {
	if c.db == nil {
		return
	}
	f, err := os.Open(c.ffmpeg)
	if err != nil {
		return
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err == nil {
		c.mu.Lock()
		c.build = hex.EncodeToString(h.Sum(nil))
		c.mu.Unlock()
	}
}

// decodeProbeVersion is part of every stored key. Bump it whenever the probe
// or how its output is judged changes: results recorded by older logic (for
// example failures caused by libva banner lines before 0.3.16) must be probed
// again rather than reused for as long as the FFmpeg binary stays the same.
const decodeProbeVersion = 2

func (c *decodeProbeCache) storeKey(k decodeKey) string {
	// A shared cluster store must not reuse another node's device capabilities.
	host, _ := os.Hostname()
	key := []any{decodeProbeVersion, host, k.accel, k.device, k.codec, k.profile, k.pixel, k.depth}
	// Re-probe VAAPI AV1 on builds that previously had no sample encoder.
	if k.accel == AccelVAAPI && k.codec == "av1" && !c.encoders["libaom-av1"] && c.encoders["libsvtav1"] {
		key = append(key, "libsvtav1")
	}
	b, _ := json.Marshal(key)
	return string(b)
}

func (c *decodeProbeCache) loadResult(k decodeKey) (bool, string, bool) {
	if c.db == nil || c.build == "" {
		return false, "", false
	}
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	var supported int
	var reason string
	err := c.db.QueryRowContext(ctx, `SELECT supported, reason FROM transcode_decode_caps WHERE build=? AND source=?`, c.build, c.storeKey(k)).Scan(&supported, &reason)
	if err != nil {
		return false, "", false
	}
	return supported != 0, "stored: " + reason, true
}

func (c *decodeProbeCache) saveResult(k decodeKey, ok bool, reason string) {
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	c.saveResultContext(ctx, k, ok, reason)
}

func (c *decodeProbeCache) saveResultContext(ctx context.Context, k decodeKey, ok bool, reason string) {
	if c.db == nil || c.build == "" {
		return
	}
	// Do not accumulate a new "stored:" prefix on each restart.
	for len(reason) >= 8 && reason[:8] == "stored: " {
		reason = reason[8:]
	}
	supported := 0
	if ok {
		supported = 1
	}
	_, err := c.db.ExecContext(ctx, `INSERT INTO transcode_decode_caps(build, source, supported, reason) VALUES(?,?,?,?) ON CONFLICT(build, source) DO UPDATE SET supported=excluded.supported, reason=excluded.reason`, c.build, c.storeKey(k), supported, reason)
	if err != nil && c.log != nil {
		c.log.Warn("hardware decode result persistence failed", "error", err)
	}
}

// Shutdown cancels probes immediately, then gives runtime rejections one shared
// short flush budget. A rejection queued just before close must not leave an
// older successful result to be reused after restart.
func (c *decodeProbeCache) flushRejections() {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	for k, reason := range c.reasons {
		if reason == "runtime decoder rejection" && ctx.Err() == nil {
			c.saveResultContext(ctx, k, false, reason)
		}
	}
}

// Called under mu. Log once for admission and once more if runtime rejection
// overrides it; repeated run planning and client retries remain quiet.
func (c *decodeProbeCache) logResult(k decodeKey, ok bool, reason string) {
	if previous, exists := c.logged[k]; exists && previous == ok {
		return
	}
	c.logged[k] = ok
	if c.log != nil {
		c.log.Info("hardware decode capability", "enabled", ok, "reason", reason,
			"accel", k.accel, "device", k.device, "codec", k.codec, "profile", k.profile,
			"pixelFormat", k.pixel, "bitDepth", k.depth, "ffmpegBuild", c.build)
	}
}

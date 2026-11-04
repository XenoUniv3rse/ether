package adapters

import "strings"

// remotePath is an absolute path on a remote sidecar's filesystem. The
// sidecar may run on any OS, whatever Ether's own host is, so its paths are
// read lexically in the style the path itself declares: POSIX
// ("/srv/media") or Windows ("D:\Media", `\\nas\share\Media`). Ether's
// host path rules never apply; the sidecar enforces real containment
// (symlinks, case) when it opens the file.
type remotePath struct {
	windows bool
	volume  string // "D:" or `\\nas\share` as written; empty for POSIX
	segs    []string
}

// parseRemoteAbs reads an absolute sidecar path. It refuses anything with
// a dot segment, a percent escape or a NUL, and for Windows paths device
// names (`\\?\`, `\\.\`) and stream or reserved characters in a segment.
func parseRemoteAbs(p string) (remotePath, bool) {
	if p == "" || strings.ContainsAny(p, "%\x00") {
		return remotePath{}, false
	}
	switch {
	case p[0] == '/':
		segs, ok := remoteSegments(p, false, true)
		return remotePath{segs: segs}, ok
	case len(p) >= 3 && isDriveLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/'):
		segs, ok := remoteSegments(p[2:], true, true)
		return remotePath{windows: true, volume: p[:2], segs: segs}, ok
	case strings.HasPrefix(p, `\\`):
		parts := strings.SplitN(strings.ReplaceAll(p[2:], "/", `\`), `\`, 3)
		if len(parts) < 2 || !validWindowsSegment(parts[0]) || !validWindowsSegment(parts[1]) {
			return remotePath{}, false
		}
		rest := ""
		if len(parts) == 3 {
			rest = parts[2]
		}
		segs, ok := remoteSegments(rest, true, true)
		return remotePath{windows: true, volume: `\\` + parts[0] + `\` + parts[1], segs: segs}, ok
	}
	return remotePath{}, false
}

// remoteRelative reads a path relative to a granted root in that root's
// style. Empty segments are refused: a relative path has no leading
// separator to excuse them.
func remoteRelative(p string, windows bool) ([]string, bool) {
	if p == "" || strings.ContainsAny(p, "%\x00") {
		return nil, false
	}
	return remoteSegments(p, windows, false)
}

func remoteSegments(p string, windows, absolute bool) ([]string, bool) {
	if !windows && strings.Contains(p, `\`) {
		return nil, false
	}
	if windows {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	raw := strings.Split(p, "/")
	segs := make([]string, 0, len(raw))
	for _, seg := range raw {
		switch {
		case seg == "":
			if !absolute {
				return nil, false
			}
		case seg == "." || seg == "..":
			return nil, false
		case windows && !validWindowsSegment(seg):
			return nil, false
		default:
			segs = append(segs, seg)
		}
	}
	return segs, true
}

func validWindowsSegment(seg string) bool {
	return seg != "" && seg != "." && seg != ".." && !strings.ContainsAny(seg, `:*?"<>|`)
}

func isDriveLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// within reports whether p lies strictly below root. Windows comparisons
// fold case, as the Windows filesystem does.
func (root remotePath) within(p remotePath) bool {
	if root.windows != p.windows || len(p.segs) <= len(root.segs) || !root.same(root.volume, p.volume) {
		return false
	}
	for i, seg := range root.segs {
		if !root.same(seg, p.segs[i]) {
			return false
		}
	}
	return true
}

func (root remotePath) same(a, b string) bool {
	if root.windows {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// String is the path in its own style, as the sidecar names it natively.
func (p remotePath) String() string {
	if p.windows {
		return p.volume + `\` + strings.Join(p.segs, `\`)
	}
	return "/" + strings.Join(p.segs, "/")
}

// remoteFile is a sidecar file validated against one granted root.
type remoteFile struct {
	full string // the file, in the sidecar's path style
	root string // the granted root it lies in, for X-Ether-File-Root
	rel  string // slash-separated path below root, for /ether/file/
	name string // the final segment
}

// resolveRemoteFile matches raw against the granted roots. Exactly one root
// must contain it; an ambiguous or unmatched path is refused.
func resolveRemoteFile(roots []string, raw string) (remoteFile, bool) {
	abs, isAbs := parseRemoteAbs(raw)
	var out remoteFile
	matches := 0
	for _, r := range roots {
		root, ok := parseRemoteAbs(r)
		if !ok {
			continue
		}
		cand := abs
		if !isAbs {
			rel, ok := remoteRelative(raw, root.windows)
			if !ok || len(rel) == 0 {
				continue
			}
			cand = remotePath{windows: root.windows, volume: root.volume, segs: append(append([]string(nil), root.segs...), rel...)}
		}
		if !root.within(cand) {
			continue
		}
		matches++
		out = remoteFile{full: cand.String(), root: root.String(), rel: strings.Join(cand.segs[len(root.segs):], "/"), name: cand.segs[len(cand.segs)-1]}
	}
	return out, matches == 1
}

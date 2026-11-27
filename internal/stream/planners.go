package stream

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/archive"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/par2"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// trackNameRe reads "[disc-]number title" from a member hint without an
// extension, e.g. "1-03 They Don't Care About Us" or "01 I'll Be There".
var trackNameRe = regexp.MustCompile(`^(?:(\d{1,2})[-.])?(\d{1,3})(?:\s*[-._)]\s*|\s+)(.+)$`)

// trackHintFromName turns a synthetic track-style member hint into a
// TrackHint. Hints that carry a file extension are real paths, not tracks.
func trackHintFromName(name string) (nzb.TrackHint, bool) {
	norm := strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	dir := path.Dir(norm)
	base := strings.TrimSpace(path.Base(norm))
	if ext := path.Ext(base); ext != "" && len(ext) <= 5 && !strings.ContainsAny(ext, " '") {
		return nzb.TrackHint{}, false
	}
	m := trackNameRe.FindStringSubmatch(base)
	if m == nil {
		return nzb.TrackHint{}, false
	}
	var h nzb.TrackHint
	if m[1] != "" {
		h.Disc, _ = strconv.Atoi(m[1])
	}
	if h.Disc == 0 && dir != "" && dir != "." {
		h.Disc = nzb.ParseDirDisc(dir)
	}
	h.Number, _ = strconv.Atoi(m[2])
	h.Title = strings.TrimSpace(m[3])
	if h.Number <= 0 {
		return nzb.TrackHint{}, false
	}
	return h, true
}

func (b *builder) pickNonVideoMember(ctx context.Context, vols []*postedFile, members []archive.Member, hint nzb.NonVideoHint, concat, allowNested bool) (*archive.Member, *plan, error) {
	m, merr := chooseNonVideoMember(members, hint)
	if merr == nil {
		return m, nil, nil
	}
	if allowNested && errors.Is(merr, ErrNoPlayableFile) {
		if p, nerr := b.tryNestedNonVideo(ctx, vols, members, hint, concat); nerr != nil {
			return nil, nil, nerr
		} else if p != nil {
			return nil, p, nil
		}
	}
	b.logNonVideoMiss(vols[0].Name(), members, hint, merr)
	return nil, nil, merr
}

func (b *builder) logNonVideoMiss(archiveName string, members []archive.Member, hint nzb.NonVideoHint, err error) {
	if b == nil || b.e == nil || b.e.log == nil {
		return
	}
	var sample []string
	for _, m := range members {
		if m.Dir {
			continue
		}
		sample = append(sample, m.Name)
		if len(sample) >= 5 {
			break
		}
	}
	var disc, num int
	var title string
	if hint.Track != nil {
		disc, num, title = hint.Track.Disc, hint.Track.Number, hint.Track.Title
	}
	b.e.log.Debug("non-video member miss",
		"archive", archiveName,
		"members", len(members),
		"sample", sample,
		"member", hint.Member,
		"disc", disc,
		"number", num,
		"title", title,
		"err", err,
	)
}

// nonVideoArchiveHint returns the NZB's non-video hint when archive member
// selection or non-video archive extraction is requested.
func (b *builder) nonVideoArchiveHint() (nzb.NonVideoHint, bool) {
	h, ok := b.rel.doc.NonVideoHint()
	if !ok {
		return nzb.NonVideoHint{}, false
	}
	if h.Member != "" || h.MemberIndex != nil || h.Track.NonEmpty() || nzb.IsNonVideoMIME(h.MIME) {
		return h, true
	}
	return nzb.NonVideoHint{}, false
}

// chooseNonVideoMember selects a non-directory archive member for a non-video
// release target (by Member name, MemberIndex, or MIME match rank + size).
func chooseNonVideoMember(members []archive.Member, hint nzb.NonVideoHint) (*archive.Member, error) {
	var nonDirs []*archive.Member
	for i := range members {
		if !members[i].Dir {
			nonDirs = append(nonDirs, &members[i])
		}
	}
	if len(nonDirs) == 0 {
		return nil, fmt.Errorf("%w: archive holds no files", ErrNoPlayableFile)
	}
	if want := strings.TrimSpace(hint.Member); want != "" {
		names := make([]string, len(nonDirs))
		for i, m := range nonDirs {
			names[i] = m.Name
		}
		idx, ok, ambig := nzb.MatchPathIndex(names, want)
		if ambig {
			return nil, fmt.Errorf("stream: archive member %q matched multiple files: %w", want, core.ErrWrongContent)
		}
		if !ok {
			// A hint that names no member exactly may still describe a track
			// ("1-03 Title", "01 Title"): fall back to the structured track
			// hint, or one read from the name, before giving up.
			track := hint.Track
			if !track.NonEmpty() {
				if parsed, okParse := trackHintFromName(want); okParse {
					track = &parsed
				}
			}
			if track.NonEmpty() {
				fallback := hint
				fallback.Member = ""
				fallback.MemberIndex = nil
				fallback.Track = track
				if m, err := chooseNonVideoMember(members, fallback); err == nil {
					return m, nil
				}
			}
			return nil, fmt.Errorf("%w: archive holds no member %q: %w", ErrNoPlayableFile, want, core.ErrWrongContent)
		}
		if nonDirs[idx].Size <= 0 {
			return nil, fmt.Errorf("%w: archive member %q is empty", ErrNoPlayableFile, nonDirs[idx].Name)
		}
		return nonDirs[idx], nil
	}
	if hint.MemberIndex != nil {
		i := *hint.MemberIndex
		if i < 0 || i >= len(nonDirs) {
			return nil, fmt.Errorf("%w: memberIndex %d outside %d members: %w", ErrNoPlayableFile, i, len(nonDirs), core.ErrWrongContent)
		}
		if nonDirs[i].Size <= 0 {
			return nil, fmt.Errorf("%w: archive member %d (%q) is empty", ErrNoPlayableFile, i, nonDirs[i].Name)
		}
		return nonDirs[i], nil
	}
	if hint.Track.NonEmpty() {
		names := make([]string, len(nonDirs))
		for i, m := range nonDirs {
			names[i] = m.Name
		}
		idx, ok, ambig := nzb.MatchTrackIndex(names, *hint.Track, hint.MIME)
		if ambig {
			return nil, fmt.Errorf("%w: track hint (disc=%d number=%d title=%q) matched multiple archive members: %w", ErrNoPlayableFile, hint.Track.Disc, hint.Track.Number, hint.Track.Title, core.ErrWrongContent)
		}
		if !ok {
			return nil, fmt.Errorf("%w: archive holds no track matching (disc=%d number=%d title=%q): %w", ErrNoPlayableFile, hint.Track.Disc, hint.Track.Number, hint.Track.Title, core.ErrWrongContent)
		}
		if nonDirs[idx].Size <= 0 {
			return nil, fmt.Errorf("%w: archive member %q is empty", ErrNoPlayableFile, nonDirs[idx].Name)
		}
		return nonDirs[idx], nil
	}
	var best *archive.Member
	bestRank := 0
	for _, m := range nonDirs {
		if m.Size <= 0 {
			continue
		}
		rank := nzb.NonVideoMatchRank(m.Name, hint.MIME)
		if rank == 0 {
			continue
		}
		if rank > bestRank || (rank == bestRank && (best == nil || m.Size > best.Size)) {
			best = m
			bestRank = rank
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: archive holds no usable %s file (%d members)", ErrNoPlayableFile, hint.MIME, len(nonDirs))
	}
	return best, nil
}

func canHandOffRawArchive(m *archive.Member, streamErr error) bool {
	if m == nil || m.Encrypted {
		return false
	}
	return errors.Is(streamErr, archive.ErrCompressed) || errors.Is(streamErr, archive.ErrSolid)
}

// planPlainRaw maps a non-video posted file directly as raw bytes without
// video-only extension inference.
func (b *builder) planPlainRaw(ctx context.Context, pf *postedFile) (*plan, error) {
	seg, err := b.e.segment(ctx, b.rel, pf, 0, b.prio, b.fo)
	if err != nil {
		return nil, err
	}
	if seg.begin != 0 {
		return nil, fmt.Errorf("stream: first article of %s begins at %d, not zero: %w", pf.Name(), seg.begin, ErrCorrupt)
	}
	if !pf.SizeKnown() {
		last := pf.numSlots() - 1
		ls, err := b.e.segment(ctx, b.rel, pf, last, b.prio, b.fo)
		if err != nil {
			return nil, err
		}
		pf.learn(last, ls.begin, ls.end(), ls.end(), 0)
	}
	name := pf.Name()
	if (pf.file.Obfuscated || name == "" || nzb.IsObfuscated(name)) && seg.name != "" && !nzb.IsObfuscated(seg.name) {
		name = path.Base(seg.name)
	}
	size := pf.Size()
	if size <= 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrNoPlayableFile, name)
	}
	p := newPlan(b.rel, kindPlain, name, size, []*postedFile{pf}, []volInfo{{size: size, dataSize: size}}, []bool{true})
	p.rawNonVideo = true
	return p, p.init()
}

// planRawArchiveVolumes builds a kindArchiveRaw plan exposing all archive
// volumes with verified sizes so kindstream.openContainerOverVolumes can
// extract a compressed or solid non-video member using the shared archive stack.
func (b *builder) planRawArchiveVolumes(ctx context.Context, vols []*postedFile, format, memberName string, concat bool) (*plan, error) {
	return b.planRawArchiveVolumesOffsets(ctx, vols, nil, nil, format, memberName, concat)
}

func (b *builder) planRawArchiveVolumesOffsets(ctx context.Context, vols []*postedFile, baseOff, chunkLen []int64, format, memberName string, concat bool) (*plan, error) {
	if baseOff == nil {
		if err := b.heads(ctx, vols); err != nil {
			return nil, err
		}
	}
	info := make([]volInfo, len(vols))
	verified := make([]bool, len(vols))
	var total int64
	for k, pf := range vols {
		var base, s int64
		if baseOff != nil {
			base = baseOff[k]
			s = chunkLen[k]
		} else {
			if !pf.SizeKnown() {
				last := pf.numSlots() - 1
				ls, err := b.e.segment(ctx, b.rel, pf, last, b.prio, b.fo)
				if err != nil {
					return nil, err
				}
				pf.learn(last, ls.begin, ls.end(), ls.end(), 0)
			}
			s = pf.Size()
		}
		if s <= 0 {
			return nil, fmt.Errorf("stream: %s archive volume %s is empty: %w", format, pf.Name(), ErrCorrupt)
		}
		if s > math.MaxInt64-total {
			return nil, fmt.Errorf("stream: %s archive volume sizes overflow: %w", format, ErrCorrupt)
		}
		info[k] = volInfo{size: base + s, dataOff: base, dataSize: s}
		verified[k] = true
		total += s
	}
	p := newPlan(b.rel, kindArchiveRaw, path.Base(memberName), total, vols, info, verified)
	p.member = memberName
	p.rawNonVideo = true
	p.archiveFormat = format
	p.archiveConcat = concat
	p.shiftedOffsets = baseOff != nil
	return p, p.init()
}

// planPlain maps a posted video file directly.
func (b *builder) planPlain(ctx context.Context, pf *postedFile) (*plan, error) {
	seg, err := b.e.segment(ctx, b.rel, pf, 0, b.prio, b.fo)
	if err != nil {
		return nil, err
	}
	if seg.begin != 0 {
		// A numbered first article is not necessarily the file's beginning:
		// damaged/reindexed NZBs can point it into the middle of the upload.
		// Reject before publishing a playable plan or caching healthy status.
		return nil, fmt.Errorf("stream: first article of %s begins at %d, not zero: %w", pf.Name(), seg.begin, ErrCorrupt)
	}
	if !pf.SizeKnown() {
		// No yEnc file size: the last part tells where the file ends.
		last := pf.numSlots() - 1
		ls, err := b.e.segment(ctx, b.rel, pf, last, b.prio, b.fo)
		if err != nil {
			return nil, err
		}
		pf.learn(last, ls.begin, ls.end(), ls.end(), 0)
	}
	name := pf.Name()
	if (pf.file.Obfuscated || name == "" || nzb.IsObfuscated(name)) && seg.name != "" && !nzb.IsObfuscated(seg.name) {
		name = path.Base(seg.name)
	}
	if !nzb.IsVideoExt(strings.TrimPrefix(path.Ext(name), ".")) && !isISOName(name) {
		ext := pf.file.Info.Ext
		if ext == "" {
			ext = sniffVideoExt(seg.data)
		}
		if ext != "" {
			if name == "" {
				name = "video." + ext
			} else {
				name = name + "." + ext
			}
		}
	}
	size := pf.Size()
	if size <= 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrNoPlayableFile, name)
	}
	p := newPlan(b.rel, kindPlain, name, size, []*postedFile{pf}, []volInfo{{size: size, dataSize: size}}, []bool{true})
	return p, p.init()
}

// planSplit maps a plain split (.001, .002 ...) of a video. Split sets are
// rare and carry no internal totals, so every volume's size is learned.
func (b *builder) planSplit(ctx context.Context, vols []*postedFile) (*plan, error) {
	seg0, err := b.e.segment(ctx, b.rel, vols[0], 0, b.prio, b.fo)
	if err != nil {
		return nil, err
	}
	if seg0.begin != 0 || isRARFormat(archive.Detect(seg0.data)) {
		_, nv := b.nonVideoArchiveHint()
		ord, err := b.orderRARVolumes(ctx, vols, orderRAROpts{
			splitSet:        true,
			checkLast:       true,
			checkStreamable: !nv,
		})
		if err != nil {
			return nil, err
		}
		if ord.kind == volumeOrderConcat {
			return b.planConcatRAROffsets(ctx, ord.vols, ord.baseOff, ord.chunkLen)
		}
		if ord.v0 != nil {
			if ord.baseOff != nil {
				return b.planRARShifted(ctx, ord.vols, ord.baseOff, ord.chunkLen)
			}
			return b.planRAROrdered(ctx, ord.vols)
		}
	}
	if err := b.heads(ctx, vols); err != nil {
		return nil, err
	}
	head, err := b.sniff(ctx, vols[0])
	if err != nil {
		return nil, err
	}
	switch f := archive.Detect(head); f {
	case archive.FormatRAR4, archive.FormatRAR5:
		if len(vols) == 1 {
			return b.planRAR(ctx, vols)
		}
		head2, err := b.sniff(ctx, vols[1])
		if err != nil {
			return nil, err
		}
		if f2 := archive.Detect(head2); f2 == archive.FormatRAR4 || f2 == archive.FormatRAR5 {
			return b.planRAR(ctx, vols)
		}
		return b.planConcatRAR(ctx, vols)
	case archive.Format7z:
		return b.plan7z(ctx, vols)
	case archive.FormatZIP:
		return b.planZIP(ctx, vols)
	case archive.FormatUnknown:
	default:
		return nil, fmt.Errorf("stream: %s archive split into plain volumes: %w", f, ErrUnsupported)
	}
	name := vols[0].Name()
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	nvHint, nonVideo := b.rel.doc.NonVideoHint()
	if nonVideo {
		matched := nvHint.ExplicitFile || nzb.NonVideoMatchRank(name, nvHint.MIME) > 0
		if !matched && nvHint.Member != "" {
			_, matched, _ = nzb.MatchPathIndex([]string{name}, nvHint.Member)
		}
		if !matched {
			return nil, fmt.Errorf("%w: split set %s does not match non-video target", ErrNoPlayableFile, vols[0].Name())
		}
	} else {
		ext := vols[0].file.Info.Ext
		if !nzb.IsVideoExt(ext) && !strings.EqualFold(ext, "iso") && !looksLikeVideo(head) {
			return nil, fmt.Errorf("%w: split set %s is not a video", ErrNoPlayableFile, vols[0].Name())
		}
	}
	info := make([]volInfo, len(vols))
	verified := make([]bool, len(vols))
	var size int64
	for k, pf := range vols {
		s := pf.Size()
		info[k] = volInfo{size: s, dataSize: s}
		verified[k] = true
		size += s
	}
	p := newPlan(b.rel, kindSplit, name, size, vols, info, verified)
	p.rawNonVideo = nonVideo
	return p, p.init()
}

// planConcatRAR maps the main video inside a single store-mode RAR archive
// that was byte-split into plain numbered volumes (.rar.001/.rar.002 or
// .001/.002 where only the first volume carries a RAR header).
func (b *builder) planConcatRAR(ctx context.Context, vols []*postedFile) (*plan, error) {
	return b.planConcatRAROffsets(ctx, vols, nil, nil)
}

func (b *builder) planConcatRAROffsets(ctx context.Context, vols []*postedFile, baseOff, chunkLen []int64) (*plan, error) {
	if baseOff == nil {
		if err := b.heads(ctx, vols); err != nil {
			return nil, err
		}
	}
	av := make([]archive.Volume, len(vols))
	info := make([]volInfo, len(vols))
	verified := make([]bool, len(vols))
	sizes := make([]int64, len(vols))
	var total int64
	for k, pf := range vols {
		var base, s int64
		var r io.ReaderAt
		if baseOff != nil {
			base = baseOff[k]
			s = chunkLen[k]
			r = offsetReaderAt{r: b.reader(ctx, pf), base: base, size: s}
		} else {
			s = pf.Size()
			r = b.reader(ctx, pf)
		}
		if s < 0 || s > math.MaxInt64-total {
			return nil, fmt.Errorf("stream: split rar size overflows: %w", ErrCorrupt)
		}
		av[k] = archive.Volume{R: r, Size: s}
		info[k] = volInfo{size: base + s}
		verified[k] = true
		sizes[k] = s
		total += s
	}
	v, err := archive.ReadRARVolume(archive.NewMultiReader(av), total, b.rarOptionsFor(ctx, vols[0], 0))
	if err != nil {
		return nil, fmt.Errorf("stream: split rar %s: %w", vols[0].Name(), err)
	}
	if err := b.checkRARVolume(v); err != nil {
		return nil, err
	}
	members, err := archive.ComposeRAR([]*archive.RARVolume{v})
	if err != nil {
		return nil, fmt.Errorf("stream: split rar %s: %w", vols[0].Name(), err)
	}
	nvHint, nv := b.nonVideoArchiveHint()
	var m *archive.Member
	if nv {
		var p *plan
		var merr error
		m, p, merr = b.pickNonVideoMember(ctx, vols, members, nvHint, true, baseOff == nil)
		if merr != nil {
			return nil, merr
		}
		if p != nil {
			return p, nil
		}
	} else {
		m = b.chooseMember(members)
		if baseOff == nil {
			if p, err := b.tryNested(ctx, vols, members, m, true); err != nil {
				return nil, err
			} else if p != nil {
				return p, nil
			}
		}
		if m == nil {
			return nil, fmt.Errorf("%w: rar %s holds no video (%d files)", ErrNoPlayableFile, vols[0].Name(), len(members))
		}
	}
	makePlan := func(m *archive.Member) (*plan, error) {
		if m.Encrypted && b.rel.doc.Password() == "" {
			return nil, archive.ErrPasswordRequired
		}
		if err := m.Streamable(); err != nil {
			if nv && canHandOffRawArchive(m, err) {
				return b.planRawArchiveVolumesOffsets(ctx, vols, baseOff, chunkLen, "rar", m.Name, true)
			}
			return nil, fmt.Errorf("stream: split rar %s: %s: %w", vols[0].Name(), m.Name, err)
		}
		resolved, err := resolveConcatSizes([]archive.Member{*m}, sizes)
		if err != nil {
			return nil, err
		}
		rm := resolved[0]
		if baseOff != nil {
			for i := range rm.Extents {
				rm.Extents[i].Offset += baseOff[rm.Extents[i].Volume]
			}
		}
		volInfos := append([]volInfo(nil), info...)
		if rm.Encryption != nil {
			p, err := b.encryptedPlan(ctx, kindRAR, &rm, vols, volInfos)
			if err == nil {
				p.shiftedOffsets = baseOff != nil
				if nv {
					p.rawNonVideo = true
					p.resolvedMember = true
				}
			}
			return p, err
		}
		lay := &layout{version: 1}
		singlePerVol := true
		for _, x := range rm.Extents {
			if x.Volume < 0 || x.Volume >= len(vols) {
				return nil, fmt.Errorf("stream: split rar extent in volume %d of %d: %w", x.Volume, len(vols), ErrCorrupt)
			}
			if volInfos[x.Volume].dataSize != 0 {
				singlePerVol = false
			} else {
				volInfos[x.Volume].dataOff, volInfos[x.Volume].dataSize = x.Offset, x.Size
			}
			lay.ext = append(lay.ext, extent{inner: lay.size, size: x.Size, vol: x.Volume, off: x.Offset})
			lay.size += x.Size
		}
		p := newPlan(b.rel, kindRAR, path.Base(m.Name), m.Size, vols, volInfos, append([]bool(nil), verified...))
		p.member = m.Name
		p.rawNonVideo = nv
		p.resolvedMember = nv
		p.shiftedOffsets = baseOff != nil
		if singlePerVol {
			return p, p.init()
		}
		p.lay = lay
		return p, nil
	}
	p, err := makePlan(m)
	if err == nil && !nv {
		b.keepSiblings(p, members, m, makePlan)
	}
	if err == nil && !nv && isISOName(p.name) {
		b.stashDiscs(members, m, makePlan)
	}
	return p, err
}

// regroupSingleRARs recovers multi-volume RAR sets whose volumes each carry a
// distinct .rar filename (e.g. Movie.001.rar / Movie.002.rar or obfuscated
// <hash>.rar files), which GroupSets initially splits into 1-volume candidates.
func (b *builder) regroupSingleRARs(ctx context.Context, cands []candidate) (bool, error) {
	var singles []*postedFile
	for _, c := range cands {
		if c.kind == nzb.KindRAR && len(c.files) == 1 {
			singles = append(singles, c.files[0])
		}
	}
	if len(singles) < 2 {
		return false, nil
	}
	var set *par2.Set
	if idx := b.rel.par2IndexFile(); idx != nil {
		set = b.rel.loadPar2(ctx, b.prio, b.fo, idx)
	}
	type probedRAR struct {
		pf      *postedFile
		format  archive.Format
		number  int // -1 unknown
		first   bool
		last    bool
		nameNum int // -1 unknown
	}
	segs := make([]*segData, len(singles))
	_ = b.parallel(ctx, len(singles), func(ctx context.Context, i int) error {
		seg, err := b.e.segment(ctx, b.rel, singles[i], 0, b.prio, b.fo)
		if err == nil {
			segs[i] = seg
		}
		return nil
	})
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	renamed := 0
	if set != nil {
		for i, pf := range singles {
			if segs[i] == nil {
				continue
			}
			if f := identifyByPar2(set, pf, segs[i]); f != nil && b.nameFromPar2(pf, f) {
				renamed++
			}
		}
		if renamed > 0 {
			return true, nil
		}
	}
	var rars []probedRAR
	for i, pf := range singles {
		seg := segs[i]
		if seg == nil || seg.begin != 0 {
			continue
		}
		f := archive.Detect(seg.data)
		if f != archive.FormatRAR4 && f != archive.FormatRAR5 {
			continue
		}
		v, err := b.readCachedRARVolume(ctx, pf, seg.data)
		if err != nil {
			continue
		}
		multi := v.MultiVolume
		hasSplitBefore := false
		hasSplitAfter := false
		for _, rf := range v.Files {
			if rf.SplitBefore {
				multi = true
				hasSplitBefore = true
			}
			if rf.SplitAfter {
				multi = true
				hasSplitAfter = true
			}
		}
		if !multi {
			continue
		}
		num := -1
		if v.Number >= 0 {
			num = v.Number
		} else if v.First {
			num = 0
		}
		rars = append(rars, probedRAR{
			pf:      pf,
			format:  f,
			number:  num,
			first:   v.First || (hasSplitAfter && !hasSplitBefore),
			last:    hasSplitBefore && !hasSplitAfter && (!v.EndFound || !v.MoreVolumes),
			nameNum: trailingRARNumber(pf.Name()),
		})
	}
	if len(rars) < 2 {
		return false, nil
	}
	// If RAR4 continuation volumes lack numbers, try their trailing 0x7b end
	// block when all volumes look streamable.
	canProbeTails := true
	for _, r := range rars {
		if seg, ok := b.e.hot.get(segKey{b.rel.digest, r.pf.segBase}); ok && seg != nil && seg.begin == 0 {
			if v, err := b.readCachedRARVolume(ctx, r.pf, seg.data); err != nil || b.checkRARVolume(v) != nil {
				canProbeTails = false
				break
			}
		}
	}
	var needTail []int
	for i, r := range rars {
		if r.format == archive.FormatRAR4 && r.number < 0 {
			if r.pf.numSlots() == 1 {
				if seg, ok := b.e.hot.get(segKey{b.rel.digest, r.pf.segBase}); ok && seg != nil {
					if n := archive.ReadRAR4TailVolumeNumber(seg.data); n >= 0 {
						rars[i].number = n
					}
				}
			} else if r.pf.numSlots() > 1 {
				needTail = append(needTail, i)
			}
		}
	}
	if canProbeTails && len(needTail) > 0 {
		_ = b.parallel(ctx, len(needTail), func(ctx context.Context, k int) error {
			idx := needTail[k]
			pf := rars[idx].pf
			slot := pf.numSlots() - 1
			msgID := pf.msgID(slot)
			if msgID == "" {
				return nil
			}
			data, _, err := b.e.bodyPart(ctx, pf, slot, msgID, b.prio, b.fo)
			if err == nil && len(data) > 0 {
				if n := archive.ReadRAR4TailVolumeNumber(data); n >= 0 {
					rars[idx].number = n
				}
			}
			return nil
		})
	}
	allNumbered := true
	allNameNumbered := true
	seenNameNum := make(map[int]bool, len(rars))
	for _, r := range rars {
		if r.number < 0 {
			allNumbered = false
		}
		if r.nameNum < 0 || seenNameNum[r.nameNum] {
			allNameNumbered = false
		} else {
			seenNameNum[r.nameNum] = true
		}
	}
	sort.SliceStable(rars, func(i, j int) bool {
		a, c := rars[i], rars[j]
		if allNumbered {
			return a.number < c.number
		}
		if a.first != c.first {
			return a.first
		}
		if a.last != c.last {
			return !a.last
		}
		if allNameNumbered && a.nameNum != c.nameNum {
			return a.nameNum < c.nameNum
		}
		if a.pf.file.SubjectIndex != c.pf.file.SubjectIndex {
			return a.pf.file.SubjectIndex < c.pf.file.SubjectIndex
		}
		return a.pf.idx < c.pf.idx
	})
	for i, r := range rars {
		vol := i
		if allNumbered {
			vol = r.number
		}
		b.rel.setFileInfo(func() {
			r.pf.file.Info = nzb.FileInfo{Kind: nzb.KindRAR, Ext: "rar", Volume: vol, SetName: "\x00regrouped-rar", NewStyle: true}
		})
	}
	return true, nil
}

// trailingRARNumber parses a trailing separator-delimited number before ".rar",
// e.g. "Movie.001.rar" -> 1 or "file_12.rar" -> 12, returning -1 if none exists.
func trailingRARNumber(name string) int {
	base := strings.ToLower(path.Base(strings.ReplaceAll(name, `\`, "/")))
	if !strings.HasSuffix(base, ".rar") {
		return -1
	}
	stem := strings.TrimSuffix(base, ".rar")
	end := len(stem)
	i := end
	for i > 0 && stem[i-1] >= '0' && stem[i-1] <= '9' {
		i--
	}
	if i == end || i == 0 {
		return -1
	}
	switch stem[i-1] {
	case '.', '_', '-', ' ':
	default:
		return -1
	}
	n, err := strconv.Atoi(stem[i:end])
	if err != nil {
		return -1
	}
	return n
}

// planRAR maps the main video (or non-video member) inside a RAR set.
func (b *builder) planRAR(ctx context.Context, vols []*postedFile) (*plan, error) {
	_, nv := b.nonVideoArchiveHint()
	if !nv {
		if err := b.checkCachedRAR(ctx, candidate{kind: nzb.KindRAR, files: vols}); err != nil {
			return nil, err
		}
	}
	ord, err := b.orderRARVolumes(ctx, vols, orderRAROpts{
		checkLast:       true,
		checkStreamable: !nv,
	})
	if err != nil {
		return nil, err
	}
	if ord.kind == volumeOrderConcat {
		return b.planConcatRAROffsets(ctx, ord.vols, ord.baseOff, ord.chunkLen)
	}
	if ord.baseOff != nil {
		return b.planRARShifted(ctx, ord.vols, ord.baseOff, ord.chunkLen)
	}
	return b.planRAROrdered(ctx, ord.vols)
}

func (b *builder) planRARShifted(ctx context.Context, vols []*postedFile, baseOff, chunkLen []int64) (*plan, error) {
	parsed := make([]*archive.RARVolume, len(vols))
	err := b.parallel(ctx, len(vols), func(ctx context.Context, k int) error {
		r := offsetReaderAt{r: b.reader(ctx, vols[k]), base: baseOff[k], size: chunkLen[k]}
		v, err := archive.ReadRARVolume(r, chunkLen[k], b.rarOptionsFor(ctx, vols[k], 0))
		if err != nil {
			return fmt.Errorf("stream: rar volume %d (%s): %w", k+1, vols[k].Name(), err)
		}
		if err := b.checkRARVolume(v); err != nil {
			return err
		}
		parsed[k] = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return b.rarFromVolumesOffsets(ctx, vols, parsed, baseOff, chunkLen)
}

func (b *builder) planRAROrdered(ctx context.Context, vols []*postedFile) (*plan, error) {
	if _, nv := b.nonVideoArchiveHint(); nv {
		if len(vols) == 1 {
			if err := b.heads(ctx, vols[:1]); err != nil {
				return nil, err
			}
			v0, err := archive.ReadRARVolume(b.reader(ctx, vols[0]), vols[0].Size(), b.rarOptionsFor(ctx, vols[0], 0))
			if err != nil {
				return nil, fmt.Errorf("stream: rar %s: %w", vols[0].Name(), err)
			}
			return b.rarFromVolumes(ctx, vols, []*archive.RARVolume{v0})
		}
		return b.rarFullScan(ctx, vols)
	}
	n := len(vols)
	if _, ok := b.rel.doc.EpisodeHint(); ok && n > 1 {
		// A season in one archive: the wanted episode is not the member that
		// spans the set from volume 1, so the uniform-layout shortcut below
		// does not apply. Read every volume's headers, unless the set is
		// plainly one other episode.
		if other, err := b.holdsOnlyOtherEpisode(ctx, vols); err != nil {
			return nil, err
		} else if other {
			return nil, fmt.Errorf("%w: rar %s holds a single other episode", ErrNoPlayableFile, vols[0].Name())
		}
		return b.rarFullScan(ctx, vols)
	}
	var v0 *archive.RARVolume
	if n == 1 || vols[0].SizeKnown() {
		if err := b.heads(ctx, vols[:1]); err != nil {
			return nil, err
		}
		var err error
		v0, err = archive.ReadRARVolume(b.reader(ctx, vols[0]), vols[0].Size(), b.rarOptionsFor(ctx, vols[0], 0))
		if err != nil {
			return nil, fmt.Errorf("stream: rar %s: %w", vols[0].Name(), err)
		}
		if err := b.checkRARVolume(v0); err != nil {
			return nil, err
		}
		if n == 1 {
			return b.rarFromVolumes(ctx, vols, []*archive.RARVolume{v0})
		}
	}
	probe := []*postedFile{vols[0]}
	if n > 1 {
		probe = append(probe, vols[1])
	}
	if n > 2 {
		probe = append(probe, vols[n-1])
	}
	if err := b.parallel(ctx, len(probe), func(ctx context.Context, i int) error {
		seg, err := b.e.segment(ctx, b.rel, probe[i], 0, b.prio, b.fo)
		if err != nil {
			return err
		}
		if seg != nil && seg.begin == 0 {
			v, err := b.readCachedRARVolume(ctx, probe[i], seg.data)
			if err != nil {
				if errors.Is(err, archive.ErrEncrypted) {
					return fmt.Errorf("stream: rar %s: %w", probe[i].Name(), err)
				}
				return nil
			}
			return b.checkRARVolume(v)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if v0 == nil {
		var err error
		v0, err = archive.ReadRARVolume(b.reader(ctx, vols[0]), vols[0].Size(), b.rarOptionsFor(ctx, vols[0], 0))
		if err != nil {
			return nil, fmt.Errorf("stream: rar %s: %w", vols[0].Name(), err)
		}
		if err := b.checkRARVolume(v0); err != nil {
			return nil, err
		}
	}
	if n == 1 {
		return b.rarFromVolumes(ctx, vols, []*archive.RARVolume{v0})
	}
	main := pickRARFile(v0.Files, v0.Format)
	v0Size := vols[0].Size()
	if main != nil {
		v0Size = rarVolSize(vols[0], main.DataOffset, main.PackedSize)
	}
	// Estimated decoded size of the whole set (middle volumes as volume 2).
	setBytes := v0Size + vols[n-1].Size()
	if n > 2 {
		setBytes += int64(n-2) * vols[1].Size()
	}
	_, hasMovieHint := b.rel.doc.MovieHint()
	if main == nil || main.Encrypted || (hasMovieHint && rarVideoFileCount(v0.Files) > 1) || b.startsWithRAR(ctx, vols[0], main) || !main.SplitAfter || main.SplitBefore || main.UnpackedSize <= 0 || main.UnpackedSize*2 < setBytes {
		for _, k := range []int{1, n - 1} {
			if k <= 0 || k >= n {
				continue
			}
			v, err := archive.ReadRARVolume(b.reader(ctx, vols[k]), vols[k].Size(), b.rarOptionsFor(ctx, vols[k], 0))
			if err != nil {
				return nil, fmt.Errorf("stream: rar volume %d (%s): %w", k+1, vols[k].Name(), err)
			}
			if err := b.checkRARVolume(v); err != nil {
				return nil, err
			}
		}
		// The video is not a single member spanning the set from volume 1
		// (or volume 1 has no video): read every volume's headers.
		return b.rarFullScan(ctx, vols)
	}

	read := func(k int) (*archive.RARFile, error) {
		maxFiles := 1
		if hasMovieHint && k == n-1 {
			maxFiles = 0
		}
		v, err := archive.ReadRARVolume(b.reader(ctx, vols[k]), vols[k].Size(), b.rarOptionsFor(ctx, vols[k], maxFiles))
		if err != nil {
			return nil, fmt.Errorf("stream: rar volume %d (%s): %w", k+1, vols[k].Name(), err)
		}
		if err := b.checkRARVolume(v); err != nil {
			return nil, err
		}
		if hasMovieHint && rarVideoFileCount(v.Files) > 1 {
			return nil, nil
		}
		if (k > 0 && rarVolumeClaimsFirst(v)) || (v.Number >= 0 && v.Number != k) {
			return nil, nil
		}
		if len(v.Files) == 0 || v.Files[0].Name != main.Name || !v.Files[0].SplitBefore {
			return nil, nil
		}
		return &v.Files[0], nil
	}
	f1, err := read(1)
	if err != nil {
		return nil, err
	}
	fl := f1
	if n > 2 {
		if fl, err = read(n - 1); err != nil {
			return nil, err
		}
	}
	if f1 == nil || fl == nil || fl.SplitAfter {
		return b.rarFullScan(ctx, vols)
	}

	info := make([]volInfo, n)
	verified := make([]bool, n)
	info[0] = volInfo{size: rarVolSize(vols[0], main.DataOffset, main.PackedSize), dataOff: main.DataOffset, dataSize: main.PackedSize}
	info[1] = volInfo{size: rarVolSize(vols[1], f1.DataOffset, f1.PackedSize), dataOff: f1.DataOffset, dataSize: f1.PackedSize}
	info[n-1] = volInfo{size: rarVolSize(vols[n-1], fl.DataOffset, fl.PackedSize), dataOff: fl.DataOffset, dataSize: fl.PackedSize}
	verified[0], verified[1], verified[n-1] = true, true, true
	rar5 := v0.Format == archive.FormatRAR5
	for k := 2; k < n-1; k++ {
		// Middle volumes repeat volume 2's layout. RAR5 headers carry the
		// volume number as a vint, which grows by a byte at 128 and 16384.
		var delta int64
		if rar5 {
			delta = int64(vintLen(uint64(k)) - vintLen(1))
		}
		info[k] = volInfo{size: info[1].size, dataOff: info[1].dataOff + delta, dataSize: info[1].dataSize - delta}
	}
	p := newPlan(b.rel, kindRAR, path.Base(main.Name), main.UnpackedSize, vols, info, verified)
	p.member = main.Name
	if err := p.init(); err != nil {
		// The uniform model does not add up: learn every volume.
		b.e.log.Debug("rar uniform layout rejected; reading all volume headers", "name", main.Name, "err", err)
		if err := p.rebuildExact(ctx, verifier{e: b.e, prio: b.prio, fo: b.fo}); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// holdsOnlyOtherEpisode is a two-fetch pre-filter for a hinted set: when its
// first volume opens with a stored video that is not the wanted episode and
// continues to the last volume, whose only content is that member's tail, the
// whole set is that one member and its other volumes need not be read. Any
// doubt (a read error, another member anywhere) answers false.
func (b *builder) holdsOnlyOtherEpisode(ctx context.Context, vols []*postedFile) (bool, error) {
	h, ok := b.rel.doc.EpisodeHint()
	if !ok || len(vols) < 2 {
		return false, nil
	}
	ends := []*postedFile{vols[0], vols[len(vols)-1]}
	if err := b.heads(ctx, ends); err != nil {
		return false, err
	}
	var parsed [2]*archive.RARVolume
	for i, pf := range ends {
		v, err := archive.ReadRARVolume(b.reader(ctx, pf), pf.Size(), b.rarOptionsFor(ctx, pf, 0))
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, nil
		}
		parsed[i] = v
	}
	var first *archive.RARFile
	for i := range parsed[0].Files {
		if f := &parsed[0].Files[i]; !f.Dir {
			first = f
			break
		}
	}
	if first == nil || !first.SplitAfter || !first.Stored || first.Encrypted ||
		!nzb.IsVideoExt(path.Ext(first.Name)) || memberMatchesHint(h, first.Name) {
		return false, nil
	}
	tail := 0
	for _, f := range parsed[1].Files {
		if f.Dir {
			continue
		}
		if !f.SplitBefore || f.Name != first.Name {
			return false, nil
		}
		tail++
	}
	return tail > 0, nil
}

// rarFullScan reads the headers of every volume and composes members.
func (b *builder) rarFullScan(ctx context.Context, vols []*postedFile) (*plan, error) {
	if err := b.heads(ctx, vols); err != nil {
		return nil, err
	}
	parsed := make([]*archive.RARVolume, len(vols))
	err := b.parallel(ctx, len(vols), func(ctx context.Context, k int) error {
		v, err := archive.ReadRARVolume(b.reader(ctx, vols[k]), vols[k].Size(), b.rarOptionsFor(ctx, vols[k], 0))
		if err != nil {
			return fmt.Errorf("stream: rar volume %d (%s): %w", k+1, vols[k].Name(), err)
		}
		if err := b.checkRARVolume(v); err != nil {
			return err
		}
		parsed[k] = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	vols, parsed, err = reorderParsedRARVolumes(vols, parsed)
	if err != nil {
		return nil, err
	}
	return b.rarFromVolumes(ctx, vols, parsed)
}

func (b *builder) rarFromVolumes(ctx context.Context, vols []*postedFile, parsed []*archive.RARVolume) (*plan, error) {
	return b.rarFromVolumesOffsets(ctx, vols, parsed, nil, nil)
}

func (b *builder) rarFromVolumesOffsets(ctx context.Context, vols []*postedFile, parsed []*archive.RARVolume, baseOff, chunkLen []int64) (*plan, error) {
	members, err := archive.ComposeRAR(parsed)
	if err != nil {
		return nil, fmt.Errorf("stream: rar %s: %w", vols[0].Name(), err)
	}
	nvHint, nv := b.nonVideoArchiveHint()
	var m *archive.Member
	if nv {
		var p *plan
		var merr error
		m, p, merr = b.pickNonVideoMember(ctx, vols, members, nvHint, false, baseOff == nil)
		if merr != nil {
			return nil, merr
		}
		if p != nil {
			return p, nil
		}
	} else {
		m = b.chooseMember(members)
		if baseOff == nil {
			if p, err := b.tryNested(ctx, vols, members, m, false); err != nil {
				return nil, err
			} else if p != nil {
				return p, nil
			}
		}
		if m == nil {
			return nil, fmt.Errorf("%w: rar %s holds no video (%d files)", ErrNoPlayableFile, vols[0].Name(), len(members))
		}
	}
	plan := func(m *archive.Member) (*plan, error) {
		if m.Encrypted && b.rel.doc.Password() == "" {
			return nil, archive.ErrPasswordRequired
		}
		if err := m.Streamable(); err != nil {
			if nv && canHandOffRawArchive(m, err) {
				return b.planRawArchiveVolumesOffsets(ctx, vols, baseOff, chunkLen, "rar", m.Name, false)
			}
			return nil, fmt.Errorf("stream: rar %s: %s: %w", vols[0].Name(), m.Name, err)
		}
		info := make([]volInfo, len(vols))
		verified := make([]bool, len(vols))
		for k := range vols {
			if baseOff != nil {
				info[k] = volInfo{size: baseOff[k] + chunkLen[k]}
			} else {
				info[k] = volInfo{size: vols[k].Size()}
			}
			verified[k] = true
		}
		if m.Encryption != nil {
			encMember := *m
			if baseOff != nil {
				encMember.Extents = append([]archive.Extent(nil), m.Extents...)
				for i := range encMember.Extents {
					x := &encMember.Extents[i]
					if x.Volume >= 0 && x.Volume < len(vols) {
						x.Offset += baseOff[x.Volume]
					}
				}
			} else {
				for _, x := range m.Extents {
					if x.Volume >= 0 && x.Volume < len(vols) {
						info[x.Volume].size = rarVolSize(vols[x.Volume], x.Offset, x.Size)
					}
				}
			}
			p, err := b.encryptedPlan(ctx, kindRAR, &encMember, vols, info)
			if err == nil {
				p.shiftedOffsets = baseOff != nil
				if nv {
					p.rawNonVideo = true
					p.resolvedMember = true
				}
			}
			return p, err
		}
		for _, x := range m.Extents {
			if x.Volume < 0 || x.Volume >= len(vols) {
				return nil, fmt.Errorf("stream: rar extent in volume %d of %d: %w", x.Volume, len(vols), ErrCorrupt)
			}
			if info[x.Volume].dataSize != 0 {
				return nil, fmt.Errorf("stream: rar member %s has two extents in volume %d: %w", m.Name, x.Volume, ErrUnsupported)
			}
			off := x.Offset
			if baseOff != nil {
				off += baseOff[x.Volume]
			}
			info[x.Volume].dataOff, info[x.Volume].dataSize = off, x.Size
			if baseOff == nil {
				info[x.Volume].size = rarVolSize(vols[x.Volume], x.Offset, x.Size)
			}
		}
		p := newPlan(b.rel, kindRAR, path.Base(m.Name), m.Size, vols, info, verified)
		p.member = m.Name
		p.rawNonVideo = nv
		p.resolvedMember = nv
		p.shiftedOffsets = baseOff != nil
		return p, p.init()
	}
	p, err := plan(m)
	if err == nil && !nv {
		b.keepSiblings(p, members, m, plan)
	}
	if err == nil && !nv && isISOName(p.name) {
		b.stashDiscs(members, m, plan)
	}
	return p, err
}

func (b *builder) keepSiblings(p *plan, members []archive.Member, m *archive.Member, plan func(*archive.Member) (*plan, error)) {
	if b.e.opts.OnLayout != nil && !isISOName(p.name) {
		for i := range members {
			other := &members[i]
			info := nzb.Classify(other.Name)
			if other.Name != m.Name && info.Kind == nzb.KindVideo && !info.Sample && !info.Extra && other.Stored && !other.Encrypted {
				if q, e := plan(other); e == nil {
					p.siblings = append(p.siblings, q)
				}
			}
		}
	}
}

// stashDiscs plans the archive's other disc images, the further discs of a
// multi-disc release, for expandISO to join.
func (b *builder) stashDiscs(members []archive.Member, chosen *archive.Member, plan func(*archive.Member) (*plan, error)) {
	for i := range members {
		m := &members[i]
		if len(b.memberISOs) >= maxDiscSiblings {
			return
		}
		if m.Dir || m.Name == chosen.Name || nzb.Classify(m.Name).Kind != nzb.KindISO || m.Size <= 0 {
			continue
		}
		if p, err := plan(m); err == nil {
			b.memberISOs = append(b.memberISOs, p)
		}
	}
}

// plan7z maps the main video inside a copy-method 7z set.
func (b *builder) plan7z(ctx context.Context, vols []*postedFile) (*plan, error) {
	n := len(vols)
	// Encrypted headers and payloads cannot use unverified volume offsets.
	if b.rel.doc.Password() != "" {
		if err := b.heads(ctx, vols); err != nil {
			return nil, err
		}
	}
	// The signature header is at the start of volume 1 and the archive
	// header at the end of the last volume: fetch both ends at once.
	last := vols[n-1]
	err := b.parallel(ctx, 2, func(ctx context.Context, i int) error {
		if i == 0 {
			_, err := b.e.segment(ctx, b.rel, vols[0], 0, b.prio, b.fo)
			return err
		}
		_, err := b.e.segment(ctx, b.rel, last, last.numSlots()-1, b.prio, b.fo)
		return err
	})
	if err != nil {
		return nil, err
	}
	head, err := b.sniff(ctx, vols[0])
	if err != nil {
		return nil, err
	}
	if archive.Detect(head) != archive.Format7z || len(head) < 32 {
		return nil, fmt.Errorf("stream: %s is not a 7z archive: %w", vols[0].Name(), ErrCorrupt)
	}
	want := 32 + int64(binary.LittleEndian.Uint64(head[12:20])) + int64(binary.LittleEndian.Uint64(head[20:28]))

	info := make([]volInfo, n)
	verified := make([]bool, n)
	sizes := func() int64 {
		var t int64
		for k := range vols {
			if k == 0 || k == n-1 || verified[k] || vols[k].SizeKnown() {
				info[k] = volInfo{size: vols[k].Size()}
				verified[k] = true
			} else {
				info[k] = volInfo{size: vols[0].Size()}
			}
			t += info[k].size
		}
		return t
	}
	total := sizes()
	if total != want {
		b.e.log.Debug("7z uniform volume sizes rejected; learning every volume", "name", vols[0].Name(), "total", total, "want", want)
		if err := b.heads(ctx, vols); err != nil {
			return nil, err
		}
		for k := range verified {
			verified[k] = true
		}
		if total = sizes(); total != want {
			return nil, fmt.Errorf("stream: 7z volumes total %d bytes, header says %d: %w", total, want, ErrCorrupt)
		}
	}
	for k := range vols {
		vols[k].setAssumed(info[k].size)
	}
	av := make([]archive.Volume, n)
	for k, pf := range vols {
		av[k] = archive.Volume{R: b.reader(ctx, pf), Size: info[k].size}
	}
	members, err := archive.List7zWithPassword(ctx, archive.NewMultiReader(av), total, b.rel.doc.Password())
	if err != nil {
		return nil, fmt.Errorf("stream: 7z %s: %w", vols[0].Name(), err)
	}
	nvHint, nv := b.nonVideoArchiveHint()
	var m *archive.Member
	if nv {
		var p *plan
		var merr error
		m, p, merr = b.pickNonVideoMember(ctx, vols, members, nvHint, true, true)
		if merr != nil {
			return nil, merr
		}
		if p != nil {
			return p, nil
		}
	} else {
		m = b.chooseMember(members)
		if p, err := b.tryNested(ctx, vols, members, m, true); err != nil {
			return nil, err
		} else if p != nil {
			return p, nil
		}
		if m == nil {
			return nil, fmt.Errorf("%w: 7z %s holds no video (%d files)", ErrNoPlayableFile, vols[0].Name(), len(members))
		}
	}
	plan := func(m *archive.Member) (*plan, error) {
		if err := m.Streamable(); err != nil {
			if nv && canHandOffRawArchive(m, err) {
				return b.planRawArchiveVolumes(ctx, vols, "7z", m.Name, false)
			}
			return nil, fmt.Errorf("stream: 7z %s: %s: %w", vols[0].Name(), m.Name, err)
		}
		if m.Encryption != nil {
			p, err := b.encryptedPlan(ctx, kind7z, m, vols, info)
			if err == nil && nv {
				p.rawNonVideo = true
				p.resolvedMember = true
			}
			return p, err
		}
		p := newPlan(b.rel, kind7z, path.Base(m.Name), m.Size, vols, info, verified)
		p.concatExt = m.Extents
		p.rawNonVideo = nv
		p.resolvedMember = nv
		return p, p.init()
	}
	p, err := plan(m)
	if err == nil && !nv {
		b.keepSiblings(p, members, m, plan)
	}
	if err == nil && !nv && isISOName(p.name) {
		b.stashDiscs(members, m, plan)
	}
	return p, err
}

// planZIP maps the main video inside a store-mode ZIP or ZIP64 set (.zip,
// .zip.001, or .z01+.zip).
func (b *builder) planZIP(ctx context.Context, vols []*postedFile) (*plan, error) {
	if err := b.heads(ctx, vols); err != nil {
		return nil, err
	}
	last := vols[len(vols)-1]
	if !last.SizeKnown() {
		slot := last.numSlots() - 1
		ls, err := b.e.segment(ctx, b.rel, last, slot, b.prio, b.fo)
		if err != nil {
			return nil, err
		}
		last.learn(slot, ls.begin, ls.end(), ls.end(), 0)
	}
	head, err := b.sniff(ctx, vols[0])
	if err != nil {
		return nil, err
	}
	if archive.Detect(head) != archive.FormatZIP {
		return nil, fmt.Errorf("stream: %s is not a zip archive: %w", vols[0].Name(), ErrCorrupt)
	}
	n := len(vols)
	info := make([]volInfo, n)
	verified := make([]bool, n)
	av := make([]archive.Volume, n)
	for k, pf := range vols {
		s := pf.Size()
		info[k] = volInfo{size: s}
		verified[k] = true
		av[k] = archive.Volume{R: b.reader(ctx, pf), Size: s}
	}
	members, err := archive.ListZIP(av)
	if err != nil {
		return nil, fmt.Errorf("stream: zip %s: %w", vols[0].Name(), err)
	}
	nvHint, nv := b.nonVideoArchiveHint()
	var m *archive.Member
	if nv {
		var p *plan
		var merr error
		m, p, merr = b.pickNonVideoMember(ctx, vols, members, nvHint, true, true)
		if merr != nil {
			return nil, merr
		}
		if p != nil {
			return p, nil
		}
	} else {
		m = b.chooseMember(members)
		if p, err := b.tryNested(ctx, vols, members, m, true); err != nil {
			return nil, err
		} else if p != nil {
			return p, nil
		}
		if m == nil {
			return nil, fmt.Errorf("%w: zip %s holds no video (%d files)", ErrNoPlayableFile, vols[0].Name(), len(members))
		}
	}
	makePlan := func(m *archive.Member) (*plan, error) {
		if err := m.Streamable(); err != nil {
			if nv && canHandOffRawArchive(m, err) {
				return b.planRawArchiveVolumes(ctx, vols, "zip", m.Name, false)
			}
			return nil, fmt.Errorf("stream: zip %s: %s: %w", vols[0].Name(), m.Name, err)
		}
		p := newPlan(b.rel, kindZIP, path.Base(m.Name), m.Size, vols, info, verified)
		p.concatExt = m.Extents
		p.rawNonVideo = nv
		p.resolvedMember = nv
		return p, p.init()
	}
	p, err := makePlan(m)
	if err == nil && !nv {
		b.keepSiblings(p, members, m, makePlan)
	}
	return p, err
}

// chooseMember is chooseMember restricted to the episode the NZB asks for
// (nzb.MetaEpisode), when it names one. A member that is not that episode is
// never played in its place.
func (b *builder) chooseMember(members []archive.Member) *archive.Member {
	if movie, ok := b.rel.doc.MovieHint(); ok {
		var features []archive.Member
		var maxSize int64
		for _, m := range members {
			info := nzb.Classify(m.Name)
			if !m.Dir && (info.Kind == nzb.KindVideo || info.Kind == nzb.KindISO) && !info.Sample && !info.Extra {
				features = append(features, m)
				if m.Size > maxSize {
					maxSize = m.Size
				}
			}
		}
		if len(features) <= 1 {
			return chooseMember(members)
		}
		// Only apply strict movie-pack title filtering when there are at least
		// two full-length features (>= 35% of the largest video) and at least
		// one carries a real movie filename (not disc streams or obfuscated
		// names). A single movie accompanied by short unlabelled clips, or a
		// multi-m2ts/VOB disc archive, is still a single-film release.
		fullCount := 0
		hasTitledFeature := false
		for _, m := range features {
			if maxSize <= 0 || m.Size*100 >= 35*maxSize {
				fullCount++
				if !isUnnamedMovieStream(m.Name) {
					hasTitledFeature = true
				}
			}
		}
		if fullCount <= 1 || !hasTitledFeature {
			return chooseMember(members)
		}
		matching := features
		if len(movie.Titles) > 0 {
			matching = nil
			for _, m := range features {
				if releaseparse.MovieFileMatches(m.Name, movie.Titles, movie.Year) {
					matching = append(matching, m)
				}
			}
		}
		if len(matching) > 1 && len(movie.WantedLanguages) > 0 {
			mc := make([]releaseparse.MovieCandidate, len(matching))
			for i, m := range matching {
				mc[i] = releaseparse.MovieCandidate{Path: m.Name, Size: m.Size}
			}
			order := releaseparse.OrderMovieCandidates(mc, movie.Titles, movie.OriginalLanguage, movie.WantedLanguages)
			chosen := matching[order[0]]
			return &chosen
		}
		return chooseMember(matching)
	}
	h, ok := b.rel.doc.EpisodeHint()
	if !ok {
		return chooseMember(members)
	}
	sizeGate := archiveEpisodeSizeGate(members, h)
	var matching []archive.Member
	for _, m := range members {
		if memberMatchesHint(h, m.Name) && sizeGate.Accept(m.Name, m.Size) {
			matching = append(matching, m)
		}
	}
	return chooseMember(matching)
}

func archiveEpisodeSizeGate(members []archive.Member, h nzb.EpisodeHint) releaseparse.PackEpisodeSizeGate {
	if h.Season <= 0 || h.Episode <= 0 {
		return releaseparse.PackEpisodeSizeGate{}
	}
	sized := make([]releaseparse.PackSizedFile, 0, len(members))
	for _, m := range members {
		info := nzb.Classify(m.Name)
		if !m.Dir && info.Kind == nzb.KindVideo && !info.Sample && !info.Extra {
			sized = append(sized, releaseparse.PackSizedFile{Path: m.Name, Size: m.Size})
		}
	}
	return releaseparse.NewPackEpisodeSizeGate(sized, h.Season, 0)
}

// isUnnamedMovieStream reports whether a video member path is an unnamed disc
// stream (BDMV/STREAM/00001.m2ts, VIDEO_TS/VTS_01_1.VOB, numeric stem) or an
// obfuscated filename rather than a named movie file in a multi-movie pack.
func isUnnamedMovieStream(name string) bool {
	norm := strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	lower := strings.ToLower(norm)
	if strings.Contains(lower, "/bdmv/") || strings.HasPrefix(lower, "bdmv/") ||
		strings.Contains(lower, "/video_ts/") || strings.HasPrefix(lower, "video_ts/") {
		return true
	}
	base := path.Base(norm)
	if nzb.IsObfuscated(base) {
		return true
	}
	stem := strings.TrimSuffix(strings.ToLower(base), strings.ToLower(path.Ext(base)))
	if stem == "" {
		return true
	}
	allDigits := true
	for i := 0; i < len(stem); i++ {
		if stem[i] < '0' || stem[i] > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return true
	}
	return strings.HasPrefix(stem, "vts_")
}

// memberMatchesHint reports whether an archive member's name is the episode h.
func memberMatchesHint(h nzb.EpisodeHint, name string) bool {
	info := releaseparse.Parse(path.Base(strings.ReplaceAll(name, `\`, "/")))
	if releaseparse.EpisodeMatches(info, h.Season, h.Episode) ||
		(h.Absolute > 0 && releaseparse.EpisodeMatchesAbsolute(info, h.Absolute)) ||
		(len(info.Episodes) == 0 && releaseparse.ClassicPackEpisodeMatches(name, h.Season, h.Episode)) {
		return true
	}
	for _, a := range h.Aliases {
		if releaseparse.EpisodeMatches(info, a[0], a[1]) ||
			(len(info.Episodes) == 0 && releaseparse.ClassicPackEpisodeMatches(name, a[0], a[1])) {
			return true
		}
	}
	return false
}

// pickRARFile chooses the likely video among the file headers of a volume.
func pickRARFile(files []archive.RARFile, format ...archive.Format) *archive.RARFile {
	var fmt archive.Format
	if len(format) > 0 {
		fmt = format[0]
	}
	members := make([]archive.Member, len(files))
	for i, f := range files {
		members[i] = rarMember(fmt, &f)
	}
	m := chooseMember(members)
	if m == nil {
		return nil
	}
	for i := range files {
		if files[i].Name == m.Name {
			return &files[i]
		}
	}
	return nil
}

func rarMember(format archive.Format, f *archive.RARFile) archive.Member {
	return archive.Member{
		Name: f.Name, Size: f.UnpackedSize, Stored: f.Stored,
		Encrypted: f.Encrypted, Encryption: f.Encryption,
		Solid: f.Solid, Dir: f.Dir, Method: f.MethodName(format),
	}
}

// checkRARVolume fails fast when a parsed RAR volume's main video is
// compressed, solid, or encrypted without a usable password.
func (b *builder) checkRARVolume(v *archive.RARVolume) error {
	if v == nil {
		return nil
	}
	if _, nv := b.nonVideoArchiveHint(); nv {
		// Non-video releases may extract compressed or solid RAR members via
		// kindstream.openContainerOverVolumes; member selection and encryption
		// checks happen once all volumes are composed in rarFromVolumes.
		return nil
	}
	files := v.Files
	if movie, ok := b.rel.doc.MovieHint(); ok && rarVideoFileCount(v.Files) > 1 {
		// A multi-movie RAR chooses its member after scanning all volumes; do
		// not reject the set early on a sibling version.
		_ = movie
		return nil
	}
	if h, ok := b.rel.doc.EpisodeHint(); ok {
		// A season archive: only the wanted episode's member matters, so
		// another episode's compression or encryption must not reject it.
		files = nil
		for _, f := range v.Files {
			if memberMatchesHint(h, f.Name) {
				files = append(files, f)
			}
		}
	}
	main := pickRARFile(files, v.Format)
	if main == nil {
		return nil
	}
	if main.Encrypted && b.rel.doc.Password() == "" {
		return archive.ErrPasswordRequired
	}
	if m := rarMember(v.Format, main); m.Streamable() != nil {
		return fmt.Errorf("stream: rar %s: %w", main.Name, m.Streamable())
	}
	return nil
}

// readCachedRARVolume parses the RAR headers present in a volume's already
// fetched first article without issuing any network reads.
func (b *builder) readCachedRARVolume(ctx context.Context, pf *postedFile, head []byte) (*archive.RARVolume, error) {
	volSize := max(pf.Size(), int64(len(head)))
	r := bytes.NewReader(head)
	var last *archive.RARVolume
	for maxFiles := 1; maxFiles <= 8; maxFiles++ {
		v, err := archive.ReadRARVolume(r, volSize, b.rarOptionsFor(ctx, pf, maxFiles))
		if err != nil {
			if last != nil {
				return last, nil
			}
			return nil, err
		}
		last = v
		if pickRARFile(v.Files, v.Format) != nil || len(v.Files) < maxFiles {
			return v, nil
		}
		f := v.Files[len(v.Files)-1]
		if f.SplitAfter || f.DataOffset+f.PackedSize >= int64(len(head)) {
			return v, nil
		}
	}
	return last, nil
}

// checkCachedRAR inspects any already-cached volume heads of a RAR candidate
// and fails fast when they show the main video cannot be streamed.
func (b *builder) checkCachedRAR(ctx context.Context, c candidate) error {
	if c.kind != nzb.KindRAR {
		return nil
	}
	for _, pf := range c.files {
		seg, ok := b.e.hot.get(segKey{b.rel.digest, pf.segBase})
		if !ok || seg == nil || seg.begin != 0 {
			continue
		}
		v, err := b.readCachedRARVolume(ctx, pf, seg.data)
		if err != nil {
			if errors.Is(err, archive.ErrEncrypted) {
				return fmt.Errorf("stream: rar %s: %w", pf.Name(), err)
			}
			continue
		}
		if err := b.checkRARVolume(v); err != nil {
			return err
		}
	}
	return nil
}

// chooseMember picks the main video of an archive: the largest video that
// is not a sample or extra; failing that (obfuscated inner names), the
// largest file without a known non-video type if it dominates the archive.
func chooseMember(members []archive.Member) *archive.Member {
	var best, disc, anon *archive.Member
	var total int64
	for i := range members {
		m := &members[i]
		if m.Dir {
			continue
		}
		total += max(m.Size, 0)
		info := nzb.Classify(m.Name)
		switch {
		case info.Kind == nzb.KindVideo && !info.Sample && !info.Extra:
			if best == nil || m.Size > best.Size {
				best = m
			}
		case info.Kind == nzb.KindISO:
			if disc == nil || m.Size > disc.Size {
				disc = m
			}
		case info.Kind == nzb.KindUnknown:
			if anon == nil || m.Size > anon.Size {
				anon = m
			}
		}
	}
	if best != nil {
		return best
	}
	if disc != nil {
		return disc // no video, but a disc image: its main feature plays
	}
	if anon != nil && (anon.Size*2 >= total || (anon.Size < 0 && total == 0)) {
		return anon
	}
	return nil
}

// shouldProbe: unidentifiable files outweigh every recognised candidate.
func (b *builder) shouldProbe(cands []candidate) bool {
	b.rel.infoMu.RLock()
	defer b.rel.infoMu.RUnlock()
	var unknown int64
	for _, pf := range b.rel.files {
		if pf.file.Info.Kind == nzb.KindUnknown {
			unknown += pf.file.Bytes()
		}
	}
	var best int64
	if len(cands) > 0 {
		best = cands[0].bytes
	}
	return unknown > best
}

// maxProbeRounds bounds identification past MaxProbes: when PAR2 or the
// archive's own volume numbers prove the leading set has volumes the first
// probes did not find, at most this many times MaxProbes heads are read.
const maxProbeRounds = 4

// errUnidentifiedVolumes reports a leading archive set with volumes that
// files left unprobed (the probe budget) or unreadable (a transient fetch
// error) may hold. It is neither a rejection nor a missing article: nothing
// proves those volumes absent.
var errUnidentifiedVolumes = errors.New("stream: archive volumes not identified")

// unknownFiles lists the unidentified files, largest first, at most
// MaxProbes of them.
func (b *builder) unknownFiles() []*postedFile {
	return b.unknownFilesExcept(nil, b.e.opts.MaxProbes)
}

// unknownFilesExcept lists unidentified files not in skip, largest first,
// at most limit of them.
func (b *builder) unknownFilesExcept(skip map[*postedFile]bool, limit int) []*postedFile {
	b.rel.infoMu.RLock()
	defer b.rel.infoMu.RUnlock()
	var unknown []*postedFile
	for _, pf := range b.rel.files {
		if pf.file.Info.Kind == nzb.KindUnknown && !skip[pf] {
			unknown = append(unknown, pf)
		}
	}
	sort.SliceStable(unknown, func(i, j int) bool { return unknown[i].file.Bytes() > unknown[j].file.Bytes() })
	if len(unknown) > limit {
		unknown = unknown[:max(limit, 0)]
	}
	return unknown
}

// identifyObfuscated identifies unidentified files from their first
// articles. Without a PAR2 index every candidate's head is probed at once.
// With one, the index is fetched in parallel with the heads: when one
// unknown file dominates the post (a plain video beside small extras),
// only its head is fetched first, and if that names it a video the other
// probes are skipped; otherwise the leading pair of heads is fetched with
// the index first so non-streamable or unrecognisable multi-part posts fail
// fast without downloading every volume's first segment.
func (b *builder) identifyObfuscated(ctx context.Context) error {
	unknown := b.unknownFiles()
	if len(unknown) == 0 {
		return nil
	}
	idx := b.rel.par2IndexFile()
	if idx == nil {
		// unknownFiles() keeps the largest MaxProbes files; when an obfuscated
		// post has more than MaxProbes volumes, a tiny obfuscated PAR2 index
		// falls outside that slice. Pull the smallest PAR2-readable unknown
		// file into the initial wave so PAR2 can still rename the whole post.
		if small := b.smallestPar2Candidate(unknown); small != nil {
			unknown = append(unknown, small)
		}
	}
	if idx == nil && len(unknown) <= 4 {
		return b.probeAll(ctx, unknown, nil)
	}
	wave := unknown
	if idx == nil {
		wave = []*postedFile{unknown[0], unknown[1], unknown[len(unknown)-1]}
	} else if len(unknown) > 1 && unknown[0].file.Bytes() >= 2*unknown[1].file.Bytes() {
		wave = unknown[:1]
	} else if len(unknown) > 2 {
		wave = unknown[:2]
	}
	var set *par2.Set
	if idx != nil {
		setCh := make(chan *par2.Set, 1)
		go func() { setCh <- b.rel.loadPar2(ctx, b.prio, b.fo, idx) }()
		_ = b.parallel(ctx, len(wave), func(ctx context.Context, i int) error {
			_, _ = b.e.segment(ctx, b.rel, wave[i], 0, b.prio, b.fo)
			return nil
		})
		set = <-setCh
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(wave) < len(unknown) {
		named, unreadable, err := b.probeObfuscated(ctx, wave, set)
		if err != nil {
			return err
		}
		if set == nil {
			set = b.rel.par2Ready()
		}
		cands := b.candidates(b.rel.files)
		if len(cands) > 0 {
			if err := b.checkCachedRAR(ctx, cands[0]); err != nil {
				return err
			}
		}
		info := wave[0].file.Info
		if info.Kind == nzb.KindVideo && !info.Sample && !info.Extra && !b.shouldProbe(cands) {
			if named[wave[0]] {
				b.e.par2Saved.Add(int64(len(unknown) - len(wave)))
			}
			return nil
		}
		if len(wave) >= 2 && wave[0].file.Info.Kind == nzb.KindUnknown && wave[1].file.Info.Kind == nzb.KindUnknown &&
			!unreadable[wave[0]] && !unreadable[wave[1]] && len(cands) == 0 {
			if !wave[0].SizeKnown() && !wave[1].SizeKnown() {
				return incompleteError("leading files %s and %s are missing", wave[0].Name(), wave[1].Name())
			}
			return nil
		}
	}
	return b.probeAll(ctx, unknown, set)
}

// smallestPar2Candidate returns the smallest PAR2-readable unknown file not
// already in exclude, so >MaxProbes obfuscated posts still find their PAR2
// index even when it is tiny compared to media volumes.
func (b *builder) smallestPar2Candidate(exclude []*postedFile) *postedFile {
	skip := make(map[*postedFile]bool, len(exclude))
	for _, pf := range exclude {
		skip[pf] = true
	}
	b.rel.infoMu.RLock()
	defer b.rel.infoMu.RUnlock()
	var best *postedFile
	for _, pf := range b.rel.files {
		if pf.file.Info.Kind != nzb.KindUnknown || skip[pf] || !par2Readable(pf) {
			continue
		}
		if best == nil || pf.file.Bytes() < best.file.Bytes() {
			best = pf
		}
	}
	return best
}

// probeAll probes the unknown files, then once more with the rest of the
// unidentified files (up to maxProbeRounds*MaxProbes in total) when the
// leading archive set is still short of volumes (by PAR2, RAR volume gaps, or
// a trailing SplitAfter marker). Re-probing the first files reads their heads
// from the caches and keeps fallback volume numbering consistent across the
// whole list. A leading set still short of volumes that unprobed or unreadable
// files may hold is reported as errUnidentifiedVolumes, never as missing
// articles.
func (b *builder) probeAll(ctx context.Context, unknown []*postedFile, set *par2.Set) error {
	_, unreadable, err := b.probeObfuscated(ctx, unknown, set)
	if err != nil {
		return err
	}
	if set == nil {
		set = b.rel.par2Ready() // an obfuscated index found among the heads
	}
	if cands := b.candidates(b.rel.files); len(cands) > 0 {
		if err := b.checkCachedRAR(ctx, cands[0]); err != nil {
			return err
		}
	}
	probed := make(map[*postedFile]bool, len(unknown))
	for _, pf := range unknown {
		probed[pf] = true
	}
	lead, short := b.leadingSetShort(set)
	if short || b.rarSetIncomplete(ctx, lead) {
		if more := b.unknownFilesExcept(probed, maxProbeRounds*b.e.opts.MaxProbes-len(unknown)); len(more) > 0 {
			unknown = append(append([]*postedFile(nil), unknown...), more...)
			for _, pf := range more {
				probed[pf] = true
			}
			if _, unreadable, err = b.probeObfuscated(ctx, unknown, set); err != nil {
				return err
			}
			if set == nil {
				set = b.rel.par2Ready()
			}
		}
	}
	lead, short = b.leadingSetShort(set)
	if !short && !b.rarSetIncomplete(ctx, lead) {
		return nil
	}
	unprobed := len(b.unknownFilesExcept(probed, len(b.rel.files)))
	stillUnknown := 0
	b.rel.infoMu.RLock()
	for pf := range unreadable {
		if pf.file.Info.Kind == nzb.KindUnknown {
			stillUnknown++
		}
	}
	b.rel.infoMu.RUnlock()
	if unprobed == 0 && stillUnknown == 0 {
		return nil // every file was read: planning reports the gaps as missing
	}
	return fmt.Errorf("%w: %s set %q is short of volumes; %d files beyond the probe budget, %d unreadable",
		errUnidentifiedVolumes, lead.kind, lead.set.Name, unprobed, stillUnknown)
}

// rarSetIncomplete reports whether an obfuscated RAR set still lacks either
// its opening volume or continuation volumes after the last probed volume.
func (b *builder) rarSetIncomplete(ctx context.Context, lead candidate) bool {
	if lead.kind != nzb.KindRAR || lead.set == nil || len(lead.files) == 0 || lead.set.Name != "\x00obfuscated-rar" {
		return false
	}
	first := lead.files[0]
	if seg, ok := b.e.hot.get(segKey{b.rel.digest, first.segBase}); ok && seg != nil && seg.begin == 0 {
		if v, err := b.readCachedRARVolume(ctx, first, seg.data); err == nil && !v.First {
			return true
		}
	}
	last := lead.files[len(lead.files)-1]
	if seg, ok := b.e.hot.get(segKey{b.rel.digest, last.segBase}); ok && seg != nil && seg.begin == 0 {
		if v, err := b.readCachedRARVolume(ctx, last, seg.data); err == nil {
			if v.EndFound && v.MoreVolumes {
				return true
			}
			for _, f := range v.Files {
				if f.SplitAfter {
					return true
				}
			}
		}
	}
	return false
}

// leadingSetShort reports whether the set planning would try first has
// volumes still unaccounted for: gaps in its volume numbers, or volumes
// PAR2 names that no posted file carries.
func (b *builder) leadingSetShort(set *par2.Set) (candidate, bool) {
	cands := b.candidates(b.rel.files)
	if len(cands) == 0 || cands[0].set == nil {
		return candidate{}, false
	}
	lead := cands[0]
	if len(lead.set.Missing) > 0 {
		return lead, true
	}
	if set == nil {
		return lead, false
	}
	posted := map[string]bool{}
	b.rel.infoMu.RLock()
	for _, pf := range b.rel.files {
		posted[pf.file.Name] = true
	}
	b.rel.infoMu.RUnlock()
	for _, f := range set.Files {
		name := f.BaseName()
		if posted[name] {
			continue
		}
		if info := nzb.Classify(name); info.Kind == lead.kind && info.SetName == lead.set.Name {
			return lead, true
		}
	}
	return lead, false
}

// probeObfuscated fetches the first article of the given unidentified files
// and re-classifies them: from the PAR2 description their head matches
// (set may be nil; a PAR2 index found among the heads is used too), else
// from the yEnc name or the content's magic bytes. It returns the files
// named from PAR2 and those whose head failed other than as missing.
func (b *builder) probeObfuscated(ctx context.Context, unknown []*postedFile, set *par2.Set) (map[*postedFile]bool, map[*postedFile]bool, error) {
	type probe struct {
		seg *segData
		err error
	}
	probes := make([]probe, len(unknown))
	_ = b.parallel(ctx, len(unknown), func(ctx context.Context, i int) error {
		seg, err := b.e.segment(ctx, b.rel, unknown[i], 0, b.prio, b.fo)
		probes[i] = probe{seg, err}
		return nil // one dead file must not stop the others
	})
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	unreadable := map[*postedFile]bool{}
	for i, pr := range probes {
		if pr.err != nil && !isMissing(pr.err) {
			unreadable[unknown[i]] = true
		}
	}
	if set == nil {
		// An obfuscated PAR2 index among the heads: the smallest readable
		// file starting with a PAR2 packet.
		var idx *postedFile
		for i, pf := range unknown {
			if pr := probes[i]; pr.err == nil && pr.seg != nil && pr.seg.begin == 0 && isPar2Head(pr.seg.data) &&
				par2Readable(pf) && (idx == nil || pf.file.Bytes() < idx.file.Bytes()) {
				idx = pf
			}
		}
		if idx != nil {
			set = b.rel.loadPar2(ctx, b.prio, b.fo, idx)
		}
	}
	snap := snapshotArchiveOrder(b.rel)
	named := map[*postedFile]bool{}
	type rarVol struct {
		pf     *postedFile
		format archive.Format
		number int // -1 unknown
	}
	var rars []rarVol
	probeByPF := make(map[*postedFile]probe, len(unknown))
	for i, pf := range unknown {
		pr := probes[i]
		probeByPF[pf] = pr
		if pr.err != nil || pr.seg == nil {
			continue
		}
		if pr.seg.begin == 0 && isPar2Head(pr.seg.data) {
			b.rel.setFileInfo(func() {
				pf.file.Info = nzb.FileInfo{Kind: nzb.KindPar2, Ext: "par2", Volume: -1, SetName: pf.file.Info.SetName}
			})
			continue
		}
		par2File := identifyByPar2(set, pf, pr.seg)
		if par2File != nil && b.nameFromPar2(pf, par2File) {
			named[pf] = true
			continue
		}
		if pr.seg.begin == 0 {
			if yn := path.Base(strings.ReplaceAll(pr.seg.name, `\`, "/")); yn != "" && yn != "." {
				if info := nzb.Classify(yn); info.Kind != nzb.KindUnknown && !nzb.IsObfuscated(yn) {
					b.rel.setFileInfo(func() { pf.file.Name, pf.file.Info, pf.file.Obfuscated = yn, info, false })
					pf.setName(yn)
					continue
				}
			}
		}
		if pr.seg.begin != 0 {
			continue
		}
		// Only sniff magic bytes when the file name itself does not classify
		// its container; already-classified obfuscated files (e.g. "<hex>.mp4"
		// or "<hex>.part01.rar") already carry their container/volume structure,
		// while earlier obfuscated-probe waves keep their unknown name so later
		// waves can still assemble and tail-probe the full RAR set.
		b.rel.infoMu.RLock()
		isUnknown := nzb.Classify(pf.file.Name).Kind == nzb.KindUnknown
		b.rel.infoMu.RUnlock()
		if !isUnknown {
			continue
		}
		head := pr.seg.data
		switch f := archive.Detect(head); f {
		case archive.FormatRAR4, archive.FormatRAR5:
			num := -1
			if v, err := b.readCachedRARVolume(ctx, pf, head); err == nil {
				if v.Number >= 0 {
					num = v.Number
				} else if v.First {
					num = 0
				}
			}
			rars = append(rars, rarVol{pf: pf, format: f, number: num})
		case archive.Format7z:
			b.rel.setFileInfo(func() {
				pf.file.Info = nzb.FileInfo{Kind: nzb.KindSevenZip, Ext: "7z", Volume: 0, SetName: fmt.Sprintf("\x00obfuscated-7z-%d", pf.idx)}
			})
		case archive.FormatZIP:
			b.rel.setFileInfo(func() {
				pf.file.Info = nzb.FileInfo{Kind: nzb.KindZIP, Ext: "zip", Volume: 0, SetName: fmt.Sprintf("\x00obfuscated-zip-%d", pf.idx)}
			})
		default:
			if ext := sniffVideoExt(head); ext != "" {
				b.rel.setFileInfo(func() {
					pf.file.Info = nzb.FileInfo{Kind: nzb.KindVideo, Ext: ext, Volume: -1, SetName: pf.file.Info.SetName}
				})
			} else if par2File != nil {
				if base := par2File.BaseName(); base != "" && base != "." && !nzb.IsObfuscated(base) {
					info := nzb.Classify(base)
					b.rel.setFileInfo(func() { pf.file.Name, pf.file.Info, pf.file.Obfuscated = base, info, false })
					pf.setName(base)
					named[pf] = true
				}
			}
		}
	}
	if reverted := b.guardArchiveRename(ctx, snap); len(reverted) > 0 {
		for pf := range reverted {
			delete(named, pf)
			b.rel.infoMu.RLock()
			isUnknown := nzb.Classify(pf.file.Name).Kind == nzb.KindUnknown
			b.rel.infoMu.RUnlock()
			if !isUnknown {
				continue
			}
			pr := probeByPF[pf]
			if pr.err != nil || pr.seg == nil || pr.seg.begin != 0 {
				continue
			}
			if f := archive.Detect(pr.seg.data); isRARFormat(f) {
				num := -1
				if v, err := b.readCachedRARVolume(ctx, pf, pr.seg.data); err == nil {
					if v.Number >= 0 {
						num = v.Number
					} else if v.First {
						num = 0
					}
				}
				rars = append(rars, rarVol{pf: pf, format: f, number: num})
			}
		}
	}
	if len(rars) == 0 {
		return named, unreadable, nil
	}
	if set == nil && len(rars) > 3 {
		// Wait until the leading volume's cached head passes checkRARVolume before
		// issuing any RAR4 tail probes, so compressed or encrypted sets still
		// fast-fail without extra segment fetches.
		canProbeTails := true
		for _, r := range rars {
			if seg, ok := b.e.hot.get(segKey{b.rel.digest, r.pf.segBase}); ok && seg != nil && seg.begin == 0 {
				if v, err := b.readCachedRARVolume(ctx, r.pf, seg.data); err != nil || b.checkRARVolume(v) != nil {
					canProbeTails = false
					break
				}
			}
		}
		// RAR4 continuation volumes record their volume number in the trailing
		// 0x7b end block rather than the main header. Once the full probe wave
		// runs without PAR2 or [i/N] subject counters (identifyObfuscated's
		// initial no-PAR2 wave has at most 3 files), probe the final segment
		// of unnumbered RAR4 volumes to recover their exact volume numbers.
		hasSubjectOrder := true
		var needTail []int
		for i, r := range rars {
			if r.pf.file.SubjectIndex <= 0 {
				hasSubjectOrder = false
			}
			if r.format == archive.FormatRAR4 && r.number < 0 {
				if r.pf.numSlots() == 1 {
					if seg, ok := b.e.hot.get(segKey{b.rel.digest, r.pf.segBase}); ok && seg != nil {
						if n := archive.ReadRAR4TailVolumeNumber(seg.data); n >= 0 {
							rars[i].number = n
						}
					}
				} else if r.pf.numSlots() > 1 {
					needTail = append(needTail, i)
				}
			}
		}
		if canProbeTails && !hasSubjectOrder && len(needTail) > 0 {
			_ = b.parallel(ctx, len(needTail), func(ctx context.Context, k int) error {
				idx := needTail[k]
				pf := rars[idx].pf
				slot := pf.numSlots() - 1
				msgID := pf.msgID(slot)
				if msgID == "" {
					return nil
				}
				data, _, err := b.e.bodyPart(ctx, pf, slot, msgID, b.prio, b.fo)
				if err == nil && len(data) > 0 {
					if n := archive.ReadRAR4TailVolumeNumber(data); n >= 0 {
						rars[idx].number = n
					}
				}
				return nil
			})
		}
	}
	allNumbered := true
	for _, r := range rars {
		if r.number < 0 {
			allNumbered = false
		}
	}
	sort.SliceStable(rars, func(i, j int) bool {
		a, c := rars[i], rars[j]
		if allNumbered {
			return a.number < c.number
		}
		if (a.number == 0) != (c.number == 0) {
			return a.number == 0 // the first volume leads
		}
		if a.pf.file.SubjectIndex != c.pf.file.SubjectIndex {
			return a.pf.file.SubjectIndex < c.pf.file.SubjectIndex
		}
		return a.pf.idx < c.pf.idx
	})
	for i, r := range rars {
		vol := i
		if allNumbered {
			vol = r.number
		}
		b.rel.setFileInfo(func() {
			r.pf.file.Info = nzb.FileInfo{Kind: nzb.KindRAR, Ext: "rar", Volume: vol, SetName: "\x00obfuscated-rar", NewStyle: true}
		})
	}
	return named, unreadable, nil
}

// looksLikeVideo recognises common container signatures.
func looksLikeVideo(b []byte) bool {
	return sniffVideoExt(b) != ""
}

// sniffVideoExt recognises common container signatures and returns the
// canonical lowercase container extension ("mkv", "webm", "mp4", "mov",
// "avi", "mpg", "flv", "ogm", "wmv", "ts", "m2ts"), or "" if none match.
func sniffVideoExt(b []byte) string {
	switch {
	case len(b) >= 4 && bytes.Equal(b[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}): // Matroska/WebM (EBML)
		if sniffEBMLWebM(b[:min(len(b), 128)]) {
			return "webm"
		}
		return "mkv"
	case len(b) >= 8 && (string(b[4:8]) == "ftyp" || string(b[4:8]) == "moov" || string(b[4:8]) == "mdat" || string(b[4:8]) == "free"):
		if len(b) >= 12 && string(b[4:8]) == "ftyp" {
			switch string(b[8:12]) {
			case "qt  ":
				return "mov"
			case "M4V ", "M4VP", "M4VH":
				return "m4v"
			}
		}
		return "mp4"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "AVI ":
		return "avi"
	case len(b) >= 4 && (bytes.Equal(b[:4], []byte{0x00, 0x00, 0x01, 0xba}) || bytes.Equal(b[:4], []byte{0x00, 0x00, 0x01, 0xb3})): // MPEG program/elementary video stream
		return "mpg"
	case len(b) >= 9 && string(b[:3]) == "FLV" && b[3] == 1 && b[4]&^byte(5) == 0 && binary.BigEndian.Uint32(b[5:9]) >= 9:
		return "flv"
	case len(b) >= 27 && string(b[:4]) == "OggS" && b[4] == 0 && b[5]&^byte(7) == 0:
		return "ogm"
	case len(b) >= 16 && bytes.Equal(b[:8], []byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11}): // ASF/WMV
		return "wmv"
	case len(b) > 376 && b[0] == 0x47 && b[188] == 0x47 && b[376] == 0x47: // MPEG-TS
		return "ts"
	case len(b) > 392 && b[4] == 0x47 && b[196] == 0x47 && b[388] == 0x47: // M2TS (192-byte packets)
		return "m2ts"
	}
	return ""
}

// sniffEBMLWebM checks whether an EBML header carries DocType (0x4282) "webm".
func sniffEBMLWebM(b []byte) bool {
	for i := 4; i+7 <= len(b); i++ {
		if b[i] == 0x42 && b[i+1] == 0x82 && b[i+2] == 0x84 && string(b[i+3:i+7]) == "webm" {
			return true
		}
	}
	return false
}

func vintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

func rarVideoFileCount(files []archive.RARFile) int {
	n := 0
	for _, f := range files {
		info := nzb.Classify(f.Name)
		if !f.Dir && (info.Kind == nzb.KindVideo || info.Kind == nzb.KindISO) && !info.Sample && !info.Extra {
			n++
		}
	}
	return n
}

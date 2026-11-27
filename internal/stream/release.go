package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/braedonsaunders/ether/internal/archive"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/vault"
)

var errEOF = io.EOF

// release is the engine's state for one NZB: its posted files with their
// indexes, and the plan (layout of the main video), built once.
type release struct {
	feedbackID   string
	liveArticles atomic.Uint64
	missingEpoch atomic.Uint64
	feedbackMu   sync.Mutex // orders health writes from concurrent reads

	saved  *vault.Layout
	e      *Engine
	digest string
	doc    *nzb.NZB
	files  []*postedFile

	partSize atomic.Int64 // first learned uniform part size, shared default

	refs int // guarded by e.mu

	// infoMu guards the nzb files' Name, Info and Obfuscated: identifying
	// obfuscated files (plan build, under planGate) rewrites them while health
	// checks and PAR2 lookups of the same release read them.
	infoMu sync.RWMutex

	planGate chan struct{}
	plan     *plan
	planErr  error // permanent rejection, cached

	// namedMu serialises NameFiles; named is its result, kept so a later
	// fetch of the same pack never probes its articles again.
	namedMu sync.Mutex
	named   []byte

	p2           par2State
	repairMu     sync.Mutex
	repair       *repairSession
	rarPasswords archive.RARPasswordCache
}

func newRelease(e *Engine, digest string, doc *nzb.NZB) *release {
	r := &release{e: e, digest: digest, doc: doc, planGate: make(chan struct{}, 1)}
	base := 0
	for i, f := range doc.Files {
		pf := newPostedFile(i, f, base, r.partSize.Load)
		base += pf.numSlots() + 1
		r.files = append(r.files, pf)
	}
	return r
}

func (r *release) notePartSize(pf *postedFile) {
	if p := pf.learnedPartSize(); p > 0 {
		r.partSize.CompareAndSwap(0, p)
	}
}

func (r *release) stableFileName(pf *postedFile) string {
	r.infoMu.RLock()
	defer r.infoMu.RUnlock()
	return pf.file.StableName()
}

// hasRenamedFilesLocked reports whether any file in r.doc has a Name different
// from its original SubjectName (requires r.infoMu held).
func (r *release) hasRenamedFilesLocked() bool {
	for _, f := range r.doc.Files {
		if orig := f.OriginalName(); orig != "" && f.Name != orig {
			return true
		}
	}
	return false
}

// ensurePlan builds the plan once. Rejections (compressed, encrypted, no
// playable file ...) are cached; transient errors are retried by the next
// caller.
func (r *release) ensurePlan(ctx context.Context, prio core.Priority, cacheID string) (*plan, error) {
	return r.ensurePlanWithCandidate(ctx, prio, cacheID, nil)
}

// ensurePlanWithCandidate announces the first identified candidate before
// reading its archive layout. Health checks can inspect its articles while
// planning continues. The callback must not block or call back into planning.
func (r *release) ensurePlanWithCandidate(ctx context.Context, prio core.Priority, cacheID string, ready func([]*postedFile)) (*plan, error) {
	select {
	case r.planGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-r.planGate }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.plan != nil {
		if ready != nil {
			ready(r.plan.vols)
		}
		return r.plan, nil
	}
	if r.planErr != nil {
		return nil, r.planErr
	}
	b := &builder{e: r.e, rel: r, prio: prio, fo: fetchOpts{cacheID: cacheID}, candidateReady: ready}
	if r.saved != nil {
		p, err := b.restore(ctx, r.saved)
		if err != nil {
			return nil, err
		}
		if p != nil {
			r.plan = p
			return p, nil
		}
		r.saved = nil
	}
	p, err := b.build(ctx)
	if err != nil {
		if IsRejection(err) && ctx.Err() == nil {
			r.planErr = err
		}
		return nil, err
	}
	r.plan = p
	return p, nil
}

// builder builds a plan: it picks the main video and maps it.
type builder struct {
	e              *Engine
	rel            *release
	prio           core.Priority
	fo             fetchOpts
	candidateReady func([]*postedFile)
	// memberISOs are the other disc images of the archive planFor just planned,
	// the further discs of a multi-disc release.
	memberISOs []*plan
}

// candidate is something that may hold the main video (or non-video target).
type candidate struct {
	kind        nzb.Kind // KindVideo, KindRAR, KindSevenZip, KindZIP, KindSplit, or KindOther for rawNonVideo
	files       []*postedFile
	set         *nzb.Set
	bytes       int64
	rawNonVideo bool
	matchRank   int
}

func (b *builder) build(ctx context.Context) (*plan, error) {
	hint, nonVideo := b.rel.doc.NonVideoHint()
	cands := b.candidates(b.rel.files)
	if !hint.ExplicitFile && b.shouldProbe(cands) {
		if err := b.identifyObfuscated(ctx); err != nil {
			return nil, err
		}
		cands = b.candidates(b.rel.files)
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("%w: nothing recognisable among %d files", ErrNoPlayableFile, len(b.rel.files))
	}
	// A season held in archives (an nzb.MetaEpisode hint) may list the sets
	// of other episodes too: one of them being broken or protected says
	// nothing about the wanted episode's set, so every set is tried.
	_, hinted := b.rel.doc.EpisodeHint()
	_, movieHinted := b.rel.doc.MovieHint()
	hinted = hinted || movieHinted || (nonVideo && !hint.ExplicitFile)
	hinted = hinted && len(cands) > 1
	if !hinted && !nonVideo {
		if err := b.checkCachedRAR(ctx, cands[0]); err != nil {
			return nil, err
		}
	}
	if b.candidateReady != nil {
		b.candidateReady(cands[0].files)
	}
	var firstErr error
	regrouped := false
	for i := 0; i < len(cands); i++ {
		c := cands[i]
		b.memberISOs = nil
		p, err := b.planFor(ctx, c)
		if err == nil && !nonVideo && isISOName(p.name) {
			// A disc image, plain, split or inside an archive: play its
			// main feature, not the image. Further discs of the release join it.
			p, err = b.expandISO(ctx, p, b.isoSiblings(cands, i))
		}
		if err == nil {
			b.e.log.Debug("stream plan", "kind", p.kind, "name", p.name, "size", p.size, "volumes", len(p.vols))
			// A PAR2 index already loaded (obfuscated post, or a health
			// check reading it alongside) checks the first volume now; the
			// head is in memory. Open never waits for PAR2.
			if err := b.rel.verifyPar2(ctx, p, b.prio, b.fo, false); err != nil {
				return nil, err
			}
			return p, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !regrouped && c.kind == nzb.KindRAR && len(c.files) == 1 && errors.Is(err, archive.ErrMissingVolume) {
			if ok, rerr := b.regroupSingleRARs(ctx, cands); rerr != nil {
				return nil, rerr
			} else if ok {
				regrouped = true
				cands = b.candidates(b.rel.files)
				firstErr = nil
				i = -1
				continue
			}
		}
		// Only fall through to smaller candidates when this one simply had
		// no video; a broken or protected main archive rejects the release.
		if !errors.Is(err, ErrNoPlayableFile) {
			if !hinted || !IsRejection(err) {
				return nil, err
			}
			// Report the set's real fault over another set's "no such
			// episode" when nothing plays.
			if firstErr == nil || errors.Is(firstErr, ErrNoPlayableFile) {
				firstErr = err
			}
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// candidates lists playable-looking files and archive sets, largest first.
func (b *builder) candidates(files []*postedFile) []candidate {
	if hint, ok := b.rel.doc.NonVideoHint(); ok {
		return b.nonVideoCandidates(files, hint)
	}
	b.rel.infoMu.RLock()
	defer b.rel.infoMu.RUnlock()
	var out []candidate
	var vols []*nzb.File
	byFile := map[*nzb.File]*postedFile{}
	for _, pf := range files {
		f := pf.file
		byFile[f] = pf
		switch {
		case (f.Info.Kind == nzb.KindVideo || f.Info.Kind == nzb.KindISO) && !f.Info.Sample && !f.Info.Extra:
			out = append(out, candidate{kind: f.Info.Kind, files: []*postedFile{pf}, bytes: f.Bytes()})
		case f.Info.Kind.IsArchive():
			vols = append(vols, f)
		}
	}
	for _, s := range nzb.GroupSets(vols) {
		if s.Kind == nzb.KindSplit && len(s.Volumes) > 0 {
			switch nzb.Classify("x." + s.Volumes[0].Info.Ext).Kind {
			case nzb.KindVideo, nzb.KindISO, nzb.KindUnknown, nzb.KindSplit, nzb.KindRAR:
			default:
				continue // e.g. a split .iso; "name.2024.001" stays (sniffed)
			}
		}
		if len(s.Volumes) > 0 && s.Volumes[0].Info.Sample {
			continue
		}
		c := candidate{kind: s.Kind, set: s, bytes: s.Bytes()}
		for _, v := range s.Volumes {
			c.files = append(c.files, byFile[v])
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].bytes > out[j].bytes })
	if movie, ok := b.rel.doc.MovieHint(); ok && len(movie.WantedLanguages) > 0 && len(out) > 1 {
		mc := make([]releaseparse.MovieCandidate, len(out))
		for i, c := range out {
			name := ""
			switch {
			case c.set != nil:
				name = c.set.Name + ".mkv"
			case len(c.files) > 0:
				name = c.files[0].Name()
			}
			mc[i] = releaseparse.MovieCandidate{Path: name, Size: c.bytes}
		}
		order := releaseparse.OrderMovieCandidates(mc, movie.Titles, movie.OriginalLanguage, movie.WantedLanguages)
		reordered := make([]candidate, len(out))
		for i, idx := range order {
			reordered[i] = out[idx]
		}
		out = reordered
	}
	return out
}

// nonVideoCandidates lists candidate loose files and archive sets for an NZB
// carrying nzb.MetaNonVideo, skipping video-only sample/extra filtering.
func (b *builder) nonVideoCandidates(files []*postedFile, hint nzb.NonVideoHint) []candidate {
	b.rel.infoMu.RLock()
	defer b.rel.infoMu.RUnlock()

	byFile := make(map[*nzb.File]*postedFile, len(files))
	for _, pf := range files {
		byFile[pf.file] = pf
	}

	if hint.ExplicitFile && hint.RawFile {
		if hint.ExactFile != "" {
			names := make([]string, len(files))
			for i, pf := range files {
				names[i] = pf.file.Name
			}
			if idx, ok, _ := nzb.MatchPathIndex(names, hint.ExactFile); ok {
				pf := files[idx]
				return []candidate{{kind: pf.file.Info.Kind, files: []*postedFile{pf}, bytes: pf.file.Bytes(), rawNonVideo: true, matchRank: 3}}
			}
			origNames := make([]string, len(files))
			for i, pf := range files {
				origNames[i] = pf.file.OriginalName()
			}
			if idx, ok, _ := nzb.MatchPathIndex(origNames, hint.ExactFile); ok {
				pf := files[idx]
				return []candidate{{kind: pf.file.Info.Kind, files: []*postedFile{pf}, bytes: pf.file.Bytes(), rawNonVideo: true, matchRank: 3}}
			}
		}
		for _, pf := range files {
			if pf.file.Info.Kind == nzb.KindPar2 {
				continue
			}
			return []candidate{{kind: pf.file.Info.Kind, files: []*postedFile{pf}, bytes: pf.file.Bytes(), rawNonVideo: true, matchRank: 3}}
		}
		return nil
	}

	var out []candidate
	var vols []*nzb.File
	var loose []*postedFile
	for _, pf := range files {
		f := pf.file
		if f.Info.Kind == nzb.KindPar2 {
			continue
		}
		if f.Info.Kind.IsArchive() {
			vols = append(vols, f)
			continue
		}
		if !hint.ExplicitFile {
			loose = append(loose, pf)
		}
	}

	if !hint.ExplicitFile {
		if hint.Member != "" {
			names := make([]string, len(loose))
			for i, pf := range loose {
				names[i] = pf.file.Name
			}
			if idx, ok, _ := nzb.MatchPathIndex(names, hint.Member); ok {
				pf := loose[idx]
				out = append(out, candidate{kind: pf.file.Info.Kind, files: []*postedFile{pf}, bytes: pf.file.Bytes(), rawNonVideo: true, matchRank: 3})
			}
		} else if hint.Track.NonEmpty() {
			names := make([]string, len(loose))
			for i, pf := range loose {
				names[i] = pf.file.Name
			}
			if idx, ok, _ := nzb.MatchTrackIndex(names, *hint.Track, hint.MIME); ok {
				pf := loose[idx]
				out = append(out, candidate{kind: pf.file.Info.Kind, files: []*postedFile{pf}, bytes: pf.file.Bytes(), rawNonVideo: true, matchRank: 3})
			}
		} else if hint.MemberIndex == nil && nzb.IsNonVideoMIME(hint.MIME) {
			for _, pf := range loose {
				if rank := nzb.NonVideoMatchRank(pf.file.Name, hint.MIME); rank > 0 {
					out = append(out, candidate{kind: pf.file.Info.Kind, files: []*postedFile{pf}, bytes: pf.file.Bytes(), rawNonVideo: true, matchRank: rank})
				}
			}
		}
	}

	for _, s := range nzb.GroupSets(vols) {
		if s.Kind == nzb.KindSplit && len(s.Volumes) > 0 {
			inner := "x." + s.Volumes[0].Info.Ext
			k := nzb.Classify(inner).Kind
			if k != nzb.KindUnknown && k != nzb.KindSplit && k != nzb.KindRAR && k != nzb.KindSevenZip && k != nzb.KindZIP &&
				nzb.NonVideoMatchRank(inner, hint.MIME) == 0 && hint.Member == "" && !hint.ExplicitFile {
				continue
			}
		}
		c := candidate{kind: s.Kind, set: s, bytes: s.Bytes()}
		if hint.Track.NonEmpty() && hint.Track.Disc > 0 {
			setDisc := nzb.ParseDirDisc(s.Name)
			if setDisc == 0 && len(s.Volumes) > 0 {
				setDisc = nzb.ParseDirDisc(s.Volumes[0].Name)
			}
			switch {
			case setDisc == hint.Track.Disc:
				c.matchRank = 2
			case setDisc == 0:
				c.matchRank = 1
			default:
				c.matchRank = 0
			}
		}
		for _, v := range s.Volumes {
			c.files = append(c.files, byFile[v])
		}
		out = append(out, c)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].matchRank != out[j].matchRank {
			return out[i].matchRank > out[j].matchRank
		}
		return out[i].bytes > out[j].bytes
	})
	return out
}

func (b *builder) hasMultipleMovieCandidates(cands []candidate) bool {
	if len(cands) <= 1 {
		return false
	}
	var maxBytes int64
	plainVideos := 0
	for _, c := range cands {
		if c.bytes > maxBytes {
			maxBytes = c.bytes
		}
		if c.kind == nzb.KindVideo || c.kind == nzb.KindISO {
			plainVideos++
		}
	}
	if plainVideos > 1 {
		return true
	}
	features := 0
	for _, c := range cands {
		if maxBytes <= 0 || c.bytes*100 >= 35*maxBytes {
			features++
		}
	}
	return features > 1
}

func (b *builder) planFor(ctx context.Context, c candidate) (*plan, error) {
	if c.set != nil && len(c.set.Missing) > 0 {
		return nil, incompleteError("%s set %q lacks volumes %v", c.kind, c.set.Name, c.set.Missing)
	}
	if len(c.files) == 0 {
		return nil, ErrNoPlayableFile
	}
	for _, pf := range c.files {
		if m := pf.file.Missing(); len(m) > 0 {
			return nil, incompleteError("%s lacks %d of %d parts (first: %d)", pf.Name(), len(m), pf.file.SegmentCount(), m[0])
		}
	}
	if c.rawNonVideo {
		return b.planPlainRaw(ctx, c.files[0])
	}
	switch c.kind {
	case nzb.KindVideo, nzb.KindISO:
		if movie, ok := b.rel.doc.MovieHint(); ok && len(movie.Titles) > 0 && b.hasMultipleMovieCandidates(b.candidates(b.rel.files)) && !releaseparse.MovieFileMatches(c.files[0].Name(), movie.Titles, movie.Year) {
			return nil, ErrNoPlayableFile
		}
		if h, ok := b.rel.doc.EpisodeHint(); ok && !memberMatchesHint(h, c.files[0].Name()) && len(b.candidates(b.rel.files)) > 1 {
			return nil, ErrNoPlayableFile
		}
		return b.planPlain(ctx, c.files[0])
	case nzb.KindRAR:
		return b.planRAR(ctx, c.files)
	case nzb.KindSevenZip:
		return b.plan7z(ctx, c.files)
	case nzb.KindZIP:
		return b.planZIP(ctx, c.files)
	case nzb.KindSplit:
		return b.planSplit(ctx, c.files)
	}
	return nil, ErrNoPlayableFile
}

// setFileInfo applies a change to nzb file identification under infoMu.
func (r *release) setFileInfo(fn func()) {
	r.infoMu.Lock()
	fn()
	r.infoMu.Unlock()
}

// head fetches slot 0 of each file in parallel (learning their sizes).
func (b *builder) heads(ctx context.Context, files []*postedFile) error {
	return b.parallel(ctx, len(files), func(ctx context.Context, i int) error {
		_, err := b.e.segment(ctx, b.rel, files[i], 0, b.prio, b.fo)
		return err
	})
}

// parallel runs fn(0..n-1) with bounded concurrency, returning the first
// error and cancelling the rest.
func (b *builder) parallel(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, b.e.opts.OpenParallel)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, i); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}(i)
	}
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

// fileReader is an io.ReaderAt over one posted file, used to parse archive
// headers.
type fileReader struct {
	ctx  context.Context
	b    *builder
	pf   *postedFile
	prio core.Priority
}

func (b *builder) reader(ctx context.Context, pf *postedFile) *fileReader {
	return &fileReader{ctx: ctx, b: b, pf: pf, prio: b.prio}
}

func (r *fileReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("stream: negative offset")
	}
	want := p
	if r.pf.SizeKnown() {
		size := r.pf.Size()
		if off >= size {
			return 0, io.EOF
		}
		if rem := size - off; int64(len(want)) > rem {
			want = want[:rem]
		}
	}
	n, err := r.b.e.readFile(r.ctx, r.b.rel, r.pf, want, off, r.prio, r.b.fo)
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

// sniff fetches (if needed) and returns the first bytes of a posted file.
func (b *builder) sniff(ctx context.Context, pf *postedFile) ([]byte, error) {
	seg, err := b.e.segment(ctx, b.rel, pf, 0, b.prio, b.fo)
	if err != nil {
		return nil, err
	}
	if seg.begin != 0 {
		return nil, nil
	}
	return seg.data, nil
}

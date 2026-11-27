package nzb

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type parsedTrack struct {
	idx      int
	disc     int
	weakDisc bool
	number   int
	altDisc  int
	altNum   int
	tokens   []string
}

var (
	reDirDisc        = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(cd|dis[ck]|disque|vol(?:ume)?|part)\s*[-_.]?\s*0*([1-9][0-9]?)(?:$|[^a-z0-9]|of\s*[0-9])`)
	reFileDiscNum    = regexp.MustCompile(`(?i)^(?:(?:cd|dis[ck]|d)\s*0*([1-9][0-9]?)\s*[-_.t ]+|([0-9]{1,2})\s*[-_.]\s*)(?:t(?:rack)?\s*)?([0-9]{1,3})(?:$|[^0-9])`)
	reFileMidDiscNum = regexp.MustCompile(`(?i)(?:\s+-\s+|\s*[-_]\s*)(?:(?:cd|dis[ck]|d)\s*0*([1-9][0-9]?)\s*[-_.t ]+|([0-9]{1,2})\s*[-_.]\s*)(?:t(?:rack)?\s*)?([0-9]{2,3})(?:$|[^0-9])`)
	reFileLeadNum    = regexp.MustCompile(`(?i)^(?:(?:track|tr|t)\s*[-_.]?\s*)?0*([1-9][0-9]{0,2})(?:$|[^0-9])`)
	reFileMidNum     = regexp.MustCompile(`(?i)(?:\s+-\s+|\s*[-_]\s*)0*([0-9]{2,3})(?:\s+-\s+|\s*[-_.]\s+)`)

	reParenOrBracket  = regexp.MustCompile(`\s*[\(\[][^\)\]]*[\)\]]`)
	reTrailingEdition = regexp.MustCompile(`(?i)\s+-\s+(?:\d{4}\s+)?(?:remaster(?:ed)?|deluxe|live|demo|bonus|radio\s+edit|single\s+version|album\s+version|acoustic|instrumental|explicit|clean|version|edit|mix|mono|stereo)\b.*$`)
)

// ParseDirDisc extracts a disc number (1..50) from directory or archive-set
// names, preferring explicit disc/CD markers over title-embedded "Vol N" or
// "Part N" tokens.
func ParseDirDisc(p string) int {
	norm := CleanArchivePath(p)
	if norm == "" {
		return 0
	}
	var bestDisc int
	bestWeak := true
	for _, seg := range strings.Split(norm, "/") {
		if d, w := parseDirDiscSegment(seg); d > 0 {
			if bestDisc == 0 || !w || bestWeak {
				bestDisc = d
				bestWeak = w
			}
		}
	}
	return bestDisc
}

func parseDirDiscSegment(seg string) (disc int, weak bool) {
	matches := reDirDisc.FindAllStringSubmatchIndex(seg, -1)
	for _, idx := range matches {
		if len(idx) < 6 {
			continue
		}
		kw := strings.ToLower(seg[idx[2]:idx[3]])
		d, err := strconv.Atoi(seg[idx[4]:idx[5]])
		if err != nil || d <= 0 || d > 50 {
			continue
		}
		isWeak := false
		if strings.HasPrefix(kw, "vol") || strings.HasPrefix(kw, "part") {
			for _, r := range seg[:idx[2]] {
				if unicode.IsLetter(r) || unicode.IsDigit(r) {
					isWeak = true
					break
				}
			}
		}
		if disc == 0 || !isWeak || weak {
			disc = d
			weak = isWeak
		}
	}
	return disc, weak
}

// MatchTrackIndex finds a unique audio file in names matching want:
//  1. Disc + track number first (disambiguated by normalised title tokens when
//     multiple candidates share a number, failing with ambiguous=true if still
//     not unique).
//  2. Normalised title tokens when no track number was given or no candidate
//     matched by number (contiguous token sequence first, then all tokens
//     present), failing with ambiguous=true when multiple candidates tie.
func MatchTrackIndex(names []string, want TrackHint, mimeFilter string) (idx int, found bool, ambiguous bool) {
	if !want.NonEmpty() {
		return -1, false, false
	}
	wantMIME := NormalizeMIME(mimeFilter)
	if wantMIME == "" {
		wantMIME = "audio/*"
	}
	var cands []parsedTrack
	for i, raw := range names {
		if !isTrackCandidateFile(raw, wantMIME) {
			continue
		}
		cands = append(cands, parseTrackCandidate(i, raw))
	}
	// If a specific audio MIME (e.g. audio/flac) found no files (e.g. release
	// holds mp3/m4a instead), retry across all audio files.
	if len(cands) == 0 && wantMIME != "audio/*" {
		for i, raw := range names {
			if !isTrackCandidateFile(raw, "audio/*") {
				continue
			}
			cands = append(cands, parseTrackCandidate(i, raw))
		}
	}
	if len(cands) == 0 {
		return -1, false, false
	}

	dropSingleWeakDisc(cands)
	promoteAltDiscNumbers(cands)

	hasDisc1 := false
	for _, c := range cands {
		if c.disc == 1 {
			hasDisc1 = true
			break
		}
	}
	wantTokens := normaliseTrackTokens(want.Title)
	var strippedTokens []string
	if stripped := stripTitleEdition(want.Title); stripped != "" {
		if st := normaliseTrackTokens(stripped); len(st) > 0 && !slicesEqual(st, wantTokens) {
			strippedTokens = st
		}
	}

	// 1. Match by disc + track number first.
	if want.Number > 0 {
		var byNum []parsedTrack
		for _, c := range cands {
			if c.number == want.Number {
				if trackDiscMatches(c.disc, want.Disc, hasDisc1) {
					byNum = append(byNum, c)
				}
				continue
			}
			// Support 3-digit disc+track numbering (e.g. 105 / 205), both
			// untagged and inside a matching disc folder (e.g. CD1/105, CD2/205).
			if want.Number < 100 {
				if want.Disc > 0 && (c.disc == 0 || c.disc == want.Disc) && c.number == want.Disc*100+want.Number {
					byNum = append(byNum, c)
				} else if want.Disc == 0 && c.disc > 0 && c.number == c.disc*100+want.Number {
					byNum = append(byNum, c)
				}
			}
		}
		if len(byNum) == 1 {
			if len(wantTokens) > 0 && !candidateMatchesAnyTokens(byNum[0], wantTokens, strippedTokens) {
				var eligible []parsedTrack
				for _, c := range cands {
					if trackDiscMatches(c.disc, want.Disc, hasDisc1) {
						eligible = append(eligible, c)
					}
				}
				if midx, mok, mambig := matchTrackByTitle(eligible, wantTokens, strippedTokens); mok && !mambig {
					return midx, true, false
				}
				if want.Disc > 0 && len(eligible) < len(cands) {
					if midx, mok, mambig := matchTrackByTitle(cands, wantTokens, strippedTokens); mok && !mambig {
						return midx, true, false
					}
				}
			}
			return byNum[0].idx, true, false
		}
		if len(byNum) > 1 {
			if len(wantTokens) > 0 {
				if midx, mok, mambig := matchTrackByTitle(byNum, wantTokens, strippedTokens); mok || mambig {
					return midx, mok, mambig
				}
			}
			return -1, false, true
		}
	}

	// 2. Fallback to normalised title tokens.
	if len(wantTokens) > 0 {
		var eligible []parsedTrack
		for _, c := range cands {
			if want.Disc > 0 && c.disc > 0 && c.disc != want.Disc {
				continue
			}
			eligible = append(eligible, c)
		}
		if midx, mok, mambig := matchTrackByTitle(eligible, wantTokens, strippedTokens); mok || mambig {
			return midx, mok, mambig
		}
		if want.Disc > 0 && len(eligible) < len(cands) {
			return matchTrackByTitle(cands, wantTokens, strippedTokens)
		}
	}
	return -1, false, false
}

func trackDiscMatches(candDisc, wantDisc int, releaseHasDisc1 bool) bool {
	if wantDisc <= 0 {
		return true
	}
	if candDisc == wantDisc {
		return true
	}
	// Single-disc releases and mixed releases where only bonus discs carry a
	// disc folder (e.g. root/01.flac + Bonus Disc 2/01.flac) leave disc 1
	// untagged: allow candDisc == 0 when wantDisc == 1 and nothing declares disc 1.
	return wantDisc == 1 && candDisc == 0 && !releaseHasDisc1
}

// dropSingleWeakDisc clears disc tags when every disc-tagged candidate in the
// release came from a weak title-embedded marker (e.g. "Oral Fixation Vol 2/")
// and all of them agree on the same single disc number.
func dropSingleWeakDisc(cands []parsedTrack) {
	onlyDisc := 0
	for _, c := range cands {
		if c.disc == 0 {
			continue
		}
		if !c.weakDisc {
			return
		}
		if onlyDisc == 0 {
			onlyDisc = c.disc
		} else if c.disc != onlyDisc {
			return
		}
	}
	if onlyDisc == 0 {
		return
	}
	for i := range cands {
		if cands[i].weakDisc {
			cands[i].disc = 0
			cands[i].weakDisc = false
		}
	}
}

// promoteAltDiscNumbers promotes zero-padded DD-NN or compact 1-digit D-N
// prefixes when the release exhibits a consistent multi-track disc-track pattern.
func promoteAltDiscNumbers(cands []parsedTrack) {
	altCount := 0
	byDisc := make(map[int]map[int]struct{})
	addNum := func(d, n int) {
		nums := byDisc[d]
		if nums == nil {
			nums = make(map[int]struct{})
			byDisc[d] = nums
		}
		nums[n] = struct{}{}
	}
	for _, c := range cands {
		if c.disc > 0 && !c.weakDisc && c.number > 0 {
			addNum(c.disc, c.number)
		}
		if c.altDisc <= 0 || c.altNum <= 0 {
			continue
		}
		if c.disc != 0 && c.disc != c.altDisc {
			continue
		}
		altCount++
		addNum(c.altDisc, c.altNum)
	}
	if altCount == 0 {
		return
	}
	promote := altCount >= 2 && altCount*2 >= len(cands)
	if !promote {
		for _, nums := range byDisc {
			if len(nums) >= 2 {
				promote = true
				break
			}
		}
	}
	if !promote {
		return
	}
	for i := range cands {
		c := &cands[i]
		if c.altDisc > 0 && c.altNum > 0 && (c.disc == 0 || c.disc == c.altDisc) {
			c.disc = c.altDisc
			c.weakDisc = false
			c.number = c.altNum
		}
	}
}

func isTrackCandidateFile(raw, wantMIME string) bool {
	norm := CleanArchivePath(raw)
	if norm == "" {
		return false
	}
	ext := strings.ToLower(path.Ext(norm))
	switch ext {
	case "", ".nfo", ".sfv", ".m3u", ".m3u8", ".cue", ".log", ".txt", ".url",
		".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp",
		".par2", ".nzb", ".rar", ".zip", ".7z", ".epub", ".pdf", ".cbz", ".cbr":
		return false
	}
	if IsNonVideoMIME(wantMIME) {
		return NonVideoMatchRank(norm, wantMIME) > 0
	}
	return NonVideoMatchRank(norm, "audio/*") > 0
}

func parseTrackCandidate(idx int, raw string) parsedTrack {
	norm := CleanArchivePath(raw)
	dir := path.Dir(norm)
	base := path.Base(norm)
	stem := strings.TrimSuffix(base, path.Ext(base))

	var disc, num, altDisc, altNum int
	weakDisc := false
	if dir != "" && dir != "." {
		for _, seg := range strings.Split(dir, "/") {
			if d, w := parseDirDiscSegment(seg); d > 0 {
				if disc == 0 || !w || weakDisc {
					disc = d
					weakDisc = w
				}
			}
		}
	}

	applyDiscNumMatch := func(explicitPrefix bool, dRaw, nRaw string, numEnd int, allowMidSingle bool) {
		d, derr := strconv.Atoi(dRaw)
		n, nerr := strconv.Atoi(nRaw)
		if derr != nil || nerr != nil || d < 1 || d > 20 || n < 1 || n > 199 {
			return
		}
		if explicitPrefix {
			disc = d
			weakDisc = false
			num = n
			return
		}
		if isFollowedByDatePart(stem, numEnd) {
			return
		}
		if len(strings.TrimLeft(dRaw, "0")) > 2 || (disc != 0 && disc != d) {
			return
		}
		if len(dRaw) == 1 && len(nRaw) >= 2 && len(nRaw) <= 3 {
			disc = d
			weakDisc = false
			num = n
			return
		}
		if len(nRaw) >= 2 && len(nRaw) <= 3 {
			altDisc = d
			altNum = n
			return
		}
		if allowMidSingle && len(dRaw) == 1 && len(nRaw) == 1 && d <= 9 && n <= 9 {
			altDisc = d
			altNum = n
		}
	}

	if idx := reFileDiscNum.FindStringSubmatchIndex(stem); len(idx) == 8 {
		explicitPrefix := idx[2] >= 0
		dStart, dEnd := idx[2], idx[3]
		if !explicitPrefix {
			dStart, dEnd = idx[4], idx[5]
		}
		applyDiscNumMatch(explicitPrefix, stem[dStart:dEnd], stem[idx[6]:idx[7]], idx[7], true)
	}
	if num == 0 && altNum == 0 {
		if idx := reFileMidDiscNum.FindStringSubmatchIndex(stem); len(idx) == 8 {
			// Skip bare chained date-like segments (e.g. "2023-05-12").
			if idx[0] == 0 || stem[idx[0]] == ' ' || stem[idx[0]-1] < '0' || stem[idx[0]-1] > '9' {
				explicitPrefix := idx[2] >= 0
				dStart, dEnd := idx[2], idx[3]
				if !explicitPrefix {
					dStart, dEnd = idx[4], idx[5]
				}
				applyDiscNumMatch(explicitPrefix, stem[dStart:dEnd], stem[idx[6]:idx[7]], idx[7], false)
			}
		}
	}
	if num == 0 {
		if m := reFileLeadNum.FindStringSubmatch(stem); len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n <= 999 {
				num = n
			}
		}
	}
	if num == 0 {
		if m := reFileMidNum.FindStringSubmatch(stem); len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n <= 999 {
				num = n
			}
		}
	}

	return parsedTrack{
		idx:      idx,
		disc:     disc,
		weakDisc: weakDisc,
		number:   num,
		altDisc:  altDisc,
		altNum:   altNum,
		tokens:   normaliseTrackTokens(stem),
	}
}

func isFollowedByDatePart(stem string, numEnd int) bool {
	if numEnd >= len(stem) {
		return false
	}
	switch stem[numEnd] {
	case '-', '_', '.':
	default:
		return false
	}
	rest := stem[numEnd+1:]
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	return digits >= 2 && digits <= 4 && (digits == len(rest) || rest[digits] < '0' || rest[digits] > '9')
}

func stripTitleEdition(title string) string {
	s := reParenOrBracket.ReplaceAllString(title, " ")
	s = reTrailingEdition.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func candidateMatchesTokens(c parsedTrack, want []string) bool {
	return len(want) > 0 && (hasContiguousTokens(c.tokens, want) || hasAllTokens(c.tokens, want))
}

func candidateMatchesAnyTokens(c parsedTrack, want, stripped []string) bool {
	return candidateMatchesTokens(c, want) || candidateMatchesTokens(c, stripped)
}

func matchTrackByTitle(cands []parsedTrack, wantTokens, strippedTokens []string) (idx int, found bool, ambiguous bool) {
	idx, found, ambiguous = matchTrackByTokens(cands, wantTokens)
	if found || ambiguous || len(strippedTokens) == 0 {
		return idx, found, ambiguous
	}
	return matchTrackByTokens(cands, strippedTokens)
}

func normaliseTrackTokens(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur.WriteRune(r)
		case r == '\'' || r == '\u2019':
			// Collapse apostrophes so "Don't" matches "dont" and "don_t" both
			// gracefully: skip apostrophe inside words.
		default:
			flush()
		}
	}
	flush()
	return out
}

func matchTrackByTokens(cands []parsedTrack, want []string) (idx int, found bool, ambiguous bool) {
	if len(want) == 0 || len(cands) == 0 {
		return -1, false, false
	}
	var contiguous []parsedTrack
	var allPresent []parsedTrack
	for _, c := range cands {
		if hasContiguousTokens(c.tokens, want) {
			contiguous = append(contiguous, c)
		} else if hasAllTokens(c.tokens, want) {
			allPresent = append(allPresent, c)
		}
	}
	if len(contiguous) == 1 {
		return contiguous[0].idx, true, false
	}
	if len(contiguous) > 1 {
		return -1, false, true
	}
	if len(allPresent) == 1 {
		return allPresent[0].idx, true, false
	}
	if len(allPresent) > 1 {
		return -1, false, true
	}
	return -1, false, false
}

func hasContiguousTokens(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		ok := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func hasAllTokens(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	counts := make(map[string]int, len(haystack))
	for _, tok := range haystack {
		counts[tok]++
	}
	for _, tok := range needle {
		if counts[tok] <= 0 {
			return false
		}
		counts[tok]--
	}
	return true
}

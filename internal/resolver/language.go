package resolver

import (
	"fmt"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// catalogLanguage is the server's language: the catalogue metadata language.
func (s *Service) catalogLanguage() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.catalogLang
}

// profileLanguageMode reads a profile's Language (Radarr-style): specific
// is the one language the profile names ("" for none), any accepts
// every language, and neither is the original behaviour — each title's
// original language alongside the preferred ones. A language Ether does
// not recognise keeps the original behaviour rather than rejecting
// everything.
func profileLanguageMode(profile core.QualityProfile) (specific string, anyLang bool) {
	switch strings.ToLower(strings.TrimSpace(profile.Language)) {
	case "", core.ProfileLanguageOriginal:
		return "", false
	case core.ProfileLanguageAny:
		return "", true
	}
	if releaseparse.KnownLanguage(profile.Language) {
		return releaseparse.LanguageName(profile.Language), false
	}
	return "", false
}

// profileRequiredLanguage returns the canonical language name when the profile
// names a specific Audio language in Required mode (the default when
// AudioLanguageMode is unset on existing profiles).
func profileRequiredLanguage(profile core.QualityProfile) string {
	lang, _ := profileLanguageMode(profile)
	if lang == "" || profile.EffectiveAudioLanguageMode() != core.AudioLanguageModeRequired {
		return ""
	}
	return lang
}

// profilePreferredLanguage returns the canonical language name when the profile
// names a specific Audio language in Preferred mode.
func profilePreferredLanguage(profile core.QualityProfile) string {
	lang, _ := profileLanguageMode(profile)
	if lang == "" || profile.EffectiveAudioLanguageMode() != core.AudioLanguageModePreferred {
		return ""
	}
	return lang
}

// profileLanguageRejection is the reason a release fails a profile's
// required Audio language, or "" when it passes, the profile sets none, or
// AudioLanguageMode is Preferred (which ranks rather than filters). A release
// declaring no audio is in the title's original language as far as anyone
// knows, so it passes when that is the profile's language or unknown; MULTI
// and DUAL carry more than one track and pass, as in audioAcceptable.
func profileLanguageRejection(info core.ReleaseInfo, profile core.QualityProfile, t *core.Title) string {
	lang := profileRequiredLanguage(profile)
	if lang == "" {
		return ""
	}
	if len(info.Languages) == 0 {
		if orig := originalLanguage(t); orig == "" || releaseparse.LanguageMatch(orig, lang) != releaseparse.LanguageNoMatch {
			return ""
		}
		return fmt.Sprintf("audio is not in the profile's language (%s)", lang)
	}
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti || l == releaseparse.LanguageDual {
			return ""
		}
		if releaseparse.LanguageMatch(l, lang) != releaseparse.LanguageNoMatch {
			return ""
		}
	}
	return fmt.Sprintf("audio is not in the profile's language (%s)", lang)
}

// wantedAudio lists the ordered preferred audio languages for ranking:
//   - Specific profile Language in Required mode: only that language
//     (non-matching releases are rejected by profileLanguageRejection).
//   - Specific profile Language in Preferred mode: the profile's language
//     ranks first (rank 0), followed by the viewer's own ordered audio
//     languages (#743) so the viewer's list refines the fallback order.
//   - Otherwise: the profile's PreferredLanguages when the owner set them,
//     else the viewer's own audio languages, else the server's catalogue
//     language (unless the viewer chose PreferOriginal).
func wantedAudio(profile core.QualityProfile, user *core.User, serverLang string) []string {
	if lang := profileRequiredLanguage(profile); lang != "" {
		return []string{lang}
	}
	if lang := profilePreferredLanguage(profile); lang != "" {
		out := []string{lang}
		if user != nil && len(user.AudioLanguages) > 0 {
			out = append(out, user.AudioLanguages...)
		}
		return out
	}
	var out []string
	switch {
	case len(profile.PreferredLanguages) > 0:
		out = append(out, profile.PreferredLanguages...)
	case user != nil && len(user.AudioLanguages) > 0:
		out = append(out, user.AudioLanguages...)
	case user != nil && user.PreferOriginal:
		return nil
	case strings.TrimSpace(serverLang) != "":
		base, _, _ := strings.Cut(strings.TrimSpace(serverLang), "-")
		out = append(out, base)
	}
	return out
}

// movieTargetLanguages returns the ordered audio languages and original
// language for multi-file movie pack file selection. An explicit "Original"
// profile language puts the title's original language ahead of viewer/server
// fallbacks; otherwise wanted audio languages lead and the original language
// trails as a fallback over unrelated dubs (unless the profile requires one
// specific language).
func movieTargetLanguages(profile core.QualityProfile, wanted []string, t *core.Title) ([]string, string) {
	orig := titleOriginalLanguage(t)
	var out []string
	add := func(v string) {
		if n := releaseparse.LanguageName(v); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if strings.EqualFold(strings.TrimSpace(profile.Language), core.ProfileLanguageOriginal) && orig != "" && len(profile.PreferredLanguages) == 0 {
		add(orig)
	}
	for _, w := range wanted {
		add(w)
	}
	if profileRequiredLanguage(profile) == "" && orig != "" {
		add(orig)
	}
	return out, releaseparse.LanguageName(orig)
}

// acceptedAudio lists the audio languages a release may declare and still
// rank with the rest:
//   - Any: nil (every language is accepted).
//   - Specific profile Language in Required mode: only that language
//     (profileLanguageRejection is the hard gate).
//   - Specific profile Language in Preferred mode: the profile's preferred
//     language, followed by any viewer audio languages (#743), plus the
//     title's original language as fallback ahead of unrelated dubs (which
//     stay playable as last-resort fallbacks).
//   - Otherwise: wantedAudio plus the title's original language.
func acceptedAudio(profile core.QualityProfile, user *core.User, serverLang string, t *core.Title) []string {
	if _, anyLang := profileLanguageMode(profile); anyLang {
		return nil
	}
	if lang := profileRequiredLanguage(profile); lang != "" {
		return []string{lang}
	}
	out := wantedAudio(profile, user, serverLang)
	if len(out) == 0 {
		if user != nil && user.PreferOriginal {
			if lang := titleOriginalLanguage(t); lang != "" {
				return []string{lang}
			}
		}
		return nil
	}
	if lang := titleOriginalLanguage(t); lang != "" {
		out = append(out, lang)
	}
	return out
}

// originalInLanguages reports whether the title's original language matches
// one of langs, or is unknown (when no original language is recorded).
func originalInLanguages(t *core.Title, langs []string) bool {
	orig := titleOriginalLanguage(t)
	if orig == "" {
		return true
	}
	for _, want := range langs {
		if releaseparse.LanguageMatch(orig, want) != releaseparse.LanguageNoMatch {
			return true
		}
	}
	return false
}

// dualSourceDub returns the canonical dub language implied by an indexer's
// sourceLang when it names a real language distinct from the title's original
// language (or when the title's original language is unknown).
func dualSourceDub(sourceLang string, t *core.Title) string {
	return dualSourceDubForOrig(sourceLang, titleOriginalLanguage(t))
}

func dualSourceDubForOrig(sourceLang, orig string) string {
	src := releaseparse.LanguageName(sourceLang)
	if src == "" || src == releaseparse.LanguageMulti || src == releaseparse.LanguageDual {
		return ""
	}
	if orig != "" && releaseparse.LanguageMatch(src, orig) != releaseparse.LanguageNoMatch {
		return ""
	}
	return src
}

// isBareDual reports whether a release declares only DUAL (no named audio
// language and no MULTI) without an indexer source language naming the dub.
func isBareDual(info core.ReleaseInfo, sourceLang string, t *core.Title) bool {
	hasDual := false
	for _, l := range info.Languages {
		if l == releaseparse.LanguageDual {
			hasDual = true
			continue
		}
		if l != "" {
			return false
		}
	}
	return hasDual && dualSourceDub(sourceLang, t) == ""
}

// audioAcceptable reports whether a release's declared audio fits the
// accepted languages. A release that declares nothing is in the title's
// original language as far as anyone knows, so it is acceptable when that
// language is. MULTI carries more than one track and is acceptable; bare DUAL
// ("original + one unknown") is acceptable only when the title's original
// language or a non-original indexer source language is accepted. A regional
// variant of an accepted language (Castilian for a Latino preference) is also
// acceptable.
func audioAcceptable(info core.ReleaseInfo, sourceLang string, accepted []string, t *core.Title) bool {
	if len(accepted) == 0 {
		return true
	}
	if len(info.Languages) == 0 {
		// An untagged release is presumed to carry the title's original
		// language. When the profile accepts only other languages (an
		// English-dub profile on an anime), it is not acceptable, so a
		// declared dub outranks it at any resolution; it still plays as a
		// fallback when no acceptable release exists.
		return originalInLanguages(t, accepted)
	}
	orig := titleOriginalLanguage(t)
	hasDual := false
	hasConcreteNonOrig := false
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti {
			return true
		}
		if l == releaseparse.LanguageDual {
			hasDual = true
			continue
		}
		if orig == "" || releaseparse.LanguageMatch(l, orig) == releaseparse.LanguageNoMatch {
			hasConcreteNonOrig = true
		}
		for _, want := range accepted {
			if releaseparse.LanguageMatch(l, want) != releaseparse.LanguageNoMatch {
				return true
			}
		}
	}
	if hasDual {
		if !hasConcreteNonOrig {
			if src := dualSourceDubForOrig(sourceLang, orig); src != "" {
				for _, want := range accepted {
					if releaseparse.LanguageMatch(src, want) != releaseparse.LanguageNoMatch {
						return true
					}
				}
			}
		}
		return originalInLanguages(t, accepted)
	}
	return false
}

// viewerLanguageMatch describes how well a release satisfies the profile's
// preferred language and/or the viewer's ordered wanted audio languages
// (wantedAudio). Rank is 0-based (0 is the first choice).
type viewerLanguageMatch struct {
	Matched          bool
	Rank             int    // 0 = 1st wanted language, 1 = 2nd, ...
	Language         string // canonical matched language name (e.g. "german", "english", "multi")
	Original         bool   // matched via the title's original language (untagged release)
	Multi            bool   // multiple ordered languages active
	ProfilePreferred string // canonical profile Language when AudioLanguageMode is Preferred
}

// defaultPreferredLanguageScore is the strong ranking bonus awarded to a
// profile's Preferred audio language when the profile does not configure an
// explicit FormatViewerLanguage weight (e.g. imported TRaSH/Dictionarry profiles).
const defaultPreferredLanguageScore = 10000

// normalizeWantedLanguages returns canonical, deduplicated wanted language
// names in preference order.
func normalizeWantedLanguages(wanted []string) []string {
	var norm []string
	for _, w := range wanted {
		name := releaseparse.LanguageName(w)
		if name == "" || name == releaseparse.LanguageMulti || name == releaseparse.LanguageDual {
			continue
		}
		if !slices.Contains(norm, name) {
			norm = append(norm, name)
		}
	}
	return norm
}

// viewerLanguageActive reports whether the viewer's ordered audio preferences
// distinguish candidates on title t. A single wanted language only applies on
// a foreign title (whose original language is known and differs from it); an
// ordered list of two or more languages always applies so earlier languages
// outrank later fallbacks.
func viewerLanguageActive(wanted []string, t *core.Title) bool {
	norm := normalizeWantedLanguages(wanted)
	if len(norm) == 0 {
		return false
	}
	if len(norm) == 1 {
		orig := titleOriginalLanguage(t)
		return orig != "" && releaseparse.LanguageMatch(orig, norm[0]) == releaseparse.LanguageNoMatch
	}
	return true
}

// viewerLanguageActiveFor reports whether profile-preferred or viewer-ordered
// audio ranking distinguishes candidates on title t. A profile with a specific
// Preferred Audio language is always active, even on titles whose original
// language matches it (so English beats Spanish/Japanese dubs on Bluey).
func viewerLanguageActiveFor(wanted []string, profile core.QualityProfile, t *core.Title) bool {
	if profilePreferredLanguage(profile) != "" {
		return true
	}
	return viewerLanguageActive(wanted, t)
}

// matchViewerLanguage matches a release against the viewer's ordered wanted
// audio languages. With a single wanted language (such as an owner with []
// falling back to the catalogue language), it grants a rank-0 match only on a
// foreign-language title whose release explicitly declares that language,
// bare MULTI, or a DUAL/MULTI from an indexer declaring that language. With
// multiple ordered languages (e.g. ["de","en"] on an English title), earlier
// languages earn a better rank than later fallbacks, untagged releases match
// at the title's original language's rank when present in wanted, and bare
// DUAL ("original + one unknown") earns no bonus unless the indexer declares
// a non-original wanted source language.
func matchViewerLanguage(info core.ReleaseInfo, sourceLang string, wanted []string, t *core.Title) viewerLanguageMatch {
	return matchAudioPreference(info, sourceLang, wanted, "", t)
}

// matchViewerLanguageFor matches a release against the profile's Preferred
// audio language (if set) composed with the viewer's ordered audio languages
// (#743). When the profile is not in Preferred mode, it delegates directly to
// matchViewerLanguage so existing single- and multi-language behaviour is
// preserved verbatim.
func matchViewerLanguageFor(info core.ReleaseInfo, sourceLang string, wanted []string, profile core.QualityProfile, t *core.Title) viewerLanguageMatch {
	return matchAudioPreference(info, sourceLang, wanted, profilePreferredLanguage(profile), t)
}

func matchAudioPreference(info core.ReleaseInfo, sourceLang string, wanted []string, profilePref string, t *core.Title) viewerLanguageMatch {
	norm := normalizeWantedLanguages(wanted)
	if len(norm) == 0 {
		return viewerLanguageMatch{}
	}
	orig := titleOriginalLanguage(t)
	multi := len(norm) > 1
	if !multi && profilePref == "" {
		if orig == "" || releaseparse.LanguageMatch(orig, norm[0]) != releaseparse.LanguageNoMatch {
			return viewerLanguageMatch{}
		}
	}

	bestRank := func(lang string) (int, bool) {
		baseRank := -1
		for i, w := range norm {
			switch releaseparse.LanguageMatch(lang, w) {
			case releaseparse.LanguageExactMatch:
				return i, true
			case releaseparse.LanguageBaseMatch:
				if baseRank < 0 {
					baseRank = i
				}
			}
		}
		if baseRank >= 0 {
			return baseRank, true
		}
		return -1, false
	}

	if len(info.Languages) == 0 {
		if orig != "" {
			if r, ok := bestRank(orig); ok {
				return viewerLanguageMatch{
					Matched:          true,
					Rank:             r,
					Language:         releaseparse.LanguageName(orig),
					Original:         true,
					Multi:            multi,
					ProfilePreferred: profilePref,
				}
			}
		}
		return viewerLanguageMatch{}
	}

	var best viewerLanguageMatch
	hasMulti := false
	hasDual := false
	hasConcreteNonOrig := false
	consider := func(rank int, lang string) {
		if !best.Matched || rank < best.Rank {
			best = viewerLanguageMatch{
				Matched:          true,
				Rank:             rank,
				Language:         lang,
				Multi:            multi,
				ProfilePreferred: profilePref,
			}
		}
	}

	for _, l := range info.Languages {
		switch l {
		case releaseparse.LanguageMulti:
			hasMulti = true
		case releaseparse.LanguageDual:
			hasDual = true
		default:
			if orig == "" || releaseparse.LanguageMatch(l, orig) == releaseparse.LanguageNoMatch {
				hasConcreteNonOrig = true
			}
			if r, ok := bestRank(l); ok {
				consider(r, releaseparse.LanguageName(l))
			}
		}
	}

	sourceDub := dualSourceDubForOrig(sourceLang, orig)
	if hasDual && !hasConcreteNonOrig && sourceDub != "" {
		if r, ok := bestRank(sourceDub); ok {
			consider(r, sourceDub)
		}
	}
	if hasMulti && !best.Matched {
		if profilePref != "" && hasConcreteNonOrig {
			// A multi-audio release listing explicit non-original languages
			// (e.g. FRENCH.MULTI) carries those languages plus the title's
			// original language; credit it only when the original language is
			// wanted, not when unrelated languages (e.g. SPANISH.FRENCH.MULTI
			// on a Korean show) exclude the preferred language.
			if orig != "" {
				if r, ok := bestRank(orig); ok {
					consider(r, releaseparse.LanguageName(orig))
				}
			}
		} else if sourceDub != "" {
			if r, ok := bestRank(sourceDub); ok {
				consider(r, sourceDub)
			}
		} else {
			consider(0, releaseparse.LanguageMulti)
		}
	}
	return best
}

// viewerLanguageScore scales a profile's FormatViewerLanguage weight by the
// 0-based rank of the matched language: rank 0 receives the full score,
// rank 1 receives half, rank 2 a quarter, and so on, always remaining non-zero
// when base is non-zero.
func viewerLanguageScore(base, rank int) int {
	if base == 0 || rank <= 0 {
		return base
	}
	scaled := base / (1 << min(rank, 30))
	if scaled == 0 {
		if base > 0 {
			return 1
		}
		return -1
	}
	return scaled
}

// viewerLanguageFormatName formats the explainable FormatMatch label for
// FormatViewerLanguage. A profile's Preferred audio language at rank 0 is
// labelled "preferred language (<Language>)"; single-language viewers keep the
// plain built-in name; ordered multi-language lists show the matched rank and
// language.
func viewerLanguageFormatName(m viewerLanguageMatch) string {
	if m.ProfilePreferred != "" && m.Rank == 0 {
		return fmt.Sprintf("preferred language (%s)", releaseparse.LanguageDisplayName(m.ProfilePreferred))
	}
	base := BuiltinFormatName(core.FormatViewerLanguage)
	if !m.Multi {
		return base
	}
	switch {
	case m.Original && m.Language != "":
		return fmt.Sprintf("%s (#%d, %s original)", base, m.Rank+1, m.Language)
	case m.Language != "":
		return fmt.Sprintf("%s (#%d, %s)", base, m.Rank+1, m.Language)
	default:
		return fmt.Sprintf("%s (#%d)", base, m.Rank+1)
	}
}

// localizedEpisodeTitle reports a release dubbed into a language that is
// neither the catalogue's nor the title's original: its name carries the
// episode title in that language ("S06E06.La.resa.dei.conti.ITA.ENG"), so
// a different title there is a translation, not another episode.
func localizedEpisodeTitle(info core.ReleaseInfo, t *core.Title, catalogLang string) bool {
	base, _, _ := strings.Cut(strings.TrimSpace(catalogLang), "-")
	for _, l := range info.Languages {
		if l == releaseparse.LanguageMulti || l == releaseparse.LanguageDual {
			continue
		}
		if base != "" && releaseparse.LanguageMatch(l, base) != releaseparse.LanguageNoMatch {
			continue
		}
		if orig := titleOriginalLanguage(t); orig != "" && releaseparse.LanguageMatch(l, orig) != releaseparse.LanguageNoMatch {
			continue
		}
		return true
	}
	return false
}

// matchesAlternativeTitle reports a release named by another of the title's
// names: a romanized original ("Sousou.no.Frieren"), a regional or working
// title. The year, episode and identifier checks still apply.
func matchesAlternativeTitle(info core.ReleaseInfo, alts []string) bool {
	for _, alt := range alts {
		if releaseparse.TitleMatches(info, alt) {
			return true
		}
	}
	return false
}

// wantedSubtitles is the canonical language names (releaseparse.LanguageName)
// a release is credited for listing: the viewer's own, else the server
// language. Normalized once per resolve so per-candidate checks stay
// allocation-free.
func wantedSubtitles(user *core.User, serverLang string) []string {
	var raw []string
	if user != nil && len(user.SubtitleLanguages) > 0 {
		raw = user.SubtitleLanguages
	} else if base, _, _ := strings.Cut(strings.TrimSpace(serverLang), "-"); base != "" {
		raw = []string{base}
	}
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, lang := range raw {
		if name := releaseparse.LanguageName(lang); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

func isCanonicalLanguageName(s string) bool {
	if len(s) < 4 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && c != '-' {
			return false
		}
	}
	return true
}

const (
	wantedSubtitlePreference    = 1
	assumedOriginalPreference   = 2
	confirmedOriginalPreference = 4
)

func originalAudioPreference(rel core.Release, t *core.Title, original string) int {
	if !audioAcceptable(rel.Info, rel.SourceLanguage, []string{original}, t) {
		return 0
	}
	if len(rel.Info.Languages) > 0 && declaresConcreteLanguage(rel.Info, original, t) {
		return confirmedOriginalPreference
	}
	return assumedOriginalPreference
}

// releaseHasWantedSubtitles uses container evidence before listing hints.
func releaseHasWantedSubtitles(rel core.Release, info core.MediaInfo, probed bool, wanted []string) bool {
	if !probed {
		return indexerListsSubtitles(rel, wanted)
	}
	return slices.ContainsFunc(info.Streams, func(stream core.StreamInfo) bool {
		return stream.Type == "subtitle" && slices.ContainsFunc(wanted, func(lang string) bool {
			return langtag.BestStreamMatchRank(lang, stream.Language, stream.Title) > 0
		})
	})
}

// indexerListsSubtitles reports whether the indexer lists a subtitle track in
// one of the wanted canonical languages. A release whose indexer lists nothing
// is not penalised: most indexers leave the attribute out.
func indexerListsSubtitles(rel core.Release, wanted []string) bool {
	if len(rel.IndexerSubtitles) == 0 || len(wanted) == 0 {
		return false
	}
	for _, have := range rel.IndexerSubtitles {
		canonical := isCanonicalLanguageName(have)
		haveBase := ""
		if canonical {
			haveBase = releaseparse.LanguageBase(have)
		}
		for _, want := range wanted {
			if canonical {
				if have == want || haveBase == releaseparse.LanguageBase(want) {
					return true
				}
				continue
			}
			if releaseparse.LanguageMatch(have, want) != releaseparse.LanguageNoMatch {
				return true
			}
		}
	}
	return false
}

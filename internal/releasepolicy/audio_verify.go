package releasepolicy

import (
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// MaxUnknownAudioProbes bounds how many unknown-audio-language candidates a
// single resolve or playback failover sequence will probe to verify required
// audio tracks.
const MaxUnknownAudioProbes = 4

// UnverifiedAudioLanguages inspects a release rejected by p.Evaluate(rel, ctx)
// and reports whether every rejecting rule is a "require" rule that failed
// solely because one or more audio-language conditions ("language" or
// "audio_matches_original") evaluated Unknown from the release name.
//
// When unverified is true, required lists the normalized language names that a
// media probe must confirm before the release may be played.
func (p *Policy) UnverifiedAudioLanguages(rel core.Release, ctx Context) (required []string, unverified bool) {
	if p == nil || len(p.rules) == 0 {
		return nil, false
	}
	deferredAny := false
	for _, r := range p.rules {
		if r.source.Disabled {
			continue
		}
		if len(r.unless) > 0 && group(r.unless, "any", rel, ctx) == Match {
			continue
		}
		st := group(r.conditions, r.source.Match, rel, ctx)
		switch r.source.Action {
		case "avoid":
			if st == Match || st == Unknown {
				return nil, false
			}
		case "require":
			if st == Match {
				continue
			}
			if len(r.fallback) > 0 && group(r.fallback, "any", rel, ctx) == Match {
				continue
			}
			if st != Unknown {
				return nil, false
			}
			assumed, langs := groupAssumingAudioMatch(r.conditions, r.source.Match, rel, ctx)
			if assumed != Match || len(langs) == 0 {
				return nil, false
			}
			deferredAny = true
			for _, l := range langs {
				if !slices.Contains(required, l) {
					required = append(required, l)
				}
			}
		}
	}
	if !deferredAny {
		return nil, false
	}
	return required, true
}

// AudioExclusion reports whether p.Evaluate(rel, ctx) rejected rel because a
// declared audio language contradicted an audio rule (rather than a non-audio
// preference), returning that rule's name and the language(s) that would pass
// when it is a "require" rule.
func (p *Policy) AudioExclusion(rel core.Release, ctx Context) (ruleName, outcome string, needed []string, ok bool) {
	if p == nil || len(p.rules) == 0 {
		return "", "", nil, false
	}
	for _, r := range p.rules {
		if r.source.Disabled {
			continue
		}
		if len(r.unless) > 0 && group(r.unless, "any", rel, ctx) == Match {
			continue
		}
		st := group(r.conditions, r.source.Match, rel, ctx)
		switch r.source.Action {
		case "require":
			if st == Match || (len(r.fallback) > 0 && group(r.fallback, "any", rel, ctx) == Match) {
				continue
			}
			out := "exclude"
			if st == Unknown {
				out = "unknown"
			}
			langs := audioConditionLanguages(r.conditions, rel, ctx, false)
			if len(langs) == 0 {
				return "", "", nil, false
			}
			// Ensure non-audio conditions weren't the actual Miss in an "all" group.
			if r.source.Match != "any" {
				for _, c := range r.conditions {
					if !c.isAudioCondition() && c.evaluate(rel, ctx) == Miss {
						return "", "", nil, false
					}
				}
			}
			return r.source.Name, out, langs, true
		case "avoid":
			if st != Match && st != Unknown {
				continue
			}
			out := "exclude"
			if st == Unknown {
				out = "unknown"
			}
			hasAudio := false
			for _, c := range r.conditions {
				if c.isAudioCondition() && c.evaluate(rel, ctx) == st {
					hasAudio = true
					break
				}
			}
			if !hasAudio {
				return "", "", nil, false
			}
			return r.source.Name, out, nil, true
		}
	}
	return "", "", nil, false
}

// RequiredAudioMissing evaluates p against concrete probed audio streams from
// media probing (replacing name-declared audio languages with probed stream
// languages). When the probed audio fails an audio-language requirement in p,
// it returns the missing language name (for example "english") and ok=false.
func (p *Policy) RequiredAudioMissing(rel core.Release, ctx Context, streams []core.StreamInfo, estimated bool) (missing string, ok bool) {
	if p == nil || len(p.rules) == 0 {
		return "", true
	}
	probed := rel
	if estimated {
		probed.Info.Languages = nil
	} else {
		probed.Info.Languages = ProbedAudioLanguages(streams)
	}
	for _, r := range p.rules {
		if r.source.Disabled || r.source.Action != "require" {
			continue
		}
		if len(r.unless) > 0 && group(r.unless, "any", probed, ctx) == Match {
			continue
		}
		st := group(r.conditions, r.source.Match, probed, ctx)
		if st == Match {
			continue
		}
		if len(r.fallback) > 0 && group(r.fallback, "any", probed, ctx) == Match {
			continue
		}
		langs := audioConditionLanguages(r.conditions, probed, ctx, false)
		if len(langs) > 0 {
			return langs[0], false
		}
	}
	return "", true
}

// PrefersOriginalAudio reports whether original audio takes priority over dubs.
func PrefersOriginalAudio(profile core.QualityProfile, user *core.User) bool {
	language := strings.ToLower(strings.TrimSpace(profile.Language))
	switch {
	case language == core.ProfileLanguageOriginal:
		return true
	case language == core.ProfileLanguageAny || releaseparse.KnownLanguage(language):
		return false
	default:
		return user != nil && user.PreferOriginal
	}
}

// ProbedAudioLanguages extracts concrete audio track language tags from probed
// media streams using container language codes and stream titles.
func ProbedAudioLanguages(streams []core.StreamInfo) []string {
	var out []string
	for _, st := range streams {
		if st.Type != "audio" {
			continue
		}
		tags := langtag.EffectiveLanguages(st.Language, st.Title)
		hasSpecific := false
		for _, tag := range tags {
			if strings.Contains(tag, "-") {
				hasSpecific = true
				break
			}
		}
		for _, tag := range tags {
			tag = strings.TrimSpace(tag)
			if tag == "" || tag == "und" || tag == "unk" || tag == "mul" || tag == "zxx" {
				continue
			}
			if hasSpecific && !strings.Contains(tag, "-") {
				continue
			}
			if !slices.Contains(out, tag) {
				out = append(out, tag)
			}
			if !strings.Contains(tag, "-") {
				if name := releaseparse.LanguageName(tag); name != "" && !slices.Contains(out, name) {
					out = append(out, name)
				}
			}
		}
	}
	return out
}

// PositiveLanguageTargets returns normalized language names explicitly asked
// for by non-disabled "require" or "prefer" rules (and "require" fallbacks).
func PositiveLanguageTargets(rules []core.ReleaseRule) []string {
	var out []string
	add := func(v string) {
		if name := releaseparse.LanguageName(v); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, r := range rules {
		if r.Disabled || (r.Action != "require" && r.Action != "prefer") {
			continue
		}
		for _, c := range r.Conditions {
			if c.Field == "language" {
				add(c.Value)
			}
		}
		if r.Action == "require" {
			for _, c := range r.Fallback {
				if c.Field == "language" {
					add(c.Value)
				}
			}
		}
	}
	return out
}

func (c condition) isAudioCondition() bool {
	return c.Field == "language" || c.Field == "audio_matches_original"
}

func (c condition) audioTargetLabel(ctx Context) string {
	switch c.Field {
	case "language":
		return canonicalLanguageLabel(c.Value)
	case "audio_matches_original":
		return canonicalLanguageLabel(ctx.OriginalLanguage)
	default:
		return ""
	}
}

func canonicalLanguageLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	fact := declaredLanguage(raw)
	if fact.name == "" {
		return ""
	}
	if fact.script != "" || fact.region != "" {
		if canon := langtag.CanonicalBCP47(raw); canon != "" {
			return canon
		}
	}
	return fact.name
}

func groupAssumingAudioMatch(cs []condition, mode string, r core.Release, ctx Context) (State, []string) {
	unknown := false
	var langs []string
	for _, c := range cs {
		s := c.evaluate(r, ctx)
		if s == Unknown && c.isAudioCondition() {
			if label := c.audioTargetLabel(ctx); label != "" {
				s = Match
				if !slices.Contains(langs, label) {
					langs = append(langs, label)
				}
			}
		}
		if mode == "any" && s == Match {
			return Match, langs
		}
		if mode != "any" && s == Miss {
			return Miss, nil
		}
		unknown = unknown || s == Unknown
	}
	if unknown {
		return Unknown, nil
	}
	if mode == "any" {
		return Miss, nil
	}
	return Match, langs
}

func audioConditionLanguages(cs []condition, r core.Release, ctx Context, onlyUnknown bool) []string {
	var out []string
	for _, c := range cs {
		if !c.isAudioCondition() {
			continue
		}
		st := c.evaluate(r, ctx)
		if st == Match || (onlyUnknown && st != Unknown) {
			continue
		}
		if label := c.audioTargetLabel(ctx); label != "" && !slices.Contains(out, label) {
			out = append(out, label)
		}
	}
	return out
}

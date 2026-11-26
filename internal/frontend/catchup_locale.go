package frontend

import (
	"strings"

	"golang.org/x/text/language"
)

// CatchUpPresentation carries the client language's labels and short date/time
// format. Guide-supplied titles are displayed verbatim.
type CatchUpPresentation struct {
	Library, DateAired, Title string
	airingLayout              string
	Language                  string
}

var catchUpPresentations = []CatchUpPresentation{
	{Library: "Catch-up", DateAired: "Date Aired", Title: "Title", airingLayout: "1/2/2006, 3:04 PM", Language: "en-US"},
	{Library: "Archiv pořadů", DateAired: "Datum vysílání", Title: "Název", airingLayout: "2. 1. 2006 15:04", Language: "cs"},
	{Library: "Programarkiv", DateAired: "Sendedato", Title: "Titel", airingLayout: "02.01.2006 15.04", Language: "da"},
	{Library: "Mediathek", DateAired: "Sendedatum", Title: "Titel", airingLayout: "02.01.2006, 15:04", Language: "de"},
	{Library: "Programas anteriores", DateAired: "Fecha de emisión", Title: "Título", airingLayout: "2/1/2006, 15:04", Language: "es"},
	{Library: "Ohjelma-arkisto", DateAired: "Lähetyspäivä", Title: "Nimi", airingLayout: "2.1.2006 klo 15.04", Language: "fi"},
	{Library: "Télévision de rattrapage", DateAired: "Date de diffusion", Title: "Titre", airingLayout: "02/01/2006 15:04", Language: "fr"},
	{Library: "Programmi in replay", DateAired: "Data di trasmissione", Title: "Titolo", airingLayout: "02/01/2006, 15:04", Language: "it"},
	{Library: "見逃し配信", DateAired: "放送日", Title: "タイトル", airingLayout: "2006/01/02 15:04", Language: "ja"},
	{Library: "다시 보기", DateAired: "방송일", Title: "제목", airingLayout: "2006. 1. 2. 15:04", Language: "ko"},
	{Library: "Programarkiv", DateAired: "Sendedato", Title: "Tittel", airingLayout: "02.01.2006, 15:04", Language: "nb"},
	{Library: "Uitzending gemist", DateAired: "Uitzenddatum", Title: "Titel", airingLayout: "02-01-2006 15:04", Language: "nl"},
	{Library: "Archiwum programów", DateAired: "Data emisji", Title: "Tytuł", airingLayout: "2.01.2006, 15:04", Language: "pl"},
	{Library: "Programas anteriores", DateAired: "Data de exibição", Title: "Título", airingLayout: "02/01/2006 15:04", Language: "pt-BR"},
	{Library: "Архив передач", DateAired: "Дата эфира", Title: "Название", airingLayout: "02.01.2006, 15:04", Language: "ru"},
	{Library: "Programarkiv", DateAired: "Sändningsdatum", Title: "Titel", airingLayout: "2006-01-02 15:04", Language: "sv"},
	{Library: "Yayın arşivi", DateAired: "Yayın tarihi", Title: "Başlık", airingLayout: "2.01.2006 15:04", Language: "tr"},
	{Library: "Архів передач", DateAired: "Дата ефіру", Title: "Назва", airingLayout: "02.01.2006, 15:04", Language: "uk"},
	{Library: "回看", DateAired: "播出日期", Title: "标题", airingLayout: "2006/1/2 15:04", Language: "zh-CN"},
	{Library: "節目回看", DateAired: "播出日期", Title: "標題", airingLayout: "2006/1/2 15:04", Language: "zh-TW"},
}

var catchUpLanguages = func() language.Matcher {
	tags := make([]language.Tag, 0, len(catchUpPresentations))
	for _, p := range catchUpPresentations {
		tags = append(tags, language.MustParse(p.Language))
	}
	return language.NewMatcher(tags)
}()

func CatchUpPresentationFor(raw string) CatchUpPresentation {
	tag, idx := language.MatchStrings(catchUpLanguages, raw)
	p := catchUpPresentations[idx]
	region, _ := tag.Region()
	code := region.String()
	if override := tag.TypeForKey("rg"); len(override) == 6 {
		code = strings.ToUpper(override[:2])
	}
	switch {
	case p.Language == "en-US" && code == "CA":
		p.Language, p.airingLayout = "en-CA", "2006-01-02, 3:04 PM"
	case p.Language == "en-US" && (code == "GB" || code == "AU" || code == "NZ" || code == "IE"):
		p.Language, p.airingLayout = "en-"+code, "02/01/2006, 15:04"
	case p.Language == "fr" && code == "CA":
		p.Language, p.airingLayout = "fr-CA", "2006-01-02 15:04"
	}
	return p
}

func (p CatchUpPresentation) EpisodeTitle(airing LiveTVProgramme) string {
	if strings.TrimSpace(airing.Item.EpisodeTitle) != "" {
		return airing.Item.EpisodeTitle
	}
	return airing.Start.Local().Format(p.airingLayout)
}

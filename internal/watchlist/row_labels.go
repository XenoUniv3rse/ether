package watchlist

import "golang.org/x/text/language"

type rowLabels struct {
	name, waiting, ready, badge, separator string
}

func (l rowLabels) split(name, status string) string {
	return name + l.separator + status
}

var rowLocales = []struct {
	tag  string
	text rowLabels
}{
	{"en", rowLabels{"Watchlist", "Waiting for a stream", "Now available", "Not yet available", ": "}},
	{"cs", rowLabels{"Seznam ke zhlédnutí", "Čeká na stream", "Nyní dostupné", "Zatím nedostupné", ": "}},
	{"da", rowLabels{"Overvågningsliste", "Venter på en stream", "Tilgængelig nu", "Ikke tilgængelig endnu", ": "}},
	{"de", rowLabels{"Merkliste", "Warten auf einen Stream", "Jetzt verfügbar", "Noch nicht verfügbar", ": "}},
	{"es", rowLabels{"Lista de seguimiento", "Esperando una fuente", "Ya disponible", "Aún no disponible", ": "}},
	{"fi", rowLabels{"Katselulista", "Odottaa suoratoistolähdettä", "Nyt saatavilla", "Ei vielä saatavilla", ": "}},
	{"fr", rowLabels{"Liste de suivi", "En attente d’une source", "Disponible maintenant", "Pas encore disponible", " : "}},
	{"it", rowLabels{"Lista da guardare", "In attesa di uno stream", "Ora disponibile", "Non ancora disponibile", ": "}},
	{"ja", rowLabels{"ウォッチリスト", "配信待ち", "視聴可能", "まだ視聴できません", "："}},
	{"ko", rowLabels{"찜 목록", "스트림 대기 중", "지금 시청 가능", "아직 시청할 수 없음", ": "}},
	{"nb", rowLabels{"Seerliste", "Venter på en strøm", "Tilgjengelig nå", "Ikke tilgjengelig ennå", ": "}},
	{"nl", rowLabels{"Kijklijst", "Wachten op een stream", "Nu beschikbaar", "Nog niet beschikbaar", ": "}},
	{"pl", rowLabels{"Lista do obejrzenia", "Oczekiwanie na transmisję", "Już dostępne", "Jeszcze niedostępne", ": "}},
	{"pt-BR", rowLabels{"Lista de acompanhamento", "Aguardando uma fonte", "Disponível agora", "Ainda indisponível", ": "}},
	{"ru", rowLabels{"Список просмотра", "Ожидание источника", "Уже доступно", "Пока недоступно", ": "}},
	{"sv", rowLabels{"Bevakningslista", "Väntar på en ström", "Tillgänglig nu", "Inte tillgänglig ännu", ": "}},
	{"tr", rowLabels{"İzleme listesi", "Yayın bekleniyor", "Şimdi izlenebilir", "Henüz izlenemiyor", ": "}},
	{"uk", rowLabels{"Список перегляду", "Очікування джерела", "Уже доступно", "Ще недоступно", ": "}},
	{"zh-CN", rowLabels{"想看清单", "等待播放源", "现已可观看", "暂不可观看", "："}},
	{"zh-TW", rowLabels{"想看清單", "等待播放來源", "現已可觀看", "暫不可觀看", "："}},
}

var rowLocaleMatcher = func() language.Matcher {
	tags := make([]language.Tag, 0, len(rowLocales))
	for _, locale := range rowLocales {
		tags = append(tags, language.MustParse(locale.tag))
	}
	return language.NewMatcher(tags)
}()

func watchlistLabels(acceptLanguage string) rowLabels {
	_, index := language.MatchStrings(rowLocaleMatcher, acceptLanguage)
	return rowLocales[index].text
}

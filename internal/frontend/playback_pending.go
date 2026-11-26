package frontend

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"golang.org/x/text/language"
)

var playbackPendingText = [...]struct{ finding, queued string }{
	{"Finding a source…", "Waiting for the download service…"},
	{"Quelle wird gesucht…", "Warten auf den Download-Dienst…"},
	{"Buscando una fuente…", "Esperando al servicio de descargas…"},
	{"Recherche d’une source…", "En attente du service de téléchargement…"},
	{"Ricerca di una fonte…", "In attesa del servizio di download…"},
	{"ソースを検索中…", "ダウンロードサービスの応答を待っています…"},
	{"소스를 찾는 중…", "다운로드 서비스를 기다리는 중…"},
	{"Een bron zoeken…", "Wachten op de downloadservice…"},
	{"Leter etter en kilde…", "Venter på nedlastingstjenesten…"},
	{"Wyszukiwanie źródła…", "Oczekiwanie na usługę pobierania…"},
	{"Buscando uma fonte…", "Aguardando o serviço de download…"},
	{"Поиск источника…", "Ожидание службы загрузки…"},
	{"Letar efter en källa…", "Väntar på nedladdningstjänsten…"},
	{"Kaynak aranıyor…", "İndirme hizmeti bekleniyor…"},
	{"Пошук джерела…", "Очікування служби завантаження…"},
	{"正在查找来源…", "正在等待下载服务…"},
	{"正在尋找來源…", "正在等待下載服務…"},
	{"Søger efter en kilde…", "Venter på downloadtjenesten…"},
	{"Etsitään lähdettä…", "Odotetaan latauspalvelua…"},
	{"Hledá se zdroj…", "Čeká se na službu stahování…"},
}

// PlaybackPending describes a retryable media open while a source is being
// found or a provider is downloading it. An unavailable source has no retry hint here.
func PlaybackPending(err error, languages ...string) (code, message string, seconds int) {
	queued := errors.Is(err, core.ErrDownloadQueued)
	switch {
	case queued, errors.Is(err, core.ErrPreparing):
		code, seconds = "preparing", core.PreparingRetrySeconds(err, 5)
	case errors.Is(err, core.ErrPrepareSlow):
		code, seconds = "preparation_pending", 2
	default:
		return "", "", 0
	}
	_, locale := language.MatchStrings(playbackErrorLocaleMatcher, languages...)
	msg := playbackPendingText[locale]
	if queued {
		// Keep the provider's queue position ("queued at TorBox behind 3
		// active downloads") so the viewer knows why it is waiting.
		if detail := core.DownloadQueuedDetail(err); detail != "" {
			return code, strings.TrimSuffix(msg.queued, "…") + " (" + detail + ")…", seconds
		}
		return code, msg.queued, seconds
	}
	return code, msg.finding, seconds
}

// SetPlaybackRetry sets the media retry headers without choosing a response body.
func SetPlaybackRetry(w http.ResponseWriter, code string, seconds int) {
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	w.Header().Set("X-Ether-Media-Error", code)
	w.Header().Set("Cache-Control", "no-store")
}

func WritePlaybackPending(w http.ResponseWriter, err error, languages ...string) bool {
	code, msg, seconds := PlaybackPending(err, languages...)
	if seconds == 0 {
		return false
	}
	SetPlaybackRetry(w, code, seconds)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
	return true
}

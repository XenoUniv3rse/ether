package vela

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"golang.org/x/text/language"
)

// Each row follows the order of downloadLocaleMatcher.
var autoSelectionMessages = map[frontend.AutoSelectionReason][20]string{
	frontend.AutoConnectionEstimate: {
		"Auto favored a lower bitrate based on this device’s recent download speeds.",
		"Auto bevorzugte aufgrund der letzten Downloadgeschwindigkeiten dieses Geräts eine niedrigere Bitrate.",
		"Auto prefirió una tasa de bits más baja según las velocidades de descarga recientes de este dispositivo.",
		"Auto a privilégié un débit inférieur d’après les vitesses de téléchargement récentes de cet appareil.",
		"Auto ha preferito un bitrate inferiore in base alle recenti velocità di download di questo dispositivo.",
		"このデバイスの最近のダウンロード速度に基づき、Auto は低いビットレートを優先しました。",
		"이 기기의 최근 다운로드 속도를 기준으로 자동 선택이 낮은 비트레이트를 우선했습니다.",
		"Auto gaf de voorkeur aan een lagere bitsnelheid op basis van recente downloadsnelheden van dit apparaat.",
		"Auto foretrakk en lavere bithastighet basert på enhetens nylige nedlastingshastigheter.",
		"Tryb Auto preferował niższą przepływność na podstawie ostatnich prędkości pobierania na tym urządzeniu.",
		"O modo Auto deu preferência a uma taxa de bits menor com base nas velocidades de download recentes deste dispositivo.",
		"Авторежим предпочёл меньший битрейт с учётом недавних скоростей загрузки на этом устройстве.",
		"Auto föredrog en lägre bithastighet utifrån enhetens senaste nedladdningshastigheter.",
		"Otomatik seçim, bu cihazın son indirme hızlarına göre daha düşük bir bit hızını tercih etti.",
		"Авторежим віддав перевагу нижчому бітрейту з огляду на нещодавні швидкості завантаження на цьому пристрої.",
		"根据此设备最近的下载速度，自动模式优先选择了较低的比特率。",
		"根據此裝置近期的下載速度，自動模式優先選擇了較低的位元速率。",
		"Auto foretrak en lavere bithastighed ud fra enhedens seneste downloadhastigheder.",
		"Automaattinen valinta suosi pienempää bittinopeutta laitteen viimeaikaisten latausnopeuksien perusteella.",
		"Automatická volba upřednostnila nižší datový tok podle nedávných rychlostí stahování na tomto zařízení.",
	},
	frontend.AutoSourceDelivery: {
		"Auto favored a lower bitrate based on recent speeds from the media source.",
		"Auto bevorzugte aufgrund der letzten Übertragungsraten der Medienquelle eine niedrigere Bitrate.",
		"Auto prefirió una tasa de bits más baja según las velocidades recientes de la fuente multimedia.",
		"Auto a privilégié un débit inférieur d’après les vitesses récentes de la source multimédia.",
		"Auto ha preferito un bitrate inferiore in base alle recenti velocità della sorgente multimediale.",
		"メディアソースの最近の転送速度に基づき、Auto は低いビットレートを優先しました。",
		"미디어 소스의 최근 전송 속도를 기준으로 자동 선택이 낮은 비트레이트를 우선했습니다.",
		"Auto gaf de voorkeur aan een lagere bitsnelheid op basis van recente overdrachtssnelheden van de mediabron.",
		"Auto foretrakk en lavere bithastighet basert på de nylige hastighetene fra mediekilden.",
		"Tryb Auto preferował niższą przepływność na podstawie ostatnich prędkości przesyłania ze źródła multimediów.",
		"O modo Auto deu preferência a uma taxa de bits menor com base nas velocidades recentes da fonte de mídia.",
		"Авторежим предпочёл меньший битрейт с учётом недавних скоростей передачи из источника медиа.",
		"Auto föredrog en lägre bithastighet utifrån mediekällans senaste överföringshastigheter.",
		"Otomatik seçim, medya kaynağının son aktarım hızlarına göre daha düşük bir bit hızını tercih etti.",
		"Авторежим віддав перевагу нижчому бітрейту з огляду на нещодавні швидкості передавання з джерела медіа.",
		"根据媒体源最近的传输速度，自动模式优先选择了较低的比特率。",
		"根據媒體來源近期的傳輸速度，自動模式優先選擇了較低的位元速率。",
		"Auto foretrak en lavere bithastighed ud fra mediekildens seneste overførselshastigheder.",
		"Automaattinen valinta suosi pienempää bittinopeutta medialähteen viimeaikaisten siirtonopeuksien perusteella.",
		"Automatická volba upřednostnila nižší datový tok podle nedávných přenosových rychlostí zdroje médií.",
	},
	frontend.AutoResumeSource: {
		"Auto reused the source from an earlier playback of this title.",
		"Auto hat die Quelle aus einer früheren Wiedergabe dieses Titels wiederverwendet.",
		"El modo Auto reutilizó la fuente de una reproducción anterior de este título.",
		"Auto a réutilisé la source d’une lecture précédente de ce titre.",
		"Auto ha riutilizzato la sorgente di una riproduzione precedente di questo titolo.",
		"Auto は、このタイトルの以前の再生で使ったソースを再利用しました。",
		"자동 선택이 이 타이틀을 이전에 재생할 때 사용한 소스를 다시 사용했습니다.",
		"Auto hergebruikte de bron van een eerdere afspeelsessie van deze titel.",
		"Auto brukte kilden fra en tidligere avspilling av denne tittelen på nytt.",
		"Tryb Auto ponownie użył źródła z wcześniejszego odtwarzania tego tytułu.",
		"O modo Auto reutilizou a fonte de uma reprodução anterior deste título.",
		"Авторежим повторно использовал источник предыдущего воспроизведения этого произведения.",
		"Auto återanvände källan från en tidigare uppspelning av den här titeln.",
		"Otomatik seçim, bu yapımın önceki oynatımında kullanılan kaynağı yeniden kullandı.",
		"Авторежим повторно використав джерело попереднього відтворення цього твору.",
		"自动模式重新使用了之前播放此内容时的来源。",
		"自動模式重新使用了先前播放此內容時的來源。",
		"Auto genbrugte kilden fra en tidligere afspilning af denne titel.",
		"Automaattinen valinta käytti uudelleen tämän nimikkeen aiemman toiston lähdettä.",
		"Automatická volba znovu použila zdroj z dřívějšího přehrávání tohoto titulu.",
	},
}

func autoSelectionReason(code frontend.AutoSelectionReason, languages string) object {
	messages, ok := autoSelectionMessages[code]
	if !ok {
		return nil
	}
	_, locale := language.MatchStrings(downloadLocaleMatcher, languages)
	return object{"code": code, "message": messages[locale]}
}

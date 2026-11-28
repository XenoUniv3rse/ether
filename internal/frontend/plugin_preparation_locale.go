package frontend

import "golang.org/x/text/language"

var pluginPreparationText = [...]string{
	"Waiting for the plugin to prepare",
	"Warten auf die Vorbereitung durch das Plugin",
	"Esperando a que el complemento termine la preparación",
	"En attente de la préparation par le module",
	"In attesa della preparazione da parte del plugin",
	"プラグインの準備を待っています",
	"플러그인이 준비를 마칠 때까지 기다리는 중",
	"Wachten tot de plug-in klaar is met voorbereiden",
	"Venter på at programtillegget skal bli ferdig",
	"Oczekiwanie na zakończenie przygotowania przez wtyczkę",
	"Aguardando a preparação pelo plugin",
	"Ожидание подготовки плагином",
	"Väntar på att insticksprogrammet ska bli klart",
	"Eklentinin hazırlığı tamamlaması bekleniyor",
	"Очікування підготовки плагіном",
	"正在等待插件完成准备",
	"正在等待外掛程式完成準備",
	"Venter på, at pluginet bliver færdigt",
	"Odotetaan, että lisäosa saa valmistelun valmiiksi",
	"Čeká se na dokončení přípravy pluginem",
}

// PluginPreparationLabel localizes the active download preparation status.
func PluginPreparationLabel(languages ...string) string {
	_, locale := language.MatchStrings(playbackErrorLocaleMatcher, languages...)
	return pluginPreparationText[locale]
}

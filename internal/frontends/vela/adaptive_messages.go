package vela

import "golang.org/x/text/language"

func setAdaptiveUnavailable(path object, reason, languages string) {
	_, locale := language.MatchStrings(downloadLocaleMatcher, languages)
	key := reason
	switch reason {
	case "encoder_hevc_unavailable", "dynamic_range_unavailable":
		key = "video_format_unavailable"
	}
	messages, ok := adaptiveMessages[key]
	if !ok {
		messages = adaptiveMessages["no_compatible_rungs"]
	}
	path["adaptiveUnavailable"] = object{"reason": reason, "message": messages[locale]}
}

// Each row follows the order of downloadLocaleMatcher.
var adaptiveMessages = map[string][20]string{
	"no_compatible_rungs": {
		"Adaptive quality is unavailable for this playback path.",
		"Für diesen Wiedergabepfad ist keine adaptive Qualität verfügbar.",
		"La calidad adaptativa no está disponible para esta ruta de reproducción.",
		"La qualité adaptative est indisponible pour ce mode de lecture.",
		"La qualità adattiva non è disponibile per questa modalità di riproduzione.",
		"この再生経路では画質の自動切り替えを利用できません。",
		"이 재생 경로에서는 자동 화질 조절을 사용할 수 없습니다.",
		"Adaptieve kwaliteit is niet beschikbaar voor deze afspeelroute.",
		"Adaptiv kvalitet er ikke tilgjengelig for denne avspillingsmåten.",
		"Adaptacyjna jakość jest niedostępna dla tej ścieżki odtwarzania.",
		"A qualidade adaptativa não está disponível para este modo de reprodução.",
		"Адаптивное качество недоступно для этого способа воспроизведения.",
		"Adaptiv kvalitet är inte tillgänglig för det här uppspelningssättet.",
		"Bu oynatma yolu için uyarlanabilir kalite kullanılamıyor.",
		"Адаптивна якість недоступна для цього способу відтворення.",
		"此播放路径不支持自适应画质。",
		"此播放路徑不支援自適應畫質。",
		"Adaptiv kvalitet er ikke tilgængelig for denne afspilningsmetode.",
		"Mukautuva laatu ei ole käytettävissä tällä toistotavalla.",
		"Pro tento způsob přehrávání není dostupná adaptivní kvalita.",
	},
	"shared_audio_unavailable": {
		"The client cannot play the common AAC audio format required by these variants.",
		"Der Client unterstützt das gemeinsame AAC-Audioformat dieser Varianten nicht.",
		"El cliente no admite el formato de audio AAC común de estas variantes.",
		"Le client ne prend pas en charge le format audio AAC commun à ces variantes.",
		"Il client non supporta il formato audio AAC comune a queste varianti.",
		"クライアントは各画質に共通する AAC 音声形式に対応していません。",
		"클라이언트에서 이 화질들이 공유하는 AAC 오디오 형식을 재생할 수 없습니다.",
		"De client ondersteunt de gemeenschappelijke AAC-audio-indeling van deze varianten niet.",
		"Klienten støtter ikke det felles AAC-lydformatet for disse variantene.",
		"Klient nie obsługuje wspólnego formatu dźwięku AAC dla tych wariantów.",
		"O cliente não é compatível com o formato de áudio AAC comum a estas variantes.",
		"Клиент не поддерживает общий для этих вариантов аудиоформат AAC.",
		"Klienten stöder inte det gemensamma AAC-ljudformatet för dessa varianter.",
		"İstemci bu sürümlerin ortak AAC ses biçimini desteklemiyor.",
		"Клієнт не підтримує спільний для цих варіантів аудіоформат AAC.",
		"客户端不支持这些变体共用的 AAC 音频格式。",
		"用戶端不支援這些變體共用的 AAC 音訊格式。",
		"Klienten understøtter ikke det fælles AAC-lydformat for disse varianter.",
		"Asiakas ei tue näiden laatuvaihtoehtojen yhteistä AAC-äänimuotoa.",
		"Klient nepodporuje společný formát zvuku AAC těchto variant.",
	},
	"video_format_unavailable": {
		"Ether cannot encode compatible variants in this video's format.",
		"Ether kann keine kompatiblen Varianten in diesem Videoformat kodieren.",
		"Ether no puede codificar variantes compatibles en este formato de vídeo.",
		"Ether ne peut pas encoder de variantes compatibles dans ce format vidéo.",
		"Ether non può codificare varianti compatibili in questo formato video.",
		"Ether はこの動画形式で互換性のある画質をエンコードできません。",
		"Ether에서 이 영상 형식에 맞는 화질을 인코딩할 수 없습니다.",
		"Ether kan geen compatibele varianten in deze video-indeling coderen.",
		"Ether kan ikke kode kompatible varianter i dette videoformatet.",
		"Ether nie może zakodować zgodnych wariantów w tym formacie wideo.",
		"O Ether não consegue codificar variantes compatíveis neste formato de vídeo.",
		"Ether не может закодировать совместимые варианты в этом формате видео.",
		"Ether kan inte koda kompatibla varianter i det här videoformatet.",
		"Ether bu video biçiminde uyumlu sürümler kodlayamıyor.",
		"Ether не може закодувати сумісні варіанти в цьому форматі відео.",
		"Ether 无法以此视频格式编码兼容的变体。",
		"Ether 無法以此視訊格式編碼相容的變體。",
		"Ether kan ikke kode kompatible varianter i dette videoformat.",
		"Ether ei voi koodata yhteensopivia laatuvaihtoehtoja tässä videomuodossa.",
		"Ether nedokáže zakódovat kompatibilní varianty v tomto formátu videa.",
	},
	"original_selected": {
		"The selected original file plays at its source quality.",
		"Die ausgewählte Originaldatei wird in Quellqualität wiedergegeben.",
		"El archivo original seleccionado se reproduce con la calidad de la fuente.",
		"Le fichier original sélectionné est lu dans sa qualité source.",
		"Il file originale selezionato viene riprodotto con la qualità della sorgente.",
		"選択したオリジナルファイルは元の画質で再生されます。",
		"선택한 원본 파일은 원본 화질로 재생됩니다.",
		"Het geselecteerde originele bestand speelt af in bronkwaliteit.",
		"Den valgte originalfilen spilles av i kildekvalitet.",
		"Wybrany oryginalny plik jest odtwarzany w jakości źródłowej.",
		"O arquivo original selecionado é reproduzido com a qualidade da fonte.",
		"Выбранный исходный файл воспроизводится в оригинальном качестве.",
		"Den valda originalfilen spelas upp i källkvalitet.",
		"Seçilen özgün dosya kaynak kalitesinde oynatılır.",
		"Вибраний оригінальний файл відтворюється у вихідній якості.",
		"所选原始文件按源画质播放。",
		"所選原始檔案以來源畫質播放。",
		"Den valgte originalfil afspilles i kildekvalitet.",
		"Valittu alkuperäistiedosto toistetaan lähdelaadulla.",
		"Vybraný původní soubor se přehrává ve zdrojové kvalitě.",
	},
	"copy_alignment_unverified": {
		"Ether could not verify that the source video cuts align with encoded variants.",
		"Ether konnte nicht bestätigen, dass die Schnitte im Quellvideo mit den kodierten Varianten übereinstimmen.",
		"Ether no pudo verificar que los cortes del vídeo original coincidan con las variantes codificadas.",
		"Ether n’a pas pu vérifier que les coupes de la vidéo source coïncident avec celles des variantes encodées.",
		"Ether non ha potuto verificare che i tagli del video sorgente coincidano con quelli delle varianti codificate.",
		"元の動画とエンコード済みの各画質で区切り位置が一致することを確認できませんでした。",
		"Ether가 원본 영상과 인코딩된 화질 간에 구간 경계가 일치하는지 확인하지 못했습니다.",
		"Ether kon niet vaststellen dat de knippunten van de bronvideo overeenkomen met de gecodeerde varianten.",
		"Ether kunne ikke bekrefte at kuttene i kildevideoen samsvarer med de kodede variantene.",
		"Ether nie mógł potwierdzić, że cięcia w źródłowym filmie pokrywają się z cięciami w zakodowanych wariantach.",
		"O Ether não conseguiu confirmar que os cortes do vídeo de origem coincidem com os das variantes codificadas.",
		"Ether не смог подтвердить, что границы сегментов исходного видео совпадают с границами кодированных вариантов.",
		"Ether kunde inte bekräfta att källvideons klippgränser matchar de kodade varianterna.",
		"Ether, kaynak videonun kesim noktalarının kodlanmış sürümlerle eşleştiğini doğrulayamadı.",
		"Ether не зміг підтвердити, що межі сегментів вихідного відео збігаються з межами кодованих варіантів.",
		"Ether 无法确认源视频与编码版本的分段边界一致。",
		"Ether 無法確認來源影片與編碼版本的分段邊界一致。",
		"Ether kunne ikke bekræfte, at kildevideoens klipgrænser matcher de kodede varianter.",
		"Ether ei voinut varmistaa, että lähdevideon leikkauskohdat vastaavat koodattujen versioiden leikkauskohtia.",
		"Ether nemohl ověřit, že hranice segmentů zdrojového videa odpovídají hranicím kódovaných variant.",
	},
}

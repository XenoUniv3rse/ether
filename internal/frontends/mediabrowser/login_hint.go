package mediabrowser

import (
	"net/http"

	"golang.org/x/text/language"
)

var loginHintLanguages = language.NewMatcher([]language.Tag{
	language.English, language.Czech, language.Danish, language.German, language.Spanish,
	language.Finnish, language.French, language.Italian, language.Japanese, language.Korean,
	language.MustParse("nb"), language.Dutch, language.Polish, language.BrazilianPortuguese,
	language.Russian, language.Swedish, language.Turkish, language.Ukrainian,
	language.SimplifiedChinese, language.TraditionalChinese,
})

var loginHintCopy = [...]struct{ jellyfin, emby string }{
	{"Using Quick Connect? Sign in on your Ether viewer page and enter the code shown here.", "Sign in with the username and password you chose on your Ether viewer page."},
	{"Používáte Quick Connect? Přihlaste se na stránce diváka Ether a zadejte zde zobrazený kód.", "Přihlaste se uživatelským jménem a heslem, které jste si zvolili na stránce diváka Ether."},
	{"Bruger du Quick Connect? Log ind på din Ether-seerside, og indtast koden, der vises her.", "Log ind med det brugernavn og den adgangskode, du valgte på din Ether-seerside."},
	{"Verwendest du Quick Connect? Melde dich auf deiner Ether-Zuschauerseite an und gib den hier angezeigten Code ein.", "Melde dich mit dem Benutzernamen und Passwort an, die du auf deiner Ether-Zuschauerseite gewählt hast."},
	{"¿Usas Quick Connect? Inicia sesión en tu página de espectador de Ether e introduce el código que se muestra aquí.", "Inicia sesión con el nombre de usuario y la contraseña que elegiste en tu página de espectador de Ether."},
	{"Käytätkö Quick Connectia? Kirjaudu Ether-katselijasivullesi ja syötä tässä näkyvä koodi.", "Kirjaudu Ether-katselijasivullasi valitsemallasi käyttäjätunnuksella ja salasanalla."},
	{"Vous utilisez Quick Connect ? Connectez-vous à votre page spectateur Ether et saisissez le code affiché ici.", "Connectez-vous avec le nom d’utilisateur et le mot de passe choisis sur votre page spectateur Ether."},
	{"Usi Quick Connect? Accedi alla tua pagina spettatore di Ether e inserisci il codice mostrato qui.", "Accedi con il nome utente e la password scelti nella tua pagina spettatore di Ether."},
	{"Quick Connect を使いますか？ Ether の視聴者ページにサインインし、ここに表示されたコードを入力してください。", "Ether の視聴者ページで設定したユーザー名とパスワードでサインインしてください。"},
	{"Quick Connect를 사용하시나요? Ether 시청자 페이지에 로그인한 후 여기에 표시된 코드를 입력하세요.", "Ether 시청자 페이지에서 설정한 사용자 이름과 비밀번호로 로그인하세요."},
	{"Bruker du Quick Connect? Logg inn på Ether-seersiden din og skriv inn koden som vises her.", "Logg inn med brukernavnet og passordet du valgte på Ether-seersiden din."},
	{"Gebruik je Quick Connect? Meld je aan op je Ether-kijkerspagina en voer de code in die hier staat.", "Meld je aan met de gebruikersnaam en het wachtwoord die je op je Ether-kijkerspagina hebt gekozen."},
	{"Korzystasz z Quick Connect? Zaloguj się na swojej stronie widza Ether i wpisz wyświetlony tutaj kod.", "Zaloguj się nazwą użytkownika i hasłem wybranymi na swojej stronie widza Ether."},
	{"Usando o Quick Connect? Entre na sua página de espectador do Ether e digite o código exibido aqui.", "Entre com o nome de usuário e a senha que você escolheu na sua página de espectador do Ether."},
	{"Используете Quick Connect? Войдите на свою страницу зрителя Ether и введите показанный здесь код.", "Войдите с именем пользователя и паролем, которые вы выбрали на своей странице зрителя Ether."},
	{"Använder du Quick Connect? Logga in på din Ether-tittarsida och ange koden som visas här.", "Logga in med användarnamnet och lösenordet som du valde på din Ether-tittarsida."},
	{"Quick Connect mi kullanıyorsunuz? Ether izleyici sayfanızda oturum açın ve burada gösterilen kodu girin.", "Ether izleyici sayfanızda seçtiğiniz kullanıcı adı ve parolayla oturum açın."},
	{"Користуєтеся Quick Connect? Увійдіть на свою сторінку глядача Ether і введіть показаний тут код.", "Увійдіть з іменем користувача й паролем, які ви вибрали на своїй сторінці глядача Ether."},
	{"使用 Quick Connect？请登录您的 Ether 观看者页面，并输入此处显示的代码。", "请使用您在 Ether 观看者页面设置的用户名和密码登录。"},
	{"使用 Quick Connect？請登入您的 Ether 觀眾頁面，並輸入此處顯示的代碼。", "請使用您在 Ether 觀眾頁面設定的使用者名稱和密碼登入。"},
}

// loginHint uses only static copy and frontend capabilities. The portal
// address can differ from the app's host and stays out of public branding.
func (i *instance) loginHint(r *http.Request) string {
	if i.host.ViewerPortalURL == nil || (i.protocol == "jellyfin" && !i.quickEnabled()) {
		return ""
	}
	_, locale := language.MatchStrings(loginHintLanguages, r.Header.Get("Accept-Language"))
	hint := loginHintCopy[locale]
	if i.protocol == "emby" {
		return hint.emby
	}
	return hint.jellyfin
}

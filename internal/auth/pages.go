package auth

import (
	"html/template"
	"net/http"
)

// The few pages a person sees in their browser: errors, the device sign-in
// form, and the consent page. Everything else happens at the forge.
var pages = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>devbot: {{.Title}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem}
input{font:inherit;font-family:monospace;letter-spacing:.1em;text-transform:uppercase;padding:.3rem}
button{font:inherit;padding:.3rem 1rem}.err{color:#b00}.warn{border-left:4px solid #b00;padding-left:.75rem}</style></head>
<body><h1>{{.Title}}</h1>
{{if .Form}}<p>Enter the code your device shows. You'll sign in to the forge next, as the person the device will act for.</p>
<p class="warn">Only enter a code from a device in front of you that you started signing in yourself. If someone sent you this code, or a link to this page, stop: entering it gives them access to devbot, and the forge, as you.</p>
{{with .Error}}<p class="err">{{.}}</p>{{end}}
<form method="post" action="device"><input name="user_code" placeholder="XXXX-XXXX" autocomplete="off" required>
<button type="submit">Continue</button></form>
{{else if .Consent}}<p>You're signed in to {{.Forge}} as <b>@{{.Login}}</b>.</p>
<p><b>{{.Client}}</b> (the name it gave itself) wants to use devbot as you: to file tasks, answer, cancel and retry them on {{.Forge}} under your name, and to see the tasks you can see.</p>
{{if .UserCode}}<p>It's the device showing the code <b>{{.UserCode}}</b>.</p>
<p class="warn"><b>Deny this unless you started the sign-in yourself, on a device you're looking at, and copied the code from its screen.</b> Anyone can register an app with devbot under any name, so the name above proves nothing. If the code came in a message, an email or a chat, whoever sent it gets your access if you allow it.</p>{{else}}<p>devbot will send you back to <b>{{.Dest}}</b>. Only allow it if that's the app you just used.</p>{{end}}
<form method="post" action="consent"><input type="hidden" name="consent" value="{{.Token}}">
<button type="submit" name="decision" value="allow">Allow</button> <button type="submit" name="decision" value="deny">Deny</button></form>
{{else}}<p>{{.Text}}</p>{{end}}
</body></html>`))

type pageData struct {
	Title, Text, Error string
	Form               bool
	// The consent page.
	Consent                                     bool
	Token, Login, Forge, Client, Dest, UserCode string
}

func render(w http.ResponseWriter, code int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	// No form-action: the device form's post redirects on to the forge.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.WriteHeader(code)
	pages.Execute(w, d)
}

func page(w http.ResponseWriter, code int, title, text string) {
	render(w, code, pageData{Title: title, Text: text})
}

func deviceForm(w http.ResponseWriter, status int, errText string) {
	render(w, status, pageData{Title: "Sign in a device", Error: errText, Form: true})
}

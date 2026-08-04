package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// Un unico admin, definido por ADMIN_EMAIL y ADMIN_PASSWORD en el entorno.
// Solo el admin crea peticiones; firmarlas sigue siendo publico.
const (
	sessionCookie = "admin_session"
	sessionTTL    = 12 * time.Hour
	loginMaxTries = 10 // por IP y por hora
)

// ponytail: sesiones en memoria. Un reinicio desloguea al admin, que es
// aceptable para un solo operador. Mover a Postgres si hay mas de una instancia.
var (
	sessMu   sync.Mutex
	sessions = map[string]time.Time{} // token -> vencimiento
)

func newSession() string {
	token := randomHex(32)
	sessMu.Lock()
	defer sessMu.Unlock()
	for t, exp := range sessions {
		if time.Now().After(exp) {
			delete(sessions, t) // limpieza oportunista, sin goroutine de fondo
		}
	}
	sessions[token] = time.Now().Add(sessionTTL)
	return token
}

func validSession(token string) bool {
	sessMu.Lock()
	defer sessMu.Unlock()
	exp, ok := sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(sessions, token)
		return false
	}
	return true
}

func dropSession(token string) {
	sessMu.Lock()
	defer sessMu.Unlock()
	delete(sessions, token)
}

func adminConfigured() bool {
	return os.Getenv("ADMIN_EMAIL") != "" && os.Getenv("ADMIN_PASSWORD") != ""
}

// checkCredentials compara en tiempo constante y siempre evalua ambos campos,
// para no filtrar por timing si el email existe o no.
func checkCredentials(email, password string) bool {
	wantEmail := normalizeEmail(os.Getenv("ADMIN_EMAIL"))
	wantPass := os.Getenv("ADMIN_PASSWORD")
	if wantEmail == "" || wantPass == "" {
		return false
	}
	// Se hashea antes de comparar para que la longitud del secreto no se
	// deduzca del tiempo de comparacion.
	okEmail := constantEq(normalizeEmail(email), wantEmail)
	okPass := constantEq(password, wantPass)
	return okEmail && okPass
}

func constantEq(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func isAdmin(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && validSession(c.Value)
}

// csrfHeader es la defensa contra CSRF. Un form multipart cross-site es un
// "simple request" para el navegador: sale sin preflight y, con SameSite=None,
// se lleva la cookie de sesion puesta. Exigir un header propio obliga al
// preflight, y ahi CORS lo frena porque el Origin no coincide con WEB_ORIGIN.
// El valor no importa: lo que protege es que el header exista.
const csrfHeader = "X-Requested-With"

// requireAdmin envuelve los handlers que solo el admin puede ejecutar.
func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r) {
			fail(w, http.StatusUnauthorized, "necesitás iniciar sesión como administrador")
			return
		}
		// Solo en los que modifican. Un GET no cambia nada, y ademas la descarga
		// del PDF es una navegacion del navegador (<a download>), que no puede
		// mandar encabezados propios. Un GET cross-site tampoco filtra nada:
		// el atacante dispara el pedido pero CORS le impide leer la respuesta.
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) == "" {
			fail(w, http.StatusForbidden, "falta el encabezado "+csrfHeader)
			return
		}
		next(w, r)
	}
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	secure := r.TLS != nil || os.Getenv("COOKIE_SECURE") == "1"
	// Lax alcanza cuando front y API comparten dominio. Si estan separados
	// (dos apps de Heroku), el navegador exige None y None exige Secure.
	sameSite := http.SameSiteLaxMode
	if os.Getenv("COOKIE_CROSS_SITE") == "1" {
		sameSite = http.SameSiteNoneMode
		secure = true
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true, // fuera del alcance de cualquier JS inyectado
		Secure:   secure,
		SameSite: sameSite,
	})
}

func adminLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "json invalido")
		return
	}
	if !adminConfigured() {
		log.Print("intento de login sin ADMIN_EMAIL/ADMIN_PASSWORD configurados")
		fail(w, http.StatusServiceUnavailable, "no hay administrador configurado")
		return
	}
	// El rate limit va antes de verificar: sin esto la contraseña del .env
	// se saca por fuerza bruta.
	if rateLimited("login:"+clientIP(r), loginMaxTries) {
		fail(w, http.StatusTooManyRequests, "demasiados intentos, esperá un rato")
		return
	}
	if !checkCredentials(in.Email, in.Password) {
		// Mismo mensaje para email y contraseña: no confirmamos cual fallo.
		fail(w, http.StatusUnauthorized, "email o contraseña incorrectos")
		return
	}
	setSessionCookie(w, r, newSession(), int(sessionTTL.Seconds()))
	w.WriteHeader(http.StatusNoContent)
}

func adminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		dropSession(c.Value)
	}
	setSessionCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// adminMe le dice al front si mostrar o no la interfaz de creacion.
// Es solo cosmetico: el permiso real lo aplica requireAdmin en el servidor.
func adminMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"admin":      isAdmin(r),
		"configured": adminConfigured(),
	})
}

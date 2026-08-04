package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// ponytail: rate limit en memoria, alcanza para un solo proceso.
// Si esto escala a mas de una instancia, mover el contador a Postgres o Redis.
var (
	rlMu   sync.Mutex
	rlHits = map[string][]time.Time{}
)

const (
	rlWindow    = time.Hour
	rlMaxPerIP  = 20 // pedidos de OTP por IP y por hora
	maxDrawing  = 200_000
	maxComment  = 500
	maxNameSize = 120
	maxAddress  = 200
	maxLocality = 120
)

func rateLimited(key string, max int) bool {
	rlMu.Lock()
	defer rlMu.Unlock()
	now := time.Now()
	kept := rlHits[key][:0]
	for _, t := range rlHits[key] {
		if now.Sub(t) < rlWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		rlHits[key] = kept
		return true
	}
	rlHits[key] = append(kept, now)
	return false
}

func petitionIDBySlug(r *http.Request, slug string) (string, error) {
	var id string
	err := db.QueryRow(r.Context(), `select id from petitions where slug = $1`, slug).Scan(&id)
	return id, err
}

func requestOTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
		// Honeypot: el front lo dibuja invisible y ninguna persona lo completa.
		// Un bot que llena todos los campos del formulario cae solo.
		Website string `json:"website"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "json invalido")
		return
	}
	if in.Website != "" {
		// Misma respuesta que el camino feliz: si devolvieramos un error, el que
		// automatiza se daria cuenta del campo y lo dejaria vacio.
		log.Printf("honeypot: pedido de OTP descartado, ip %s", clientIP(r))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	email := normalizeEmail(in.Email)
	if !validEmail(email) {
		fail(w, http.StatusBadRequest, "email invalido")
		return
	}
	if rateLimited("otp:"+clientIP(r), rlMaxPerIP) {
		fail(w, http.StatusTooManyRequests, "demasiados pedidos, probá más tarde")
		return
	}

	id, err := petitionIDBySlug(r, r.PathValue("slug"))
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "peticion inexistente")
		return
	}
	if err != nil {
		log.Printf("otp lookup: %v", err)
		fail(w, http.StatusInternalServerError, "error interno")
		return
	}

	// Cooldown por email: evita usar la plataforma para bombardear una casilla.
	var last time.Time
	err = db.QueryRow(r.Context(),
		`select created_at from otps where petition_id = $1 and email = $2
		 order by created_at desc limit 1`, id, email).Scan(&last)
	if err == nil && time.Since(last) < otpCooldown {
		fail(w, http.StatusTooManyRequests, "esperá un minuto antes de pedir otro código")
		return
	}

	// Los codigos vencidos ya no sirven para nada: no los conservamos.
	db.Exec(r.Context(), `delete from otps where expires_at < now()`)

	code, hash := newOTP()
	if _, err := db.Exec(r.Context(),
		`insert into otps (petition_id, email, code_hash, expires_at) values ($1,$2,$3,$4)`,
		id, email, hash, time.Now().Add(otpTTL)); err != nil {
		log.Printf("insert otp: %v", err)
		fail(w, http.StatusInternalServerError, "no se pudo generar el código")
		return
	}
	sendOTP(email, code)

	// Respuesta identica exista o no el email: no revelamos quien ya firmo.
	w.WriteHeader(http.StatusNoContent)
}

// mailFrom devuelve la direccion y el nombre visible del remitente. El nombre
// es lo que decide si el destinatario confia en el codigo.
func mailFrom() (addr, name string) {
	// Con Gmail el From tiene que coincidir con la cuenta autenticada o lo
	// reescribe, por eso SMTP_USER es el ultimo recurso.
	addr = cmp(os.Getenv("MAIL_FROM"), cmp(os.Getenv("SMTP_FROM"), os.Getenv("SMTP_USER")))
	name = cmp(os.Getenv("MAIL_FROM_NAME"), cmp(os.Getenv("SMTP_FROM_NAME"), "Peticiones"))
	return addr, name
}

// logMailConfig deja en el arranque por donde va a salir el mail. Sin esto,
// "no llega el codigo" obliga a adivinar entre falta de config, credenciales
// mal o puertos bloqueados.
func logMailConfig() {
	from, name := mailFrom()
	switch {
	case os.Getenv("BREVO_API_KEY") != "":
		log.Printf("mail: Brevo (HTTPS), from %s <%s>", name, from)
	case os.Getenv("RESEND_API_KEY") != "":
		log.Printf("mail: Resend (HTTPS), from %s <%s>", name, from)
	case os.Getenv("SMTP_HOST") != "":
		log.Printf("mail: SMTP %s:%s, from %s <%s>",
			os.Getenv("SMTP_HOST"), cmp(os.Getenv("SMTP_PORT"), "587"), name, from)
	default:
		log.Print("mail: sin configurar, los códigos OTP salen por consola")
	}
}

func otpSubject(code string) string { return code + " es tu código para firmar" }

func otpBody(code string) string {
	return "Tu código para firmar es " + code +
		"\n\nVence en 10 minutos. Si no lo pediste, ignorá este mensaje.\n"
}

// sendOTP elige por donde sale el mail. La API HTTP va primero porque muchos
// hostings (Render en su plan gratuito, entre otros) bloquean los puertos SMTP
// de salida: ahi el unico camino que queda abierto es HTTPS.
func sendOTP(email, code string) {
	switch {
	case os.Getenv("BREVO_API_KEY") != "":
		go sendViaBrevo(email, code)
	case os.Getenv("RESEND_API_KEY") != "":
		go sendViaResend(email, code)
	case os.Getenv("SMTP_HOST") != "":
		go sendViaSMTP(email, code)
	default:
		log.Printf("[dev] sin proveedor de mail configurado, OTP para %s: %s", email, code)
	}
}

// postJSON hace el pedido y loguea el cuerpo si el proveedor lo rechaza en el
// momento. Ojo: un 2xx solo confirma que el pedido entro en la cola. Brevo, por
// ejemplo, devuelve 201 y recien despues descarta el mensaje si el remitente no
// esta verificado. La entrega real se mira en el panel del proveedor.
func postJSON(url string, headers map[string]string, payload any, email, proveedor string) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("envio de OTP a %s fallido al armar el cuerpo: %v", email, err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("envio de OTP a %s fallido al armar el pedido: %v", email, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		log.Printf("envio de OTP a %s fallido: %v", email, err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		log.Printf("OTP a %s rechazado por %s (%d): %s", email, proveedor, res.StatusCode, raw)
		return
	}
	// "encolado" y no "enviado": un 2xx acá solo dice que el proveedor acepto
	// el pedido. El rechazo real (remitente sin verificar, dominio sin
	// autenticar, cuota) llega despues y por otro canal. Decir "enviado" hace
	// buscar el problema en el lado equivocado.
	log.Printf("OTP a %s encolado en %s; la entrega se confirma en el panel del proveedor",
		email, proveedor)
}

// sendViaBrevo va por 443, asi que no la toca el bloqueo de puertos SMTP.
// MAIL_FROM tiene que ser un remitente verificado en Brevo, o de un dominio
// autenticado ahi. Si no lo es, la API igual responde 201 y descarta el mensaje
// despues: no hay forma de enterarse desde el codigo.
func sendViaBrevo(email, code string) {
	from, name := mailFrom()
	postJSON("https://api.brevo.com/v3/smtp/email",
		map[string]string{"api-key": os.Getenv("BREVO_API_KEY"), "accept": "application/json"},
		map[string]any{
			"sender":      map[string]string{"name": name, "email": from},
			"to":          []map[string]string{{"email": email}},
			"subject":     otpSubject(code),
			"textContent": otpBody(code),
		}, email, "Brevo")
}

// sendViaResend necesita un dominio propio verificado: sin eso solo entrega a
// la casilla con la que se creo la cuenta. Mejor entregabilidad que Brevo sin
// dominio, porque el SPF y el DKIM quedan alineados.
func sendViaResend(email, code string) {
	from, name := mailFrom()
	postJSON("https://api.resend.com/emails",
		map[string]string{"Authorization": "Bearer " + os.Getenv("RESEND_API_KEY")},
		map[string]any{
			"from":    fmt.Sprintf("%s <%s>", name, from),
			"to":      []string{email},
			"subject": otpSubject(code),
			"text":    otpBody(code),
		}, email, "Resend")
}

func sendViaSMTP(email, code string) {
	host := os.Getenv("SMTP_HOST")
	from, rawName := mailFrom()
	// QEncoding para que los acentos no lleguen rotos al header.
	name := mime.QEncoding.Encode("utf-8", rawName)
	// Date y Message-ID son obligatorios en RFC 5322. Sin ellos el mensaje
	// puntua como spam aunque el servidor de salida lo acepte.
	domain := from
	if _, d, ok := strings.Cut(from, "@"); ok {
		domain = d
	}
	msg := fmt.Appendf(nil,
		"From: %s <%s>\r\nTo: %s\r\n"+
			"Subject: %s\r\n"+
			"Date: %s\r\nMessage-ID: <%s@%s>\r\n"+
			"Auto-Submitted: auto-generated\r\n"+
			"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=\"utf-8\"\r\n\r\n"+
			"%s",
		name, from, email,
		mime.QEncoding.Encode("utf-8", otpSubject(code)),
		time.Now().Format(time.RFC1123Z), randomHex(16), domain,
		otpBody(code))

	auth := smtp.PlainAuth("", os.Getenv("SMTP_USER"), os.Getenv("SMTP_PASS"), host)
	addr := host + ":" + cmp(os.Getenv("SMTP_PORT"), "587")
	if err := smtp.SendMail(addr, auth, from, []string{email}, msg); err != nil {
		log.Printf("envio de OTP a %s fallido: %v", email, err)
		log.Print("si es un timeout, el hosting bloquea los puertos SMTP de salida: usá RESEND_API_KEY")
		return
	}
	// Log explicito: el silencio no puede significar a la vez "salio bien"
	// y "ni siquiera se intento".
	// SMTP si entrega de verdad en esta llamada: el servidor acepto el mensaje
	// en la sesion. Aun asi puede rebotar despues, pero el salto es real.
	log.Printf("OTP a %s aceptado por %s", email, addr)
}

func signPetition(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Code     string `json:"code"`
		Name     string `json:"name"`
		DNI      string `json:"dni"`
		Address  string `json:"address"`
		Locality string `json:"locality"`
		Phone    string `json:"phone"`
		Comment  string `json:"comment"`
		Drawing  string `json:"drawing"`
		Hash     string `json:"content_hash"`
		Website  string `json:"website"` // honeypot, ver requestOTP
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDrawing+8192)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "json invalido")
		return
	}
	if in.Website != "" {
		// Acá sí conviene un error: sin OTP válido no iba a firmar igual, y el
		// mensaje genérico no delata cuál campo lo dejó afuera.
		log.Printf("honeypot: intento de firma descartado, ip %s", clientIP(r))
		fail(w, http.StatusBadRequest, "no se pudo registrar la firma")
		return
	}
	email := normalizeEmail(in.Email)
	name := strings.TrimSpace(in.Name)
	// DNI y celular se guardan normalizados: el mismo documento escrito con o
	// sin puntos tiene que ser el mismo dato para quien despues lea el padron.
	dni := normalizeDNI(in.DNI)
	phone := normalizePhone(in.Phone)
	address := strings.TrimSpace(in.Address)
	locality := strings.TrimSpace(in.Locality)
	switch {
	case !validEmail(email):
		fail(w, http.StatusBadRequest, "email invalido")
		return
	case name == "" || len(name) > maxNameSize:
		fail(w, http.StatusBadRequest, "nombre requerido")
		return
	case !validDNI(dni):
		fail(w, http.StatusBadRequest, "DNI invalido: son 7 u 8 dígitos")
		return
	case address == "" || len(address) > maxAddress:
		fail(w, http.StatusBadRequest, "domicilio requerido (máx. 200 caracteres)")
		return
	case locality == "" || len(locality) > maxLocality:
		fail(w, http.StatusBadRequest, "localidad requerida (máx. 120 caracteres)")
		return
	case !validPhone(phone):
		fail(w, http.StatusBadRequest, "celular invalido: entre 8 y 15 dígitos")
		return
	case len(in.Comment) > maxComment:
		fail(w, http.StatusBadRequest, "comentario demasiado largo")
		return
	case in.Drawing == "":
		fail(w, http.StatusBadRequest, "dibujá tu firma antes de continuar")
		return
	// decodeDrawing no solo mira el prefijo: decodifica el PNG y comprueba que
	// tenga dimensiones. Un data URL bien formado con basura adentro romperia
	// el armado del PDF mucho despues, cuando ya no se sabe de donde salio.
	// ponytail: no se detecta un lienzo en blanco. Habria que analizar los
	// pixeles y solo frena a quien arma el pedido a mano, que ya paso el OTP.
	case decodeDrawing(&in.Drawing) == nil:
		fail(w, http.StatusBadRequest, "la firma dibujada no es una imagen válida")
		return
	}

	var id, currentHash string
	err := db.QueryRow(r.Context(),
		`select id, content_hash from petitions where slug = $1`, r.PathValue("slug")).Scan(&id, &currentHash)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "peticion inexistente")
		return
	}
	if err != nil {
		log.Printf("sign lookup: %v", err)
		fail(w, http.StatusInternalServerError, "error interno")
		return
	}
	// El firmante declara que hash vio. Si no coincide, el documento cambio
	// entre que lo leyo y que firmo: se aborta, nunca se firma a ciegas.
	if in.Hash != currentHash {
		fail(w, http.StatusConflict, "el documento cambió, recargá la página antes de firmar")
		return
	}

	var rec otpRecord
	var otpID int64
	err = db.QueryRow(r.Context(),
		`select id, code_hash, attempts, expires_at from otps
		  where petition_id = $1 and email = $2 order by created_at desc limit 1`,
		id, email).Scan(&otpID, &rec.CodeHash, &rec.Attempts, &rec.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusBadRequest, "pedí un código primero")
		return
	}
	if err != nil {
		log.Printf("otp read: %v", err)
		fail(w, http.StatusInternalServerError, "error interno")
		return
	}

	switch checkOTP(rec, in.Code, time.Now()) {
	case otpTooManyAttempts:
		fail(w, http.StatusTooManyRequests, "código bloqueado, pedí uno nuevo")
		return
	case otpExpired:
		fail(w, http.StatusBadRequest, "código vencido, pedí uno nuevo")
		return
	case otpWrong:
		db.Exec(r.Context(), `update otps set attempts = attempts + 1 where id = $1`, otpID)
		fail(w, http.StatusBadRequest, "código incorrecto")
		return
	}

	var comment *string
	if c := strings.TrimSpace(in.Comment); c != "" {
		comment = &c
	}
	drawing := &in.Drawing // obligatorio, ya validado arriba

	tag, err := db.Exec(r.Context(),
		`insert into signatures
		   (petition_id, name, email, dni, address, locality, phone,
		    comment, drawing, content_hash, ip, user_agent)
		 values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 on conflict do nothing`,
		id, name, email, dni, address, locality, phone,
		comment, drawing, currentHash, clientIP(r), r.UserAgent())
	if err != nil {
		log.Printf("insert signature: %v", err)
		fail(w, http.StatusInternalServerError, "no se pudo registrar la firma")
		return
	}
	// El OTP se quema si o si, haya firmado o ya estuviera firmado.
	db.Exec(r.Context(), `delete from otps where petition_id = $1 and email = $2`, id, email)

	if tag.RowsAffected() == 0 {
		// Un solo mensaje para las dos causas: decir cual choco confirmaria que
		// ese DNI ya firmo, y el padron no es publico.
		fail(w, http.StatusConflict, "ya hay una firma registrada con ese email o ese DNI en esta petición")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"content_hash": currentHash})
}

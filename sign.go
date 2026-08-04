package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "json invalido")
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

// sendOTP usa SMTP si esta configurado; en desarrollo imprime el codigo.
func sendOTP(email, code string) {
	host := os.Getenv("SMTP_HOST")
	if host == "" {
		log.Printf("[dev] sin SMTP_HOST, OTP para %s: %s", email, code)
		return
	}
	user := os.Getenv("SMTP_USER")
	// Gmail reescribe el From si no coincide con la cuenta autenticada,
	// asi que por defecto usamos esa misma direccion.
	from := cmp(os.Getenv("SMTP_FROM"), user)
	// El nombre visible es lo que decide si el destinatario confia en el codigo.
	// QEncoding para que los acentos no lleguen rotos al header.
	name := mime.QEncoding.Encode("utf-8", cmp(os.Getenv("SMTP_FROM_NAME"), "Peticiones"))
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
			"Tu código para firmar es %s\r\n\r\n"+
			"Vence en 10 minutos. Si no lo pediste, ignorá este mensaje.\r\n",
		name, from, email,
		mime.QEncoding.Encode("utf-8", code+" es tu código para firmar"),
		time.Now().Format(time.RFC1123Z), randomHex(16), domain,
		code)
	auth := smtp.PlainAuth("", user, os.Getenv("SMTP_PASS"), host)
	addr := host + ":" + cmp(os.Getenv("SMTP_PORT"), "587")
	go func() {
		if err := smtp.SendMail(addr, auth, from, []string{email}, msg); err != nil {
			log.Printf("envio de OTP a %s fallido: %v", email, err)
			return
		}
		// Log explicito: el silencio no puede significar a la vez "salio bien"
		// y "ni siquiera se intento".
		log.Printf("OTP enviado a %s via %s", email, addr)
	}()
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
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDrawing+8192)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "json invalido")
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
	case in.Drawing != "" && !strings.HasPrefix(in.Drawing, "data:image/png;base64,"):
		fail(w, http.StatusBadRequest, "firma dibujada invalida")
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

	var comment, drawing *string
	if c := strings.TrimSpace(in.Comment); c != "" {
		comment = &c
	}
	if in.Drawing != "" {
		drawing = &in.Drawing
	}

	tag, err := db.Exec(r.Context(),
		`insert into signatures
		   (petition_id, name, email, dni, address, locality, phone,
		    comment, drawing, content_hash, ip, user_agent)
		 values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 on conflict (petition_id, email) do nothing`,
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
		fail(w, http.StatusConflict, "ese email ya firmó esta petición")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"content_hash": currentHash})
}

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// contentHash congela la version exacta que el firmante acepto.
// Texto y PDF se prefijan distinto para que nunca colisionen entre si.
func contentHash(title string, body []byte, isPDF bool) string {
	kind := "text"
	if isPDF {
		kind = "pdf"
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n", kind, title)
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		// separa acentos de la letra base y descarta la marca
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case strings.ContainsRune("áàâä", r):
			b.WriteRune('a')
		case strings.ContainsRune("éèêë", r):
			b.WriteRune('e')
		case strings.ContainsRune("íìîï", r):
			b.WriteRune('i')
		case strings.ContainsRune("óòôö", r):
			b.WriteRune('o')
		case strings.ContainsRune("úùûü", r):
			b.WriteRune('u')
		case r == 'ñ':
			b.WriteRune('n')
		case unicode.IsSpace(r) || r == '-' || r == '_':
			b.WriteRune('-')
		}
	}
	s := strings.Trim(nonSlug.ReplaceAllString(b.String(), "-"), "-")
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	if s == "" {
		s = "peticion"
	}
	return s + "-" + randomHex(3)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // rand.Read solo falla si el SO no tiene entropia
	}
	return hex.EncodeToString(b)
}

// newOTP devuelve el codigo en claro (se envia por mail) y su hash (se guarda).
func newOTP() (code, hash string) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		panic(err)
	}
	code = fmt.Sprintf("%06d", n.Int64())
	return code, hashOTP(code)
}

func hashOTP(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

const (
	otpTTL         = 10 * time.Minute
	otpMaxAttempts = 5
	otpCooldown    = 60 * time.Second
)

type otpRecord struct {
	CodeHash  string
	Attempts  int
	ExpiresAt time.Time
}

type otpResult int

const (
	otpOK otpResult = iota
	otpExpired
	otpTooManyAttempts
	otpWrong
)

// checkOTP no toca la base: decide sobre el registro ya leido.
// Comparacion en tiempo constante para no filtrar el codigo por timing.
func checkOTP(rec otpRecord, code string, now time.Time) otpResult {
	if rec.Attempts >= otpMaxAttempts {
		return otpTooManyAttempts
	}
	if now.After(rec.ExpiresAt) {
		return otpExpired
	}
	if subtle.ConstantTimeCompare([]byte(rec.CodeHash), []byte(hashOTP(code))) != 1 {
		return otpWrong
	}
	return otpOK
}

func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s.]+\.[^@\s]+$`)

func validEmail(s string) bool { return len(s) <= 254 && emailRe.MatchString(s) }

// normalizeDNI deja solo los digitos: la gente lo escribe con puntos, con
// espacios o sin nada, y las tres formas son el mismo documento.
func normalizeDNI(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validDNI acepta 7 u 8 digitos. Se valida sobre el DNI ya normalizado.
// No se verifica contra el RENAPER: esto es firma electronica con evidencia,
// no identidad probada.
func validDNI(s string) bool {
	return len(s) >= 7 && len(s) <= 8
}

// formatDNI vuelve a poner los puntos de miles para mostrarlo: se guarda en
// crudo y se presenta como lo lee cualquiera, 12.345.678.
func formatDNI(s string) string {
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	head := len(s) % 3
	if head == 0 {
		head = 3
	}
	b.WriteString(s[:head])
	for i := head; i < len(s); i += 3 {
		b.WriteByte('.')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// normalizePhone conserva el + inicial (prefijo internacional) y los digitos.
// Todo lo demas (espacios, guiones, parentesis) es decoracion del que escribe.
func normalizePhone(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validPhone pide entre 8 y 15 digitos: 8 cubre un fijo local sin
// caracteristica y 15 es el maximo de la E.164.
func validPhone(s string) bool {
	digits := len(strings.TrimPrefix(s, "+"))
	return digits >= 8 && digits <= 15
}

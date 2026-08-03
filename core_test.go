package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestContentHashDistingueVersionYTipo(t *testing.T) {
	a := contentHash("Basta de pozos", []byte("cuerpo"), false)
	if a != contentHash("Basta de pozos", []byte("cuerpo"), false) {
		t.Fatal("hash no determinista")
	}
	if a == contentHash("Basta de pozos", []byte("cuerpo "), false) {
		t.Fatal("un cambio de un byte en el cuerpo debe cambiar el hash")
	}
	if a == contentHash("Basta de pozo", []byte("cuerpo"), false) {
		t.Fatal("un cambio en el titulo debe cambiar el hash")
	}
	if a == contentHash("Basta de pozos", []byte("cuerpo"), true) {
		t.Fatal("texto y pdf con mismos bytes no pueden compartir hash")
	}
}

func TestSlugify(t *testing.T) {
	s := slugify("¡Señal más Peatonal en Ruta 40!")
	if !strings.HasPrefix(s, "senal-mas-peatonal-en-ruta-40-") {
		t.Fatalf("slug inesperado: %s", s)
	}
	a, b := slugify("Igual"), slugify("Igual")
	if a == b {
		t.Fatal("dos peticiones con el mismo titulo deben recibir slugs distintos")
	}
	if s := slugify("###"); !strings.HasPrefix(s, "peticion-") {
		t.Fatalf("titulo sin caracteres validos debe caer al default: %s", s)
	}
}

func TestCheckOTP(t *testing.T) {
	now := time.Now()
	code, hash := newOTP()
	if len(code) != 6 {
		t.Fatalf("codigo debe ser de 6 digitos, es %q", code)
	}
	fresh := otpRecord{CodeHash: hash, ExpiresAt: now.Add(otpTTL)}

	if got := checkOTP(fresh, code, now); got != otpOK {
		t.Fatalf("codigo correcto rechazado: %v", got)
	}
	if got := checkOTP(fresh, "000000", now); got == otpOK && "000000" != code {
		t.Fatal("codigo incorrecto aceptado")
	}
	expired := fresh
	expired.ExpiresAt = now.Add(-time.Second)
	if got := checkOTP(expired, code, now); got != otpExpired {
		t.Fatalf("codigo vencido aceptado: %v", got)
	}
	burned := fresh
	burned.Attempts = otpMaxAttempts
	if got := checkOTP(burned, code, now); got != otpTooManyAttempts {
		t.Fatalf("intentos agotados no bloquearon: %v", got)
	}
}

func TestClientIP(t *testing.T) {
	t.Setenv("TRUST_PROXY", "")
	for addr, want := range map[string]string{
		"192.168.1.5:54321": "192.168.1.5",
		"[::1]:54321":       "::1",
		"[2803:9800::1]:80": "2803:9800::1",
	} {
		r := &http.Request{RemoteAddr: addr}
		if got := clientIP(r); got != want {
			t.Errorf("clientIP(%q) = %q, esperaba %q", addr, got, want)
		}
	}

	// Sin TRUST_PROXY el header no se cree: cualquiera puede falsificarlo.
	spoofed := &http.Request{RemoteAddr: "10.0.0.1:9999", Header: http.Header{}}
	spoofed.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := clientIP(spoofed); got != "10.0.0.1" {
		t.Errorf("X-Forwarded-For aceptado sin TRUST_PROXY: %q", got)
	}
	t.Setenv("TRUST_PROXY", "1")
	if got := clientIP(spoofed); got != "1.2.3.4" {
		t.Errorf("con TRUST_PROXY debe usarse el header: %q", got)
	}
}

func TestValidEmail(t *testing.T) {
	ok := []string{"a@b.co", "walter.tapia+x@gmail.com"}
	bad := []string{"", "a@b", "a b@c.com", "@b.com", "a@@b.com", strings.Repeat("a", 250) + "@b.com"}
	for _, e := range ok {
		if !validEmail(e) {
			t.Errorf("deberia ser valido: %q", e)
		}
	}
	for _, e := range bad {
		if validEmail(e) {
			t.Errorf("deberia ser invalido: %q", e)
		}
	}
}

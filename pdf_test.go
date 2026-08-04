package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"
)

func pngDataURL(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(w/2, h/2, color.Black)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestDecodeDrawing(t *testing.T) {
	ok := pngDataURL(t, 600, 240)
	got := decodeDrawing(&ok)
	if got == nil {
		t.Fatal("un PNG valido en data URL deberia decodificarse")
	}
	if got.w != 600 || got.h != 240 {
		t.Fatalf("dimensiones mal leidas: %dx%d", got.w, got.h)
	}

	// Nada de esto debe romper el armado del PDF: se ignora y se sigue.
	jpeg := "data:image/jpeg;base64,/9j/4AAQ"
	naked := "no soy una data url"
	badB64 := "data:image/png;base64,!!!no-es-base64!!!"
	notPNG := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("esto no es un png"))
	for name, v := range map[string]*string{
		"nil": nil, "jpeg": &jpeg, "sin prefijo": &naked,
		"base64 rota": &badB64, "png falso": &notPNG,
	} {
		if decodeDrawing(v) != nil {
			t.Errorf("%s deberia descartarse", name)
		}
	}
}

func samplePetition() petition {
	return petition{
		Slug:        "senal-peatonal-ruta-40-a1b2c3",
		Title:       "Señal peatonal en Ruta 40",
		ContentHash: strings.Repeat("a", 64),
		CreatedAt:   time.Now(),
	}
}

func makeSigners(t *testing.T, n int, withDrawing bool) []signer {
	t.Helper()
	out := make([]signer, n)
	for i := range out {
		dni := fmt.Sprintf("%08d", 20000000+i)
		address := fmt.Sprintf("Av. Siempreviva %d", 100+i)
		locality := "San Carlos de Bariloche"
		phone := fmt.Sprintf("+549294%06d", i)
		out[i] = signer{
			ID:          int64(i + 1),
			Name:        fmt.Sprintf("Firmante Número %d", i+1),
			DNI:         &dni,
			Address:     &address,
			Locality:    &locality,
			Phone:       &phone,
			ContentHash: strings.Repeat("a", 64),
			CreatedAt:   time.Now(),
		}
		if withDrawing {
			d := pngDataURL(t, 600, 240)
			out[i].Drawing = &d
		}
	}
	return out
}

// countPages cuenta objetos /Type /Page (no /Pages) en el PDF crudo.
func countPages(b []byte) int {
	return bytes.Count(b, []byte("/Type /Page\n")) + bytes.Count(b, []byte("/Type /Page/")) +
		bytes.Count(b, []byte("/Type /Page "))
}

func TestBuildSignaturesPDFPaginaLlena(t *testing.T) {
	p := samplePetition()
	// 10 firmas entran justo en una hoja; la 11 obliga a abrir la segunda.
	for _, tc := range []struct{ signers, wantMin int }{{10, 1}, {11, 2}, {21, 3}} {
		out, err := buildSignaturesPDF(p, "", makeSigners(t, tc.signers, true), false, true)
		if err != nil {
			t.Fatalf("%d firmas: %v", tc.signers, err)
		}
		if !bytes.HasPrefix(out, []byte("%PDF-")) {
			t.Fatalf("%d firmas: la salida no es un PDF", tc.signers)
		}
		if got := countPages(out); got < tc.wantMin {
			t.Errorf("%d firmas: %d hojas, esperaba al menos %d", tc.signers, got, tc.wantMin)
		}
	}
}

func TestBuildSignaturesPDFSinTrazoNiFirmas(t *testing.T) {
	p := samplePetition()

	// Firmas sin dibujo: se emite igual, con la leyenda en lugar del trazo.
	out, err := buildSignaturesPDF(p, "", makeSigners(t, 3, false), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Fatal("sin trazo la salida deberia seguir siendo un PDF")
	}

	// Peticion sin ninguna firma: tiene que dar un PDF valido, no un error.
	out, err = buildSignaturesPDF(p, "", nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if countPages(out) < 1 {
		t.Fatal("sin firmas igual tiene que haber una hoja")
	}
}

func TestBuildSignaturesPDFConCuerpoLargo(t *testing.T) {
	p := samplePetition()
	body := strings.Repeat("Pedimos una senda peatonal con acentos: ñ á é í ó ú ü. ", 200)
	out, err := buildSignaturesPDF(p, body, makeSigners(t, 2, true), true, true)
	if err != nil {
		t.Fatal(err)
	}
	// El cuerpo desborda una hoja: sin salto automatico se perderia texto.
	if got := countPages(out); got < 3 {
		t.Fatalf("cuerpo largo + firmas deberia dar 3+ hojas, dio %d", got)
	}
}

func TestClip(t *testing.T) {
	if got := clip("corto", 10); got != "corto" {
		t.Errorf("no deberia recortar: %q", got)
	}
	// Recorte por runas: con bytes, un nombre con acentos quedaria partido.
	long := "Ñoño Ñoño Ñoño Ñoño"
	got := clip(long, 10)
	if r := []rune(got); len(r) != 10 {
		t.Errorf("clip dio %d runas, esperaba 10: %q", len(r), got)
	}
}

func TestDownloadName(t *testing.T) {
	got := downloadName(samplePetition())
	if !strings.HasPrefix(got, "senal-peatonal-ruta-40-firmado-") || !strings.HasSuffix(got, ".pdf") {
		t.Fatalf("nombre inesperado: %s", got)
	}
	if strings.Contains(got, "a1b2c3") {
		t.Errorf("el sufijo aleatorio del slug no deberia aparecer: %s", got)
	}
}

func TestSignerLinesDatosPersonales(t *testing.T) {
	s := makeSigners(t, 1, false)[0]

	// Descarga del admin: DNI con puntos, celular, domicilio y localidad.
	got := strings.Join(signerLines(s, true), "\n")
	for _, want := range []string{"DNI 20.000.000", "Cel. +5492940000", "Av. Siempreviva 100", "San Carlos de Bariloche", "Firmado el "} {
		if !strings.Contains(got, want) {
			t.Errorf("falta %q en:\n%s", want, got)
		}
	}

	// Descarga publica: sobrevive la localidad, nada mas.
	pub := strings.Join(signerLines(s, false), "\n")
	for _, leaked := range []string{"DNI", "20.000.000", "Cel.", "Av. Siempreviva"} {
		if strings.Contains(pub, leaked) {
			t.Errorf("%q no puede salir en la descarga publica:\n%s", leaked, pub)
		}
	}
	if !strings.Contains(pub, "San Carlos de Bariloche") || !strings.Contains(pub, "Firmado el ") {
		t.Errorf("la localidad y la fecha si son publicas:\n%s", pub)
	}
}

// Una firma vieja sin estos datos no puede romper el armado ni dejar lineas
// colgadas con un separador y nada al lado.
func TestSignerLinesFirmaVieja(t *testing.T) {
	old := signer{ID: 1, Name: "Firmante Sin Datos", CreatedAt: time.Now()}
	lines := signerLines(old, true)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "Firmado el ") {
		t.Fatalf("sin datos solo deberia quedar la fecha: %q", lines)
	}
}

func TestBuildSignaturesPDFConDatosPersonales(t *testing.T) {
	p := samplePetition()
	// 10 firmas con las lineas extra tienen que seguir entrando en una hoja:
	// si el alto de celda se pasa, aparece una segunda hoja a medio llenar.
	out, err := buildSignaturesPDF(p, "", makeSigners(t, 10, true), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := countPages(out); got != 1 {
		t.Errorf("10 firmas con datos personales dieron %d hojas, esperaba 1", got)
	}
}

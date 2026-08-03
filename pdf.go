package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/png"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
)

// Grilla de firmas: 2 columnas x 5 filas = 10 por hoja.
const (
	sigCols, sigRows = 2, 5
	pageMargin       = 15.0
	colGap           = 10.0
	cellH            = 47.0
	drawBoxH         = 26.0 // alto reservado al trazo dentro de la celda
	gridTop          = pageMargin + 26
)

// buildSignaturesPDF arma las hojas de firmas. Si withBody, antepone el texto
// de la peticion; para peticiones PDF se omite porque el original ya lo trae.
func buildSignaturesPDF(p petition, body string, signers []signer, withBody bool) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(pageMargin, pageMargin, pageMargin)
	pdf.SetAutoPageBreak(false, pageMargin)
	// fpdf usa fuentes core en cp1252: el traductor mapea los acentos.
	tr := pdf.UnicodeTranslatorFromDescriptor("cp1252")

	pageW, _ := pdf.GetPageSize()
	usableW := pageW - 2*pageMargin
	colW := (usableW - colGap*(sigCols-1)) / sigCols

	if withBody {
		pdf.AddPage()
		pdf.SetFont("Arial", "B", 18)
		pdf.MultiCell(0, 8, tr(p.Title), "", "L", false)
		pdf.Ln(3)
		pdf.SetFont("Arial", "", 9)
		pdf.SetTextColor(110, 110, 110)
		pdf.MultiCell(0, 5, tr(fmt.Sprintf("Publicada el %s", p.CreatedAt.Format("02/01/2006 15:04"))), "", "L", false)
		pdf.Ln(4)
		pdf.SetTextColor(0, 0, 0)
		pdf.SetFont("Arial", "", 11)
		// AutoPageBreak activo solo para el cuerpo, que si puede ser largo.
		pdf.SetAutoPageBreak(true, pageMargin)
		pdf.MultiCell(0, 6, tr(body), "", "L", false)
		pdf.SetAutoPageBreak(false, pageMargin)
	}

	total := len(signers)
	for i, s := range signers {
		slot := i % (sigCols * sigRows)
		if slot == 0 {
			pdf.AddPage()
			signaturesHeader(pdf, tr, p, total)
		}
		x := pageMargin + float64(slot%sigCols)*(colW+colGap)
		y := gridTop + float64(slot/sigCols)*cellH
		drawSignatureCell(pdf, tr, s, x, y, colW)
	}

	if total == 0 {
		pdf.AddPage()
		signaturesHeader(pdf, tr, p, 0)
		pdf.SetFont("Arial", "I", 11)
		pdf.SetY(gridTop)
		pdf.MultiCell(0, 6, tr("Esta petición todavía no tiene firmas."), "", "L", false)
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func signaturesHeader(pdf *fpdf.Fpdf, tr func(string) string, p petition, total int) {
	pdf.SetFont("Arial", "B", 13)
	pdf.SetTextColor(0, 0, 0)
	pdf.CellFormat(0, 7, tr("Firmas de: "+p.Title), "", 1, "L", false, 0, "")
	pdf.SetFont("Arial", "", 8)
	pdf.SetTextColor(110, 110, 110)
	pdf.CellFormat(0, 4, tr(fmt.Sprintf("%d firma(s) · SHA-256 del documento: %s", total, p.ContentHash)), "", 1, "L", false, 0, "")
	pdf.SetDrawColor(200, 200, 200)
	pdf.Line(pageMargin, gridTop-6, 210-pageMargin, gridTop-6)
	pdf.SetTextColor(0, 0, 0)
}

func drawSignatureCell(pdf *fpdf.Fpdf, tr func(string) string, s signer, x, y, w float64) {
	// Trazo arriba, datos abajo: el orden de una firma en papel.
	if img := decodeDrawing(s.Drawing); img != nil {
		name := fmt.Sprintf("sig%d", s.ID)
		opt := fpdf.ImageOptions{ImageType: "PNG"}
		pdf.RegisterImageOptionsReader(name, opt, bytes.NewReader(img.data))
		// Escala preservando proporcion: un trazo estirado no parece una firma.
		scale := min(w/float64(img.w), drawBoxH/float64(img.h))
		dw, dh := float64(img.w)*scale, float64(img.h)*scale
		pdf.ImageOptions(name, x+(w-dw)/2, y+(drawBoxH-dh), dw, dh, false, opt, 0, "")
	} else {
		pdf.SetFont("Arial", "I", 8)
		pdf.SetTextColor(150, 150, 150)
		pdf.SetXY(x, y+drawBoxH-6)
		pdf.CellFormat(w, 5, tr("(firma electrónica sin trazo)"), "", 0, "C", false, 0, "")
	}

	lineY := y + drawBoxH + 2
	pdf.SetDrawColor(120, 120, 120)
	pdf.Line(x, lineY, x+w, lineY)

	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont("Arial", "B", 10)
	pdf.SetXY(x, lineY+1.5)
	pdf.CellFormat(w, 5, tr(clip(s.Name, 34)), "", 0, "L", false, 0, "")

	pdf.SetFont("Arial", "", 8)
	pdf.SetTextColor(110, 110, 110)
	pdf.SetXY(x, lineY+6.5)
	pdf.CellFormat(w, 4, tr("Firmado el "+s.CreatedAt.Local().Format("02/01/2006 15:04")), "", 0, "L", false, 0, "")
	pdf.SetTextColor(0, 0, 0)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

type drawing struct {
	data []byte
	w, h int
}

// decodeDrawing acepta solo PNG en data URL, que es lo unico que emite el
// canvas del front. Cualquier otra cosa se ignora en vez de romper el PDF.
func decodeDrawing(d *string) *drawing {
	if d == nil {
		return nil
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(*d, prefix) {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(*d, prefix))
	if err != nil {
		return nil
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || format != "png" || cfg.Width == 0 || cfg.Height == 0 {
		return nil
	}
	return &drawing{data: raw, w: cfg.Width, h: cfg.Height}
}

func downloadName(p petition) string {
	base := p.Slug
	if i := strings.LastIndex(base, "-"); i > 0 {
		base = base[:i] // saca el sufijo aleatorio del slug
	}
	return fmt.Sprintf("%s-firmado-%s.pdf", base, time.Now().Format("20060102"))
}

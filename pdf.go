package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/jackc/pgx/v5"
	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// Grilla de firmas: 2 columnas x 5 filas = 10 por hoja.
const (
	sigCols, sigRows = 2, 5
	pageMargin       = 15.0
	colGap           = 10.0
	cellH            = 47.0
	// El trazo cede alto para que entren DNI, domicilio/localidad y celular
	// debajo del nombre sin bajar de 10 firmas por hoja.
	drawBoxH = 20.0
	gridTop  = pageMargin + 26
)

// buildSignaturesPDF arma las hojas de firmas. Si withBody, antepone el texto
// de la peticion; para peticiones PDF se omite porque el original ya lo trae.
// withPersonal decide si cada celda lleva DNI, domicilio y celular: van solo
// en la descarga del admin, nunca en la publica.
func buildSignaturesPDF(p petition, body string, signers []signer, withBody, withPersonal bool) ([]byte, error) {
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
		drawSignatureCell(pdf, tr, s, x, y, colW, p.ContentHash, withPersonal)
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

func drawSignatureCell(pdf *fpdf.Fpdf, tr func(string) string, s signer, x, y, w float64, currentHash string, withPersonal bool) {
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
	dy := lineY + 6.5
	for _, line := range signerLines(s, withPersonal) {
		pdf.SetXY(x, dy)
		pdf.CellFormat(w, 4, tr(clip(line, 44)), "", 0, "L", false, 0, "")
		dy += 4
	}

	// Una firma sobre una version anterior sigue siendo valida, pero es sobre
	// otro texto: decirlo es la unica forma honesta de anexarla al documento.
	if currentHash != "" && s.ContentHash != "" && s.ContentHash != currentHash {
		pdf.SetFont("Arial", "I", 7)
		pdf.SetTextColor(170, 90, 0)
		pdf.SetXY(x, dy)
		pdf.CellFormat(w, 3.5, tr("Firmada sobre una versión anterior ("+clip(s.ContentHash, 13)+")"), "", 0, "L", false, 0, "")
	}
	pdf.SetTextColor(0, 0, 0)
}

// signerLines arma las lineas de datos bajo el nombre. Sin withPersonal solo
// sale la localidad: DNI, domicilio y celular no van en la descarga publica.
func signerLines(s signer, withPersonal bool) []string {
	var lines []string
	if withPersonal {
		var ids []string
		if d := deref(s.DNI); d != "" {
			ids = append(ids, "DNI "+formatDNI(d))
		}
		if p := deref(s.Phone); p != "" {
			ids = append(ids, "Cel. "+p)
		}
		if len(ids) > 0 {
			lines = append(lines, strings.Join(ids, " · "))
		}
	}

	var place []string
	if a := deref(s.Address); withPersonal && a != "" {
		place = append(place, a)
	}
	if l := deref(s.Locality); l != "" {
		place = append(place, l)
	}
	if len(place) > 0 {
		lines = append(lines, strings.Join(place, ", "))
	}

	return append(lines, "Firmado el "+s.CreatedAt.Local().Format("02/01/2006 15:04"))
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

// inkPDF es la tinta de las firmas en el PDF. El trazo llega con el color que
// tenia el tema de quien firmo: los que firmaron en modo oscuro guardaron
// blanco, y sobre una hoja blanca eso es una firma que no existe. Se repinta.
var inkPDF = color.NRGBA{R: 17, G: 24, B: 39, A: 255}

// inkify repinta el trazo conservando el canal alfa, asi que el suavizado de
// bordes sobrevive y la firma no queda dentada. Se aplica siempre: sobre un
// trazo que ya era oscuro no cambia nada visible.
func inkify(raw []byte) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	dst := image.NewNRGBA(b)

	// Camino rapido para el formato que devuelve png.Decode con un PNG RGBA,
	// que es lo que emite el canvas del front. Recorrer Pix directo en vez de
	// llamar At() por pixel: la llamada por interfaz, 144.000 veces por firma,
	// era casi todo el costo de armar el padron.
	if n, ok := src.(*image.NRGBA); ok {
		for i := 0; i+3 < len(n.Pix); i += 4 {
			if a := n.Pix[i+3]; a != 0 {
				dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = inkPDF.R, inkPDF.G, inkPDF.B, a
			}
		}
	} else {
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				// RGBA() devuelve alfa premultiplicado en 16 bits; >>8 lo baja a 8.
				_, _, _, a := src.At(x, y).RGBA()
				if a == 0 {
					continue // fuera del trazo: queda transparente
				}
				c := inkPDF
				c.A = uint8(a >> 8)
				dst.SetNRGBA(x, y, c)
			}
		}
	}
	// BestSpeed y no el default: el PNG se embebe en el PDF y se comprime otra
	// vez ahi, asi que apretarlo al maximo aca es tiempo tirado. Con el nivel
	// por defecto, armar un padron de miles de firmas se iba a casi un minuto.
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
	// Si el repintado falla, se usa el trazo original: una firma con el color
	// equivocado sigue siendo mejor que una celda vacia.
	if inked, err := inkify(raw); err == nil {
		raw = inked
	} else {
		log.Printf("no se pudo repintar una firma, se usa el original: %v", err)
	}
	return &drawing{data: raw, w: cfg.Width, h: cfg.Height}
}

// maxSignersInPDF acota la memoria del armado. 5000 firmas son ~500 hojas:
// mas que eso pide streaming, no un buffer.
const maxSignersInPDF = 5000

func allSigners(ctx context.Context, petitionID string) ([]signer, error) {
	rows, err := db.Query(ctx,
		`select `+signerCols+` from signatures
		  where petition_id = $1 order by id asc limit $2`, petitionID, maxSignersInPDF)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []signer{}
	for rows.Next() {
		s, err := scanSigner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func downloadPetition(w http.ResponseWriter, r *http.Request) {
	var id, body string
	var bodyPtr *string
	var original []byte
	var p petition
	err := db.QueryRow(r.Context(),
		`select p.id, p.slug, p.title, p.body, p.pdf, p.pdf_name, p.content_hash, p.created_at,
		        (select count(*) from signatures where petition_id = p.id)
		   from petitions p where p.slug = $1`, r.PathValue("slug")).
		Scan(&id, &p.Slug, &p.Title, &bodyPtr, &original, &p.PDFName, &p.ContentHash, &p.CreatedAt, &p.Signatures)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "peticion inexistente")
		return
	}
	if err != nil {
		log.Printf("download lookup: %v", err)
		fail(w, http.StatusInternalServerError, "error al leer la peticion")
		return
	}
	if bodyPtr != nil {
		body = *bodyPtr
	}

	signers, err := allSigners(r.Context(), id)
	if err != nil {
		log.Printf("download signers: %v", err)
		fail(w, http.StatusInternalServerError, "error al leer las firmas")
		return
	}

	// Peticion de texto: el cuerpo se compone acá. Peticion PDF: el original ya
	// lo trae, asi que solo se generan las hojas de firmas y se anexan.
	// El ultimo true son los datos personales: la ruta esta detras de
	// requireAdmin, asi que este PDF es el que se presenta ante quien
	// corresponda y lleva DNI, domicilio y celular.
	isPDF := original != nil
	pages, err := buildSignaturesPDF(p, body, signers, !isPDF, true)
	if err != nil {
		log.Printf("build pdf: %v", err)
		fail(w, http.StatusInternalServerError, "no se pudo generar el PDF")
		return
	}

	out := pages
	if isPDF {
		var merged bytes.Buffer
		err = api.MergeRaw([]io.ReadSeeker{
			bytes.NewReader(original),
			bytes.NewReader(pages),
		}, &merged, false, nil)
		if err != nil {
			log.Printf("merge pdf: %v", err)
			fail(w, http.StatusInternalServerError, "no se pudo anexar las firmas al PDF")
			return
		}
		out = merged.Bytes()
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.Header().Set("Content-Disposition", "attachment; filename=\""+downloadName(p)+"\"")
	w.Write(out)
}

func downloadName(p petition) string {
	base := p.Slug
	if i := strings.LastIndex(base, "-"); i > 0 {
		base = base[:i] // saca el sufijo aleatorio del slug
	}
	return fmt.Sprintf("%s-firmado-%s.pdf", base, time.Now().Format("20060102"))
}

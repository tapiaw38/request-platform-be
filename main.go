package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxPDFBytes = 5 << 20 // 5MB; el bytea de Postgres aguanta esto sin despeinarse

//go:embed schema.sql
var schemaSQL string

var db *pgxpool.Pool

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/request_platform?sslmode=disable"
	}
	var err error
	db, err = pgxpool.New(context.Background(), dsn)
	if err != nil {
		log.Fatalf("conexion a postgres: %v", err)
	}
	defer db.Close()

	// schema.sql es idempotente (create ... if not exists), asi que correrlo en
	// cada arranque hace de migracion sin release phase ni herramienta aparte.
	if _, err := db.Exec(context.Background(), schemaSQL); err != nil {
		log.Fatalf("aplicar schema: %v", err)
	}

	if !adminConfigured() {
		log.Print("aviso: sin ADMIN_EMAIL/ADMIN_PASSWORD nadie puede crear peticiones")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/admin/login", adminLogin)
	mux.HandleFunc("POST /api/admin/logout", adminLogout)
	mux.HandleFunc("GET /api/admin/me", adminMe)

	// Crear es solo del admin; leer y firmar es publico.
	mux.HandleFunc("POST /api/petitions", requireAdmin(createPetition))
	mux.HandleFunc("GET /api/petitions", listPetitions)
	mux.HandleFunc("GET /api/petitions/{slug}", getPetition)
	mux.HandleFunc("GET /api/petitions/{slug}/signers", listSigners)
	mux.HandleFunc("GET /api/petitions/{slug}/doc", getPetitionDoc)
	mux.HandleFunc("GET /api/petitions/{slug}/download", downloadPetition)
	mux.HandleFunc("POST /api/petitions/{slug}/otp", requestOTP)
	mux.HandleFunc("POST /api/petitions/{slug}/sign", signPetition)

	addr := ":" + cmp(os.Getenv("PORT"), "8080")
	log.Printf("api escuchando en %s", addr)
	log.Fatal(http.ListenAndServe(addr, cors(mux)))
}

func cmp(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// cors abre solo el origen del front de desarrollo. En produccion se sirve
// el build de Vue desde el mismo dominio y esto queda sin efecto.
func cors(next http.Handler) http.Handler {
	origin := cmp(os.Getenv("WEB_ORIGIN"), "http://localhost:5173")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		// La cookie de sesion del admin no viaja sin esto. Va de la mano con un
		// origen unico y explicito: con "*" el navegador lo rechaza.
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// clientIP confia en X-Forwarded-For solo si TRUST_PROXY esta activo,
// porque el header lo puede falsificar cualquiera.
func clientIP(r *http.Request) string {
	if os.Getenv("TRUST_PROXY") == "1" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	// SplitHostPort y no Cut: con IPv6 el RemoteAddr es "[::1]:54321".
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type petition struct {
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Body        *string   `json:"body"`
	PDFName     *string   `json:"pdf_name"`
	ContentHash string    `json:"content_hash"`
	CreatedAt   time.Time `json:"created_at"`
	Signatures  int       `json:"signatures"`
}

func createPetition(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxPDFBytes); err != nil {
		fail(w, http.StatusBadRequest, "formulario invalido")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	if title == "" || len(title) > 200 {
		fail(w, http.StatusBadRequest, "titulo requerido (max 200 caracteres)")
		return
	}

	var pdf []byte
	var pdfName string
	if f, fh, err := r.FormFile("pdf"); err == nil {
		defer f.Close()
		if fh.Size > maxPDFBytes {
			fail(w, http.StatusRequestEntityTooLarge, "el PDF supera 5MB")
			return
		}
		pdf, err = io.ReadAll(io.LimitReader(f, maxPDFBytes))
		if err != nil {
			fail(w, http.StatusBadRequest, "no se pudo leer el PDF")
			return
		}
		if !strings.HasPrefix(string(pdf), "%PDF-") {
			fail(w, http.StatusBadRequest, "el archivo no es un PDF")
			return
		}
		pdfName = fh.Filename
	}

	if (body == "") == (pdf == nil) {
		fail(w, http.StatusBadRequest, "enviá un cuerpo de texto o un PDF, no ambos")
		return
	}

	isPDF := pdf != nil
	content := pdf
	if !isPDF {
		content = []byte(body)
	}
	hash := contentHash(title, content, isPDF)

	var bodyArg, nameArg *string
	if isPDF {
		nameArg = &pdfName
	} else {
		bodyArg = &body
	}

	slug := slugify(title)
	_, err := db.Exec(r.Context(),
		`insert into petitions (slug, title, body, pdf, pdf_name, content_hash)
		 values ($1,$2,$3,$4,$5,$6)`,
		slug, title, bodyArg, pdf, nameArg, hash)
	if err != nil {
		log.Printf("insert petition: %v", err)
		fail(w, http.StatusInternalServerError, "no se pudo crear la peticion")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"slug": slug, "content_hash": hash})
}

func listPetitions(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(r.Context(),
		`select p.slug, p.title, p.body, p.pdf_name, p.content_hash, p.created_at,
		        count(s.id)
		   from petitions p
		   left join signatures s on s.petition_id = p.id
		  group by p.id
		  order by p.created_at desc
		  limit 100`)
	if err != nil {
		log.Printf("list petitions: %v", err)
		fail(w, http.StatusInternalServerError, "error al listar")
		return
	}
	defer rows.Close()

	out := []petition{}
	for rows.Next() {
		var p petition
		if err := rows.Scan(&p.Slug, &p.Title, &p.Body, &p.PDFName, &p.ContentHash, &p.CreatedAt, &p.Signatures); err != nil {
			log.Printf("scan petition: %v", err)
			fail(w, http.StatusInternalServerError, "error al listar")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

type signer struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Comment   *string   `json:"comment"`
	Drawing   *string   `json:"drawing"`
	CreatedAt time.Time `json:"created_at"`
}

const signersPage = 10

// fetchSigners pagina por id descendente y no por offset: con offset, una firma
// nueva corre la ventana y el usuario ve repetida la ultima de la pagina previa.
// before == 0 trae la primera pagina.
func fetchSigners(ctx context.Context, petitionID string, before int64) ([]signer, error) {
	rows, err := db.Query(ctx,
		`select id, name, comment, drawing, created_at from signatures
		  where petition_id = $1 and ($2 = 0 or id < $2)
		  order by id desc limit $3`, petitionID, before, signersPage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []signer{}
	for rows.Next() {
		var s signer
		if err := rows.Scan(&s.ID, &s.Name, &s.Comment, &s.Drawing, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func listSigners(w http.ResponseWriter, r *http.Request) {
	id, err := petitionIDBySlug(r, r.PathValue("slug"))
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "peticion inexistente")
		return
	}
	if err != nil {
		log.Printf("signers lookup: %v", err)
		fail(w, http.StatusInternalServerError, "error interno")
		return
	}

	var before int64
	if v := r.URL.Query().Get("before"); v != "" {
		before, err = strconv.ParseInt(v, 10, 64)
		if err != nil || before < 0 {
			fail(w, http.StatusBadRequest, "cursor invalido")
			return
		}
	}

	signers, err := fetchSigners(r.Context(), id, before)
	if err != nil {
		log.Printf("list signers: %v", err)
		fail(w, http.StatusInternalServerError, "error al leer firmas")
		return
	}
	writeJSON(w, http.StatusOK, signers)
}

func getPetition(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var id string
	var p petition
	err := db.QueryRow(r.Context(),
		`select p.id, p.slug, p.title, p.body, p.pdf_name, p.content_hash, p.created_at,
		        (select count(*) from signatures where petition_id = p.id)
		   from petitions p where p.slug = $1`, slug).
		Scan(&id, &p.Slug, &p.Title, &p.Body, &p.PDFName, &p.ContentHash, &p.CreatedAt, &p.Signatures)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "peticion inexistente")
		return
	}
	if err != nil {
		log.Printf("get petition: %v", err)
		fail(w, http.StatusInternalServerError, "error al leer")
		return
	}

	// Primera pagina; el resto llega por /signers. p.Signatures trae el total.
	signers, err := fetchSigners(r.Context(), id, 0)
	if err != nil {
		log.Printf("list signers: %v", err)
		fail(w, http.StatusInternalServerError, "error al leer firmas")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"petition": p, "signers": signers})
}

func getPetitionDoc(w http.ResponseWriter, r *http.Request) {
	var pdf []byte
	var name *string
	err := db.QueryRow(r.Context(),
		`select pdf, pdf_name from petitions where slug = $1`, r.PathValue("slug")).Scan(&pdf, &name)
	if errors.Is(err, pgx.ErrNoRows) || pdf == nil {
		fail(w, http.StatusNotFound, "esta peticion no tiene PDF")
		return
	}
	if err != nil {
		log.Printf("get doc: %v", err)
		fail(w, http.StatusInternalServerError, "error al leer el PDF")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline; filename=\""+cmp(deref(name), "peticion.pdf")+"\"")
	w.Write(pdf)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

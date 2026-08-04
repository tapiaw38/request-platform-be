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
	// RDS rechaza el texto plano de entrada, pero otros proveedores lo aceptan
	// callados y ahi las credenciales viajan expuestas sin que nadie se entere.
	if strings.Contains(dsn, "sslmode=disable") && !strings.Contains(dsn, "localhost") && !strings.Contains(dsn, "127.0.0.1") {
		log.Print("ATENCION: DATABASE_URL usa sslmode=disable contra un host remoto; usá sslmode=require")
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

	// Crear, editar y borrar es solo del admin; leer y firmar es publico.
	mux.HandleFunc("POST /api/petitions", requireAdmin(createPetition))
	mux.HandleFunc("PUT /api/petitions/{slug}", requireAdmin(updatePetition))
	mux.HandleFunc("DELETE /api/petitions/{slug}", requireAdmin(deletePetition))
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
	UpdatedAt   time.Time `json:"updated_at"`
	Signatures  int       `json:"signatures"`
}

// petitionForm es el contenido ya validado de un multipart de alta o edicion.
// body y pdf son mutuamente excluyentes: lo garantiza parsePetitionForm y lo
// vuelve a exigir el check de la tabla.
type petitionForm struct {
	title   string
	body    *string
	pdf     []byte
	pdfName *string
	hash    string
}

// parsePetitionForm lee y valida el multipart que comparten alta y edicion.
// Si algo falla ya escribio la respuesta de error: el handler solo corta.
func parsePetitionForm(w http.ResponseWriter, r *http.Request) (petitionForm, bool) {
	var out petitionForm
	if err := r.ParseMultipartForm(maxPDFBytes); err != nil {
		fail(w, http.StatusBadRequest, "formulario invalido")
		return out, false
	}
	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	if title == "" || len(title) > 200 {
		fail(w, http.StatusBadRequest, "titulo requerido (max 200 caracteres)")
		return out, false
	}

	var pdf []byte
	var pdfName string
	if f, fh, err := r.FormFile("pdf"); err == nil {
		defer f.Close()
		if fh.Size > maxPDFBytes {
			fail(w, http.StatusRequestEntityTooLarge, "el PDF supera 5MB")
			return out, false
		}
		pdf, err = io.ReadAll(io.LimitReader(f, maxPDFBytes))
		if err != nil {
			fail(w, http.StatusBadRequest, "no se pudo leer el PDF")
			return out, false
		}
		if !strings.HasPrefix(string(pdf), "%PDF-") {
			fail(w, http.StatusBadRequest, "el archivo no es un PDF")
			return out, false
		}
		pdfName = fh.Filename
	}

	if (body == "") == (pdf == nil) {
		fail(w, http.StatusBadRequest, "enviá un cuerpo de texto o un PDF, no ambos")
		return out, false
	}

	isPDF := pdf != nil
	content := pdf
	if !isPDF {
		content = []byte(body)
	}
	out = petitionForm{title: title, pdf: pdf, hash: contentHash(title, content, isPDF)}
	if isPDF {
		out.pdfName = &pdfName
	} else {
		out.body = &body
	}
	return out, true
}

func createPetition(w http.ResponseWriter, r *http.Request) {
	in, ok := parsePetitionForm(w, r)
	if !ok {
		return
	}

	slug := slugify(in.title)
	_, err := db.Exec(r.Context(),
		`insert into petitions (slug, title, body, pdf, pdf_name, content_hash)
		 values ($1,$2,$3,$4,$5,$6)`,
		slug, in.title, in.body, in.pdf, in.pdfName, in.hash)
	if err != nil {
		log.Printf("insert petition: %v", err)
		fail(w, http.StatusInternalServerError, "no se pudo crear la peticion")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"slug": slug, "content_hash": in.hash})
}

// updatePetition reemplaza titulo y contenido. El slug NO cambia aunque cambie
// el titulo: el link ya esta circulando y romperlo perjudica a quien todavia no
// firmo. Las firmas anteriores tampoco se tocan: cada una guarda el hash de la
// version que esa persona leyo, asi que quedan atadas a ese texto y no al nuevo.
func updatePetition(w http.ResponseWriter, r *http.Request) {
	in, ok := parsePetitionForm(w, r)
	if !ok {
		return
	}

	slug := r.PathValue("slug")
	// Los campos del tipo que no se manda se ponen en null explicitamente:
	// pasar de texto a PDF (o al reves) tiene que dejar un solo contenido vivo,
	// que es justo lo que exige el check de la tabla.
	tag, err := db.Exec(r.Context(),
		`update petitions
		    set title = $2, body = $3, pdf = $4, pdf_name = $5,
		        content_hash = $6, updated_at = now()
		  where slug = $1`,
		slug, in.title, in.body, in.pdf, in.pdfName, in.hash)
	if err != nil {
		log.Printf("update petition: %v", err)
		fail(w, http.StatusInternalServerError, "no se pudo editar la peticion")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, http.StatusNotFound, "peticion inexistente")
		return
	}
	// Los OTP pendientes apuntan a la version vieja: si alguien los usa ahora
	// choca con el 409 del hash. Se queman para que pida uno nuevo y lea el
	// documento actual antes de firmarlo.
	db.Exec(r.Context(),
		`delete from otps where petition_id = (select id from petitions where slug = $1)`, slug)

	writeJSON(w, http.StatusOK, map[string]string{"slug": slug, "content_hash": in.hash})
}

// deletePetition borra la peticion y, por cascade, sus firmas y sus OTP.
// Es irreversible: la confirmacion la pide el front.
func deletePetition(w http.ResponseWriter, r *http.Request) {
	tag, err := db.Exec(r.Context(), `delete from petitions where slug = $1`, r.PathValue("slug"))
	if err != nil {
		log.Printf("delete petition: %v", err)
		fail(w, http.StatusInternalServerError, "no se pudo eliminar la peticion")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, http.StatusNotFound, "peticion inexistente")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func listPetitions(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(r.Context(),
		`select p.slug, p.title, p.body, p.pdf_name, p.content_hash, p.created_at, p.updated_at,
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
		if err := rows.Scan(&p.Slug, &p.Title, &p.Body, &p.PDFName, &p.ContentHash,
			&p.CreatedAt, &p.UpdatedAt, &p.Signatures); err != nil {
			log.Printf("scan petition: %v", err)
			fail(w, http.StatusInternalServerError, "error al listar")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

type signer struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Locality *string `json:"locality"`
	// Datos personales: viajan solo hacia el admin, redactSigners los borra
	// antes de responderle a cualquier otro. omitempty para que el front
	// distinga "no me corresponde verlo" de "la firma vieja no lo tiene".
	DNI     *string `json:"dni,omitempty"`
	Address *string `json:"address,omitempty"`
	Phone   *string `json:"phone,omitempty"`

	Comment *string `json:"comment"`
	Drawing *string `json:"drawing"`
	// Hash del documento al momento de firmar. Si no coincide con el de la
	// peticion, esta firma es sobre una version anterior a una edicion.
	ContentHash string    `json:"content_hash"`
	CreatedAt   time.Time `json:"created_at"`
}

const signersPage = 10

const signerCols = `id, name, dni, address, locality, phone, comment, drawing, content_hash, created_at`

func scanSigner(rows interface{ Scan(...any) error }) (signer, error) {
	var s signer
	err := rows.Scan(&s.ID, &s.Name, &s.DNI, &s.Address, &s.Locality, &s.Phone,
		&s.Comment, &s.Drawing, &s.ContentHash, &s.CreatedAt)
	return s, err
}

// redactSigners borra los datos que no son publicos. La localidad se conserva:
// es lo que una planilla de firmas muestra al lado del nombre. DNI, domicilio y
// celular no: son datos personales que se piden para presentar la peticion ante
// quien corresponda, no para publicarlos en la web.
func redactSigners(signers []signer) []signer {
	for i := range signers {
		signers[i].DNI, signers[i].Address, signers[i].Phone = nil, nil, nil
	}
	return signers
}

// fetchSigners pagina por id descendente y no por offset: con offset, una firma
// nueva corre la ventana y el usuario ve repetida la ultima de la pagina previa.
// before == 0 trae la primera pagina.
func fetchSigners(ctx context.Context, petitionID string, before int64) ([]signer, error) {
	rows, err := db.Query(ctx,
		`select `+signerCols+` from signatures
		  where petition_id = $1 and ($2 = 0 or id < $2)
		  order by id desc limit $3`, petitionID, before, signersPage)
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
	if !isAdmin(r) {
		signers = redactSigners(signers)
	}
	writeJSON(w, http.StatusOK, signers)
}

func getPetition(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var id string
	var p petition
	err := db.QueryRow(r.Context(),
		`select p.id, p.slug, p.title, p.body, p.pdf_name, p.content_hash, p.created_at, p.updated_at,
		        (select count(*) from signatures where petition_id = p.id)
		   from petitions p where p.slug = $1`, slug).
		Scan(&id, &p.Slug, &p.Title, &p.Body, &p.PDFName, &p.ContentHash,
			&p.CreatedAt, &p.UpdatedAt, &p.Signatures)
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
	if !isAdmin(r) {
		signers = redactSigners(signers)
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

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

	// El indice unico de DNI va aparte y no es fatal: si la base ya venia con
	// un DNI repetido de antes de esta regla, crear el indice falla, y quedarse
	// sin arrancar por eso seria peor que seguir con el dato viejo duplicado.
	if _, err := db.Exec(context.Background(),
		`create unique index if not exists signatures_unique_dni
		   on signatures (petition_id, dni)`); err != nil {
		log.Printf("ATENCION: no se pudo crear el indice unico de DNI: %v", err)
		log.Print("hay DNI repetidos. Para encontrarlos: select petition_id, dni, count(*) " +
			"from signatures where dni is not null group by 1,2 having count(*) > 1;")
	}

	if !adminConfigured() {
		log.Print("aviso: sin ADMIN_EMAIL/ADMIN_PASSWORD nadie puede crear peticiones")
	}
	logMailConfig()

	initStore(context.Background())

	mux := http.NewServeMux()
	// Primero: es la puerta que golpea el front para despertar al servicio, y
	// tambien la que mira un pinger externo. GET tambien atiende HEAD.
	mux.HandleFunc("GET /api/health", health)

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
	mux.HandleFunc("DELETE /api/petitions/{slug}/signers/{id}", requireAdmin(deleteSigner))
	mux.HandleFunc("GET /api/petitions/{slug}/doc", getPetitionDoc)
	// La descarga es solo del admin: lleva DNI, domicilio y celular de cada
	// firmante, o sea el padron completo con datos personales.
	mux.HandleFunc("GET /api/petitions/{slug}/download", requireAdmin(downloadPetition))
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

// allowedOrigins lee WEB_ORIGIN, que acepta varios separados por coma. Poder
// habilitar dos a la vez es lo que permite mudar el front a un dominio propio
// sin dejarlo roto entre que se cambia la variable y propaga el DNS.
//
// La barra final se recorta: el header Origin de un navegador nunca la lleva,
// asi que "https://x.com/" no coincide con nada y el fallo no dice por que.
func allowedOrigins() []string {
	raw := strings.SplitSeq(cmp(os.Getenv("WEB_ORIGIN"), "http://localhost:5173"), ",")
	var out []string
	for o := range raw {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// cors responde con el origen del pedido si esta en la lista. Con credenciales
// el navegador exige un origen unico y explicito: no acepta "*" ni una lista.
func cors(next http.Handler) http.Handler {
	origins := allowedOrigins()
	log.Printf("cors: origenes permitidos %v", origins)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Origin")
		allow := origins[0] // sin Origin (curl, navegacion directa) no hay nada que validar
		for _, o := range origins {
			if o == got {
				allow = got
				break
			}
		}
		// Sin Vary, un cache intermedio puede servirle a un origen la respuesta
		// que se armo para otro.
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Origin", allow)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, "+csrfHeader)
		// Sin esto el preflight de PUT y DELETE falla y el navegador reporta un
		// "NetworkError" generico. GET y POST no lo necesitan porque estan en la
		// lista segura de CORS: por eso login, listado y firma andaban mientras
		// editar y eliminar no.
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		// El navegador cachea el preflight y deja de pagar un viaje extra por
		// cada borrado.
		w.Header().Set("Access-Control-Max-Age", "86400")
		// La cookie de sesion del admin no viaja sin esto. Va de la mano con un
		// origen unico y explicito: con "*" el navegador lo rechaza.
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		// Sin esto el front no puede leer el nombre del archivo al descargar el
		// PDF por fetch: CORS oculta todo header que no sea de la lista segura.
		w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
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

// storePDF decide donde van los bytes: al store si hay S3 configurado, o a la
// columna bytea si no, que es exactamente lo que hacia antes de existir S3.
// Devuelve el par (bytea, clave) listo para el insert o el update.
func storePDF(ctx context.Context, slug string, pdf []byte) (raw []byte, key *string, err error) {
	if pdf == nil {
		return nil, nil, nil
	}
	if !storeEnabled() {
		return pdf, nil, nil
	}
	k, err := putPDF(ctx, slug, pdf)
	if err != nil {
		return nil, nil, err
	}
	return nil, &k, nil
}

func createPetition(w http.ResponseWriter, r *http.Request) {
	in, ok := parsePetitionForm(w, r)
	if !ok {
		return
	}

	slug := slugify(in.title)
	raw, key, err := storePDF(r.Context(), slug, in.pdf)
	if err != nil {
		log.Printf("subir pdf: %v", err)
		fail(w, http.StatusBadGateway, "no se pudo guardar el PDF")
		return
	}

	_, err = db.Exec(r.Context(),
		`insert into petitions (slug, title, body, pdf, pdf_key, pdf_name, content_hash)
		 values ($1,$2,$3,$4,$5,$6,$7)`,
		slug, in.title, in.body, raw, key, in.pdfName, in.hash)
	if err != nil {
		log.Printf("insert petition: %v", err)
		// El objeto ya subido no le sirve a nadie si la fila no existe.
		dropPDF(key)
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
	raw, key, err := storePDF(r.Context(), slug, in.pdf)
	if err != nil {
		log.Printf("subir pdf: %v", err)
		fail(w, http.StatusBadGateway, "no se pudo guardar el PDF")
		return
	}

	// Los campos del tipo que no se manda se ponen en null explicitamente:
	// pasar de texto a PDF (o al reves) tiene que dejar un solo contenido vivo,
	// que es justo lo que exige el check de la tabla. Devolver la clave vieja
	// en el mismo update evita leerla antes en otra consulta.
	var oldKey *string
	err = db.QueryRow(r.Context(),
		// El CTE se evalua sobre la instantanea previa al update, asi que trae
		// la clave vieja aunque la misma sentencia la este pisando. Un subselect
		// suelto en el returning depende de sutilezas de snapshot; esto no.
		`with anterior as (select pdf_key from petitions where slug = $1)
		 update petitions
		    set title = $2, body = $3, pdf = $4, pdf_key = $5, pdf_name = $6,
		        content_hash = $7, updated_at = now()
		  where slug = $1
	   returning (select pdf_key from anterior)`,
		slug, in.title, in.body, raw, key, in.pdfName, in.hash).Scan(&oldKey)
	if errors.Is(err, pgx.ErrNoRows) {
		dropPDF(key) // no habia peticion que editar: el objeto recien subido sobra
		fail(w, http.StatusNotFound, "peticion inexistente")
		return
	}
	if err != nil {
		log.Printf("update petition: %v", err)
		dropPDF(key)
		fail(w, http.StatusInternalServerError, "no se pudo editar la peticion")
		return
	}
	// Recien con la fila ya actualizada se borra el PDF anterior: si se hiciera
	// antes y el update fallara, la peticion quedaria apuntando a la nada.
	if oldKey != nil && (key == nil || *oldKey != *key) {
		dropPDF(oldKey)
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
	// El cascade de Postgres limpia firmas y OTP, pero no sabe nada del bucket:
	// returning trae la clave para poder borrar tambien el objeto.
	var key *string
	err := db.QueryRow(r.Context(),
		`delete from petitions where slug = $1 returning pdf_key`, r.PathValue("slug")).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "peticion inexistente")
		return
	}
	if err != nil {
		log.Printf("delete petition: %v", err)
		fail(w, http.StatusInternalServerError, "no se pudo eliminar la peticion")
		return
	}
	dropPDF(key)
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

// deleteSigner borra una firma puntual. El delete va acotado por petition_id
// ademas del id: sin eso, con adivinar un numero se podria borrar una firma de
// otra peticion, que es justo lo que un id secuencial hace facil.
func deleteSigner(w http.ResponseWriter, r *http.Request) {
	petitionID, err := petitionIDBySlug(r, r.PathValue("slug"))
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "peticion inexistente")
		return
	}
	if err != nil {
		log.Printf("delete signer lookup: %v", err)
		fail(w, http.StatusInternalServerError, "error interno")
		return
	}

	signerID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || signerID <= 0 {
		fail(w, http.StatusBadRequest, "firma invalida")
		return
	}

	tag, err := db.Exec(r.Context(),
		`delete from signatures where petition_id = $1 and id = $2`, petitionID, signerID)
	if err != nil {
		log.Printf("delete signer: %v", err)
		fail(w, http.StatusInternalServerError, "no se pudo eliminar la firma")
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, http.StatusNotFound, "firma inexistente")
		return
	}
	// Queda registro de quien la borro y cuando: una firma que desaparece sin
	// rastro es peor que una firma de mas.
	log.Printf("admin elimino la firma %d de la peticion %s, ip %s",
		signerID, r.PathValue("slug"), clientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func getPetitionDoc(w http.ResponseWriter, r *http.Request) {
	var pdf []byte
	var key, name *string
	err := db.QueryRow(r.Context(),
		`select pdf, pdf_key, pdf_name from petitions where slug = $1`,
		r.PathValue("slug")).Scan(&pdf, &key, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusNotFound, "esta peticion no tiene PDF")
		return
	}
	if err != nil {
		log.Printf("get doc: %v", err)
		fail(w, http.StatusInternalServerError, "error al leer el PDF")
		return
	}

	// Los bytes salen de la base o del store segun donde esten. El cliente no
	// ve la diferencia: mismo endpoint, mismo Content-Type, misma respuesta.
	pdf, err = readPDF(r.Context(), pdf, key)
	if errors.Is(err, errBlobNotFound) {
		fail(w, http.StatusNotFound, "esta peticion no tiene PDF")
		return
	}
	if err != nil {
		log.Printf("get doc del store: %v", err)
		fail(w, http.StatusBadGateway, "no se pudo leer el PDF")
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

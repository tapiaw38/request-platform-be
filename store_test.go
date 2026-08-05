package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeStore es un blobStore en memoria, para las pruebas que no necesitan
// hablar S3 de verdad.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  error
}

func newFakeStore() *fakeStore { return &fakeStore{objects: map[string][]byte{}} }

func (f *fakeStore) Key(slug string) string { return "petitions/" + slug + "/obj.pdf" }

func (f *fakeStore) Put(_ context.Context, key string, body []byte, _ string) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = body
	return nil
}

func (f *fakeStore) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	if !ok {
		return nil, errBlobNotFound
	}
	return b, nil
}

func (f *fakeStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

// useStore instala un store para el test y lo saca al terminar, porque store
// es global y los tests comparten proceso.
func useStore(t *testing.T, s blobStore) {
	t.Helper()
	prev := store
	store = s
	t.Cleanup(func() { store = prev })
}

// El punto de toda la compatibilidad hacia atras: una peticion vieja tiene los
// bytes en la columna y ninguna clave, y tiene que seguir leyendose igual.
func TestReadPDFCompatibilidadHaciaAtras(t *testing.T) {
	useStore(t, newFakeStore())
	legacy := []byte("%PDF-1.4 peticion vieja")

	got, err := readPDF(context.Background(), legacy, nil)
	if err != nil || string(got) != string(legacy) {
		t.Fatalf("una peticion vieja debe leerse de la columna: %q, %v", got, err)
	}

	// Aunque quedara una clave colgada, los bytes de la base mandan: son los
	// que el firmante vio y los que entraron en el content_hash.
	key := "petitions/x/y.pdf"
	got, err = readPDF(context.Background(), legacy, &key)
	if err != nil || string(got) != string(legacy) {
		t.Fatalf("la columna tiene prioridad sobre el store: %q, %v", got, err)
	}
}

func TestReadPDFDesdeElStore(t *testing.T) {
	f := newFakeStore()
	useStore(t, f)
	key := f.Key("mi-peticion")
	f.objects[key] = []byte("%PDF-1.4 en s3")

	got, err := readPDF(context.Background(), nil, &key)
	if err != nil || string(got) != "%PDF-1.4 en s3" {
		t.Fatalf("deberia leerse del store: %q, %v", got, err)
	}

	falta := "petitions/no/existe.pdf"
	if _, err := readPDF(context.Background(), nil, &falta); !errors.Is(err, errBlobNotFound) {
		t.Fatalf("una clave inexistente debe dar errBlobNotFound, dio %v", err)
	}

	// Peticion de texto: ni bytes ni clave.
	if _, err := readPDF(context.Background(), nil, nil); !errors.Is(err, errBlobNotFound) {
		t.Fatalf("sin bytes ni clave debe dar errBlobNotFound, dio %v", err)
	}
}

// Si el proceso arranca sin S3 pero la peticion apunta al store, el error tiene
// que decir que falta configuracion y no "no tiene PDF": el archivo existe.
func TestReadPDFSinStoreConfigurado(t *testing.T) {
	useStore(t, nil)
	key := "petitions/x/y.pdf"
	_, err := readPDF(context.Background(), nil, &key)
	if err == nil || !strings.Contains(err.Error(), "no hay S3 configurado") {
		t.Fatalf("deberia avisar que falta configuracion, dio %v", err)
	}
}

func TestStorePDFCaeAlByteaSinS3(t *testing.T) {
	useStore(t, nil)
	pdf := []byte("%PDF-1.4")
	raw, key, err := storePDF(context.Background(), "slug", pdf)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(pdf) || key != nil {
		t.Fatalf("sin S3 el PDF va a la columna: raw=%q key=%v", raw, key)
	}

	// Con S3 pasa lo contrario: la columna queda vacia y se guarda la clave.
	f := newFakeStore()
	useStore(t, f)
	raw, key, err = storePDF(context.Background(), "slug", pdf)
	if err != nil {
		t.Fatal(err)
	}
	if raw != nil || key == nil {
		t.Fatalf("con S3 el PDF va al store: raw=%v key=%v", raw, key)
	}
	if string(f.objects[*key]) != string(pdf) {
		t.Fatal("el objeto no llego al store")
	}

	// Una peticion de texto no sube nada.
	raw, key, err = storePDF(context.Background(), "slug", nil)
	if raw != nil || key != nil || err != nil {
		t.Fatalf("sin PDF no hay nada que guardar: %v %v %v", raw, key, err)
	}
}

func TestS3ConfigConfigured(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    s3Config
		want bool
	}{
		{"completo", s3Config{Bucket: "b", Region: "us-east-1"}, true},
		// Las credenciales pueden venir del rol de la instancia.
		{"sin claves", s3Config{Bucket: "b", Region: "us-east-1"}, true},
		{"sin bucket", s3Config{Region: "us-east-1"}, false},
		{"sin region", s3Config{Bucket: "b"}, false},
		{"vacio", s3Config{}, false},
	} {
		if got := tc.c.configured(); got != tc.want {
			t.Errorf("%s: configured() = %v, esperaba %v", tc.name, got, tc.want)
		}
	}
}

func TestS3StoreKeyEsUnicaPorSubida(t *testing.T) {
	s := &s3Store{bucket: "b", prefix: "petitions"}
	a, b := s.Key("mi-peticion-a1b2c3"), s.Key("mi-peticion-a1b2c3")
	if a == b {
		t.Fatal("dos subidas de la misma peticion no pueden compartir clave")
	}
	if !strings.HasPrefix(a, "petitions/mi-peticion-a1b2c3/") || !strings.HasSuffix(a, ".pdf") {
		t.Fatalf("clave inesperada: %s", a)
	}
}

// s3Fake habla lo justo del protocolo para PutObject, GetObject y DeleteObject
// en path-style. Sirve para probar el cableado real del SDK (endpoint, region,
// credenciales, firma) sin depender de AWS.
func s3Fake(t *testing.T) (*httptest.Server, map[string][]byte) {
	t.Helper()
	var mu sync.Mutex
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path-style: /{bucket}/{key...}
		path := strings.TrimPrefix(r.URL.Path, "/")
		bucket, key, _ := strings.Cut(path, "/")
		if bucket != "mi-bucket" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[key] = body
			w.Header().Set("ETag", `"fake"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			b, ok := objects[key]
			if !ok {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
				return
			}
			w.Write(b)
		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, objects
}

func TestS3StoreContraUnS3DeVerdad(t *testing.T) {
	srv, objects := s3Fake(t)
	s, err := newS3Store(context.Background(), s3Config{
		Region: "us-east-1", Bucket: "mi-bucket",
		AccessKey: "clave", SecretKey: "secreta",
		Endpoint: srv.URL, Prefix: "petitions",
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	key := s.Key("senal-peatonal-a1b2c3")
	pdf := []byte("%PDF-1.4\nun documento\n%%EOF")

	if err := s.Put(ctx, key, pdf, "application/pdf"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if string(objects[key]) != string(pdf) {
		t.Fatalf("el objeto no llego entero: %q", objects[key])
	}

	got, err := s.Get(ctx, key)
	if err != nil || string(got) != string(pdf) {
		t.Fatalf("Get: %q, %v", got, err)
	}

	if _, err := s.Get(ctx, "petitions/no/existe.pdf"); !errors.Is(err, errBlobNotFound) {
		t.Fatalf("una clave inexistente debe mapearse a errBlobNotFound, dio %v", err)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := objects[key]; ok {
		t.Fatal("el objeto deberia haberse borrado")
	}
}

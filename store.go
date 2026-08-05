package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// blobStore guarda los PDF fuera de la base. Es una interfaz y no el cliente de
// S3 directo para que el resto del codigo no sepa donde termina el archivo, que
// es lo que permite convivir con las peticiones viejas guardadas en Postgres.
type blobStore interface {
	// Key arma la clave del objeto para una peticion. La decide el store
	// porque el prefijo es parte de su configuracion.
	Key(slug string) string
	Put(ctx context.Context, key string, body []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// store es nil cuando no hay S3 configurado. En ese caso los PDF nuevos siguen
// yendo al bytea de Postgres, igual que siempre: la falta de credenciales no
// puede dejar la aplicacion sin poder crear peticiones.
var store blobStore

func storeEnabled() bool { return store != nil }

// s3Config junta las variables de entorno con los mismos nombres que usa
// auth-api-be, para no tener dos convenciones en el mismo stack.
type s3Config struct {
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// Endpoint solo se usa contra un S3 que no es el de AWS (MinIO, R2) o en
	// tests. Vacio en produccion.
	Endpoint string
	Prefix   string
}

func loadS3Config() s3Config {
	return s3Config{
		Region:    os.Getenv("AWS_REGION"),
		Bucket:    os.Getenv("AWS_BUCKET"),
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		Endpoint:  os.Getenv("AWS_ENDPOINT_URL"),
		Prefix:    cmp(os.Getenv("AWS_PREFIX"), "petitions"),
	}
}

// configured pide bucket y region. Las credenciales pueden venir del rol de la
// instancia (IRSA, perfil de EC2), asi que su ausencia no descalifica.
func (c s3Config) configured() bool { return c.Bucket != "" && c.Region != "" }

type s3Store struct {
	client *s3.Client
	bucket string
	prefix string
}

func newS3Store(ctx context.Context, c s3Config) (*s3Store, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(c.Region)}
	// Con las dos claves explicitas se usan esas; sin ellas, la cadena por
	// defecto del SDK (rol de la instancia, ~/.aws, variables del entorno).
	if c.AccessKey != "" && c.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, ""),
		))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
			// MinIO y compania no resuelven el bucket como subdominio.
			o.UsePathStyle = true
		}
	})
	return &s3Store{client: client, bucket: c.Bucket, prefix: strings.Trim(c.Prefix, "/")}, nil
}

// Key arma la ruta del objeto. Lleva el slug para poder mirar el bucket y
// entender que es cada cosa, y un sufijo aleatorio para que reemplazar el PDF
// de una peticion nunca reescriba la clave anterior: si algo sale mal a mitad
// de camino, el archivo viejo sigue entero.
func (s *s3Store) Key(slug string) string {
	return fmt.Sprintf("%s/%s/%s.pdf", s.prefix, slug, randomHex(8))
}

func (s *s3Store) Put(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
		ContentType:   aws.String(contentType),
		// Sin ACL public-read: el bucket queda privado y los bytes se sirven
		// desde la API, que es la que ya sabe quien puede ver que.
		ServerSideEncryption: types.ServerSideEncryptionAes256,
	})
	return err
}

func (s *s3Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var missing *types.NoSuchKey
		if errors.As(err, &missing) {
			return nil, errBlobNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(io.LimitReader(out.Body, maxPDFBytes+1))
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}

var errBlobNotFound = errors.New("objeto inexistente en el store")

// initStore deja store en nil si no hay nada configurado. Se avisa por log en
// los dos casos: el silencio no puede significar a la vez "va a S3" y "sigue
// yendo a la base".
func initStore(ctx context.Context) {
	c := loadS3Config()
	if !c.configured() {
		log.Print("sin AWS_BUCKET/AWS_REGION: los PDF nuevos se guardan en Postgres, como hasta ahora")
		return
	}
	s, err := newS3Store(ctx, c)
	if err != nil {
		// Arrancar igual y caer al bytea es preferible a no arrancar: la
		// aplicacion sigue sirviendo todo lo que ya existe.
		log.Printf("no se pudo inicializar S3 (%v); los PDF nuevos van a Postgres", err)
		return
	}
	store = s
	log.Printf("PDF nuevos hacia s3://%s/%s", c.Bucket, s.prefix)
}

// putPDF sube el PDF y devuelve la clave. Solo se llama con store != nil.
func putPDF(ctx context.Context, slug string, pdf []byte) (string, error) {
	key := store.Key(slug)
	// Timeout propio y sin la cancelacion del request: si la persona cierra la
	// pestaña a mitad de la subida, se termina igual y la fila queda coherente.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := store.Put(ctx, key, pdf, "application/pdf"); err != nil {
		return "", err
	}
	return key, nil
}

// dropPDF borra el objeto sin hacer fallar la operacion que lo pidio. Un objeto
// huerfano en el bucket cuesta centavos; abortar el borrado de una peticion
// porque S3 tuvo un mal momento deja al admin sin poder hacer su trabajo.
func dropPDF(key *string) {
	if key == nil || *key == "" || !storeEnabled() {
		return
	}
	k := *key
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := store.Delete(ctx, k); err != nil {
			log.Printf("no se pudo borrar %s del store: %v", k, err)
		}
	}()
}

// readPDF resuelve de donde salen los bytes: de la columna bytea si la peticion
// es de las viejas, o del store si tiene clave. Es el unico lugar que conoce
// esa diferencia, y por eso el resto del codigo no se entera de la migracion.
func readPDF(ctx context.Context, pdf []byte, key *string) ([]byte, error) {
	if pdf != nil {
		return pdf, nil
	}
	if key == nil || *key == "" {
		return nil, errBlobNotFound
	}
	if !storeEnabled() {
		// La peticion apunta al store pero el proceso arranco sin credenciales:
		// decirlo claro, porque el archivo existe y el problema es de config.
		return nil, fmt.Errorf("la peticion tiene el PDF en el store pero no hay S3 configurado")
	}
	return store.Get(ctx, *key)
}

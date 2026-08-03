# request-platform-be

API de peticiones públicas con firma electrónica. Go 1.25, `net/http` de la
stdlib + `pgx`. Sin framework.

Front: [request-platform-fe](https://github.com/tapiaw38/request-platform-fe).

## Correr

```sh
docker compose up -d --wait      # postgres :5432, carga schema.sql la primera vez
go run .                         # :8080
```

Sin `SMTP_HOST` los códigos OTP salen por consola, que es lo que se quiere en
desarrollo.

Variables opcionales: `DATABASE_URL`, `PORT`, `WEB_ORIGIN`, `TRUST_PROXY=1`
(sólo si hay un reverse proxy adelante).

## Mail (OTP)

```sh
cp .env.example .env     # completar SMTP_USER y SMTP_PASS
set -a && . ./.env && set +a && go run .
```

Con Gmail, `SMTP_PASS` es un **App Password** de 16 caracteres
(<https://myaccount.google.com/apppasswords>, requiere 2FA), nunca la
contraseña de la cuenta. `.env` está en `.gitignore`.

Cambiar de proveedor no toca código, son las mismas cuatro variables `SMTP_*`.

## Qué evidencia queda por firma

Nombre, email verificado por OTP, comentario, firma dibujada (opcional), IP,
user-agent, timestamp y el **SHA-256 del documento tal como estaba al firmar**.
Si el contenido cambia después, cada firma sigue apuntando a la versión que esa
persona leyó, y firmar con un hash desactualizado devuelve `409`.

## Endpoints

| Método | Ruta | |
|---|---|---|
| POST | `/api/petitions` | multipart: `title` + (`body` \| `pdf`) |
| GET | `/api/petitions` | últimas 100 con contador de firmas |
| GET | `/api/petitions/{slug}` | detalle + primeras 10 firmas + total |
| GET | `/api/petitions/{slug}/signers?before={id}` | página de 10, cursor por id |
| GET | `/api/petitions/{slug}/doc` | PDF original |
| POST | `/api/petitions/{slug}/otp` | `{email}` → manda código |
| POST | `/api/petitions/{slug}/sign` | `{email, code, name, comment, drawing, content_hash}` |

## Tests

```sh
go test ./...
```

## Deuda deliberada

- PDF en `bytea`. Mover a S3/R2 si los documentos pasan de unos MB.
- Rate limit en memoria, por proceso. Mover a Postgres si corre en más de una instancia.
- Sin firma digital de Ley 25.506: esto es firma electrónica con evidencia. Para
  el nivel jurídico máximo hay que integrar un certificador licenciado, no
  construirlo acá.

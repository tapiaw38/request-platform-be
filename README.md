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

`schema.sql` va embebido en el binario (`go:embed`) y se aplica en cada
arranque. Es idempotente, así que hace de migración sin herramienta aparte.

## Administrador

Un solo admin, definido por entorno. **Es el único que puede crear peticiones;
leerlas y firmarlas es público.**

```sh
ADMIN_EMAIL=admin@tudominio.com
ADMIN_PASSWORD=algo-largo-y-aleatorio
```

Sin esas dos variables nadie puede crear nada, y el arranque lo avisa por log.
La contraseña se compara en tiempo constante y el login tiene rate limit por IP.
La sesión es una cookie `HttpOnly` de 12 horas.

Si el front vive en otro dominio que la API, hace falta `COOKIE_SECURE=1` y
`COOKIE_CROSS_SITE=1`: sin eso el navegador no manda la cookie en pedidos
cross-site y el admin no puede crear nada.

## Desplegar en Heroku

```sh
heroku create tu-app-be
heroku addons:create heroku-postgresql:essential-0
heroku config:set ADMIN_EMAIL=... ADMIN_PASSWORD=... \
  WEB_ORIGIN=https://tu-app-fe.herokuapp.com \
  COOKIE_SECURE=1 COOKIE_CROSS_SITE=1 TRUST_PROXY=1 \
  SMTP_HOST=... SMTP_USER=... SMTP_PASS=...
git push heroku main
```

`DATABASE_URL` y `PORT` los inyecta Heroku solo, no hay que definirlas. El
`Procfile` levanta el binario y el schema se aplica al arrancar.

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
| POST | `/api/admin/login` | `{email, password}` → cookie de sesión |
| POST | `/api/admin/logout` | cierra la sesión |
| GET | `/api/admin/me` | `{admin, configured}` |
| POST | `/api/petitions` | **admin** · multipart: `title` + (`body` \| `pdf`) |
| GET | `/api/petitions` | últimas 100 con contador de firmas |
| GET | `/api/petitions/{slug}` | detalle + primeras 10 firmas + total |
| GET | `/api/petitions/{slug}/signers?before={id}` | página de 10, cursor por id |
| GET | `/api/petitions/{slug}/doc` | PDF original |
| GET | `/api/petitions/{slug}/download` | PDF con las firmas anexadas |
| POST | `/api/petitions/{slug}/otp` | `{email}` → manda código |
| POST | `/api/petitions/{slug}/sign` | `{email, code, name, comment, drawing, content_hash}` |

## Tests

```sh
go test ./...
```

## Descarga con firmas

`GET /api/petitions/{slug}/download` devuelve un PDF con las firmas en grilla de
2×5 por hoja: trazo, línea, nombre completo y fecha. El trazo se escala
preservando proporción, y quien firmó sin dibujar aparece con una leyenda en vez
del trazo.

- Petición de **texto**: se compone el cuerpo y se le anexan las hojas de firmas.
- Petición **PDF**: se conserva el original tal cual y se le anexan las hojas
  (fusión con pdfcpu).

## Deuda deliberada

- PDF en `bytea`. Mover a S3/R2 si los documentos pasan de unos MB.
- Rate limit en memoria, por proceso. Mover a Postgres si corre en más de una instancia.
- Sin firma digital de Ley 25.506: esto es firma electrónica con evidencia. Para
  el nivel jurídico máximo hay que integrar un certificador licenciado, no
  construirlo acá.

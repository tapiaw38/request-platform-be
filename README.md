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

### `DATABASE_URL` en producción

Terminala **siempre** en `?sslmode=require`. Con `sslmode=disable` las
credenciales y el contenido de las firmas viajan en texto plano por internet, y
RDS con `rds.force_ssl` rechaza la conexión directamente:

```
FATAL: no pg_hba.conf entry for host "...", no encryption (SQLSTATE 28000)
```

`sslmode=disable` es aceptable sólo contra el Postgres local del
`docker-compose`, que no sale de tu máquina.

## Administrador

Un solo admin, definido por entorno. **Es el único que puede crear, editar y
eliminar peticiones; leerlas y firmarlas es público.**

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

Las rutas de admin exigen además el encabezado **`X-Requested-With`**. No lleva
ningún secreto: alcanza con que exista. Un form `multipart` cross-site sale sin
preflight, así que con `SameSite=None` un sitio ajeno podría crear peticiones con
tu cookie puesta; pedir un encabezado propio obliga al preflight y ahí CORS lo
frena. El front lo manda solo en los pedidos que modifican.

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

Nombre, **DNI**, **domicilio**, **localidad**, **celular**, email verificado por
OTP, comentario, firma dibujada (opcional), IP, user-agent, timestamp y el
**SHA-256 del documento tal como estaba al firmar**. Si el contenido cambia
después, cada firma sigue apuntando a la versión que esa persona leyó, y firmar
con un hash desactualizado devuelve `409`.

DNI y celular se guardan normalizados (sólo dígitos, más el `+` inicial del
celular): el mismo documento escrito con puntos, con espacios o sin nada tiene
que ser el mismo dato para quien después lea el padrón. El DNI se valida por
forma —7 u 8 dígitos—, no contra el RENAPER: esto es firma electrónica con
evidencia, no identidad probada.

### Qué de eso es público

**DNI, domicilio y celular no se publican.** Se piden para presentar la petición
ante quien corresponda, no para dejarlos en la web:

| | público | admin |
|---|---|---|
| nombre, localidad, comentario, trazo, fecha | sí | sí |
| DNI, domicilio, celular | **no** | sí |

Vale para las tres salidas: `/signers`, el detalle de la petición y el PDF de
`/download`, que arma la grilla con esos datos sólo si la request trae la cookie
del admin. El filtro es del servidor, no del front.

## Editar y eliminar

| | |
|---|---|
| `PUT /api/petitions/{slug}` | reemplaza título y contenido |
| `DELETE /api/petitions/{slug}` | borra la petición **y todas sus firmas** |

Editar **no cambia el slug** aunque cambie el título: el link ya está circulando
y romperlo perjudica a quien todavía no firmó.

Editar tampoco toca las firmas anteriores. Cada una guarda el hash de la versión
que esa persona leyó, así que sigue atada a ese texto y no al nuevo; en el PDF
esas firmas salen marcadas como «firmada sobre una versión anterior». Los OTP
pendientes se queman en la edición: apuntaban a la versión vieja y ahora chocan
contra el `409`, así que es mejor que la persona pida uno nuevo y lea el
documento actual.

Eliminar es irreversible y arrastra firmas y OTP por `on delete cascade`. La
confirmación la pide el front; la API no pregunta dos veces.

## Endpoints

| Método | Ruta | |
|---|---|---|
| POST | `/api/admin/login` | `{email, password}` → cookie de sesión |
| POST | `/api/admin/logout` | cierra la sesión |
| GET | `/api/admin/me` | `{admin, configured}` |
| POST | `/api/petitions` | **admin** · multipart: `title` + (`body` \| `pdf`) |
| PUT | `/api/petitions/{slug}` | **admin** · mismo multipart; el slug no cambia |
| DELETE | `/api/petitions/{slug}` | **admin** · borra la petición y sus firmas |
| GET | `/api/petitions` | últimas 100 con contador de firmas |
| GET | `/api/petitions/{slug}` | detalle + primeras 10 firmas + total |
| GET | `/api/petitions/{slug}/signers?before={id}` | página de 10, cursor por id |
| GET | `/api/petitions/{slug}/doc` | PDF original |
| GET | `/api/petitions/{slug}/download` | PDF con las firmas anexadas |
| POST | `/api/petitions/{slug}/otp` | `{email}` → manda código |
| POST | `/api/petitions/{slug}/sign` | `{email, code, name, dni, address, locality, phone, comment, drawing, content_hash}` |

## Tests

```sh
go test ./...
```

## Una firma por persona

`unique (petition_id, email)` y `unique (petition_id, dni)`. Sin la segunda, la
misma persona firma dos veces con dos correos y el padrón la cuenta dos veces.

El DNI se guarda normalizado —sólo dígitos—, así que `30.555.123` y `30555123`
son el mismo documento. El índice único se crea aparte del schema y su fallo no
es fatal: si una base vieja ya tiene un DNI repetido, arrancar igual es mejor
que no arrancar, y el log dice cómo encontrarlo.

## Descarga con firmas

`GET /api/petitions/{slug}/download` devuelve un PDF con las firmas en grilla de
2×5 por hoja: trazo, línea, nombre completo, datos y fecha. El trazo se escala
preservando proporción, y quien firmó sin dibujar aparece con una leyenda en vez
del trazo.

Si la request trae la cookie del admin, cada celda suma DNI, domicilio y
celular. Sin ella sale la misma grilla con nombre, localidad y fecha: es el
mismo endpoint, decidiendo por sesión (ver «Qué de eso es público»). Las firmas
de una versión previa a una edición salen marcadas.

- Petición de **texto**: se compone el cuerpo y se le anexan las hojas de firmas.
- Petición **PDF**: se conserva el original tal cual y se le anexan las hojas
  (fusión con pdfcpu).

## Deuda deliberada

- PDF en `bytea`. Mover a S3/R2 si los documentos pasan de unos MB.
- Rate limit en memoria, por proceso. Mover a Postgres si corre en más de una instancia.
- Sin firma digital de Ley 25.506: esto es firma electrónica con evidencia. Para
  el nivel jurídico máximo hay que integrar un certificador licenciado, no
  construirlo acá.

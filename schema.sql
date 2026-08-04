create extension if not exists pgcrypto;

create table if not exists petitions (
  id           uuid primary key default gen_random_uuid(),
  slug         text unique not null,
  title        text not null,
  body         text,        -- markdown; null cuando la peticion es un PDF
  pdf          bytea,       -- ponytail: bytea alcanza para docs < 5MB, mover a S3 si crecen
  pdf_name     text,
  content_hash text not null,
  created_at   timestamptz not null default now(),
  check ((body is null) <> (pdf is null))
);

create table if not exists otps (
  id           bigserial primary key,
  petition_id  uuid not null references petitions(id) on delete cascade,
  email        text not null,
  code_hash    text not null,
  attempts     int  not null default 0,
  expires_at   timestamptz not null,
  created_at   timestamptz not null default now()
);
create index if not exists otps_lookup on otps (petition_id, email, created_at desc);

create table if not exists signatures (
  id           bigserial primary key,
  petition_id  uuid not null references petitions(id) on delete cascade,
  name         text not null,
  email        text not null,
  dni          text,        -- ver alter de abajo: nullable por las firmas viejas
  address      text,
  locality     text,
  phone        text,
  comment      text,
  drawing      text,        -- data:image/png;base64,... opcional
  content_hash text not null, -- hash del documento tal como estaba al firmar
  ip           text,
  user_agent   text,
  created_at   timestamptz not null default now(),
  unique (petition_id, email)
);
-- Indice para la paginacion por cursor (id desc), no por created_at.
create index if not exists signatures_by_petition on signatures (petition_id, id desc);

-- Migracion de bases que ya existian antes de pedir estos datos. Quedan
-- nullable a proposito: las firmas viejas no los tienen y no se pueden
-- inventar. La obligatoriedad la aplica el handler, no la tabla.
alter table signatures add column if not exists dni      text;
alter table signatures add column if not exists address  text;
alter table signatures add column if not exists locality text;
alter table signatures add column if not exists phone    text;

-- Una peticion editada cambia de content_hash; updated_at deja ver cuando.
alter table petitions add column if not exists updated_at timestamptz not null default now();

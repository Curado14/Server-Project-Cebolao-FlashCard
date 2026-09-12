# Estrutura Recomendada do Banco de Dados — Photo Edge Orchestrator

**Banco principal:** SQLite  
**Modo recomendado:** WAL  
**Objetivo:** persistência local robusta, rastreabilidade, idempotência, recuperação após falhas e execução assíncrona.

---

## 1. Princípios da estrutura

A estrutura do banco deve seguir os seguintes princípios:

- SQLite é a **fonte de verdade operacional**.
- O sistema de arquivos armazena os arquivos binários.
- O banco armazena:
  - identidade;
  - metadados;
  - estados;
  - filas;
  - tentativas;
  - integridade;
  - auditoria.
- Arquivos originais são imutáveis.
- Derivados podem ser recriados.
- Jobs são persistentes.
- Channels do Go não representam a fila oficial.
- Toda operação importante deve ser idempotente.
- Estados derivados devem poder ser reconstruídos.
- O sistema deve tolerar reinicialização em qualquer ponto do pipeline.

---

## 2. Visão geral

```text
                         EVENTS
                           │
                           │ 1:N
                           ▼
DEVICES ───────────────► PHOTOS
                           │
           ┌───────────────┼──────────────────┐
           │               │                  │
           ▼               ▼                  ▼
INGESTION_REQUESTS     PHOTO_FILES           JOBS
                           │                   │
                           │                   └────► JOB_ATTEMPTS
                           │
                           └────► FILE_INTEGRITY

PHOTOS ───────────────────────► ARTIFACTS
                                    │
                                    ├────► PRESETS
                                    │
                                    └────► EXPORTS
                                              │
                                              ├────► EXPORT_DESTINATIONS
                                              └────► EXPORT_ATTEMPTS

PHOTOS / JOBS / EXPORTS ─────────► AUDIT_EVENTS

PHOTO_FILES ─────────────────────► QUARANTINE_ENTRIES
```

---

# 3. Domínios do banco

A estrutura é dividida em cinco grupos.

```text
CORE
├── events
├── devices
└── photos

INGESTÃO E ARQUIVOS
├── ingestion_requests
├── photo_files
└── file_integrity

EXECUÇÃO
├── jobs
└── job_attempts

PROCESSAMENTO E EXPORTAÇÃO
├── presets
├── artifacts
├── export_destinations
├── exports
└── export_attempts

CONTROLE
├── quarantine_entries
├── audit_events
└── schema_migrations
```

---

# 4. Tabela `events`

Representa um evento fotográfico.

```sql
CREATE TABLE events (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,

    starts_at       TEXT,
    ends_at         TEXT,

    status          TEXT NOT NULL
                    CHECK (
                        status IN (
                            'DRAFT',
                            'ACTIVE',
                            'CLOSED',
                            'ARCHIVED'
                        )
                    ),

    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
) STRICT;
```

## Índices

```sql
CREATE INDEX idx_events_status
ON events(status);
```

---

# 5. Tabela `devices`

Representa cada dispositivo autorizado a enviar fotografias.

```sql
CREATE TABLE devices (
    id              TEXT PRIMARY KEY,

    name            TEXT NOT NULL,

    device_type     TEXT NOT NULL
                    CHECK (
                        device_type IN (
                            'WEB',
                            'ANDROID',
                            'IOS',
                            'CAMERA',
                            'FTP',
                            'OTHER'
                        )
                    ),

    token_hash      BLOB,

    enabled         INTEGER NOT NULL DEFAULT 1
                    CHECK (enabled IN (0, 1)),

    created_at      TEXT NOT NULL,
    last_seen_at    TEXT
) STRICT;
```

Exemplos:

```text
camera_01
iphone_01
android_02
sony_a7
ftp_camera_01
```

---

# 6. Tabela `photos`

Representa a identidade lógica da fotografia.

A fotografia não deve ser confundida com seu arquivo físico.

```sql
CREATE TABLE photos (
    id                  TEXT PRIMARY KEY,

    event_id            TEXT NOT NULL,
    device_id           TEXT NOT NULL,

    captured_at         TEXT,
    received_at         TEXT NOT NULL,

    status              TEXT NOT NULL
                        CHECK (
                            status IN (
                                'RECEIVING',
                                'STORED',
                                'PROCESSING',
                                'READY',
                                'EXPORT_PENDING',
                                'EXPORTED',
                                'FAILED',
                                'QUARANTINED'
                            )
                        ),

    last_error_code     TEXT,

    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,

    FOREIGN KEY(event_id)
        REFERENCES events(id),

    FOREIGN KEY(device_id)
        REFERENCES devices(id)
) STRICT;
```

## Observação

`photos.status` deve ser tratado como uma **visão consolidada do estado**.

A fonte operacional detalhada deve ser:

```text
jobs
artifacts
exports
```

---

# 7. Tabela `ingestion_requests`

Responsável pela idempotência da ingestão HTTP.

```sql
CREATE TABLE ingestion_requests (
    id                  TEXT PRIMARY KEY,

    device_id           TEXT NOT NULL,

    idempotency_key     TEXT NOT NULL,

    photo_id            TEXT,

    request_hash        TEXT,
    content_sha256      TEXT,

    status              TEXT NOT NULL
                        CHECK (
                            status IN (
                                'RECEIVING',
                                'ACCEPTED',
                                'REJECTED',
                                'CONFLICT'
                            )
                        ),

    first_received_at   TEXT NOT NULL,
    last_seen_at        TEXT NOT NULL,

    FOREIGN KEY(device_id)
        REFERENCES devices(id),

    FOREIGN KEY(photo_id)
        REFERENCES photos(id),

    UNIQUE(device_id, idempotency_key)
) STRICT;
```

## Comportamento esperado

```text
mesma chave + mesmo conteúdo
        ↓
retornar o mesmo photo_id

mesma chave + conteúdo diferente
        ↓
HTTP 409 Conflict
```

---

# 8. Tabela `photo_files`

Representa cada arquivo físico relacionado a uma fotografia.

```sql
CREATE TABLE photo_files (
    id              TEXT PRIMARY KEY,

    photo_id        TEXT NOT NULL,

    kind            TEXT NOT NULL
                    CHECK (
                        kind IN (
                            'ORIGINAL',
                            'NORMALIZED',
                            'DERIVATIVE',
                            'THUMBNAIL',
                            'EXPORT_COPY'
                        )
                    ),

    storage_path    TEXT NOT NULL UNIQUE,

    media_type      TEXT NOT NULL,
    extension       TEXT,

    size_bytes      INTEGER NOT NULL
                    CHECK (size_bytes >= 0),

    sha256          TEXT NOT NULL,

    width           INTEGER,
    height          INTEGER,

    created_at      TEXT NOT NULL,
    verified_at     TEXT,

    FOREIGN KEY(photo_id)
        REFERENCES photos(id),

    UNIQUE(photo_id, kind, sha256)
) STRICT;
```

## Princípio

```text
photo_id
   │
   ├── ORIGINAL
   ├── NORMALIZED
   ├── DERIVATIVE
   ├── THUMBNAIL
   └── EXPORT_COPY
```

O caminho físico nunca deve ser a identidade do arquivo.

---

# 9. Tabela `file_integrity`

Mantém o histórico das verificações de integridade.

```sql
CREATE TABLE file_integrity (
    id                  INTEGER PRIMARY KEY,

    file_id             TEXT NOT NULL,

    algorithm           TEXT NOT NULL,

    hash_value          TEXT NOT NULL,

    verification_status TEXT NOT NULL
                        CHECK (
                            verification_status IN (
                                'VALID',
                                'INVALID',
                                'MISSING'
                            )
                        ),

    verified_at         TEXT NOT NULL,

    FOREIGN KEY(file_id)
        REFERENCES photo_files(id)
) STRICT;
```

Exemplos de algoritmo:

```text
SHA256
SHA512
BLAKE3
```

---

# 10. Tabela `jobs`

É a fila persistente oficial do sistema.

```sql
CREATE TABLE jobs (
    id                  TEXT PRIMARY KEY,

    photo_id            TEXT NOT NULL,

    kind                TEXT NOT NULL
                        CHECK (
                            kind IN (
                                'VALIDATE',
                                'EXTRACT_METADATA',
                                'NORMALIZE',
                                'PROCESS',
                                'GENERATE_THUMBNAIL',
                                'EXPORT',
                                'VERIFY_EXPORT',
                                'CLEANUP'
                            )
                        ),

    status              TEXT NOT NULL
                        CHECK (
                            status IN (
                                'PENDING',
                                'RUNNING',
                                'RETRY_WAIT',
                                'SUCCEEDED',
                                'DEAD',
                                'CANCELLED'
                            )
                        ),

    priority            INTEGER NOT NULL DEFAULT 100,

    attempt_count       INTEGER NOT NULL DEFAULT 0,
    max_attempts        INTEGER NOT NULL DEFAULT 10,

    available_at        TEXT NOT NULL,

    lease_owner         TEXT,
    lease_until         TEXT,

    payload_json        TEXT,

    last_error_code     TEXT,
    last_error_message  TEXT,

    created_at          TEXT NOT NULL,
    started_at          TEXT,
    finished_at         TEXT,
    updated_at          TEXT NOT NULL,

    FOREIGN KEY(photo_id)
        REFERENCES photos(id)
) STRICT;
```

---

# 11. Índices da fila

Esses índices são essenciais para evitar scans desnecessários.

```sql
CREATE INDEX idx_jobs_pending
ON jobs(priority, available_at)
WHERE status IN ('PENDING', 'RETRY_WAIT');
```

```sql
CREATE INDEX idx_jobs_running_lease
ON jobs(lease_until)
WHERE status = 'RUNNING';
```

```sql
CREATE INDEX idx_jobs_photo
ON jobs(photo_id);
```

---

# 12. Reserva de job

Fluxo recomendado:

```text
PENDING
   ↓
worker seleciona job
   ↓
RUNNING
   ↓
lease_owner
lease_until
   ↓
processamento fora da transação
   ↓
SUCCEEDED
ou
RETRY_WAIT
ou
DEAD
```

Se o processo morrer:

```text
RUNNING
+
lease_until < agora
        ↓
recovery
        ↓
PENDING / RETRY_WAIT
```

---

# 13. Tabela `job_attempts`

Registra cada tentativa de execução.

Nunca sobrescrever histórico anterior.

```sql
CREATE TABLE job_attempts (
    id              INTEGER PRIMARY KEY,

    job_id          TEXT NOT NULL,

    attempt_no      INTEGER NOT NULL,

    worker_id       TEXT,

    started_at      TEXT NOT NULL,
    finished_at     TEXT,

    duration_ms     INTEGER,

    result          TEXT NOT NULL
                    CHECK (
                        result IN (
                            'RUNNING',
                            'SUCCESS',
                            'RETRY',
                            'FAILURE'
                        )
                    ),

    error_code      TEXT,
    error_message   TEXT,

    created_at      TEXT NOT NULL,

    FOREIGN KEY(job_id)
        REFERENCES jobs(id),

    UNIQUE(job_id, attempt_no)
) STRICT;
```

---

# 14. Tabela `presets`

Mantém as versões das configurações de processamento.

```sql
CREATE TABLE presets (
    id              TEXT PRIMARY KEY,

    name            TEXT NOT NULL,
    version         INTEGER NOT NULL,

    config_json     TEXT NOT NULL,
    config_hash     TEXT NOT NULL,

    active          INTEGER NOT NULL DEFAULT 1
                    CHECK(active IN (0, 1)),

    created_at      TEXT NOT NULL,

    UNIQUE(name, version)
) STRICT;
```

Exemplo:

```text
sports-web / v1
sports-web / v2
thumbnail / v1
```

---

# 15. Tabela `artifacts`

Representa os resultados do processamento.

```sql
CREATE TABLE artifacts (
    id                  TEXT PRIMARY KEY,

    photo_id            TEXT NOT NULL,

    file_id             TEXT NOT NULL,

    preset_id           TEXT,

    processor           TEXT NOT NULL,
    processor_version   TEXT NOT NULL,

    status              TEXT NOT NULL
                        CHECK (
                            status IN (
                                'CREATING',
                                'READY',
                                'INVALID',
                                'SUPERSEDED'
                            )
                        ),

    sha256              TEXT NOT NULL,

    created_at          TEXT NOT NULL,

    FOREIGN KEY(photo_id)
        REFERENCES photos(id),

    FOREIGN KEY(file_id)
        REFERENCES photo_files(id),

    FOREIGN KEY(preset_id)
        REFERENCES presets(id)
) STRICT;
```

## Regra

Novo preset:

```text
sports-web-v1
        ↓
Artifact A

sports-web-v2
        ↓
Artifact B
```

Nunca sobrescrever silenciosamente o Artifact A.

---

# 16. Tabela `export_destinations`

Define destinos de exportação.

```sql
CREATE TABLE export_destinations (
    id              TEXT PRIMARY KEY,

    name            TEXT NOT NULL,

    type            TEXT NOT NULL
                    CHECK (
                        type IN (
                            'LOCAL_FOLDER',
                            'MANIFEST',
                            'MOCK'
                        )
                    ),

    config_json     TEXT NOT NULL,

    enabled         INTEGER NOT NULL DEFAULT 1
                    CHECK(enabled IN (0, 1)),

    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
) STRICT;
```

Exemplo:

```json
{
  "path": "./data/exports/corrida_001"
}
```

Segredos não devem ser armazenados em `config_json`.

---

# 17. Tabela `exports`

Representa a entrega lógica de um artefato para um destino.

```sql
CREATE TABLE exports (
    id                  TEXT PRIMARY KEY,

    artifact_id         TEXT NOT NULL,
    destination_id      TEXT NOT NULL,

    status              TEXT NOT NULL
                        CHECK (
                            status IN (
                                'PENDING',
                                'EXPORTING',
                                'EXPORTED',
                                'VERIFIED',
                                'FAILED'
                            )
                        ),

    destination_path    TEXT,
    receipt_id          TEXT,

    sha256              TEXT,

    created_at          TEXT NOT NULL,
    exported_at         TEXT,
    verified_at         TEXT,

    FOREIGN KEY(artifact_id)
        REFERENCES artifacts(id),

    FOREIGN KEY(destination_id)
        REFERENCES export_destinations(id),

    UNIQUE(artifact_id, destination_id)
) STRICT;
```

Essa restrição é importante para impedir exportações duplicadas.

---

# 18. Tabela `export_attempts`

Mantém o histórico das tentativas de exportação.

```sql
CREATE TABLE export_attempts (
    id              INTEGER PRIMARY KEY,

    export_id       TEXT NOT NULL,

    attempt_no      INTEGER NOT NULL,

    started_at      TEXT NOT NULL,
    finished_at     TEXT,

    result          TEXT NOT NULL
                    CHECK (
                        result IN (
                            'RUNNING',
                            'SUCCESS',
                            'RETRY',
                            'FAILURE'
                        )
                    ),

    bytes_written   INTEGER,

    error_code      TEXT,
    error_message   TEXT,

    FOREIGN KEY(export_id)
        REFERENCES exports(id),

    UNIQUE(export_id, attempt_no)
) STRICT;
```

---

# 19. Tabela `quarantine_entries`

Registra formalmente arquivos ou fotografias colocados em quarentena.

```sql
CREATE TABLE quarantine_entries (
    id                  TEXT PRIMARY KEY,

    photo_id            TEXT,
    file_id             TEXT,

    reason_code         TEXT NOT NULL,
    reason_message      TEXT,

    created_at          TEXT NOT NULL,

    resolved_at         TEXT,
    resolution          TEXT,

    FOREIGN KEY(photo_id)
        REFERENCES photos(id),

    FOREIGN KEY(file_id)
        REFERENCES photo_files(id)
) STRICT;
```

A existência de um arquivo em `/quarantine` não deve ser a única evidência do problema.

---

# 20. Tabela `audit_events`

Trilha de auditoria append-only.

```sql
CREATE TABLE audit_events (
    id              INTEGER PRIMARY KEY,

    entity_type     TEXT NOT NULL,
    entity_id       TEXT NOT NULL,

    action          TEXT NOT NULL,

    actor_type      TEXT NOT NULL,
    actor_id        TEXT,

    request_id      TEXT,

    metadata_json   TEXT,

    created_at      TEXT NOT NULL
) STRICT;
```

Exemplos:

```text
PHOTO / ACCEPTED
JOB / LEASE_ACQUIRED
JOB / RETRY_SCHEDULED
ARTIFACT / CREATED
EXPORT / VERIFIED
FILE / QUARANTINED
```

---

# 21. Tabela `schema_migrations`

Controla a evolução do banco.

```sql
CREATE TABLE schema_migrations (
    version         INTEGER PRIMARY KEY,

    name            TEXT NOT NULL,

    checksum        TEXT NOT NULL,

    applied_at      TEXT NOT NULL
) STRICT;
```

Exemplo:

```text
001_initial_schema
002_add_artifacts
003_add_exports
004_add_audit_events
```

---

# 22. Relacionamentos

```text
EVENT
 │
 └────< PHOTO >──── DEVICE
          │
          ├────< INGESTION_REQUEST
          │
          ├────< PHOTO_FILE
          │          │
          │          └────< FILE_INTEGRITY
          │
          ├────< JOB
          │          │
          │          └────< JOB_ATTEMPT
          │
          ├────< ARTIFACT
          │          │
          │          ├──── PRESET
          │          │
          │          └────< EXPORT
          │                     │
          │                     ├──── EXPORT_DESTINATION
          │                     │
          │                     └────< EXPORT_ATTEMPT
          │
          ├────< QUARANTINE_ENTRY
          │
          └────< AUDIT_EVENT
```

---

# 23. Configuração SQLite recomendada

```sql
PRAGMA journal_mode = WAL;

PRAGMA foreign_keys = ON;

PRAGMA synchronous = FULL;

PRAGMA busy_timeout = 5000;
```

## Justificativa

### `WAL`

Permite boa concorrência entre leituras e escrita.

### `foreign_keys`

Garante integridade referencial.

### `synchronous = FULL`

Prioriza durabilidade.

É coerente com a garantia:

```text
se a API respondeu 202 Accepted
        ↓
o servidor já assumiu responsabilidade
        ↓
a fotografia não pode ser perdida silenciosamente
```

### `busy_timeout`

Evita que contenções curtas causem erro imediato.

---

# 24. Sistema de arquivos

SQLite não deve armazenar os bytes das fotografias.

```text
data/
├── db/
│   └── orchestrator.db
│
├── incoming/
│   └── .tmp/
│
├── originals/
│   └── <event_id>/<yyyy-mm-dd>/<photo_id>.<ext>
│
├── processed/
│   └── <event_id>/<preset>/<artifact_id>.<ext>
│
├── thumbnails/
│   └── <event_id>/<photo_id>.jpg
│
├── quarantine/
│
├── exports/
│
└── backups/
```

---

# 25. Relação SQLite + filesystem

```text
                  SQLite
                     │
      ┌──────────────┼───────────────┐
      │              │               │
   metadata       estados           jobs
      │              │               │
      └──────────────┼───────────────┘
                     │
                     ▼
                file_id/path
                     │
                     ▼
                Filesystem
                     │
       ┌─────────────┼──────────────┐
       │             │              │
   originals      processed       exports
```

---

# 26. Reconciliação

SQLite e filesystem não compartilham uma única transação ACID.

Por isso o sistema deve possuir rotina explícita de recovery.

## Caso A

```text
arquivo existe
+
registro não foi concluído
        ↓
recovery analisa arquivo
        ↓
verifica hash
        ↓
completa estado
```

## Caso B

```text
banco diz STORED
+
arquivo não existe
        ↓
FAILED / QUARANTINE
```

## Caso C

```text
arquivo existe
+
hash diverge
        ↓
QUARANTINE
```

## Caso D

```text
.part antigo
+
nenhuma ingestão ativa
        ↓
cleanup após janela segura
```

---

# 27. Estratégia de IDs

Para entidades expostas pela API:

```text
ULID
```

Recomendado para:

```text
event_id
photo_id
job_id
artifact_id
export_id
file_id
```

Para tabelas internas de alto volume:

```text
INTEGER PRIMARY KEY
```

pode ser utilizado em:

```text
job_attempts
export_attempts
audit_events
file_integrity
```

---

# 28. Índices mínimos adicionais

```sql
CREATE INDEX idx_photos_event_received
ON photos(event_id, received_at);
```

```sql
CREATE INDEX idx_photos_status
ON photos(status, received_at);
```

```sql
CREATE INDEX idx_photo_files_photo
ON photo_files(photo_id);
```

```sql
CREATE INDEX idx_artifacts_photo
ON artifacts(photo_id);
```

```sql
CREATE INDEX idx_exports_status
ON exports(status, created_at);
```

```sql
CREATE INDEX idx_audit_entity
ON audit_events(entity_type, entity_id, created_at);
```

---

# 29. Fases recomendadas de implementação

## Fase 1 — núcleo durável

```text
events
devices
photos
ingestion_requests
photo_files
jobs
job_attempts
schema_migrations
```

Objetivo:

```text
upload
→ persistência
→ idempotência
→ fila
→ restart
→ recuperação
```

---

## Fase 2 — processamento

Adicionar:

```text
presets
artifacts
file_integrity
quarantine_entries
```

Objetivo:

```text
original
→ processamento
→ derivado versionado
→ validação
```

---

## Fase 3 — exportação

Adicionar:

```text
export_destinations
exports
export_attempts
```

Objetivo:

```text
artifact
→ export
→ retry
→ verificação
```

---

## Fase 4 — auditoria

Adicionar:

```text
audit_events
```

Objetivo:

```text
request_id
   ↓
photo_id
   ↓
job_id
   ↓
artifact_id
   ↓
export_id
```

---

# 30. Estrutura mínima recomendada para V1

Caso seja necessário começar com uma estrutura menor:

```text
events
devices
photos
ingestion_requests
photo_files
jobs
job_attempts
schema_migrations
```

Esse conjunto já permite construir:

```text
cliente
  ↓
HTTP upload
  ↓
idempotência
  ↓
arquivo .part
  ↓
original
  ↓
SQLite
  ↓
job
  ↓
worker
```

com recuperação após reinicialização.

---

# 31. Estrutura completa recomendada

```text
photo-edge-orchestrator.db
│
├── CORE
│   ├── events
│   ├── devices
│   └── photos
│
├── INGESTION
│   └── ingestion_requests
│
├── STORAGE
│   ├── photo_files
│   └── file_integrity
│
├── QUEUE
│   ├── jobs
│   └── job_attempts
│
├── PROCESSING
│   ├── presets
│   └── artifacts
│
├── EXPORTING
│   ├── export_destinations
│   ├── exports
│   └── export_attempts
│
└── CONTROL
    ├── quarantine_entries
    ├── audit_events
    └── schema_migrations
```

---

# 32. Princípio central

A robustez do banco não deve depender apenas do SQLite.

Ela surge da combinação:

```text
SQLite
+
filesystem imutável
+
idempotência
+
jobs persistentes
+
leases
+
tentativas
+
hashes
+
reconciliação
+
auditoria
+
backups
```

O requisito central do sistema deve permanecer:

> Depois que a API responde `202 Accepted`, o sistema assumiu responsabilidade durável pela fotografia.

Portanto, qualquer reinicialização ou falha intermediária deve resultar em:

```text
retomada
```

ou:

```text
estado de erro explicitamente rastreável
```

e nunca em perda silenciosa.

---

# 33. Regra arquitetural final

```text
Foto lógica
    ≠
arquivo físico
    ≠
job
    ≠
artifact
    ≠
exportação
```

Cada conceito deve possuir identidade e ciclo de vida próprios.

Essa separação permite que o Photo Edge Orchestrator cresça sem transformar `photos` em uma tabela central excessivamente acoplada e difícil de evoluir.

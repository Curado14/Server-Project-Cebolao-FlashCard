# Photo Edge Orchestrator

## Responsabilidade e fronteiras

O processo recebe JPEGs autenticados na rede local, assume responsabilidade somente depois da durabilidade composta em filesystem e SQLite, e oferece processamento/exportacao local. SQLite e a fonte operacional da verdade; canais de wake-up nao fazem parte da fila.

Modulos observados: configuracao/roots, API HTTP, persistencia/migracoes, filestore, fila/processamento, exportacao local e observabilidade local. O processo nao abre conexoes externas nem armazena tokens em claro.

## Entrypoints

- HTTP: `POST /api/v1/photos`, `GET /api/v1/photos/{id}`, `GET /api/v1/events/{id}/summary`, `/health/live`, `/health/ready`, `/metrics`.
- Worker: `StartWorkers` consulta jobs persistentes e chama `ProcessPending`.
- Inicializacao: `Open` cria roots, migra SQLite, provisiona evento/dispositivo/preset e recupera leases vencidos.
- Observabilidade: `AccessLog` envolve o mux do servico, gera `request_id` por tentativa HTTP, emite NDJSON via `log/slog` em stdout e expoe metricas Prometheus locais sem labels de entidade.

## Modelo de dados critico

| Entidade | Papel | Constraints | Evidencia |
| --- | --- | --- | --- |
| photos | identidade e estado da foto | `UNIQUE(device_id,idempotency_key)`, FK event/device | `internal/orchestrator/db.go`, `service.go:ingest` |
| photo_files | caminho, tamanho e hash | FK photo, unicidade por tipo | `db.go`, `processing.go` |
| jobs | fila at-least-once | `operation_key` unico, estados e lease | `db.go`, `processing.go:ProcessPending` |
| artifacts/exports | resultados e entrega | preset versionado, unicidades | `db.go`, `processing.go`, `export.go` |
| audit_events | trilha append-only de aceite, replay, jobs, recovery, quarentena e exportacao | migration v2 adiciona `job_id`, `event_id`, `camera_id`, `status`, `error_code` e indices por request/job | `db.go:migrateAuditV2`, `service.go:audit` |

## Invariantes locais

- Caminhos fisicos sao derivados pelo servidor e permanecem sob `Root` (`storage.go:safePart`, `within`).
- O original e gravado e sincronizado antes da transacao de aceite; nao e sobrescrito pelo processamento.
- A fila sobrevive a reinicio e lease vencido volta a `PENDING` (`service.go:recover`).
- Exportacao so consolida `VERIFIED` depois de copiar, sincronizar e conferir hash (`export.go:ExportArtifact`).
- `request_id` identifica a tentativa HTTP e nao e a `Idempotency-Key`; replays da mesma foto gravam nova linha de auditoria com outro `request_id`.
- Logs, auditoria e metricas usam allowlist de campos e nao registram token, Authorization, chave idempotente bruta, corpo de imagem ou EXIF.

## Observabilidade local

Cada requisicao em `s.Handler()` passa por `AccessLog`, que emite `http.request_completed` como JSON em stdout com campos base `time`, `level`, `msg`, `event`, `service`, `version`, `instance_id` e `component`, alem de `request_id`, metodo, path, status e duracao. Eventos de ingestao (`photo.validation_failed`, `photo.persisted`, `photo.accepted`, `idempotency.replayed`, `photo.rejected`), worker (`job.started`, `job.completed`, `job.retry_scheduled`, `job.quarantined`), recovery (`job.recovered_after_restart`), storage (`storage.write_failed`, `storage.low_space`), banco (`database.unavailable`, `database.slow_query`), ciclo de vida (`service.started`, `service.ready`, `shutdown.started`, `shutdown.completed`) e exportacao (`photo.exported`) sao emitidos quando a transicao real ocorre.

SQLite permanece a fonte de verdade: logs sao diagnosticos e metricas sao sinais operacionais. A trilha de auditoria guarda a correlacao consultavel por `request_id`, `photo_id`, `job_id`, `event_id`, `camera_id`, `status` e `error_code` quando esses campos existem na transicao. Falha de escrita de log diagnostico depois do startup nao altera o resultado de negocio e incrementa `photoedge_logging_errors_total`.

## Gaps

Capacidade real do Lenovo, power-loss fisico, politica juridica de retencao e qualidade visual permanecem fora da evidencia automatizada.

# Photo Edge Orchestrator

## Responsabilidade e fronteiras

O processo recebe JPEGs autenticados na rede local, assume responsabilidade somente depois da durabilidade composta em filesystem e SQLite, e oferece processamento/exportacao local. SQLite e a fonte operacional da verdade; canais de wake-up nao fazem parte da fila.

Modulos observados: configuracao/roots, API HTTP, persistencia/migracoes, filestore, fila/processamento, exportacao local e health/metricas. O processo nao abre conexoes externas nem armazena tokens em claro.

## Entrypoints

- HTTP: `POST /api/v1/photos`, `GET /api/v1/photos/{id}`, `GET /api/v1/events/{id}/summary`, `/health/live`, `/health/ready`, `/metrics`.
- Worker: `StartWorkers` consulta jobs persistentes e chama `ProcessPending`.
- Inicializacao: `Open` cria roots, migra SQLite, provisiona evento/dispositivo/preset e recupera leases vencidos.

## Modelo de dados critico

| Entidade | Papel | Constraints | Evidencia |
| --- | --- | --- | --- |
| photos | identidade e estado da foto | `UNIQUE(device_id,idempotency_key)`, FK event/device | `internal/orchestrator/db.go`, `service.go:ingest` |
| photo_files | caminho, tamanho e hash | FK photo, unicidade por tipo | `db.go`, `processing.go` |
| jobs | fila at-least-once | `operation_key` unico, estados e lease | `db.go`, `processing.go:ProcessPending` |
| artifacts/exports | resultados e entrega | preset versionado, unicidades | `db.go`, `processing.go`, `export.go` |

## Invariantes locais

- Caminhos fisicos sao derivados pelo servidor e permanecem sob `Root` (`storage.go:safePart`, `within`).
- O original e gravado e sincronizado antes da transacao de aceite; nao e sobrescrito pelo processamento.
- A fila sobrevive a reinicio e lease vencido volta a `PENDING` (`service.go:recover`).
- Exportacao so consolida `VERIFIED` depois de copiar, sincronizar e conferir hash (`export.go:ExportArtifact`).

## Gaps

Capacidade real do Lenovo, power-loss fisico, politica juridica de retencao e qualidade visual permanecem fora da evidencia automatizada.

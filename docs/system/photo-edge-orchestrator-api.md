# API local

## Autenticacao e ingestao

`POST /api/v1/photos` exige `Authorization: Bearer <device token>` e `Idempotency-Key`, recebe `multipart/form-data` com campo `photo` e opcional `event_id`. A autenticacao consulta o dispositivo persistido por `id`, exige `enabled=1` e compara em tempo constante o hash SHA-256 do token recebido com `devices.token_hash`; o token nunca e persistido ou exposto.

Cada chamada recebe um `request_id` proprio no middleware HTTP. Se `X-Request-Id` vier ausente, invalido ou igual a `Idempotency-Key`, o servidor gera um novo identificador ASCII limitado e o devolve em `X-Request-Id`. O valor de `Idempotency-Key` bruta nao e copiado para logs ou auditoria; quando necessario, a auditoria usa `idempotency_key_hash` SHA-256 truncado para 16 hexadecimais.

O arquivo e limitado por `MaxUploadBytes`, decodificado como JPEG e validado por dimensoes/pixels. O nome enviado e apenas metadado sanitizado. O servidor cria o caminho fisico, calcula SHA-256, sincroniza o temporario, renomeia para `originals/<event>/<date>/<photo>.jpg` e somente entao confirma foto, arquivo, request, job e auditoria na mesma transacao SQLite.

## Respostas

- `202`: `photo_id`, `status`, `received_at` e `status_url`; replay da mesma chave e bytes devolve a mesma identidade.
- `401`: autenticacao ausente/invalida, token revogado ou dispositivo desabilitado; nenhuma foto, arquivo, job ou auditoria de aceite e criada.
- `409`: chave idempotente ja vinculada a bytes diferentes.
- `400`, `413`, `415`, `422` e `503`: entradas invalidas, limite, formato, JPEG invalido ou storage indisponivel, com Problem Details sanitizado.

Replays idempotentes com os mesmos bytes continuam retornando `202` e a mesma identidade de foto. A tentativa de replay grava nova linha em `audit_events` com `action='REPLAYED'` e `request_id` distinto, sem duplicar `photos`, `ingestion_requests`, `photo_files` ou `jobs`.

`GET /api/v1/photos/{id}` retorna estado, caminho controlado, hash e tamanho. Health separa processo vivo (`/health/live`) de capacidade (`/health/ready`).

## Health e metricas

`/health/live` retorna o processo vivo com `version` e `instance_id`. `/health/ready` retorna `200` somente quando root, SQLite ping, migration v2, `PRAGMA integrity_check`, medicao de espaco livre e probe de escrita passam; em falha retorna `503` com Problem Details sanitizado. O intake repete essa verificacao antes de autenticar e consumir o upload.

`/metrics` verifica o endereco real do cliente e responde `403` a clientes que nao sejam loopback. Quando autorizado, expoe Prometheus `text/plain; version=0.0.4` com series de volume, latencia, fila, workers, storage, SQLite e logging. As metricas nao usam labels de entidade; os aliases temporarios `photoedge_accepted_total` e `photoedge_rejected_total` leem os mesmos contadores canonicos.

## Observabilidade HTTP

`AccessLog` emite uma linha JSON por requisicao concluida com `event=http.request_completed`, `service=photo-edge-orchestrator`, `version`, `instance_id`, `component=http`, `request_id`, metodo, path, status, codigo de erro sanitizado e duracao. O log nao copia `Authorization`, token, chave idempotente bruta, corpo, EXIF, `remote_addr` ou `User-Agent`.

## Evidencias

- Implementacao: `internal/orchestrator/service.go:photos`, `ingest`, `auth`, `readyCheck`, `metrics`; `internal/orchestrator/access_log.go:AccessLog`.
- Testes: `internal/orchestrator/protected_test.go:TestProtectedIdempotencyConcurrent`, `TestProtectedHostileLimitFuzz`, `TestProtectedPrivacyLocalOnlyReadyHealthMetricsAuditSecret`; `internal/orchestrator/robust_observability_contract_test.go:TestContractRequestCorrelationReplayAndTelemetryPrivacy`, `TestContractMetricsAliasesAndCountersAreConcurrencySafe`, `TestContractPendingMigrationMakesReadinessAndIntakeUnavailable`.

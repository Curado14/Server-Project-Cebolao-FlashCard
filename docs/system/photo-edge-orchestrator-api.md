# API local

## Autenticacao e ingestao

`POST /api/v1/photos` exige `Authorization: Bearer <device token>` e `Idempotency-Key`, recebe `multipart/form-data` com campo `photo` e opcional `event_id`. A autenticacao consulta o dispositivo persistido por `id`, exige `enabled=1` e compara em tempo constante o hash SHA-256 do token recebido com `devices.token_hash`; o token nunca e persistido ou exposto.

O arquivo e limitado por `MaxUploadBytes`, decodificado como JPEG e validado por dimensoes/pixels. O nome enviado e apenas metadado sanitizado. O servidor cria o caminho fisico, calcula SHA-256, sincroniza o temporario, renomeia para `originals/<event>/<date>/<photo>.jpg` e somente entao confirma foto, arquivo, request, job e auditoria na mesma transacao SQLite.

## Respostas

- `202`: `photo_id`, `status`, `received_at` e `status_url`; replay da mesma chave e bytes devolve a mesma identidade.
- `401`: autenticacao ausente/invalida, token revogado ou dispositivo desabilitado; nenhuma foto, arquivo, job ou auditoria de aceite e criada.
- `409`: chave idempotente ja vinculada a bytes diferentes.
- `400`, `413`, `415`, `422` e `503`: entradas invalidas, limite, formato, JPEG invalido ou storage indisponivel, com Problem Details sanitizado.

`GET /api/v1/photos/{id}` retorna estado, caminho controlado, hash e tamanho. Health separa processo vivo (`/health/live`) de capacidade (`/health/ready`). Metricas locais expõem contadores sem token, bytes de imagem ou payload livre.

## Evidencias

- Implementacao: `internal/orchestrator/service.go:photos`, `ingest`, `auth`.
- Testes: `internal/orchestrator/protected_test.go:TestProtectedIdempotencyConcurrent`, `TestProtectedHostileLimitFuzz`, `TestProtectedPrivacyLocalOnlyReadyHealthMetricsAuditSecret`.

`/metrics` verifica o endereco real do cliente e responde `403` a clientes que nao sejam loopback. O readiness nao anuncia banco saudavel quando ping, integridade ou probe de escrita falham.

# Operacao local

## Inicializacao

`Config.Validate` exige root, bind, device/evento, token, `instance_id` e limites positivos. O bind padrao e `127.0.0.1:8090` e permanece loopback-only quando definido diretamente no objeto `Config`. Quando `PHOTOEDGE_BIND` e fornecido via ambiente, `ConfigFromEnv` marca `AllowNetworkBind=true` e permite bind LAN explicito, preservando o uso local por iPhone/Android na mesma rede.

`PHOTOEDGE_INSTANCE_ID` define a identidade local sanitizada do processo; sem essa variavel, o servico usa hostname sanitizado e cai para `local-instance` se necessario. `PHOTOEDGE_MIN_FREE_BYTES` configura o minimo de espaco livre para readiness/intake e aceita somente inteiro positivo; o default e `1073741824` bytes.

`Open` cria roots, abre SQLite, aplica migrations, provisiona evento/dispositivo/preset, executa recovery e so entao marca o servico pronto. Falha ao construir o logger JSON impede inicializacao segura; eventos diagnosticos depois do startup sao best-effort, nao substituem auditoria e, quando a escrita do logger retorna erro, incrementam `photoedge_logging_errors_total` sem alterar a resposta HTTP ou a transacao de negocio.

## Saude e readiness

Readiness verifica root, SQLite ping, `schema_migrations.version=2` e colunas v2 de auditoria, `PRAGMA integrity_check`, medicao de espaco livre, limite minimo de espaco e probe de escrita local. Falha em qualquer item retorna `503` e o intake tambem retorna `503` antes de autenticar ou ler bytes do upload. Liveness apenas informa que o processo esta vivo e inclui `version` e `instance_id`.

O limite de espaco e fechado por padrao: se a medicao falha ou se o espaco livre e menor ou igual a `PHOTOEDGE_MIN_FREE_BYTES`, o servico nao fica ready. A metrica `photoedge_storage_free_bytes` recebe a ultima medicao bem-sucedida.

## Logs

O logger usa `log/slog` com `JSONHandler` em stdout. Cada linha e um objeto JSON com `time` UTC RFC3339Nano, `level`, `msg`, `event`, `service=photo-edge-orchestrator`, `version`, `instance_id` e `component`. Campos contextuais incluem somente valores da allowlist implementada: `request_id`, `photo_id`, `job_id`, `event_id`, `camera_id`, `attempt`, `duration_ms`, `status_code`, `error_code`, `bytes`, `mime_type`, `sha256_prefix`, `idempotency_key_hash`, `method` e `path` quando aplicaveis.

Eventos implementados incluem `http.request_completed`, `auth.failed`, `idempotency.replayed`, `photo.validation_failed`, `photo.persisted`, `photo.accepted`, `photo.rejected`, `job.started`, `job.completed`, `job.retry_scheduled`, `job.quarantined`, `job.recovered_after_restart`, `storage.write_failed`, `storage.low_space`, `database.unavailable`, `database.slow_query`, `service.started`, `service.ready`, `shutdown.started`, `shutdown.completed`, `photo.exported` e falhas de exportacao. Logs nao carregam token, Authorization, chave idempotente bruta, corpo de imagem, EXIF, caminho absoluto, `remote_addr`, `User-Agent` ou `response_bytes`.

`database.slow_query` usa limite estrito de 100 ms na consulta de integridade do readiness e na selecao de job do worker, incluindo espera pela conexao SQLite. Nao ha instrumentacao global de todas as consultas. `database.unavailable` identifica falhas de acesso ao banco no readiness, intake e selecao de job, sem SQL ou mensagem livre. `storage.write_failed` identifica falhas de criacao/escrita/sync/fechamento do processado, escrita/rename do original e exportacao, e movimento/restauracao da quarentena com codigos estaveis. `photo.validation_failed` acompanha rejeicoes de campos, formato, limite e JPEG no intake.

O handler do logger conta erros retornados pelo writer sem tentar registrar outro log de erro, evitando recursao. A contagem permanece ativa em loggers derivados com atributos ou grupos. Falha diagnostica nao impede aceite duravel nem conclusao do worker (`TestReviewWriterFailureAcrossBusinessLifecycle`); o teste `TestReviewSlowWorkerQuery` verifica a medicao durante uma espera real pela conexao SQLite.

O evento `job.lease_acquired` e DEBUG e ocorre somente apos commit da reserva, com `photo_id`, `job_id` e `attempt`; o nivel INFO padrao o filtra. `TestReviewLeaseEventAfterCommit` captura DEBUG e prova ausencia de evento quando a auditoria causa rollback.

## Metricas

`GET /metrics` e restrito a loopback e usa exposicao Prometheus `text/plain; version=0.0.4`. As series canonicas sao:

| Nome | Tipo |
| --- | --- |
| `photoedge_photos_accepted_total` | counter |
| `photoedge_photos_rejected_total` | counter |
| `photoedge_http_request_duration_ms` | histogram |
| `photoedge_photo_persist_duration_ms` | histogram |
| `photoedge_queue_depth` | gauge |
| `photoedge_jobs_active` | gauge |
| `photoedge_jobs_completed_total` | counter |
| `photoedge_jobs_failed_total` | counter |
| `photoedge_jobs_quarantined_total` | counter |
| `photoedge_storage_free_bytes` | gauge |
| `photoedge_sqlite_errors_total` | counter |
| `photoedge_logging_errors_total` | counter |

Os aliases temporarios `photoedge_accepted_total` e `photoedge_rejected_total` expoem os mesmos valores dos contadores canonicos, sem contador independente. As metricas nao usam labels por foto, job, request, idempotencia, path, IP, filename, token ou mensagem livre; histogramas usam apenas `le`.

## Harness de robustez local

`tests/robustness/runner.py` e uma suite operacional Python local para validar o servico em modo black-box/gray-box, sem dependencia externa e sem alterar o processo Go. O operador inicia o Photo Edge separadamente com `PHOTOEDGE_*` apontando para root e SQLite descartaveis; o harness le `PHOTOEDGE_ROBUST_*` de `tests/robustness/config.local.env` ou do ambiente e recusa o `data/` do repositorio como root ou banco.

Os cenarios automaticos sao `smoke`, `single-upload`, `sequential-upload`, `concurrent-upload`, `invalid-inputs`, `idempotency` e `all`. Uploads usam `POST /api/v1/photos`, Bearer, `Idempotency-Key`, multipart `photo` e `event_id` opcional. O runner aceita o `202` vigente com `photo_id`, `status`, `received_at` e `status_url`, e falha se o corpo depender de `job_id`.

Para cada aceite validado, SQLite permanece a fonte de verdade: o runner abre o banco em `mode=ro`, consulta `photos`, `photo_files`, `jobs` e `audit_events`, e verifica que o original persistido existe sob o root configurado. Logs NDJSON sao opcionais e so sao lidos quando o operador fornece um arquivo de captura de stdout; ausencia de captura aparece no relatorio como `logs=not-run`, nao como evidencia de produto. Metricas sao coletadas somente quando a base URL e loopback e sao verificadas contra o formato Prometheus e a politica sem labels de entidade.

Cada execucao grava relatorios JSON e Markdown em `tests/robustness/reports/`, ignorados pelo Git. Os relatorios registram comando, ambiente Python/plataforma, configuracao sanitizada, contagens HTTP, latencias, aceites, rejeicoes esperadas, inconsistencias, limitacoes e cobertura REQ/PO/EVD. Token, header Authorization, chave idempotente bruta e bytes de fixture sao redigidos e verificados por testes unitarios do harness.

Restart recovery, degradacao de disco, carga maior, celular real e power-loss fisico permanecem procedimentos SYSTEM manuais em root descartavel e exigem aprovacao explicita antes de execucao.

## Falhas e mitigacao

Falha de limite, autenticacao ou JPEG e rejeitada sem aceite. Falha de banco, migration, integridade, probe ou storage impede resposta `202`. Falha de worker permanece observavel no job, em `job_attempts`, auditoria e metricas, podendo retornar por retry. Exportacao indisponivel nao e consolidada como sucesso.

## Backup, rollback e privacidade

Backups devem ser feitos antes de migracoes e manter banco e originais necessarios. Rollback de deploy retira readiness e preserva dados, leases, originais e quarentena; nao ha retencao automatica de dados aceitos. A compensacao de intake remove somente o arquivo da tentativa nao aceita; falhas usam `original_cleanup_failed` ou `original_cleanup_unconfirmed` em `storage.write_failed`, sem caminhos ou erros livres. EXIF/GPS nao e extraido nem copiado para respostas. Tokens sao armazenados apenas como hash.

## Evidencias

- Implementacao: `internal/orchestrator/config.go:ConfigFromEnv`, `Validate`; `access_log.go:NewJSONLogger`, `AccessLog`; `service.go:Open`, `Close`, `checkReadiness`, `metrics`, `observeHTTP`; `metrics.go:writeMetrics`.
- Testes: `TestContractJSONLog`, `TestContractTelemetryPrivacy`, `TestContractCanonicalFailureEvents`, `TestContractLoggerFailure`, `TestContractMinFreeBytesConfig`, `TestContractReadinessFaultMatrix`, `TestContractMetricsAliasesAndCountersAreConcurrencySafe`, `TestContractLocalOnly`, `TestProtectedPrivacyLocalOnlyReadyHealthMetricsAuditSecret`.
- Harness: `tests/robustness/runner.py`, `tests/robustness/core.py`, `tests/robustness/test_config_and_reports.py`, `tests/robustness/test_contract_scenarios.py`.

## Gaps

Teste de carga em Lenovo, permissao real de instalacao, power-loss fisico, retencao juridica final, validacao mobile real, restart recovery automatizado, degradacao de disco automatizada e validacao SYSTEM em Windows+Unix exigem evidencia operacional complementar.

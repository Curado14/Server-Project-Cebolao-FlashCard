# Dados e recovery

## Persistencia

`Open` usa SQLite em `data/db/orchestrator.db` com WAL, foreign keys, `synchronous=FULL`, busy timeout e uma conexao operacional serializada. A migracao cria schema strict para eventos, dispositivos, fotos, requests, arquivos, jobs/attempts, presets, artifacts, destinos/exports, quarentena e auditoria.

`migrateAuditV2` e uma migracao aditiva e idempotente sobre `audit_events`. Ela preserva linhas antigas, adiciona colunas nullable `job_id`, `event_id`, `camera_id`, `status` e `error_code`, cria os indices `idx_audit_request(request_id, created_at)` e `idx_audit_job(job_id, created_at)`, e registra `schema_migrations.version=2` somente depois de aplicar colunas e indices com sucesso. Readiness falha quando essa versao ou as colunas obrigatorias estao ausentes.

O filesystem possui `incoming/.tmp`, `originals`, `processed`, `quarantine`, `exports`, `backups` e `db`, todos criados com modo restrito. O banco guarda metadados, estados e hashes; os bytes ficam no filesystem. Na inicializacao, `.part` stale e originais sem registro sao movidos para `quarantine` com auditoria, nunca apagados silenciosamente.

## Estados e transacoes

Uma foto aceita passa de `RECEIVING` para `STORED` na transacao final; o job nasce `PENDING`; o evento de auditoria `ACCEPTED` e gravado na mesma transacao. Se a insercao de auditoria falhar, a transacao e revertida e o HTTP nao retorna `202`.

`service.go:ingest` compensa a falha de aceite removendo somente o original definitivo criado pela tentativa, depois do rollback e antes de liberar o mutex. O replay retorna antes de registrar essa compensacao; arquivos de fotos aceitas nao sao removidos. Se houve tentativa de commit com erro, a remocao exige consulta bem-sucedida confirmando ausencia de referencias em `photos` e `photo_files`. Banco indisponivel nessa verificacao preserva o arquivo e emite `storage.write_failed` com `original_cleanup_unconfirmed`; falha de remocao emite `original_cleanup_failed`. O HTTP continua sem `202`. Isso nao fornece atomicidade contra interrupcao fisica entre filesystem e SQLite.

`processing.go:ProcessPending` emite `job.lease_acquired` em DEBUG apos o commit de lease, tentativa START e auditoria, antes de `job.started`, com `photo_id`, `job_id` e `attempt`. Rollback da reserva nao emite nenhum dos dois eventos. O nivel INFO padrao filtra o evento DEBUG.

Replays idempotentes com os mesmos bytes nao criam nova foto, request, arquivo ou job. Eles registram uma linha de auditoria `REPLAYED` para a mesma foto com um `request_id` novo da tentativa HTTP e `idempotency_key_hash` truncado, sem salvar a chave bruta.

O worker reserva atomicamente `PENDING`/`RETRY_WAIT` como `RUNNING`, registra lease, incremento de tentativa, `job_attempts.START` e auditoria `JOB/STARTED` na mesma transacao. Se a auditoria falhar, a reserva volta atrás e o job nao fica `RUNNING` sem trilha. Processa fora da transacao e finaliza `SUCCEEDED` ou `RETRY_WAIT`/`DEAD` com erro sanitizado; o fechamento de falha grava update de job, `job_attempts.FINISH` e auditoria `RETRY_SCHEDULED` ou `QUARANTINED` em uma transacao obrigatoria. O `operation_key` e unicidade de artifact impedem duplicacao logica.

O processador decodifica JPEG, gera novo arquivo JPEG em `processed`, registra preset `sports-web-v1`, dimensoes e hash, e marca a foto `READY`. Reexecucao nao altera o original.

## Recovery e exportacao

Na inicializacao, leases vencidos retornam a `PENDING` e geram auditoria `RECOVERED_AFTER_RESTART` na mesma transacao; se a auditoria falhar, o recovery retorna erro e nao confirma a transicao silenciosamente. O processamento consulta SQLite em cada ciclo. A exportacao aceita somente `LOCAL_FOLDER` ou `MANIFEST` cuja pasta esteja sob a root; `export_attempts.RUNNING` e auditoria `EXPORT/STARTED` sao transacionais antes da escrita. O fluxo escreve `.part`, sincroniza, renomeia, rele e verifica SHA-256 antes de registrar `VERIFIED`, receipt e auditoria `VERIFIED`. Falhas de exportacao atualizam tentativa/export e auditoria de falha em uma transacao obrigatoria.

Arquivos ausentes, hash divergente, originais orfaos e `.part` stale entram em quarentena com `error_code` estavel em `audit_events`; caminhos absolutos nao sao usados como identidade de auditoria para arquivos orfaos ou partes temporarias.

`service.go:quarantineFile` insere a auditoria antes do rename, dentro de uma transacao SQLite aberta. Falha no insert preserva o arquivo de origem para a proxima recuperacao; falha no rename reverte a auditoria. Se o commit falhar depois do movimento, tenta restaurar a origem e propaga ambos os erros caso a restauracao tambem falhe. Nomes de arquivo nao entram no metadata da auditoria. O contador e o evento de quarentena so sao emitidos apos commit. Recovery, ingestao, worker e exportacao compartilham o mutex do servico para evitar transicoes concorrentes no mesmo processo. SQLite e filesystem nao oferecem uma transacao conjunta: interrupcao fisica entre rename e commit continua uma limitacao operacional, sem garantia de atomicidade entre os dois meios.

A criacao de `exports`, a primeira tentativa e sua auditoria `STARTED` compartilham uma transacao. Falha na auditoria final de exportacao preserva a tentativa `RUNNING`, a exportacao `PENDING` e a foto `READY`, sem receipt confirmado. Bytes ja escritos podem permanecer no destino; uma nova chamada reexecuta a verificacao antes de confirmar o sucesso. No worker, falha na auditoria de conclusao ou retry preserva o lease `RUNNING` e a tentativa inicial, sem confirmar FINISH, artifacts ou metricas de conclusao/falha. O lease vencido permite nova recuperacao; o original nao e alterado.

## Evidencias

- Implementacao: `internal/orchestrator/db.go:migrate`, `migrateAuditV2`, `requiredSchemaReady`; `service.go:ingest`, `audit`, `recover`, `quarantine`; `processing.go:ProcessPending`; `export.go:ExportArtifact`.
- Testes: `TestProtectedSQLiteMigrationConstraintQueue`, `TestProtectedLeaseRetryAttemptStateful`, `TestProtectedArtifactPresetIntegrity`, `TestProtectedExportReceiptManifestDestination`.
- Testes adicionais: `TestProtectedCrashRecoverRestartQuarantine`, `TestContractMigrationV2IsAdditiveAndIdempotent`, `TestContractAcceptanceAuditIsAtomic`, `TestContractAuditTransitionsAppendOnly`, `TestContractCriticalAuditFailuresBlockTransitions`, `TestContractRecoveryObservability`, `TestContractExportObservability`.
- Falhas de auditoria: `review_fixes_internal_test.go:TestReviewWorkerFinalAuditRollback`, `TestReviewFileRecoveryAuditRollback`, `TestReviewPhotoQuarantineAuditRollback`, `TestReviewExportFinalAuditRollback` usam triggers SQLite reais e consultam estados/attempts/metricas apos rollback.
- Aceite e lease: `TestReviewAcceptanceFilesystemCompensation` verifica filesystem, tabelas, concorrencia, preservacao de original aceito, replay e nova tentativa; `TestReviewLeaseEventAfterCommit` verifica evento DEBUG correlacionado e ausencia no rollback.

Exportacao cria a linha logica antes da escrita, reutiliza `UNIQUE(artifact_id,destination_id)` em retries, registra `export_attempts` e retorna sem criar nova exportacao quando o destino verificado ainda possui o hash esperado. Falhas mantem a exportacao pendente para recuperacao. O fluxo completo pode fechar e reabrir o servico: o estado `VERIFIED`, receipt e cadeia de IDs sao recuperados do SQLite.

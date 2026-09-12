# Dados e recovery

## Persistencia

`Open` usa SQLite em `data/db/orchestrator.db` com WAL, foreign keys, `synchronous=FULL`, busy timeout e uma conexao operacional serializada. A migracao cria schema strict para eventos, dispositivos, fotos, requests, arquivos, jobs/attempts, presets, artifacts, destinos/exports, quarentena e auditoria.

O filesystem possui `incoming/.tmp`, `originals`, `processed`, `quarantine`, `exports`, `backups` e `db`, todos criados com modo restrito. O banco guarda metadados, estados e hashes; os bytes ficam no filesystem. Na inicializacao, `.part` stale e originais sem registro sao movidos para `quarantine` com auditoria, nunca apagados silenciosamente.

## Estados e transacoes

Uma foto aceita passa de `RECEIVING` para `STORED` na transacao final; o job nasce `PENDING`. O worker reserva atomicamente `PENDING`/`RETRY_WAIT` como `RUNNING`, registra lease e incremento de tentativa, e grava eventos imutaveis `START` e `FINISH` append-only em `job_attempts`. Processa fora da transacao e finaliza `SUCCEEDED` ou `RETRY_WAIT`/`DEAD` com erro sanitizado. O `operation_key` e unicidade de artifact impedem duplicacao logica.

O processador decodifica JPEG, gera novo arquivo JPEG em `processed`, registra preset `sports-web-v1`, dimensoes e hash, e marca a foto `READY`. Reexecucao nao altera o original.

## Recovery e exportacao

Na inicializacao, leases vencidos retornam a `PENDING`; o processamento consulta SQLite em cada ciclo. A exportacao aceita somente `LOCAL_FOLDER` ou `MANIFEST` cuja pasta esteja sob a root, escreve `.part`, sincroniza, renomeia, relê e verifica SHA-256 antes de registrar `VERIFIED` e receipt.

## Evidencias

- Implementacao: `internal/orchestrator/db.go:migrate`, `processing.go:ProcessPending`, `export.go:ExportArtifact`.
- Testes: `TestProtectedSQLiteMigrationConstraintQueue`, `TestProtectedLeaseRetryAttemptStateful`, `TestProtectedArtifactPresetIntegrity`, `TestProtectedExportReceiptManifestDestination`.
- Testes adicionais: `TestProtectedCrashRecoverRestartQuarantine`.

Exportacao cria a linha logica antes da escrita, reutiliza `UNIQUE(artifact_id,destination_id)` em retries, registra `export_attempts` e retorna sem criar nova exportacao quando o destino verificado ainda possui o hash esperado. Falhas mantem a exportacao pendente para recuperacao. O fluxo completo pode fechar e reabrir o servico: o estado `VERIFIED`, receipt e cadeia de IDs sao recuperados do SQLite.

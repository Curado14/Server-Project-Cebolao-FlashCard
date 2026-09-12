# Operacao local

## Inicializacao

`Config.Validate` exige root, bind, device/evento, token e limites positivos. O bind deve ser loopback (`127.0.0.1`, `::1` ou `localhost`); `0.0.0.0`, `:8090` e enderecos LAN sao rejeitados. O armazenamento e provisionado localmente antes do banco e do listener. O binario padrao escuta `127.0.0.1:8090`; nenhuma configuracao de HTTP, FTP, cloud ou bind publico e carregada pelo nucleo.

## Saude e observabilidade

Readiness verifica root, ping do SQLite, `PRAGMA integrity_check` e escrita de probe local; falha com `503` antes de aceitar intake inseguro. O handler de intake repete essa verificacao antes de processar autenticacao e bytes. Liveness apenas informa que o processo esta vivo. Metricas contam aceites/rejeicoes e so respondem a clientes loopback. Auditoria liga dispositivo, chave de request e foto; jobs/artifacts/exports preservam a cadeia por IDs. Erros de API nao incluem token nem bytes.

## Falhas e mitigacao

Falha de limite, autenticacao ou JPEG e rejeitada sem aceite. Falha de banco/storage impede resposta `202`. Falha de worker permanece observavel no job e pode retornar por retry. Exportacao indisponivel nao e consolidada como sucesso.

## Backup, rollback e privacidade

Backups devem ser feitos antes de migracoes e manter banco e originais necessarios. Rollback retira readiness e preserva dados, leases, originais e quarentena; nao ha exclusao automatica. EXIF/GPS nao e extraido nem copiado para respostas. Tokens sao armazenados apenas como hash.

## Gaps

Teste de carga em Lenovo, permissao real de instalacao, power-loss fisico e politica final de EXIF/retencao exigem validacao operacional complementar.

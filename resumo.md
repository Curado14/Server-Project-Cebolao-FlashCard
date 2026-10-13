# Resumo do Projeto Photo Edge Orchestrator

## Objetivo da proposta

O Photo Edge Orchestrator resolve o problema de receber, organizar e processar fotografias esportivas durante um evento sem depender da internet ou de serviços externos. Ele cria uma camada Edge local entre o celular ou a camera e a area de entrega, reduzindo o risco de perda, duplicidade e interrupcao do trabalho causado por fluxos manuais.

O servidor roda no notebook Lenovo conectado a rede local do evento. O iPhone envia as fotos por HTTP, o sistema confirma o recebimento somente depois de gravar o arquivo e os dados de controle, e o processamento continua em segundo plano.

## O que o projeto entrega

- API HTTP local para receber fotografias JPEG.
- Autenticacao por token Bearer, com o token armazenado no banco somente como hash.
- Upload em `multipart/form-data` pelo campo `photo`, compativel com o app Atalhos do iPhone.
- Chave de idempotencia para evitar duplicacao em reenvios e permitir repetir uma requisicao com seguranca.
- Validacao de tamanho, formato JPEG, dimensoes e quantidade de pixels.
- Gravacao duravel do original no sistema de arquivos antes da resposta `202 Accepted`.
- SQLite como fonte operacional da verdade, com WAL, foreign keys, transacoes e fila persistente.
- Estados rastreaveis da foto, desde o recebimento ate o processamento.
- Worker com concorrencia controlada, tentativas, leases e recuperacao apos reinicio.
- Preservacao do original e geracao de derivado JPEG processado.
- Exportacao local para pastas autorizadas, com arquivo temporario, sincronizacao e verificacao de hash.
- Auditoria de aceite, processamento, falhas, quarentena e exportacao.
- Endpoints de liveness, readiness, status individual, resumo do evento e metricas locais.
- Logs HTTP estruturados sem expor token, bytes da imagem ou dados sensiveis.
- Quarentena para arquivos incompletos ou sem registro, evitando exclusao silenciosa.

## Fluxo entregue

```text
Captura no iPhone
    -> Atalho converte a imagem para JPEG
    -> POST autenticado /api/v1/photos
    -> arquivo original + registro SQLite
    -> resposta 202 com photo_id
    -> job persistente
    -> processamento em segundo plano
    -> status READY
    -> exportacao local opcional
```

O fluxo foi validado na rede local com uma foto enviada pelo iPhone. O servidor registrou `POST /api/v1/photos` com status `202`, recebeu aproximadamente 7,5 MB e devolveu um `photo_id` para acompanhamento.

## Respostas e acompanhamento

- `202`: foto aceita e colocada na fila.
- `400`: requisicao incompleta ou invalida, por exemplo sem `Idempotency-Key`.
- `401`: token ausente ou invalido.
- `409`: mesma chave de idempotencia usada com conteudo diferente.
- `413`: arquivo acima do limite configurado.
- `415` ou `422`: formato ou imagem invalida.
- `503`: banco ou armazenamento indisponivel.

Principais endpoints:

- `GET /health/live`: confirma que o processo esta vivo.
- `GET /health/ready`: confirma banco, armazenamento e capacidade de receber.
- `POST /api/v1/photos`: recebe a foto.
- `GET /api/v1/photos/{photo_id}`: consulta o status e os metadados.
- `GET /api/v1/events/{event_id}/summary`: consulta o resumo do evento.

## O que foi retirado ou nao faz parte

O projeto nao publica fotos automaticamente em TrizSports, Banlek ou qualquer outra plataforma remota. Tambem nao inclui reconhecimento facial, selecao automatica de atletas, edicao generativa por IA, processamento universal de RAW, aplicativo publicado nas lojas ou sincronizacao automatica com a nuvem.

Esses limites mantem a V1 focada no problema principal: garantir uma ingestao local confiavel, duravel, observavel e recuperavel, deixando futuras integracoes isoladas em adaptadores quando houver uma decisao para isso.

## Estado atual

O nucleo funcional esta implementado em Go e pode ser executado localmente com o bind configurado para a rede do evento. O teste pratico com iPhone ja comprovou a comunicacao LAN, a autenticacao, o upload multipart e a resposta assincrona `202 Accepted`.

Para considerar o projeto pronto para producao, ainda devem ser validados em campo a capacidade real do notebook sob carga, a protecao da rede Wi-Fi, a politica de retencao e privacidade das imagens, os backups e o comportamento diante de perda fisica de energia.

# Contexto Android: Termux + MacroDroid

## Finalidade

Este documento define a integracao Android para o Photo Edge Orchestrator. O objetivo e que o fotografo use a camera nativa do telefone normalmente e que cada nova foto JPEG seja enviada para o servidor local do evento assim que for gravada.

O fluxo nao publica em TrizSports, Banlek, nuvem ou qualquer plataforma externa. Ele apenas entrega o original ao servidor Photo Edge na rede Wi-Fi local, que ja possui armazenamento duravel, fila de processamento, auditoria e consulta de status.

## Contexto do servidor

O servidor roda no notebook do evento. Durante a validacao atual, os valores locais sao:

| Item | Valor de desenvolvimento |
| --- | --- |
| Endereco do servidor | `http://192.168.15.12:8090` |
| Health | `GET /health/live` |
| Upload | `POST /api/v1/photos` |
| Evento | `local-event` |
| Token de desenvolvimento | `local-development-token` |

O endereco IP pode mudar quando a rede mudar. O script deve concentrar `SERVER_URL`, `EVENT_ID` e o caminho do token em uma unica configuracao, sem espalhar esses valores pelo codigo.

Antes de testar o Android, o processo Go deve estar exposto na LAN do evento:

```powershell
$env:PHOTOEDGE_BIND = "0.0.0.0:8090"
$env:PHOTOEDGE_TOKEN = "local-development-token"
go run .\cmd\photoedge
```

O telefone e o notebook precisam estar no mesmo Wi-Fi. No navegador do Android, `http://192.168.15.12:8090/health/live` deve devolver `{"status":"alive"}` antes de configurar automacao.

## Arquitetura proposta

```text
Fotografo usa Camera nativa
    -> Android grava JPEG em DCIM/Camera
    -> MacroDroid detecta o novo arquivo concluido
    -> MacroDroid chama uma tarefa do Termux, passando o caminho do arquivo
    -> script valida, enfileira e envia multipart HTTP
    -> Photo Edge grava original + SQLite e devolve 202
    -> script registra sucesso ou mantem arquivo pendente para novo envio
```

Responsabilidades:

- Camera nativa: captura a foto. O fotografo continua usando o aplicativo de camera que preferir.
- MacroDroid: observa a pasta configurada e inicia a tarefa uma vez para cada arquivo novo. Ele nao deve montar HTTP, guardar token nem apagar a foto.
- Termux: contem a configuracao privada, faz a validacao do arquivo, cria uma chave de idempotencia, monta o `multipart/form-data`, registra tentativas e reenvia pendencias.
- Photo Edge: autentica, persiste o original antes da resposta `202`, evita duplicidade e processa em segundo plano.

## Dependencias no telefone

Instalar todas as aplicacoes Termux pela mesma origem, de preferencia F-Droid:

- Termux: ambiente que executa o script.
- Termux:API: necessario somente se alguma funcao do script usar recursos Android via `termux-*`.
- Termux:Task: ponte para o MacroDroid iniciar uma tarefa do Termux. Confirmar no MacroDroid a acao disponivel para executar plugin/intent do Termux:Task.
- MacroDroid: gatilho de arquivo novo e chamada da tarefa do Termux.

Nao misturar Termux de fontes diferentes, por exemplo Termux instalado por F-Droid com plugin instalado pela Play Store. As assinaturas podem ser incompativeis.

O Termux deve receber permissao de acesso a fotos e videos. Executar `termux-setup-storage` uma vez e aceitar a permissao do Android. Em versoes recentes do Android, testar o caminho real escolhido pela Camera; em geral sera `DCIM/Camera`, acessivel no Termux por `~/storage/shared/DCIM/Camera`.

## Contrato HTTP de upload

Para cada foto, o script envia uma requisicao:

```text
POST http://192.168.15.12:8090/api/v1/photos
Authorization: Bearer <token>
Idempotency-Key: <chave-unica-e-estavel-por-arquivo>
Content-Type: multipart/form-data  (gerado pelo curl)

photo: arquivo JPEG
event_id: local-event
```

Exemplo de comando que o script devera executar:

```bash
curl --fail-with-body --silent --show-error \
  --connect-timeout 5 --max-time 90 \
  -X POST "$SERVER_URL/api/v1/photos" \
  -H "Authorization: Bearer $PHOTOEDGE_TOKEN" \
  -H "Idempotency-Key: $IDEMPOTENCY_KEY" \
  -F "photo=@$PHOTO_PATH;type=image/jpeg" \
  -F "event_id=$EVENT_ID"
```

Nao definir manualmente o cabecalho `Content-Type`. O `curl -F` cria o limite multipart correto. O campo `photo` precisa apontar para o arquivo JPEG, nao para um texto com o caminho do arquivo.

## Requisitos do script Termux

O script deve receber um unico argumento: o caminho absoluto da foto criada pela camera. Antes do upload ele deve:

1. Confirmar que o arquivo existe, e uma imagem JPEG, e que seu tamanho ficou estavel por alguns segundos. Isso evita tentar enviar enquanto a Camera ainda esta escrevendo.
2. Copiar ou mover uma referencia para uma fila privada, por exemplo `~/photoedge/pending/`. A foto original em `DCIM/Camera` nunca deve ser removida pelo fluxo automatico.
3. Criar e persistir uma chave de idempotencia por arquivo. A mesma chave deve ser reutilizada em todas as tentativas daquele arquivo; uma nova chave somente para uma nova foto.
4. Ler token, URL e evento de um arquivo privado de configuracao, por exemplo `~/.config/photoedge/config.env`, com permissao `600`.
5. Enviar via `curl` usando o contrato acima.
6. Tratar `202` como aceite. Somente entao mover o item de fila para `sent/`, preservando resposta, data e `photo_id` em um pequeno arquivo de recibo.
7. Em erro de rede, timeout, `5xx` ou `503`, manter o item em `pending/` e retornar erro para o MacroDroid. Uma proxima execucao deve tentar as pendencias antes ou junto da nova foto.
8. Em `400`, `401`, `409`, `413`, `415` ou `422`, registrar a resposta em `failed/` e nao repetir indefinidamente. Esses erros indicam configuracao, chave ou arquivo que precisa de correcao.

Uma chave adequada pode combinar um identificador fixo do telefone, tamanho, data de modificacao e hash SHA-256 do arquivo. O valor precisa ser ASCII e ter tamanho razoavel. Exemplo: `realme-c73-<sha256>`. Assim, uma repeticao por timeout continua idempotente mesmo depois de reiniciar o telefone.

## Configuracao sugerida do Termux

Arquivo `~/.config/photoedge/config.env`:

```bash
SERVER_URL="http://192.168.15.12:8090"
EVENT_ID="local-event"
PHOTOEDGE_TOKEN="local-development-token"
DEVICE_LABEL="realme-c73"
```

Esse arquivo e secreto porque contem o token. Ele nao deve ser salvo em repositorio, anexado em logs, exibido em notificacoes nem copiado para a configuracao visual do MacroDroid.

Estrutura local esperada:

```text
~/photoedge/
  pending/     itens aguardando envio ou reenvio
  sent/        recibos de uploads aceitos pelo servidor
  failed/      itens que exigem intervencao
  logs/        logs locais sem token
```

## Configuracao esperada no MacroDroid

O MacroDroid deve ser minimo e delegar toda logica de rede ao Termux.

Gatilho:

- Evento de novo arquivo em `DCIM/Camera`.
- Filtrar extensoes `.jpg` e `.jpeg`.
- Ignorar arquivos temporarios, miniaturas e arquivos com nome iniciado por ponto.
- Aplicar pequeno atraso inicial, por exemplo 3 a 5 segundos, antes de chamar o script. O script continua sendo responsavel pela verificacao final de estabilidade.

Acoes:

1. Chamar a tarefa `photoedge-upload` pelo conector Termux:Task ou pelo mecanismo compativel de plugin/intent do MacroDroid.
2. Passar o caminho completo do arquivo que disparou o gatilho como primeiro argumento.
3. Em falha, mostrar uma notificacao curta sem token: `Foto pendente para envio`. Em sucesso, a notificacao pode ser silenciosa ou omitida para nao interromper a captura.

Restricoes operacionais:

- Desativar otimizacao de bateria para MacroDroid, Termux e Termux:Task, se instalado. Realme UI pode encerrar tarefas em segundo plano de forma agressiva.
- Permitir execucao em segundo plano e inicio automatico para MacroDroid, quando a interface do aparelho oferecer essas opcoes.
- Testar com a tela bloqueada, pois a automacao precisa funcionar sem abrir Termux manualmente.
- Configurar o gatilho para evitar duas execucoes sobre o mesmo arquivo. O script tambem deve usar lock por arquivo como segunda protecao.

## Estados e recuperacao

| Situacao | Acao do script |
| --- | --- |
| Servidor responde `202` | salvar recibo com `photo_id`; marcar item como enviado |
| Sem Wi-Fi ou timeout | manter em `pending/` e tentar novamente depois |
| Servidor responde `401` | parar novas tentativas; revisar token |
| Servidor responde `409` | preservar resposta; revisar se a chave foi reutilizada para outra foto |
| Servidor responde `400`, `413`, `415` ou `422` | mover para `failed/`; revisar arquivo/configuracao |
| Telefone reinicia ou MacroDroid falha | tarefa de reenvio percorre `pending/` no proximo disparo ou em agenda periodica |

O servidor pode aceitar o upload e a resposta se perder no caminho. Por isso a chave de idempotencia persistida e obrigatoria: ao reenviar os mesmos bytes com a mesma chave, o servidor devolve a mesma identidade da foto em vez de criar duplicidade.

## Validacao em fases

1. Iniciar o Photo Edge no notebook com bind `0.0.0.0:8090` e validar `/health/live` pelo navegador do Android.
2. Instalar Termux e executar manualmente o script apontando para uma foto existente. Esperado: `202` e um `photo_id`.
3. Confirmar no notebook: `GET /api/v1/photos/{photo_id}` deve eventualmente chegar a `READY`.
4. Configurar MacroDroid para chamar o script com uma foto nova e testar com o telefone desbloqueado.
5. Repetir com tela bloqueada e com a Camera nativa fechada.
6. Desligar o Wi-Fi, tirar uma foto, religar o Wi-Fi e confirmar que a fila `pending/` foi enviada sem duplicacao.

## Limites desta integracao

Esta integracao automatiza a ingestao local. Ela nao garante que todos os fabricantes Android permitirao observar arquivos e iniciar apps em segundo plano da mesma forma. Se o Realme bloquear a automacao, ajustar as permissoes de bateria e auto-inicializacao e usar a fila Termux como fonte de recuperacao. A Camera permanece a fonte da imagem e o Photo Edge permanece a fonte de verdade para o aceite e processamento.

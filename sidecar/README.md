# Ponte de chamada — sidecar do motor `whatsapp.wasm`

Esta pasta é a **metade Node** da ponte. Sem ela o lado Go (`wasmbridge.go`,
`wasmaudio.go`, `lidbin/`) não serve pra nada, e vice-versa.

## Por que existe

O meowcaller **reimplementa** o protocolo de mídia do WhatsApp na unha. Isso funciona pra
chamada de **saída**, mas na chamada **recebida** o áudio do cliente nunca chegava — o
atendente ficava surdo. Meses de ajuste em assinatura de SSRC, consent e multi-relay não
moveram o ponteiro.

O sidecar não imita: ele roda o **`whatsapp.wasm`**, o motor original do WhatsApp Web
(mesma coisa que o [SheIITear](https://github.com/purpshell) faz), fora do navegador. O
gateway continua dono da conexão e da sinalização; aqui vive só o motor de mídia.

```
whatsmeow gateway (Go)  ── stanza base64 ──>  sidecar (whatsapp.wasm + relay WebRTC)
   conexão + sinalização  <── stanza pra enviar ──
                          <───── PCM (UDP) ─────>   áudio nos dois sentidos
```

## A causa raiz do bug de três meses

```
whatsmeow  Marshal  198453946806345@lid  ->  fa <user> 76      (JIDPair)
Baileys / WhatsApp Web                   ->  f7 01 00 <user>   (ADJID, domínio LID)
```

O whatsmeow escolhe **JIDPair** porque `Device == 0`; o Baileys escolhe **ADJID** porque
`:0` é um device explícito. O motor `whatsapp.wasm` **só reconhece o domínio `@lid` no
ADJID** — com JIDPair ele lê o creator como `@s.whatsapp.net`, deriva o **SSRC errado**, se
inscreve num fluxo que não existe (nenhum RTP entra) e anuncia um SSRC que o celular do
chamador ignora (o chamador não ouve a gente). **Uma causa, as duas direções mudas.**

São **duas exigências simultâneas**, medidas contra o motor real com as 4 combinações:

| codificação | resultado |
|---|---|
| JIDPair + byte de flags | `mismatched peer id and creator id` |
| JIDPair sem flags | não parseia |
| **ADJID + byte de flags** | **aceita** |
| ADJID sem flags | não parseia |

O `waBinary.Marshal` do whatsmeow **já inclui** o byte de flags; o `encodeBinaryNode` do
Baileys não. Então não tire o byte. `lidbin/` é uma cópia do encoder do whatsmeow com essa
única diferença deliberada.

## Arquivos

| arquivo | o que é |
|---|---|
| `sidecar.mts` | o serviço: motor + transporte de relay + HTTP de sinalização + UDP de áudio |
| `referencia.mts` | SheIITear **puro**, sem ponte — o controle que provou que o motor original recebe áudio inbound |
| `replay.mts`, `teste-wasm.mts` | bancada: reinjeta uma oferta capturada no motor sem precisar de ligação de verdade |

## Como rodar

```bash
# dependências: @roamhq/wrtc (o relay é datachannel WebRTC, não UDP puro) + tsx
# a lib do SheIITear (11 MB) vai em lib/sheitear/
node --import tsx sidecar.mts
```

Variáveis:

| env | pra que |
|---|---|
| `SIDECAR_PORT` (9099) | HTTP de sinalização |
| `SIDECAR_AUDIO_PORT` (9098) | UDP de áudio (PCM mono 16 kHz float32 LE) |
| `SELF_PN_DEV`, `SELF_LID` | **identidade do número** — um sidecar por número |
| `PEER_DOMINIO` (`lid`) | `pn` volta ao comportamento antigo, pra comparar |

Do lado do gateway: `WHATSMEOW_WASM_BRIDGE=1`, `WHATSMEOW_WASM_BRIDGE_URL`,
`WHATSMEOW_WASM_BRIDGE_JID` (números que passam pela ponte).

## Armadilhas que custaram caro

- **Os dois motores brigando.** Registrar a chamada em `e.calls` faz o meowcaller voltar a
  agir nela — responde relaylatency em duplicidade, sobe a mídia dele, assina o relay com o
  SSRC dele. Sintoma: **`Subscription succeeds` some do log**, ~21 frames e `rx timed out`,
  e o chamador ouve um "toque no meio da chamada". `ehDaPonte()` existe por isso.
- **Fila do microfone.** Teto de 200 ms, não 1 s: o `setInterval(20ms)` do Node atrasa a
  cada volta e a fila cresce sozinha, deixando a voz do atendente segundos atrás.
- **`enableLogs: true` não basta** — tem que registrar o callback `onLog`, senão os
  diagnósticos do motor ficam invisíveis.
- **Silêncio absoluto** faz o motor reiniciar a captura ("all N samples are filled with
  zero"). Mandar ruído inaudível.
- **O motor é pesado**: com 4 vCPU ele sobe com *0 ready workers* e não atende. Numa
  máquina de 8 cores sobe com 12.
- **Testar codificação não precisa de ligação**: dá pra reinjetar uma oferta capturada
  (`/var/lib/whatsmeow-gateway/callstanzas/*-wasm.json`) direto no `POST /offer`.

## Limite conhecido

Um sidecar = **um número**. Pra vários, é preciso um sidecar por número (cada um na sua
porta) e o gateway escolhendo pelo `connectionId`. Hoje o `WHATSMEOW_WASM_BRIDGE_JID` só
impede que os outros números caiam na ponte errada.

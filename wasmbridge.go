package meowcaller

// Ponte pro motor REAL do WhatsApp (whatsapp.wasm) que roda no sidecar Node.
//
// Por que existe: o inbound (cliente liga pro sistema) nunca entregou o áudio do cliente.
// Tudo que se tentou aqui dentro — assinatura de SSRC, multi-relay, consent — não moveu o
// ponteiro, porque o meowcaller REIMPLEMENTA o protocolo do relay, e o relay se comporta
// diferente com o callee. O sidecar não imita: roda o motor original. Aqui a gente só
// carrega stanza de um lado pro outro.
//
//   gateway (Go)  ── <offer>/<relay>/<ack> em base64 ──>  sidecar (whatsapp.wasm)
//                 <── stanza pronta pra enviar ─────────
//
// ⚠️ LIGADA (WHATSMEOW_WASM_BRIDGE=1) o meowcaller NÃO participa mais das chamadas
// RECEBIDAS: quem atende é o motor. Desligada (padrão), nada neste arquivo roda e o
// comportamento de hoje — inclusive o outbound, que FUNCIONA — fica intacto.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/purpshell/meowcaller/lidbin"
	"github.com/rs/zerolog"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// pontewasmLigada diz se a ponte está habilitada. Padrão: desligada.
func pontewasmLigada() bool { return os.Getenv("WHATSMEOW_WASM_BRIDGE") == "1" }

func pontewasmURL() string {
	if u := strings.TrimRight(os.Getenv("WHATSMEOW_WASM_BRIDGE_URL"), "/"); u != "" {
		return u
	}
	return "http://127.0.0.1:9099"
}

type pontewasm struct {
	e    *engine
	log  zerolog.Logger
	url  string
	http *http.Client

	mu       sync.Mutex
	peers    map[string]types.JID // callID → JID do chamador, pra rotear a resposta
	creators map[string]types.JID // callID → call-creator ORIGINAL (com domínio @lid)
}

// pw devolve a ponte da engine, criando (e ligando o laço de saída) na primeira chamada.
func (e *engine) pw() *pontewasm {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if e.ponte == nil {
		e.ponte = &pontewasm{
			e:        e,
			log:      e.c.log.With().Str("comp", "wasm-bridge").Logger(),
			url:      pontewasmURL(),
			http:     &http.Client{Timeout: 70 * time.Second},
			peers:    map[string]types.JID{},
			creators: map[string]types.JID{},
		}
		go e.ponte.laçoDeSaída()
	}
	return e.ponte
}

// postar manda um JSON pro sidecar. Best-effort: erro só vira log.
func (p *pontewasm) postar(rota string, corpo map[string]any) (map[string]any, error) {
	b, _ := json.Marshal(corpo)
	req, err := http.NewRequest(http.MethodPost, p.url+rota, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("sidecar %s: %s", rota, resp.Status)
	}
	return out, nil
}

// oferta entrega a chamada recebida ao motor. `callKey` é a chave JÁ EM CLARO — o motor
// espera o <enc> descriptografado (é o que o SheIITear faz em #maybeDecryptEnc).
func (p *pontewasm) oferta(ev *events.CallOffer, callKey []byte) {
	if ev == nil || ev.Data == nil {
		return
	}
	// Cópia do nó: trocar o <enc> no original mexeria na chamada de verdade.
	copia := *ev.Data
	if kids, ok := ev.Data.Content.([]waBinary.Node); ok {
		novos := make([]waBinary.Node, len(kids))
		copy(novos, kids)
		for i := range novos {
			if novos[i].Tag == "enc" {
				novos[i].Content = append([]byte(nil), callKey...)
			}
		}
		copia.Content = novos
	}
	// ⭐ NÃO é o Marshal do whatsmeow. Medido em 10/09 contra o motor real, com as 4
	// combinações possíveis: ele só aceita a oferta quando o JID de LID vem como ADJID
	// (o whatsmeow manda JIDPair quando o device é 0, e aí o motor lê o creator como
	// @s.whatsapp.net e descarta: "mismatched peer id and creator id") E quando o byte
	// de flags está presente (sem ele não parseia nada).
	raw := lidbin.MarshalParaWasm(copia, true)

	// O motor casa o peerJid com o call-creator de DENTRO da stanza; quem manda é o creator.
	peer := ev.CallCreator
	if peer.IsEmpty() {
		peer = ev.From
	}
	p.mu.Lock()
	p.peers[ev.CallID] = ev.From
	p.creators[ev.CallID] = peer
	p.mu.Unlock()

	// tcToken (privacy token do chamador): o SheIITear passa e a ponte não passava.
	// Medido na referência: 11 bytes. Best-effort — sem ele o motor ainda aceita a oferta.
	corpo := map[string]any{
		"callId":         ev.CallID,
		"payloadWasm":    base64.StdEncoding.EncodeToString(raw),
		"peerJid":        peer.String(),
		"peerPlatform":   ev.RemotePlatform,
		"peerAppVersion": ev.RemoteVersion,
		"timestamp":      fmt.Sprintf("%d", ev.Timestamp.Unix()),
	}
	if st := p.e.c.wa.Store; st != nil && st.PrivacyTokens != nil {
		if pt, errTok := st.PrivacyTokens.GetPrivacyToken(context.Background(), peer.ToNonAD()); errTok == nil && pt != nil && len(pt.Token) > 0 {
			corpo["tcToken"] = base64.StdEncoding.EncodeToString(pt.Token)
			p.log.Debug().Int("bytes", len(pt.Token)).Msg("tcToken do chamador anexado")
		}
	}
	if _, err := p.postar("/offer", corpo); err != nil {
		p.log.Error().Err(err).Str("call_id", ev.CallID).Msg("sidecar recusou a oferta")
		return
	}
	p.log.Info().Str("call_id", ev.CallID).Int("bytes", len(raw)).Str("peer", peer.String()).
		Msg("⭐ oferta entregue ao motor whatsapp.wasm — a chamada é dele agora")

	// Milestone 1: atende sozinho, pra medir se o áudio do cliente finalmente chega.
	// (Com a ponte ligada não há toque no navegador — é uma chave de teste.)
	if os.Getenv("WHATSMEOW_WASM_BRIDGE_AUTOACCEPT") != "0" {
		go func() {
			time.Sleep(1200 * time.Millisecond)
			if _, err := p.postar("/accept", map[string]any{"callId": ev.CallID}); err != nil {
				p.log.Warn().Err(err).Msg("accept no sidecar falhou")
			}
		}()
	}
}

// sinalBruto repassa um nó <call> qualquer (relay, transport, terminate…) pro motor.
// Só o filho interessa — é o mesmo recorte que o SheIITear manda.
func (p *pontewasm) sinalBruto(node *waBinary.Node) {
	if node == nil {
		return
	}
	kids := node.GetChildren()
	if len(kids) != 1 {
		return
	}
	filho := kids[0]
	if filho.Tag == "offer" {
		return // a oferta vai por p.oferta(), com a chave em claro
	}
	raw := lidbin.MarshalParaWasm(filho, true)
	ag := node.AttrGetter()
	cag := filho.AttrGetter()
	callID := cag.String("call-id")
	peer := cag.String("call-creator")
	if peer == "" {
		peer = ag.String("from")
	}
	if callID != "" {
		p.mu.Lock()
		if _, ok := p.peers[callID]; !ok {
			p.peers[callID] = ag.JID("from")
		}
		p.mu.Unlock()
	}
	if _, err := p.postar("/signal", map[string]any{
		"callId":         callID,
		"payloadWasm":    base64.StdEncoding.EncodeToString(raw),
		"peerJid":        peer,
		"peerPlatform":   ag.String("platform"),
		"peerAppVersion": ag.String("version"),
		"timestamp":      ag.String("t"),
	}); err != nil {
		p.log.Warn().Err(err).Str("tag", filho.Tag).Msg("sinal não chegou no motor")
	} else {
		p.log.Debug().Str("tag", filho.Tag).Str("call_id", callID).Msg("sinal repassado ao motor")
	}
}

// ackBruto entrega um <ack class="call"> ao motor. Sem isso ele trava esperando a lista
// de relays (o ack do servidor é quem carrega a alocação).
func (p *pontewasm) ackBruto(node *waBinary.Node, msgType string) {
	if node == nil {
		return
	}
	raw := lidbin.MarshalParaWasm(*node, true)
	ag := node.AttrGetter()
	tipo := msgType
	if tipo == "" {
		tipo = ag.String("type")
	}
	_, _ = p.postar("/signal", map[string]any{
		"tipo":        "ack",
		"payloadWasm": base64.StdEncoding.EncodeToString(raw),
		"ackError":    ag.String("error"),
		"msgType":     tipo,
		"peerJid":     ag.String("from"),
	})
}

// recibo entrega ao motor um <receipt> de chamada — o SheIITear repassa esses (CB:receipt)
// e a ponte não repassava. Vai o nó INTEIRO, não o filho (é o que handleSignalingReceipt
// espera).
func (p *pontewasm) recibo(node *waBinary.Node) {
	if node == nil {
		return
	}
	kids := node.GetChildren()
	if len(kids) == 0 {
		return
	}
	cag := kids[0].AttrGetter()
	callID := cag.OptionalString("call-id")
	if callID == "" {
		callID = cag.OptionalString("call_id")
	}
	if callID == "" {
		return // recibo de mensagem comum, não é da chamada
	}
	raw := lidbin.MarshalParaWasm(*node, true)
	ag := node.AttrGetter()
	peer := cag.OptionalString("call-creator")
	if peer == "" {
		peer = ag.String("from")
	}
	p.log.Info().Str("call_id", callID).Msg("⬇️ recibo de chamada repassado ao motor")
	_, _ = p.postar("/signal", map[string]any{
		"tipo": "receipt", "callId": callID,
		"payloadWasm": base64.StdEncoding.EncodeToString(raw),
		"peerJid":     peer,
	})
}

// laçoDeSaída fica pendurado no /out do sidecar e envia o que o motor produzir.
func (p *pontewasm) laçoDeSaída() {
	for {
		respostas, err := p.buscarSaida()
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		for _, r := range respostas {
			p.enviarResposta(r)
		}
	}
}

type saidaWasm struct {
	CallID  string `json:"callId"`
	PeerJID string `json:"peerJid"`
	B64     string `json:"b64"`
}

func (p *pontewasm) buscarSaida() ([]saidaWasm, error) {
	resp, err := p.http.Get(p.url + "/out?ms=25000")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Respostas []saidaWasm `json:"respostas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Respostas, nil
}

// enviarResposta embrulha a stanza do motor num <call> e manda; o ack do servidor volta
// pro motor, que é o que destrava a lista de relays.
func (p *pontewasm) enviarResposta(r saidaWasm) {
	raw, err := base64.StdEncoding.DecodeString(r.B64)
	if err != nil {
		return
	}
	// O motor às vezes entrega a stanza com o byte de flags do frame na frente (é por isso
	// que o SheIITear tenta decodificar das duas formas). Tenta crua e depois sem o 1º byte.
	acao, err := waBinary.Unmarshal(raw)
	if err != nil && len(raw) > 1 {
		if alt, err2 := waBinary.Unmarshal(raw[1:]); err2 == nil {
			acao, err = alt, nil
		}
	}
	if err != nil {
		p.log.Error().Err(err).Str("hex", hex.EncodeToString(raw[:min(48, len(raw))])).
			Int("bytes", len(raw)).Msg("stanza do motor não decodificou")
		return
	}
	callID := r.CallID
	if callID == "" {
		callID = acao.AttrGetter().String("call-id")
	}
	p.mu.Lock()
	para, ok := p.peers[callID]
	criador, temCriador := p.creators[callID]
	p.mu.Unlock()

	// O motor devolve o call-creator no domínio @s.whatsapp.net (é assim que ele lê o LID
	// da nossa stanza). Mandar assim pro WhatsApp é um JID que não existe — repõe o
	// call-creator ORIGINAL, com o domínio @lid que veio na oferta.
	if temCriador && acao.Attrs != nil {
		if cc, existe := acao.Attrs["call-creator"]; existe {
			mesmo := false
			switch v := cc.(type) {
			case types.JID:
				mesmo = v.User == criador.User
			case string:
				mesmo = strings.HasPrefix(v, criador.User+"@") || v == criador.User
			}
			if mesmo {
				acao.Attrs["call-creator"] = criador
			}
		}
	}

	if !ok {
		// Sem mapa: usa o peer que o motor devolveu (vem normalizado :0@s.whatsapp.net).
		if j, err := types.ParseJID(r.PeerJID); err == nil {
			para = j.ToNonAD()
		} else {
			p.log.Warn().Str("call_id", callID).Msg("sem destino pra stanza do motor")
			return
		}
	}

	di := p.e.c.wa.DangerousInternals()
	id := di.GenerateRequestID()
	espera := di.WaitResponse(id)
	no := waBinary.Node{
		Tag:     "call",
		Attrs:   waBinary.Attrs{"to": para.ToNonAD(), "id": id},
		Content: []waBinary.Node{*acao},
	}
	if err := di.SendNode(context.Background(), no); err != nil {
		p.log.Error().Err(err).Str("tag", acao.Tag).Msg("envio da stanza do motor falhou")
		return
	}
	filhos := make([]string, 0, 4)
	for _, f := range acao.GetChildren() {
		filhos = append(filhos, f.Tag)
	}
	p.log.Info().Str("tag", acao.Tag).Str("call_id", callID).Str("to", para.String()).
		Strs("dentro", filhos).Msg("⬆️ stanza do motor enviada")

	// O <ack> do servidor é o que pode TRAZER MAIS RELAYS — é onde o inbound descobre o
	// relay do chamador (o `fccm1c01` que aparece no relaylatency e não está na nossa
	// lista). Sem ele o motor fica assinado nos relays errados e nunca recebe o RTP.
	go func() {
		select {
		case ack := <-espera:
			if ack == nil {
				p.log.Warn().Str("tag", acao.Tag).Msg("ack do servidor veio vazio")
				return
			}
			temRelay := findRelay(ack) != nil
			p.log.Info().Str("resposta_a", acao.Tag).
				Str("tipo", ack.AttrGetter().String("type")).
				Str("erro", ack.AttrGetter().String("error")).
				Bool("traz_relay", temRelay).
				Msg("⬇️ ack do servidor — repassando ao motor")
			p.ackBruto(ack, acao.Tag)
		case <-time.After(20 * time.Second):
			p.log.Warn().Str("tag", acao.Tag).Msg("⚠️ o servidor NÃO respondeu ack em 20s")
		}
	}()
}

// encerrou avisa o motor que a chamada morreu e limpa o mapa.
func (p *pontewasm) encerrou(callID string) {
	p.mu.Lock()
	delete(p.peers, callID)
	delete(p.creators, callID)
	p.mu.Unlock()
	_, _ = p.postar("/hangup", map[string]any{"callId": callID})
}

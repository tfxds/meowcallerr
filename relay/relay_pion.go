package relay

import (
	"fmt"
	"io"
	"net"
	"regexp"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"
)

// Transporte de relay ALTERNATIVO baseado em pion/webrtc. É ADITIVO ao transporte manual
// (ConnectRelayMedia em relay.go): a diferença é que aqui um webrtc.PeerConnection real
// faz o ICE-consent / connectivity-checks / consent-freshness automaticamente e por
// completo. No INBOUND (somos callee) o relay exige que a gente "assine" a mídia do peer
// via ICE-consent; o stack manual fazia isso de forma intermitente, e o pion faz certo.
//
// Modelado fielmente no WaCalls internal/voip/transport/sctprelay.go (connectToRelay +
// modifySdpForRelay): CreateDataChannel("wa-web-call", Ordered:false), CreateOffer,
// SetLocalDescription, munge do SDP (offer vira a answer do relay) e SetRemoteDescription.
//
// NÃO VALIDADO: só exercitado contra um relay real (igual o transporte manual).

// WARelayDTLSFingerprint é o fingerprint DTLS fixo que os relays do WhatsApp apresentam.
// O pion precisa dele no SDP (answer) pra validar o handshake DTLS — o transporte manual
// pula a verificação (InsecureSkipVerify) e por isso não precisa dessa constante.
// Fonte: WaCalls internal/voip/core/types.go (WADTLSFingerprint).
const WARelayDTLSFingerprint = "sha-256 F9:CA:0C:98:A3:CC:71:D6:42:CE:5A:E2:53:D2:15:20:D3:1B:BA:D8:57:A4:F0:AF:BE:0B:FB:F3:6B:0C:A0:68"

// pionRelayBufferSize é a capacidade do buffer de RX. Se encher, o OnMessage (assíncrono,
// roda na goroutine do pion) descarta o pacote MAIS ANTIGO em vez de bloquear o callback.
const pionRelayBufferSize = 256

// pionRelayOpenTimeout é o teto pra o DataChannel abrir (DTLS+SCTP+OnOpen).
const pionRelayOpenTimeout = 12 * time.Second

// pionRelayChannel é um RelayChannel respaldado por um webrtc.PeerConnection. As mensagens
// que chegam no DataChannel ("wa-web-call") são empurradas pra um channel bufferizado que o
// Recv consome; o Send escreve direto no DataChannel.
type pionRelayChannel struct {
	pc        *webrtc.PeerConnection
	dc        *webrtc.DataChannel
	rx        chan []byte
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
	log       zerolog.Logger
}

// Garante em tempo de compilação que satisfazemos a interface.
var _ RelayChannel = (*pionRelayChannel)(nil)

// ConnectRelayMediaPion conecta a stack de mídia ao relay usando pion/webrtc. Bloqueia até
// o DataChannel abrir (OnOpen) ou estourar pionRelayOpenTimeout. As credenciais ICE
// (ufrag/pwd) vêm via WithRelayICECredentials; sem elas o agente ICE não autentica no relay
// e a abertura tende a expirar.
func ConnectRelayMediaPion(relayAddr *net.UDPAddr, opts ...Option) (RelayChannel, error) {
	cfg := resolveConfig(opts)
	lg := cfg.log
	fingerprint := cfg.dtlsFingerprint
	if fingerprint == "" {
		fingerprint = WARelayDTLSFingerprint
	}
	lg.Debug().Str("relay_addr", relayAddr.String()).Msg("connecting relay media stack (pion)")

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, &CallTransportError{Op: "connect", Err: fmt.Errorf("new peerconnection: %w", err)}
	}

	rc := &pionRelayChannel{
		pc:     pc,
		rx:     make(chan []byte, pionRelayBufferSize),
		closed: make(chan struct{}),
		log:    lg,
	}

	// DataChannel não-ordenado, igual o WaCalls ("wa-web-call").
	ordered := false
	dc, err := pc.CreateDataChannel("wa-web-call", &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		_ = pc.Close()
		return nil, &CallTransportError{Op: "connect", Err: fmt.Errorf("create datachannel: %w", err)}
	}
	rc.dc = dc

	opened := make(chan struct{})
	var openOnce sync.Once
	dc.OnOpen(func() {
		lg.Debug().Msg("relay(pion) datachannel open")
		openOnce.Do(func() { close(opened) })
	})
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		// O pion pode reaproveitar o buffer do msg.Data depois do callback; copiamos.
		data := make([]byte, len(msg.Data))
		copy(data, msg.Data)
		select {
		case <-rc.closed:
			return
		default:
		}
		// Não bloqueia o callback do pion: se o buffer encher, descarta o MAIS ANTIGO.
		select {
		case rc.rx <- data:
		default:
			select {
			case <-rc.rx:
			default:
			}
			select {
			case rc.rx <- data:
			default:
			}
		}
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		lg.Debug().Str("state", s.String()).Msg("relay(pion) ICE state")
		if s == webrtc.ICEConnectionStateFailed || s == webrtc.ICEConnectionStateClosed {
			// Close() faz pc.Close(); rodamos em goroutine pra não travar o callback do pion.
			go rc.Close()
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		rc.Close()
		return nil, &CallTransportError{Op: "connect", Err: fmt.Errorf("create offer: %w", err)}
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		rc.Close()
		return nil, &CallTransportError{Op: "connect", Err: fmt.Errorf("set local description: %w", err)}
	}

	// Mungir a NOSSA offer e usá-la como a ANSWER do relay (não há sinalização SDP de volta:
	// o relay não fala SDP, então sintetizamos a answer dele).
	munged := modifySdpForRelayPion(offer.SDP, relayAddr, cfg.iceUfrag, cfg.icePwd, fingerprint)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: munged}); err != nil {
		rc.Close()
		return nil, &CallTransportError{Op: "connect", Err: fmt.Errorf("set remote description: %w", err)}
	}

	select {
	case <-opened:
		lg.Debug().Msg("relay(pion) media stack ready")
		return rc, nil
	case <-time.After(pionRelayOpenTimeout):
		rc.Close()
		return nil, &CallTransportError{Op: "connect", Err: fmt.Errorf("relay datachannel open timed out (ICE/DTLS didn't complete)")}
	case <-rc.closed:
		return nil, &CallTransportError{Op: "connect", Err: fmt.Errorf("relay closed before datachannel opened")}
	}
}

// Regex de munge do SDP, portados 1:1 do WaCalls internal/voip/transport/sctprelay.go.
var (
	rePionSetup       = regexp.MustCompile(`a=setup:actpass`)
	rePionUfragLine   = regexp.MustCompile(`a=ice-ufrag:[^\r\n]+`)
	rePionPwdLine     = regexp.MustCompile(`a=ice-pwd:[^\r\n]+`)
	rePionFingerprint = regexp.MustCompile(`a=fingerprint:[^\r\n]+`)
	rePionMaxMsg      = regexp.MustCompile(`a=max-message-size:[^\r\n]+`)
	rePionIceOptions  = regexp.MustCompile(`a=ice-options:[^\r\n]+\r?\n`)
	rePionCandidate   = regexp.MustCompile(`a=candidate:[^\r\n]+\r?\n`)
	rePionEndCand     = regexp.MustCompile(`a=end-of-candidates\r?\n?`)
)

// modifySdpForRelayPion transforma a NOSSA offer na ANSWER sintética do relay. Portado de
// WaCalls modifySdpForRelay: setup actpass→passive (o relay é o servidor DTLS, nós o
// cliente, igual o transporte manual), ice-ufrag = base64(auth_token), ice-pwd = <key>,
// fingerprint = o fixo do relay, e um único candidate host com o IP/porta do relayAddr.
// ufrag/pwd só são reescritos se vierem preenchidos (sem credenciais o agente ICE não
// autentica — o caminho manual não passa ICE, então não tem esse problema).
func modifySdpForRelayPion(sdp string, relayAddr *net.UDPAddr, iceUfrag, icePwd, fingerprint string) string {
	out := rePionSetup.ReplaceAllString(sdp, "a=setup:passive")
	if iceUfrag != "" {
		out = rePionUfragLine.ReplaceAllString(out, "a=ice-ufrag:"+iceUfrag)
	}
	if icePwd != "" {
		out = rePionPwdLine.ReplaceAllString(out, "a=ice-pwd:"+icePwd)
	}
	if fingerprint != "" {
		out = rePionFingerprint.ReplaceAllString(out, "a=fingerprint:"+fingerprint)
	}
	out = rePionMaxMsg.ReplaceAllString(out, "a=max-message-size:1500")
	out = rePionIceOptions.ReplaceAllString(out, "")
	out = rePionCandidate.ReplaceAllString(out, "")
	out = rePionEndCand.ReplaceAllString(out, "")
	candidate := fmt.Sprintf("a=candidate:2 1 udp 2122262783 %s %d typ host generation 0 network-cost 5", relayAddr.IP.String(), relayAddr.Port)
	out += candidate + "\r\n" + "a=end-of-candidates" + "\r\n"
	return out
}

// Send escreve um pacote de mídia/STUN como mensagem binária no DataChannel.
func (c *pionRelayChannel) Send(data []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, &CallTransportError{Op: "send", Err: io.ErrClosedPipe}
	default:
	}
	if err := c.dc.Send(data); err != nil {
		c.log.Debug().Err(err).Int("packet_bytes", len(data)).Msg("relay(pion) send failed")
		return 0, &CallTransportError{Op: "send", Err: err}
	}
	c.log.Trace().Int("packet_bytes", len(data)).Msg("sent relay packet (pion)")
	return len(data), nil
}

// Recv copia a próxima mensagem do DataChannel pra buf e devolve o tamanho. Retorna erro
// (EOF) quando o canal é fechado, pra desbloquear o loop de RX do engine no teardown.
func (c *pionRelayChannel) Recv(buf []byte) (int, error) {
	select {
	case data := <-c.rx:
		n := copy(buf, data)
		c.log.Trace().Int("packet_bytes", n).Msg("received relay packet (pion)")
		return n, nil
	case <-c.closed:
		return 0, &CallTransportError{Op: "recv", Err: io.EOF}
	}
}

// Close derruba o PeerConnection (e com ele DataChannel/SCTP/DTLS/ICE). Idempotente.
func (c *pionRelayChannel) Close() error {
	c.closeOnce.Do(func() {
		c.log.Debug().Msg("tearing down relay media channel (pion)")
		close(c.closed)
		if c.dc != nil {
			_ = c.dc.Close()
		}
		if c.pc != nil {
			c.closeErr = c.pc.Close()
		}
	})
	return c.closeErr
}

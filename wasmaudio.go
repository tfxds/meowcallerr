package meowcaller

// Cano de áudio entre o gateway e o sidecar (motor whatsapp.wasm), por UDP em localhost.
//
// Por que UDP e não WebSocket: é áudio ao vivo em loopback — perder um frame é melhor que
// atrasar, e assim não entra dependência de framing nem no Go nem no Node.
//
// Formato dos dois lados: PCM mono 16 kHz em float32 little-endian, cru. O gateway manda
// frames de FrameSamples (960 = 60 ms, o passo do meowcaller); o sidecar reparte em blocos
// de 320 (20 ms, o passo do motor) e devolve o que o motor decodifica do cliente.

import (
	"encoding/binary"
	"math"
	"net"
	"os"
	"sync"
	"time"
)

const bytesPorAmostra = 4

func portaAudioSidecar() string {
	if p := os.Getenv("WHATSMEOW_WASM_AUDIO_PORT"); p != "" {
		return p
	}
	return "9098"
}

// canoAudio move áudio entre a chamada (Player/AudioSink) e o sidecar.
type canoAudio struct {
	conn   *net.UDPConn
	call   *Call
	pw     *pontewasm
	parar  chan struct{}
	uma    sync.Once
	pendep []float32 // sobra do que veio do sidecar, até fechar um frame de 960
	mu     sync.Mutex
}

// abrirCanoAudio liga o áudio da chamada ao sidecar e começa a bombear nos dois sentidos.
func (p *pontewasm) abrirCanoAudio(c *Call) (*canoAudio, error) {
	destino, err := net.ResolveUDPAddr("udp", "127.0.0.1:"+portaAudioSidecar())
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, destino)
	if err != nil {
		return nil, err
	}
	ca := &canoAudio{conn: conn, call: c, pw: p, parar: make(chan struct{})}
	go ca.bombearParaOCliente() // mic do atendente → motor → cliente
	go ca.receberDoCliente()    // voz do cliente → motor → navegador
	p.log.Info().Str("call_id", c.id).Str("porta", portaAudioSidecar()).
		Msg("🎧 cano de áudio aberto com o motor")
	return ca, nil
}

func (ca *canoAudio) fechar() {
	ca.uma.Do(func() {
		close(ca.parar)
		_ = ca.conn.Close()
	})
}

// bombearParaOCliente lê o Player da chamada a cada 60 ms e manda pro motor. Quando não há
// nada tocando manda quase-silêncio: o motor precisa de fluxo contínuo (com o buffer todo
// zerado ele reinicia a captura), e o relay derruba a ponte se a gente parar de enviar.
func (ca *canoAudio) bombearParaOCliente() {
	tick := time.NewTicker(FrameSamples * time.Second / SampleRate) // 60 ms
	defer tick.Stop()
	quase := make([]float32, FrameSamples)
	for i := range quase {
		quase[i] = float32((float64(i%7) - 3) * 1e-5)
	}
	buf := make([]byte, FrameSamples*bytesPorAmostra)
	for {
		select {
		case <-ca.parar:
			return
		case <-tick.C:
			player, _ := ca.call.playerAndSink()
			frame := quase
			if player != nil {
				if f := player.nextFrame(); len(f) == FrameSamples {
					frame = f
				}
			}
			for i, v := range frame {
				binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
			}
			if _, err := ca.conn.Write(buf); err != nil {
				return
			}
		}
	}
}

// receberDoCliente lê o PCM que o motor decodifica do cliente e entrega ao sink da chamada
// (que é o WSPipe do navegador). O motor manda em blocos de 20 ms; aqui a gente reagrupa
// nos frames de 60 ms que o resto do sistema espera.
func (ca *canoAudio) receberDoCliente() {
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ca.parar:
			return
		default:
		}
		_ = ca.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := ca.conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		amostras := n / bytesPorAmostra
		if amostras == 0 {
			continue
		}
		ca.mu.Lock()
		for i := 0; i < amostras; i++ {
			ca.pendep = append(ca.pendep, math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:])))
		}
		var prontos [][]float32
		for len(ca.pendep) >= FrameSamples {
			f := make([]float32, FrameSamples)
			copy(f, ca.pendep[:FrameSamples])
			ca.pendep = ca.pendep[FrameSamples:]
			prontos = append(prontos, f)
		}
		ca.mu.Unlock()

		_, sink := ca.call.playerAndSink()
		if sink == nil {
			continue
		}
		for _, f := range prontos {
			if err := sink.WriteFrame(f); err != nil {
				return
			}
		}
	}
}

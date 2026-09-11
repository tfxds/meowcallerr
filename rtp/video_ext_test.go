package rtp

import (
	"encoding/binary"
	"testing"
)

// TestAudioHeaderNaoMudouComVideoExtension é o teste que importa de verdade: a extensão de
// vídeo foi adicionada AO LADO da palavra de áudio, então todo pacote de voz tem que sair
// byte a byte igual ao que saía antes. O áudio custou três meses pra funcionar.
func TestAudioHeaderNaoMudouComVideoExtension(t *testing.T) {
	semExtensao := RtpHeader{
		PayloadType:    RtpPayloadTypeOpus,
		SequenceNumber: 7,
		Timestamp:      160,
		Ssrc:           0xdeadbeef,
		Marker:         true,
	}
	if got := semExtensao.ByteSize(); got != WhatsappRtpHeaderSize {
		t.Fatalf("ByteSize sem extensão = %d, queria %d", got, WhatsappRtpHeaderSize)
	}
	buf := EncodeRtpHeader(&semExtensao)
	if len(buf) != 16 {
		t.Fatalf("áudio simples = %d bytes, queria 16", len(buf))
	}
	if buf[0]&0x10 == 0 {
		t.Error("X deveria estar ligado (o WhatsApp sempre manda o perfil 0xdebe)")
	}
	if p := binary.BigEndian.Uint16(buf[12:14]); p != WhatsappRtpExtensionProfile {
		t.Errorf("perfil = %#x, queria %#x", p, WhatsappRtpExtensionProfile)
	}
	if w := binary.BigEndian.Uint16(buf[14:16]); w != 0 {
		t.Errorf("palavras de extensão = %d, queria 0", w)
	}

	palavra := WhatsappRtpExtensionDtxWord
	comDtx := semExtensao
	comDtx.ExtensionWord = &palavra
	if got := comDtx.ByteSize(); got != WhatsappRtpHeaderDtxSize {
		t.Fatalf("ByteSize com DTX = %d, queria %d", got, WhatsappRtpHeaderDtxSize)
	}
	dtx := EncodeRtpHeader(&comDtx)
	if len(dtx) != 20 {
		t.Fatalf("áudio DTX = %d bytes, queria 20", len(dtx))
	}
	if w := binary.BigEndian.Uint16(dtx[14:16]); w != 1 {
		t.Errorf("palavras de extensão no DTX = %d, queria 1", w)
	}
	if v := binary.BigEndian.Uint32(dtx[16:20]); v != WhatsappRtpExtensionDtxWord {
		t.Errorf("palavra DTX = %#x, queria %#x", v, WhatsappRtpExtensionDtxWord)
	}
}

// TestRotacaoCvoNosDoisBitsBaixos fixa a razão de ser da correção: quem recebe gira o vídeo
// pelos dois bits baixos do MediaFrameInfo, não pela orientação anunciada na stanza.
func TestRotacaoCvoNosDoisBitsBaixos(t *testing.T) {
	for _, giro := range []int{0, 1, 2, 3} {
		ext := &VideoRtpExtension{MediaFrameInfo: VideoMediaFrameInfoIDR | uint8(giro)}
		if got := ext.DisplayOrientation(); got != giro {
			t.Errorf("DisplayOrientation(%d) = %d", giro, got)
		}
		// O marcador de IDR tem que sobreviver ao carimbo da rotação.
		if ext.MediaFrameInfo&VideoMediaFrameInfoIDR == 0 {
			t.Errorf("giro %d apagou o bit de IDR", giro)
		}
	}
}

func TestExtensaoDeVideoEncode(t *testing.T) {
	quadro := uint16(9)
	ext := &VideoRtpExtension{
		MediaFrameInfo:    VideoMediaFrameInfoIDR | 2,
		FrameNumber:       &quadro,
		TransportSequence: 0x1234,
	}
	bruto := ext.encode()
	if len(bruto)%4 != 0 {
		t.Fatalf("encode = %d bytes, tem que ser múltiplo de 4", len(bruto))
	}
	// Cabeçalho de 1 byte: id 3, tamanho-1 = 2 (MediaFrameInfo + 2 bytes de frame number).
	if bruto[0] != 0x32 {
		t.Errorf("cabeçalho do frame info = %#x, queria 0x32", bruto[0])
	}
	if bruto[1] != (VideoMediaFrameInfoIDR | 2) {
		t.Errorf("MediaFrameInfo = %#x", bruto[1])
	}
	if n := binary.BigEndian.Uint16(bruto[2:4]); n != 9 {
		t.Errorf("frame number = %d, queria 9", n)
	}

	// Sem frame number o campo encolhe pra 1 byte e o cabeçalho vira 0x30.
	semQuadro := &VideoRtpExtension{MediaFrameInfo: VideoMediaFrameInfoDelta}
	if b := semQuadro.encode(); b[0] != 0x30 {
		t.Errorf("cabeçalho sem frame number = %#x, queria 0x30", b[0])
	}
}

func TestHeaderDeVideoCarregaAExtensao(t *testing.T) {
	quadro := uint16(1)
	ext := &VideoRtpExtension{
		MediaFrameInfo:    VideoMediaFrameInfoIDR | 3,
		FrameNumber:       &quadro,
		TransportSequence: 5,
	}
	hdr := RtpHeader{
		PayloadType:    RtpPayloadTypeH264,
		SequenceNumber: 1,
		Timestamp:      90000,
		Ssrc:           0x11223344,
		Marker:         true,
		VideoExtension: ext,
	}
	esperado := 16 + len(ext.encode())
	if got := hdr.ByteSize(); got != esperado {
		t.Fatalf("ByteSize = %d, queria %d", got, esperado)
	}
	buf := EncodeRtpHeader(&hdr)
	if len(buf) != esperado {
		t.Fatalf("encode = %d bytes, queria %d", len(buf), esperado)
	}
	if buf[0]&0x10 == 0 {
		t.Error("X tem que estar ligado")
	}
	if p := binary.BigEndian.Uint16(buf[12:14]); p != WhatsappRtpExtensionProfile {
		t.Errorf("perfil = %#x", p)
	}
	if w := binary.BigEndian.Uint16(buf[14:16]); int(w) != len(ext.encode())/4 {
		t.Errorf("palavras de extensão = %d, queria %d", w, len(ext.encode())/4)
	}
	// O corpo tem que ser exatamente o encode da extensão.
	corpo := buf[16:]
	for i, b := range ext.encode() {
		if corpo[i] != b {
			t.Fatalf("byte %d do corpo = %#x, queria %#x", i, corpo[i], b)
		}
	}
	// E o comprimento anunciado tem que bater com o que o parser de tamanho calcula.
	if n, ok := RtpHeaderByteLength(buf); !ok || n != esperado {
		t.Errorf("RtpHeaderByteLength = %d ok=%v, queria %d", n, ok, esperado)
	}
}

package rtp

import (
	"bytes"
	"encoding/binary"

	"github.com/purpshell/meowcaller/srtp"
)

// RTP WARP framing: WhatsApp's 16-byte speech / 20-byte DTX headers (extension
// profile 0xdebe), Opus payload classifiers, and the send-side sequencer.

const (
	RtpPayloadTypeOpus uint8 = 120
	// RtpPayloadTypeH264 é o payload type do vídeo (H.264). Não veio no rtp.go novo do
	// upstream (lá o vídeo migrou pra outro commit, fora desta cadeia de RTCP), mas o nosso
	// engine_media.go demultiplexa áudio/vídeo por ele. Mantido como adição local.
	RtpPayloadTypeH264          uint8  = 97
	WhatsappRtpExtensionProfile uint16 = 0xdebe
	WhatsappRtpHeaderSize       int    = 16
	WhatsappRtpHeaderDtxSize    int    = 20
	WhatsappRtpExtensionDtxWord uint32 = 0x30010000

	rtpVersion          = 2
	srtpAuthTagLen      = 10
	srtpAuthTagLenShort = 4
)

// OpusPrimingFrame1 is the Android first priming frame (18 bytes).
var OpusPrimingFrame1 = [18]byte{
	0x12, 0x36, 0x26, 0x2b, 0x4a, 0xc8, 0x2b, 0x09, 0xc9, 0x1f, 0x34, 0xc2, 0xd6, 0x7a, 0x01, 0x73,
	0x1b, 0x2e,
}

// OpusPrimingFrame1Wasm is the WASM/Web caller priming frame (24 bytes).
var OpusPrimingFrame1Wasm = [24]byte{
	0x32, 0x36, 0x26, 0x2b, 0x4a, 0xcb, 0x1b, 0x5f, 0xba, 0x91, 0x68, 0x7e, 0xb8, 0x50, 0x93, 0x58,
	0xe6, 0xd0, 0xa3, 0xa9, 0xd7, 0x1d, 0x81, 0x8c,
}

// OpusPrimingFrame2 is the second priming frame (5 bytes).
var OpusPrimingFrame2 = [5]byte{0x90, 0xb8, 0x14, 0x14, 0xc4}

// IsWhatsappOpusRtpPayload reports whether the payload type is WhatsApp Opus.
func IsWhatsappOpusRtpPayload(payloadType uint8) bool {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L28-L30
	return payloadType == RtpPayloadTypeOpus || payloadType == 121
}

// IsOpusDtxPayload reports DTX / comfort-noise frames (RFC 0x10, mlow 0x90, warmup).
func IsOpusDtxPayload(payload []byte) bool {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L33-L46
	switch len(payload) {
	case 0:
		return false
	case 1:
		return payload[0] == 0x10 || payload[0] == 0x88 || payload[0] == 0x90
	default:
		if len(payload) > 15 {
			return false
		}
		b0 := payload[0]
		if (b0&0xf8) == 0x08 || b0 == 0x0a {
			return true
		}
		return (b0&0xf0) == 0x30 && len(payload) <= 6
	}
}

// IsOpusMlowSpeechPayload reports mlow speech frames (20ms 0x48..0x4f or 60ms 0x50..0x57).
func IsOpusMlowSpeechPayload(payload []byte) bool {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L49-L55
	if len(payload) < 18 {
		return false
	}
	b0 := payload[0]
	return (b0&0xf8) == 0x48 || (b0&0xf8) == 0x50
}

// IsOpusPrimingPayload reports whether the payload equals a priming frame.
func IsOpusPrimingPayload(payload []byte) bool {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L57-L59
	return bytes.Equal(payload, OpusPrimingFrame1[:]) || bytes.Equal(payload, OpusPrimingFrame2[:])
}

// EstimateSrtpRtpWireBytes estimates the on-wire SRTP size (header + opus + tag).
func EstimateSrtpRtpWireBytes(opusPayload []byte) int {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L62-L79
	dtx := IsOpusDtxPayload(opusPayload)
	headerSize := WhatsappRtpHeaderSize
	if dtx {
		headerSize = WhatsappRtpHeaderDtxSize
	}
	shortTag := (dtx && headerSize == WhatsappRtpHeaderDtxSize) ||
		(!dtx && headerSize == WhatsappRtpHeaderSize &&
			(IsOpusPrimingPayload(opusPayload) || len(opusPayload) <= 18))
	tagLen := srtpAuthTagLen
	if shortTag {
		tagLen = srtpAuthTagLenShort
	}
	return headerSize + len(opusPayload) + tagLen
}

// Bits de MediaFrameInfo da extensão de vídeo. Os DOIS BITS BAIXOS são a rotação CVO
// (quartos de volta no sentido horário, TS 26.114) — ver VideoRtpExtension.DisplayOrientation.
const (
	VideoMediaFrameInfoIDR   uint8 = 0x08
	VideoMediaFrameInfoDelta uint8 = 0x20
)

// VideoRtpExtension é o conjunto de metadados que o WhatsApp manda na extensão 0xdebe dos
// pacotes de VÍDEO — diferente do áudio, que usa uma única palavra de 4 bytes (ExtensionWord).
type VideoRtpExtension struct {
	MediaFrameInfo    uint8
	FrameNumber       *uint16
	InitialBandwidth  uint16
	ShortOffset       int16
	TransportSequence uint16
}

// DisplayOrientation devolve a rotação CVO em quartos de volta no sentido horário.
// Fonte: ETSI TS 26.114 (https://www.etsi.org/deliver/etsi_ts/126100_126199/126114/15.05.00_60/ts_126114v150500p.pdf)
func (e *VideoRtpExtension) DisplayOrientation() int {
	return int(e.MediaFrameInfo & 0x03)
}

// encode serializa a extensão no formato de cabeçalho de 1 byte (id<<4 | tamanho-1),
// com padding até múltiplo de 4.
func (e *VideoRtpExtension) encode() []byte {
	frameInfoLength := 1
	if e.FrameNumber != nil {
		frameInfoLength = 3
	}
	ext := make([]byte, 0, 16)
	ext = append(ext, 0x30|byte(frameInfoLength-1), e.MediaFrameInfo)
	if e.FrameNumber != nil {
		ext = binary.BigEndian.AppendUint16(ext, *e.FrameNumber)
	}
	ext = append(ext, 0x51)
	ext = binary.BigEndian.AppendUint16(ext, e.InitialBandwidth)
	ext = append(ext, 0x61)
	ext = binary.BigEndian.AppendUint16(ext, uint16(e.ShortOffset))
	ext = append(ext, 0x91)
	ext = binary.BigEndian.AppendUint16(ext, e.TransportSequence)
	for len(ext)%4 != 0 {
		ext = append(ext, 0)
	}
	return ext
}

// RtpHeader is the fixed RTP header plus an optional 0xdebe extension word.
type RtpHeader struct {
	Marker         bool
	PayloadType    uint8
	SequenceNumber uint16
	Timestamp      uint32
	Ssrc           uint32
	ExtensionWord  *uint32            // nil = no 0xdebe extension word (áudio)
	VideoExtension *VideoRtpExtension // nil = sem bloco de extensão de vídeo
}

// ByteSize is the on-wire header size (16, 20 com a palavra de áudio, ou 16+bloco no vídeo).
func (h *RtpHeader) ByteSize() int {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L93-L99
	// O vídeo vem PRIMEIRO: quando VideoExtension é nil o caminho do áudio fica idêntico
	// byte a byte ao de antes, que é o que mantém a voz (duramente conquistada) intocada.
	if h.VideoExtension != nil {
		return WhatsappRtpHeaderSize + len(h.VideoExtension.encode())
	}
	if h.ExtensionWord != nil {
		return WhatsappRtpHeaderDtxSize
	}
	return WhatsappRtpHeaderSize
}

// RtpHeaderByteLength returns the full on-wire header size (12 + CSRC + ext); ok=false if malformed.
func RtpHeaderByteLength(data []byte) (int, bool) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L103-L127
	if len(data) < 12 {
		return 0, false
	}
	if (data[0]>>6)&0x03 != rtpVersion {
		return 0, false
	}
	cc := int(data[0] & 0x0f)
	headerLen := 12 + cc*4
	if len(data) < headerLen {
		return 0, false
	}
	hasExtension := (data[0]>>4)&1 == 1
	if hasExtension {
		if len(data) < headerLen+4 {
			return 0, false
		}
		extWords := (int(data[headerLen+2]) << 8) | int(data[headerLen+3])
		headerLen += 4 + extWords*4
		if len(data) < headerLen {
			return 0, false
		}
	}
	return headerLen, true
}

// IsRtpVersion2 reports a version-2 RTP packet.
func IsRtpVersion2(data []byte) bool {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L129-L131
	return len(data) >= 12 && (data[0]>>6)&0x03 == rtpVersion
}

// ParseRtpHeader parses the fixed RTP header fields (the extension word is not decoded).
func ParseRtpHeader(data []byte) (RtpHeader, bool) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L134-L144
	if _, ok := RtpHeaderByteLength(data); !ok {
		return RtpHeader{}, false
	}
	return RtpHeader{
		Marker:         (data[1]>>7)&1 == 1,
		PayloadType:    data[1] & 0x7f,
		SequenceNumber: (uint16(data[2]) << 8) | uint16(data[3]),
		Timestamp:      binary.BigEndian.Uint32(data[4:8]),
		Ssrc:           binary.BigEndian.Uint32(data[8:12]),
		ExtensionWord:  nil,
	}, true
}

// EncodeRtpHeader encodes the RTP header (16 or 20 bytes with the 0xdebe extension).
func EncodeRtpHeader(header *RtpHeader) []byte {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L146-L175
	size := header.ByteSize()
	buf := make([]byte, size)
	buf[0] = rtpVersion << 6
	if size > 12 {
		buf[0] |= 0x10 // X=1 (WhatsApp 0xdebe extension)
	}
	buf[1] = header.PayloadType & 0x7f
	if header.Marker {
		buf[1] |= 0x80
	}
	buf[2] = byte(header.SequenceNumber >> 8)
	buf[3] = byte(header.SequenceNumber)
	binary.BigEndian.PutUint32(buf[4:8], header.Timestamp)
	binary.BigEndian.PutUint32(buf[8:12], header.Ssrc)
	if size >= 16 {
		binary.BigEndian.PutUint16(buf[12:14], WhatsappRtpExtensionProfile)
		var extWords uint16
		switch {
		case header.VideoExtension != nil:
			extWords = uint16(len(header.VideoExtension.encode()) / 4)
		case header.ExtensionWord != nil:
			extWords = 1
		}
		binary.BigEndian.PutUint16(buf[14:16], extWords)
	}
	if header.VideoExtension != nil {
		copy(buf[16:], header.VideoExtension.encode())
		return buf
	}
	if size >= 20 && header.ExtensionWord != nil {
		binary.BigEndian.PutUint32(buf[16:20], *header.ExtensionWord)
	}
	return buf
}

// RtpStream is the send-side RTP sequencer: seq starts at 1, timestamp advances per packet.
type RtpStream struct {
	Ssrc             uint32
	seq              uint16
	timestamp        uint32
	samplesPerPacket uint32
	speechStarted    bool
	audioPacketIndex int
	warpPiggyback    bool
}

// NewRtpStream builds a sequencer for ssrc with samplesPerPacket per packet.
func NewRtpStream(ssrc, samplesPerPacket uint32, warpPiggyback bool) *RtpStream {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L189-L200
	return &RtpStream{
		Ssrc:             ssrc,
		seq:              1,
		samplesPerPacket: samplesPerPacket,
		warpPiggyback:    warpPiggyback,
	}
}

// resolveWarpExtension picks the extension word: the DTX word for DTX frames, else
// the warp audio piggyback word when piggyback is enabled, else nil.
func (s *RtpStream) resolveWarpExtension(dtx bool) *uint32 {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L201-L212
	if dtx {
		w := WhatsappRtpExtensionDtxWord
		return &w
	}
	if !s.warpPiggyback {
		return nil
	}
	idx := s.audioPacketIndex
	s.audioPacketIndex++
	return srtp.AudioPiggybackExtensionFor(idx, true, srtp.WarpPiggybackStartPacket)
}

// NextPacket builds the next RTP header for payload, latching the marker on the first speech frame.
func (s *RtpStream) NextPacket(payload []byte, marker bool) RtpHeader {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L213-L234
	dtx := IsOpusDtxPayload(payload)
	priming := IsOpusPrimingPayload(payload)
	speech := !dtx && !priming
	useMarker := marker || (speech && !s.speechStarted)
	if speech {
		s.speechStarted = true
	}
	header := RtpHeader{
		Marker:         useMarker,
		PayloadType:    RtpPayloadTypeOpus,
		SequenceNumber: s.seq,
		Timestamp:      s.timestamp,
		Ssrc:           s.Ssrc,
		ExtensionWord:  s.resolveWarpExtension(dtx),
	}
	s.seq++
	s.timestamp += s.samplesPerPacket
	return header
}

// NextPreSpeechPacket builds a pre-speech ladder packet (advances seq/timestamp, no marker/latch).
func (s *RtpStream) NextPreSpeechPacket() RtpHeader {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtp.rs#L235-L247
	header := RtpHeader{
		Marker:         false,
		PayloadType:    RtpPayloadTypeOpus,
		SequenceNumber: s.seq,
		Timestamp:      s.timestamp,
		Ssrc:           s.Ssrc,
		ExtensionWord:  s.resolveWarpExtension(false),
	}
	s.seq++
	s.timestamp += s.samplesPerPacket
	return header
}

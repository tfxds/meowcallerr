package rtp

import "encoding/binary"

// RTCP: WhatsApp compact reports (PT 208/209) and a Sender Report (PT 200). The
// SR's NTP timestamp is taken as a nowMs argument so this stays pure/no-clock.

const (
	RtcpPtSr         uint8 = 200
	RtcpPtWaCompact  uint8 = 208
	RtcpPtWaCompact2 uint8 = 209
	RtcpHeaderLen    int   = 8
	SrtcpTrailerLen  int   = 14

	ntpUnixOffsetSecs uint64 = 2208988800
)

// IsRtcpPacket reports whether data is an RTCP packet (vs a WhatsApp RTP packet).
func IsRtcpPacket(data []byte) bool {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtcp.rs#L16-L28
	if len(data) < RtcpHeaderLen+SrtcpTrailerLen {
		return false
	}
	if (data[0]>>6)&0x03 != 2 {
		return false
	}
	// WhatsApp RTP uses X=1 (byte0 0x90) and a 7-bit PT in byte1; RTCP uses the full byte1 as PT.
	if data[0]&0x10 != 0 && data[1]&0x7f == RtpPayloadTypeOpus {
		return false
	}
	return data[1] >= 64
}

// RtcpPayloadType returns the RTCP payload type; ok=false if not an RTCP packet.
func RtcpPayloadType(data []byte) (uint8, bool) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtcp.rs#L30-L32
	if !IsRtcpPacket(data) {
		return 0, false
	}
	return data[1], true
}

// ParseRtcpSenderSsrc returns the sender SSRC (bytes 4-7); ok=false if malformed.
func ParseRtcpSenderSsrc(data []byte) (uint32, bool) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtcp.rs#L34-L39
	if len(data) < 8 || (data[0]>>6)&0x03 != 2 {
		return 0, false
	}
	return binary.BigEndian.Uint32(data[4:8]), true
}

// RtcpSenderStats are the Sender Report counters.
type RtcpSenderStats struct {
	PacketsSent  uint32
	OctetsSent   uint32
	RtpTimestamp uint32
}

// BuildCompactRtcp208 builds the 12-byte compact RTCP (PT 208, RC=1).
func BuildCompactRtcp208(localSsrc, remoteSsrc uint32) [12]byte {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtcp.rs#L49-L58
	var buf [12]byte
	buf[0] = 0x81 // V=2, P=0, RC=1
	buf[1] = RtcpPtWaCompact
	buf[3] = 2 // (2+1)*4 = 12 bytes
	binary.BigEndian.PutUint32(buf[4:8], localSsrc)
	binary.BigEndian.PutUint32(buf[8:12], remoteSsrc)
	return buf
}

// BuildCompactRtcp209 builds the 8-byte compact RTCP (PT 209, RC=1).
func BuildCompactRtcp209(localSsrc uint32) [8]byte {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtcp.rs#L61-L69
	var buf [8]byte
	buf[0] = 0x81
	buf[1] = RtcpPtWaCompact2
	buf[3] = 1 // (1+1)*4 = 8 bytes
	binary.BigEndian.PutUint32(buf[4:8], localSsrc)
	return buf
}

// BuildSenderReport builds the 28-byte Sender Report (PT 200, RC=0); nowMs is wall-clock ms.
func BuildSenderReport(localSsrc uint32, stats *RtcpSenderStats, nowMs uint64) [28]byte {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/rtcp.rs#L72-L89
	var buf [28]byte
	buf[0] = 0x80 // V=2, RC=0
	buf[1] = RtcpPtSr
	buf[3] = 6 // (6+1)*4 = 28 bytes
	binary.BigEndian.PutUint32(buf[4:8], localSsrc)
	// NTP timestamp: seconds (upper 32) since 1900, fraction (lower 32). Both truncate
	// to u32 (wrapping), matching the reference encoder.
	ntpSec := uint32((nowMs / 1000) + ntpUnixOffsetSecs)
	ntpFrac := uint32(float64(nowMs%1000) / 1000.0 * 4294967296.0)
	binary.BigEndian.PutUint32(buf[8:12], ntpSec)
	binary.BigEndian.PutUint32(buf[12:16], ntpFrac)
	binary.BigEndian.PutUint32(buf[16:20], stats.RtpTimestamp)
	binary.BigEndian.PutUint32(buf[20:24], stats.PacketsSent)
	binary.BigEndian.PutUint32(buf[24:28], stats.OctetsSent)
	return buf
}

// ─── Compound SR+SDES 1:1 (portado do upstream purpshell/meowcaller) ──────────
// O datasheet do upstream chama este de "the existing byte-verified one-report
// Sender Report plus SDES packet" — é o formato que o motor real manda numa chamada
// 1:1. O SR de 28 bytes sozinho (BuildSenderReport) é só a primeira parte dele.

// WhatsappRtcpCnameLen é o tamanho do CNAME randômico que a WhatsApp usa no SDES.
const WhatsappRtcpCnameLen = 18

// RtcpPtSdes é o payload type do Source Description (RFC 3550).
const RtcpPtSdes uint8 = 202

// RtcpReceptionReport é o bloco de recepção RFC 3550 que vai dentro do Sender Report.
// A WhatsApp acrescenta 24 bytes de estatística de transporte depois dele.
type RtcpReceptionReport struct {
	Ssrc                       uint32
	FractionLost               uint8
	CumulativeLost             int32
	ExtendedHighestSequence    uint32
	Jitter                     uint32
	LastSenderReport           uint32
	DelaySinceLastSenderReport uint32
}

// BuildWhatsappRtcpCname monta o CNAME de 18 bytes no formato nativo (`xxxxx@pjxxxxxx.org`).
func BuildWhatsappRtcpCname(entropy [12]byte) [WhatsappRtcpCnameLen]byte {
	const hexChars = "0123456789abcdef"
	var randomHex [11]byte
	for nibble := range randomHex {
		b := entropy[6+nibble/2]
		if nibble&1 == 0 {
			randomHex[nibble] = hexChars[b>>4]
		} else {
			randomHex[nibble] = hexChars[b&0x0f]
		}
	}
	var cname [WhatsappRtcpCnameLen]byte
	copy(cname[:5], randomHex[:5])
	copy(cname[5:8], "@pj")
	copy(cname[8:14], randomHex[5:])
	copy(cname[14:], ".org")
	return cname
}

// BuildSourceDescription monta o SDES de uma chunk, do jeito da WhatsApp.
func BuildSourceDescription(localSsrc uint32, cname *[WhatsappRtcpCnameLen]byte, profileExtension bool) [32]byte {
	var packet [32]byte
	packet[0] = 0x81
	if profileExtension {
		packet[0] |= 0x10
	}
	packet[1] = RtcpPtSdes
	binary.BigEndian.PutUint16(packet[2:4], 7)
	binary.BigEndian.PutUint32(packet[4:8], localSsrc)
	packet[8] = 1
	packet[9] = WhatsappRtcpCnameLen
	copy(packet[10:28], cname[:])
	return packet
}

func appendReceptionReport(out []byte, report *RtcpReceptionReport) []byte {
	out = binary.BigEndian.AppendUint32(out, report.Ssrc)
	out = append(out, report.FractionLost)
	lost := uint32(report.CumulativeLost)
	out = append(out, byte(lost>>16), byte(lost>>8), byte(lost))
	out = binary.BigEndian.AppendUint32(out, report.ExtendedHighestSequence)
	out = binary.BigEndian.AppendUint32(out, report.Jitter)
	out = binary.BigEndian.AppendUint32(out, report.LastSenderReport)
	out = binary.BigEndian.AppendUint32(out, report.DelaySinceLastSenderReport)
	return out
}

// BuildSenderReportWithSdes monta o compound SR+SDES periódico da WhatsApp.
func BuildSenderReportWithSdes(localSsrc uint32, stats *RtcpSenderStats, nowMs uint64, cname *[WhatsappRtcpCnameLen]byte, profileExtension bool) []byte {
	return BuildSenderReportWithSdesAndReception(localSsrc, stats, nowMs, cname, nil, profileExtension)
}

// BuildSenderReportWithSdesAndReception monta o compound 1:1 nativo. O bloco de recepção é
// o layout RFC 3550 seguido de 24 campos zerados de transporte/BWE — que é como o nativo
// representa "valor indisponível".
func BuildSenderReportWithSdesAndReception(localSsrc uint32, stats *RtcpSenderStats, nowMs uint64, cname *[WhatsappRtcpCnameLen]byte, report *RtcpReceptionReport, profileExtension bool) []byte {
	sr := BuildSenderReport(localSsrc, stats, nowMs)
	if profileExtension {
		sr[0] |= 0x10
	}
	sdes := BuildSourceDescription(localSsrc, cname, profileExtension)
	out := make([]byte, 0, len(sr)+48+len(sdes))
	out = append(out, sr[:]...)
	if report != nil {
		out[0] |= 1
		out = appendReceptionReport(out, report)
		out = append(out, make([]byte, 24)...)
		binary.BigEndian.PutUint16(out[2:4], uint16(len(out)/4-1))
	}
	out = append(out, sdes[:]...)
	return out
}

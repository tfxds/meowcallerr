package wacallsrelay

// Constantes que no WaCalls vinham de internal/voip/core. Trazidas verbatim pro port
// do SctpRelayManager (transporte multi-relay pion que recebe o stream do peer no inbound).
// Source of truth: github.com/JotaDev66/WaCalls internal/voip/core/types.go
const (
	// WARelayPort é a porta default do relay quando o endpoint não traz porta (o offer
	// normalmente traz :3478; isto é só fallback, igual o WaCalls).
	WARelayPort = 3480

	// WADTLSFingerprint é o fingerprint DTLS FIXO que o WaCalls injeta no SDP da answer do
	// relay (modifySdpForRelay). O relay do WhatsApp espera exatamente este fingerprint.
	WADTLSFingerprint = "sha-256 F9:CA:0C:98:A3:CC:71:D6:42:CE:5A:E2:53:D2:15:20:D3:1B:BA:D8:57:A4:F0:AF:BE:0B:FB:F3:6B:0C:A0:68"
)

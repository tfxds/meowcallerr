package relay

import "github.com/rs/zerolog"

// Option configures optional, non-behavioral aspects of the relay channel —
// currently the diagnostic logger. The zero configuration logs nothing.
type Option func(*config)

type config struct {
	log zerolog.Logger
	// Credenciais ICE usadas só pelo transporte pion (ConnectRelayMediaPion) ao mungir o
	// SDP do relay: iceUfrag = base64(auth_token) e icePwd = <key> ASCII. Vazias no
	// transporte manual (que não faz ICE).
	iceUfrag        string
	icePwd          string
	dtlsFingerprint string
}

func resolveConfig(opts []Option) config {
	c := config{log: zerolog.Nop()}
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

// WithLogger sets the zerolog logger for debug/trace diagnostics. The library never
// configures logging itself; without this option the channel is silent at zero cost.
// Pass the logger from a context, e.g. WithLogger(*zerolog.Ctx(ctx)).
func WithLogger(l zerolog.Logger) Option {
	return func(c *config) { c.log = l }
}

// WithRelayICECredentials informa as credenciais ICE que o transporte pion grava no SDP
// mungido do relay (resposta): ufrag = base64(auth_token) selecionado e pwd = <key> ASCII
// do relay. É com elas que o agente ICE do pion autentica os connectivity-checks /
// consent-freshness no relay — exatamente o passo que o transporte manual fazia de forma
// intermitente. Ignorada pelo transporte manual.
func WithRelayICECredentials(ufrag, pwd string) Option {
	return func(c *config) {
		c.iceUfrag = ufrag
		c.icePwd = pwd
	}
}

// WithRelayDTLSFingerprint sobrescreve o fingerprint DTLS gravado no SDP mungido do relay.
// Sem ele o transporte pion usa WARelayDTLSFingerprint (o fingerprint fixo do relay do WA).
func WithRelayDTLSFingerprint(fp string) Option {
	return func(c *config) { c.dtlsFingerprint = fp }
}

func pickLog(log []zerolog.Logger) zerolog.Logger {
	if len(log) > 0 {
		return log[0]
	}
	return zerolog.Nop()
}

/**
 * TESTE DE REFERÊNCIA — SheIITear ORIGINAL, sem ponte nenhuma.
 *
 * Pergunta única que este script responde: **o cliente de referência recebe o áudio de uma
 * chamada de ENTRADA neste servidor?**
 *   - Se NÃO recebe → o problema não é o nosso código; é o 225 (rede / relay).
 *   - Se recebe → a ponte tem uma diferença que a leitura de código não achou.
 *
 * Roda no MESMO servidor do gateway de propósito: assim a única variável é o código.
 * Usa um NÚMERO SEPARADO (nunca o de produção — dois dispositivos linkados brigam pela
 * chamada e um rejeita pelo outro, o que estragaria a medição).
 *
 *   node --import tsx referencia.mts
 */
import { writeFileSync } from "node:fs";
const LIB = process.env.SHEITEAR_LIB || "/opt/wa-call-service/lib/sheitear";
const { VoipClient } = await import(`${LIB}/src/index.mjs`) as any;
import QRCode from "qrcode";

const AUTH = process.env.AUTH_DIR || "./auth/teste-referencia";
const QR_PNG = process.env.QR_PNG || "/tmp/qr-referencia.png";

const t0 = Date.now();
const log = (m: string) => console.log(`[${((Date.now() - t0) / 1000).toFixed(1)}s] ${m}`);

const cliente = new VoipClient({
  authDir: AUTH,
  onQr: async (qr: string) => {
    try {
      await QRCode.toFile(QR_PNG, qr, { width: 420, margin: 2 });
      log(`📷 QR gravado em ${QR_PNG} — escanear em Aparelhos conectados`);
    } catch (e: any) {
      log("falhou gravar o QR: " + e?.message);
    }
  },
});

cliente.on("incoming", (chamada: any) => {
  log(`📞 CHAMADA RECEBENDO ${chamada.callId} — atendendo`);

  let frames = 0;
  let pico = 0;
  let primeiraVoz = 0;
  chamada.on("audio", (pcm: Float32Array) => {
    frames += 1;
    let soma = 0;
    for (let i = 0; i < pcm.length; i++) soma += pcm[i] * pcm[i];
    const rms = Math.sqrt(soma / (pcm.length || 1));
    if (rms > pico) pico = rms;
    // rms alto e SUSTENTADO é voz; o motor emite um tom sintético curto no começo.
    if (rms > 0.01 && frames > 60 && !primeiraVoz) {
      primeiraVoz = frames;
      log(`🔊🔊 VOZ DE VERDADE no frame ${frames} (rms ${rms.toFixed(4)})`);
    }
    if (frames === 1 || frames % 50 === 0) {
      log(`🔊 áudio: frames=${frames} rms=${rms.toFixed(5)} pico=${pico.toFixed(5)}`);
    }
  });

  chamada.on("connected", () => log("✅ chamada CONECTADA"));
  chamada.on("ringing", () => log("🔔 tocando"));
  chamada.waitForEnd().then((motivo: string) => {
    log(`🔚 fim (${motivo}) — frames=${frames} pico=${pico.toFixed(5)} ` +
        (primeiraVoz ? `VOZ a partir do frame ${primeiraVoz}` : "❌ NENHUMA VOZ"));
  });

  setTimeout(() => { try { chamada.accept(); log("accept enviado"); } catch (e: any) { log("accept falhou: " + e?.message); } }, 800);
});

/**
 * Pareamento por CÓDIGO em vez de QR: o QR gira a cada ~20s e some antes de chegar no
 * celular; o código de 8 caracteres dura minutos e é só digitar.
 * Ligar com PAIR_PHONE=5548920046588 (só dígitos, com DDI).
 */
const PAIR_PHONE = (process.env.PAIR_PHONE || "").replace(/\D/g, "");
function pedirCodigo() {
  if (!PAIR_PHONE) return;
  const timer = setInterval(async () => {
    const sock: any = (cliente as any)._getSock?.();
    if (!sock || typeof sock.requestPairingCode !== "function") return;
    if (sock.authState?.creds?.registered) { clearInterval(timer); return; }
    clearInterval(timer);
    try {
      const codigo = await sock.requestPairingCode(PAIR_PHONE);
      log(`🔑🔑 CÓDIGO DE PAREAMENTO: ${String(codigo).match(/.{1,4}/g)?.join("-")}`);
    } catch (e: any) {
      log("código de pareamento falhou: " + e?.message);
    }
  }, 700);
}

async function main() {
  log("subindo o SheIITear original...");
  pedirCodigo();
  await cliente.connect();
  log("✅ conectado ao WhatsApp — pronto pra receber chamada");
}

main().catch((e) => { log("❌ " + (e?.stack || e?.message)); process.exit(1); });

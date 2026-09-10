/**
 * SIDECAR DE CHAMADA — roda o motor real do WhatsApp (whatsapp.wasm) como serviço.
 *
 * O gateway (Go) continua dono da conexão e da sinalização; aqui vive o motor de mídia:
 * o mesmo `whatsapp.wasm` que o WhatsApp Web usa, mais o transporte de relay (WebRTC).
 * Fronteira: stanza em base64 pra dentro, stanza em base64 pra fora.
 *
 *   POST /offer   { callId, payloadWasm, peerJid, ... }   → entrega a oferta ao motor
 *   POST /signal  { callId, payloadWasm, tipo, peerJid }  → demais stanzas (relay, terminate…)
 *   POST /accept  { callId }                              → atende de verdade
 *   POST /hangup  { callId }                              → encerra
 *   GET  /out?ms=25000                                    → long-poll: stanzas que o motor quer enviar
 *   GET  /health                                          → estado do motor
 *
 * O motor leva ~23s pra subir, então ele sobe UMA vez no boot e fica vivo.
 */
import http from "node:http";
import dgram from "node:dgram";
import { createHmac } from "node:crypto";
import { WasmEngine } from "/opt/wa-sidecar/lib/sheitear/src/wasm-engine.mjs";
import { RelayRtcTransport } from "/opt/wa-sidecar/lib/sheitear/src/relay-transport.mjs";

const BASE = "/opt/wa-sidecar/lib/sheitear";
const PORTA = Number(process.env.SIDECAR_PORT || 9099);

// Identidade do nosso número. Vem do gateway pelo ambiente (uma instância por sidecar).
const SELF_PN_DEV = process.env.SELF_PN_DEV || "554896552034:88@s.whatsapp.net";
const SELF_PN = SELF_PN_DEV.split(":")[0] + "@s.whatsapp.net";
const SELF_LID = process.env.SELF_LID || "39621610192990:88@lid";

const t0 = Date.now();
const log = (m: string) => console.log(`[${((Date.now() - t0) / 1000).toFixed(1)}s] ${m}`);

const SHA256_LEN = 32;
const hkdf = (key: Uint8Array, salt: Uint8Array | null, info: Uint8Array, length: number) => {
  const sal = salt && salt.length > 0 ? Buffer.from(salt) : Buffer.alloc(SHA256_LEN, 0);
  const prk = createHmac("sha256", sal).update(key).digest();
  const blocos = Math.ceil(length / SHA256_LEN);
  const okm = Buffer.alloc(blocos * SHA256_LEN);
  let ant = Buffer.alloc(0);
  for (let i = 1; i <= blocos; i += 1) {
    ant = createHmac("sha256", prk).update(ant).update(info).update(Buffer.from([i])).digest();
    ant.copy(okm, (i - 1) * SHA256_LEN);
  }
  return new Uint8Array(okm.buffer, okm.byteOffset, length);
};
const hmac = (data: Uint8Array, key: Uint8Array) => {
  const r = createHmac("sha256", Buffer.from(key)).update(data).digest();
  return new Uint8Array(r.buffer, r.byteOffset, r.byteLength);
};

let engine: any = null;
let relay: any = null;
let pronto = false;

/** Chamada corrente (o motor só toca uma por vez). */
let atual: { callId: string; peerJid: string; estado: number; desde: number } | null = null;

/** Fila de stanzas que o motor quer enviar — o gateway busca em /out (long-poll). */
type Saida = { callId: string; peerJid: string; b64: string; em: number };
const fila: Saida[] = [];
let acordar: (() => void) | null = null;
const enfileirar = (s: Saida) => {
  fila.push(s);
  if (acordar) { const f = acordar; acordar = null; f(); }
};

/** Áudio que chega do cliente — a prova de que o inbound finalmente funciona. */
let framesRx = 0;
let picoRms = 0;

/** Captura (microfone do atendente, que chega do gateway por UDP). */
let capPtr = 0, capSamples = 0, capTimer: NodeJS.Timeout | null = null;

// ─── Cano de áudio com o gateway (UDP em localhost) ────────────────────────────
// PCM mono 16 kHz float32 little-endian cru nos dois sentidos. O gateway manda blocos de
// 960 amostras (60 ms); o motor consome 320 (20 ms) e devolve o que decodifica do cliente.
const PORTA_AUDIO = Number(process.env.SIDECAR_AUDIO_PORT || 9098);
const udp = dgram.createSocket("udp4");
let gateway: { porta: number; host: string } | null = null;
let filaMic: Float32Array[] = [];   // o que veio do gateway, esperando virar bloco de 320
let sobraMic = new Float32Array(0);

udp.on("message", (msg, rinfo) => {
  gateway = { porta: rinfo.port, host: rinfo.address };
  const n = Math.floor(msg.byteLength / 4);
  if (!n) return;
  const f = new Float32Array(n);
  for (let i = 0; i < n; i++) f[i] = msg.readFloatLE(i * 4);
  filaMic.push(f);
  // Teto de ~200 ms (3200 amostras a 16 kHz). O setInterval de 20 ms do Node atrasa um
  // pouquinho a cada volta, então a fila cresce sozinha e o atendente chega com segundos
  // de atraso no celular do cliente. Em voz ao vivo, descartar o mais VELHO é melhor que
  // atrasar: o teto baixo é o que segura a latência.
  let acum = sobraMic.length;
  for (const b of filaMic) acum += b.length;
  while (acum > 3200 && filaMic.length) { acum -= filaMic.shift()!.length; }
});
udp.bind(PORTA_AUDIO, "127.0.0.1", () => log(`🎧 áudio UDP em 127.0.0.1:${PORTA_AUDIO}`));

/** Puxa exatamente `n` amostras da fila do microfone; completa com silêncio se faltar. */
function puxarDoMic(n: number): Float32Array {
  const out = new Float32Array(n);
  let escrito = 0;
  if (sobraMic.length) {
    const usa = Math.min(sobraMic.length, n);
    out.set(sobraMic.subarray(0, usa), 0);
    sobraMic = sobraMic.subarray(usa);
    escrito += usa;
  }
  while (escrito < n && filaMic.length) {
    const bloco = filaMic.shift()!;
    const usa = Math.min(bloco.length, n - escrito);
    out.set(bloco.subarray(0, usa), escrito);
    escrito += usa;
    if (usa < bloco.length) sobraMic = bloco.subarray(usa);
  }
  // Silêncio absoluto faz o motor reiniciar a captura; um ruído inaudível evita isso.
  for (let i = escrito; i < n; i++) out[i] = (Math.random() - 0.5) * 1e-4;
  return out;
}

/**
 * O motor compara o peerJid com o CREATOR de dentro da stanza e IGNORA a oferta se
 * diferirem ("mismatched peer id and creator id").
 *
 * ⭐ O SheIITear (referência, medido em 10/09) entrega `<lid>:0@lid` — e o SSRC do peer é
 * derivado DESSE JID. Domínio errado = SSRC errado = inscrição num fluxo que não existe,
 * que é exatamente o sintoma ("failed to receive rtp"). O `@s.whatsapp.net` aqui era um
 * remendo da época em que o self-LID ainda estava errado.
 * PEER_DOMINIO=pn volta pro comportamento antigo, pra poder comparar.
 */
const PEER_DOMINIO = process.env.PEER_DOMINIO === "pn" ? "s.whatsapp.net" : "lid";
const peerParaOMotor = (jid: string) =>
  String(jid || "").split("@")[0].split(":")[0] + ":0@" + PEER_DOMINIO;

async function subirMotor() {
  log("subindo o motor whatsapp.wasm...");

  relay = new RelayRtcTransport({
    onTransportMessage: (data: Uint8Array, ip: string, port: number) =>
      engine?.handleOnTransportMessage(data, ip, port),
    onIceRtt: (rtt: number, ip: string, port: number) => engine?.updateIceRtt(rtt, ip, port),
  });

  engine = new WasmEngine({
    resourcesPath: BASE,
    enableLogs: true,
    callbacks: {
      onSignalingXmpp: (peerJid: string, callId: string, xml: Uint8Array) => {
        enfileirar({ callId, peerJid, b64: Buffer.from(xml).toString("base64"), em: Date.now() });
        log(`📤 motor quer enviar ${xml.length}b em ${callId} (fila=${fila.length}) hex=${
          Buffer.from(xml).slice(0, 24).toString("hex")}`);
      },
      sendDataToRelay: (data: Uint8Array, ip: string, port: number) => relay?.send(data, ip, port),
      onCallEvent: (tipo: number, dados?: string) => aoEventoDoMotor(tipo, dados),
      onAudioCaptureInit: (cfg: any) => {
        capSamples = (cfg?.framesPerChunk || 320) * (cfg?.channels || 1);
        capPtr = engine.malloc(capSamples * 4);
        log(`🎙️ captura iniciada: ${cfg?.sampleRate}Hz x${cfg?.channels} ${cfg?.framesPerChunk}f`);
      },
      onAudioCaptureStart: () => {
        if (capTimer) clearInterval(capTimer);
        capTimer = setInterval(() => {
          if (engine && capPtr) {
            try { engine.sendAudioData(puxarDoMic(capSamples), capPtr); } catch {}
          }
        }, 20);
      },
      onAudioCaptureStop: () => {
        if (capTimer) { clearInterval(capTimer); capTimer = null; }
        if (engine && capPtr) { try { engine.free(capPtr); } catch {} capPtr = 0; }
      },
      onAudioPlaybackData: (pcm: Float32Array) => {
        // Voz do cliente → gateway → sink da chamada → navegador do atendente.
        if (gateway) {
          const b = Buffer.allocUnsafe(pcm.length * 4);
          for (let i = 0; i < pcm.length; i++) b.writeFloatLE(pcm[i], i * 4);
          udp.send(b, gateway.porta, gateway.host, () => {});
        }
        framesRx += 1;
        let soma = 0;
        for (let i = 0; i < pcm.length; i++) soma += pcm[i] * pcm[i];
        const rms = Math.sqrt(soma / (pcm.length || 1));
        if (rms > picoRms) picoRms = rms;
        if (framesRx === 1 || framesRx % 50 === 0) {
          log(`🔊 ÁUDIO DO CLIENTE: frames=${framesRx} rms=${rms.toFixed(5)} pico=${picoRms.toFixed(5)}`);
        }
      },
      cryptoHkdf: hkdf,
      hmacSha256: hmac,
      onLog: (nivel: string, msg: string) => {
        if (nivel === "error" || /mismatch|ignore|fail|relay/i.test(msg)) {
          log(`  motor[${nivel}] ${String(msg).slice(0, 220)}`);
        }
      },
    },
  });

  await engine.initialize();
  engine.initVoipStack(SELF_PN_DEV, SELF_PN, SELF_LID);
  await engine.waitForVoipStackReady();
  try { engine.updateNetworkMedium(2, 0); } catch {}
  pronto = true;
  log("✅ motor pronto — aceitando ofertas");
}

/** Eventos do motor: 156 traz a lista de relays (sem isso não sobe mídia), 16 o estado. */
function aoEventoDoMotor(tipo: number, dados?: string) {
  if (tipo === 156 && dados) {
    try { relay?.updateRelayList(JSON.parse(dados)); log("📡 lista de relays recebida — subindo transporte"); }
    catch (e: any) { log("relay list falhou: " + e?.message); }
    return;
  }
  if (tipo === 16 && dados) {
    try {
      const p = JSON.parse(dados);
      const info = p.call_info ?? p.callInfo ?? {};
      const estado = Number(info.call_state ?? info.callState ?? 0);
      if (atual) atual.estado = estado;
      log(`📞 estado da chamada = ${estado}`);
    } catch {}
    return;
  }
  log(`📞 evento do motor: ${tipo}`);
}

const corpo = (req: http.IncomingMessage) => new Promise<any>((res) => {
  let b = ""; req.on("data", c => b += c); req.on("end", () => { try { res(JSON.parse(b || "{}")); } catch { res({}); } });
});

http.createServer(async (req, res) => {
  const responder = (code: number, obj: any) => {
    res.writeHead(code, { "Content-Type": "application/json" });
    res.end(JSON.stringify(obj));
  };
  const url = new URL(req.url || "/", "http://x");

  if (url.pathname === "/health") {
    return responder(200, {
      pronto, uptime: (Date.now() - t0) / 1000,
      chamada: atual, framesRx, picoRms, fila: fila.length,
    });
  }

  // Long-poll: o gateway fica pendurado aqui e recebe as stanzas assim que o motor produz.
  if (url.pathname === "/out") {
    const espera = Math.min(Number(url.searchParams.get("ms") || 25000), 55000);
    if (!fila.length) {
      await new Promise<void>((r) => {
        const tm = setTimeout(() => { acordar = null; r(); }, espera);
        acordar = () => { clearTimeout(tm); r(); };
      });
    }
    const lote = fila.splice(0, fila.length);
    return responder(200, { respostas: lote });
  }

  if (req.method !== "POST") return responder(404, { erro: "rota desconhecida" });
  if (!pronto) return responder(503, { erro: "motor ainda subindo" });
  const b = await corpo(req);

  if (url.pathname === "/offer") {
    if (!b.payloadWasm || !b.callId) return responder(400, { erro: "payloadWasm e callId obrigatórios" });
    // Limpa a chamada anterior ANTES de aceitar a nova. Sem isso os canais de relay da
    // chamada passada continuam vivos e entregam pacotes velhos no motor — foi o que
    // produziu "bind record not found" e "Error parsing stun message integrity" com
    // transaction-ids que nunca pedimos, e nenhum RTP do peer entrava.
    if (atual) {
      log(`♻️ descartando a chamada anterior ${atual.callId} antes da nova`);
      try { engine.endCall(0, false); } catch {}
      try { relay?.closeAll(); } catch {}
      filaMic = []; sobraMic = new Float32Array(0);
      await new Promise(r => setTimeout(r, 150));
    }
    const peer = peerParaOMotor(b.peerJid);
    atual = { callId: b.callId, peerJid: peer, estado: 0, desde: Date.now() };
    framesRx = 0; picoRms = 0;
    log(`⬅️ oferta ${b.callId} do gateway (peer=${peer})`);
    engine.handleSignalingOffer({
      payload: b.payloadWasm,
      peerPlatform: Number(b.peerPlatform || 0),
      peerAppVersion: String(b.peerAppVersion || "0"),
      epochId: String(b.epochId || "0"),
      timestamp: String(b.timestamp || "0"),
      isOffline: !!b.isOffline,
      isOfferNotContact: false,
      peerJid: peer,
      tcToken: b.tcToken ? new Uint8Array(Buffer.from(b.tcToken, "base64")) : undefined,
    });
    return responder(200, { ok: true });
  }

  if (url.pathname === "/signal") {
    if (!b.payloadWasm) return responder(400, { erro: "payloadWasm obrigatório" });
    const peer = b.peerJid ? peerParaOMotor(b.peerJid) : (atual?.peerJid || "");
    const tc = b.tcToken ? new Uint8Array(Buffer.from(b.tcToken, "base64")) : undefined;
    if (b.tipo === "ack") {
      engine.handleSignalingAck({
        payload: b.payloadWasm, ackError: String(b.ackError || "0"),
        msgType: String(b.msgType || ""), peerJid: peer, extraData: tc,
      });
    } else if (b.tipo === "receipt") {
      engine.handleSignalingReceipt({ payload: b.payloadWasm, peerJid: peer, tcToken: tc });
    } else {
      engine.handleSignalingMessage({
        payload: b.payloadWasm,
        peerPlatform: String(b.peerPlatform || "0"),
        peerAppVersion: String(b.peerAppVersion || "0"),
        epochId: String(b.epochId || "0"), timestamp: String(b.timestamp || "0"),
        isOffline: !!b.isOffline, peerJid: peer, tcToken: tc,
      });
    }
    return responder(200, { ok: true });
  }

  if (url.pathname === "/accept") {
    const id = String(b.callId || atual?.callId || "");
    try {
      const inst = (engine as any)?.publicInstance;
      const ret = inst?.acceptCall?.(id, false);
      log(`✅ acceptCall(${id}) → ${ret}`);
      return responder(200, { ok: true, ret });
    } catch (e: any) { return responder(500, { erro: e?.message }); }
  }

  if (url.pathname === "/hangup") {
    try { engine.endCall(0, !!b.enviarTerminate); } catch {}
    try { relay?.closeAll(); } catch {}
    filaMic = []; sobraMic = new Float32Array(0); gateway = null;
    log(`🔚 chamada encerrada (frames recebidos=${framesRx}, pico rms=${picoRms.toFixed(5)})`);
    atual = null;
    return responder(200, { ok: true, framesRx, picoRms });
  }

  responder(404, { erro: "rota desconhecida" });
}).listen(PORTA, "127.0.0.1", () => log(`sidecar ouvindo em 127.0.0.1:${PORTA}`));

subirMotor().catch(e => { log("❌ motor falhou: " + (e?.stack || e?.message)); process.exit(1); });

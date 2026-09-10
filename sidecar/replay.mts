/**
 * ETAPA 3 — injeta uma oferta REAL capturada no motor whatsapp.wasm e observa o que ele
 * devolve. Offline: não conecta em nada, não envia nada. Só quer saber se o motor ENTENDE
 * a oferta e produz as stanzas de resposta certas.
 */
import fs from "node:fs";
import path from "node:path";
import { WasmEngine } from "/root/nextflow/backend/wa-call-service/lib/sheitear/src/wasm-engine.mjs";

const BASE = "/root/nextflow/backend/wa-call-service/lib/sheitear";
const DIR = "/root/wa-wasm-bridge/stanzas";

const t0 = Date.now();
const log = (m: string) => console.log(`[${((Date.now() - t0) / 1000).toFixed(1)}s] ${m}`);

const arq = fs.readdirSync(DIR).filter(f => f.endsWith("-wasm.json")).sort().pop()!;
const oferta = JSON.parse(fs.readFileSync(path.join(DIR, arq), "utf8"));

// O nosso número (quem RECEBE) — o motor precisa se identificar.
const SELF_PN = "554896552034@s.whatsapp.net";
const SELF_PN_DEV = "554896552034:88@s.whatsapp.net";
const SELF_LID = "39621610192990:88@lid";   // NOSSO lid (whatsmeow_device.lid) — antes eu usava o do chamador

let respostas = 0;

async function main() {
  log(`oferta: ${oferta.callId} de ${oferta.callCreatorAlt} (${oferta.peerPlatform}) | chave: ${String(oferta.callKeyHex).slice(0,16)}...`);

  const engine = new WasmEngine({
    resourcesPath: BASE,
    enableLogs: true,
    callbacks: {
      // ⭐ É POR AQUI que o motor devolve o que deveria ser enviado ao WhatsApp.
      onSignalingXmpp: (peerJid: string, callId: string, xmlPayload: Uint8Array) => {
        respostas++;
        const txt = Buffer.from(xmlPayload).toString("utf8").replace(/[^\x20-\x7e]/g, ".");
        log(`📤 RESPOSTA #${respostas} → peer=${peerJid} call=${callId} ${xmlPayload.length}b`);
        log(`     ${txt.slice(0, 160)}`);
      },
      onCallEvent: (eventType: number, eventData?: string) => {
        log(`📞 evento do motor: tipo=${eventType} ${eventData ? String(eventData).slice(0, 120) : ""}`);
      },
      onVoipReady: () => log("motor: VoIP pronto"),
      // O motor só fala se a gente registrar este callback (enableLogs sozinho não basta).
      onLog: (nivel: string, msg: string) => {
        if (/error|warn/i.test(nivel) || /offer|call|voip|peer|sign/i.test(msg)) {
          log(`  motor[${nivel}] ${String(msg).slice(0, 220)}`);
        }
      },
    },
  });

  await engine.initialize();
  engine.initVoipStack(SELF_PN_DEV, SELF_PN, SELF_LID);
  await engine.waitForVoipStackReady();
  log("✅ pilha pronta — injetando a oferta real...");

  engine.handleSignalingOffer({
    payload: oferta.payloadWasm,
    peerPlatform: 0,
    peerAppVersion: String(oferta.peerAppVersion || "0"),
    epochId: "0",
    timestamp: "0",
    isOffline: false,
    isOfferNotContact: false,
    // ⭐ O motor compara o peerJid com o CREATOR de dentro da stanza e ignora a oferta se
    // diferirem ("mismatched peer id and creator id"). A stanza traz o número do LID com
    // domínio @s.whatsapp.net e device :0 — é essa a forma exata que ele espera.
    peerJid: String(oferta.peerJid).split("@")[0].split(":")[0] + ":0@s.whatsapp.net",
  });
  log("oferta injetada — aguardando 12s pra ver o que o motor produz...");

  await new Promise(r => setTimeout(r, 12000));
  log(respostas > 0
    ? `✅ o motor ENTENDEU a oferta e produziu ${respostas} stanza(s) de resposta`
    : "⚠️ nenhuma resposta — o motor não reagiu à oferta");
  process.exit(0);
}

main().catch(e => { log("❌ " + (e?.stack || e?.message || e)); process.exit(1); });

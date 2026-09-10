/**
 * ETAPA 1 — prova de vida do motor real do WhatsApp fora do navegador.
 *
 * Só carrega o whatsapp.wasm, inicializa a pilha de VoIP e reporta. Não conecta em nada,
 * não toca no gateway, não usa Baileys. Se isto passar, a ponte é viável.
 */
import { WasmEngine } from "/root/nextflow/backend/wa-call-service/lib/sheitear/src/wasm-engine.mjs";

const BASE = "/root/nextflow/backend/wa-call-service/lib/sheitear";

// JIDs de mentira: nesta etapa só queremos ver a pilha subir.
const SELF_PN = "554896552034:88@s.whatsapp.net";
const SELF_LID = "198453946806345:88@lid";

const t0 = Date.now();
const log = (m: string) => console.log(`[${((Date.now() - t0) / 1000).toFixed(1)}s] ${m}`);

async function main() {
  log("criando engine...");
  const engine = new WasmEngine({ resourcesPath: BASE, enableLogs: false });

  log("initialize() — compila o wasm e roda o loader...");
  await engine.initialize();
  log("✅ WASM compilado e loader executado");

  log("initVoipStack()...");
  engine.initVoipStack(SELF_PN, SELF_PN.split(":")[0] + "@s.whatsapp.net", SELF_LID);

  log("waitForVoipStackReady()...");
  await engine.waitForVoipStackReady();
  log("✅ PILHA DE VOIP PRONTA — o motor real do WhatsApp subiu fora do navegador");

  try { (engine as any).updateNetworkMedium?.(2, 0); log("updateNetworkMedium ok"); } catch (e: any) { log("updateNetworkMedium falhou: " + e?.message); }
  process.exit(0);
}

main().catch((e) => { log("❌ FALHOU: " + (e?.stack || e?.message || e)); process.exit(1); });

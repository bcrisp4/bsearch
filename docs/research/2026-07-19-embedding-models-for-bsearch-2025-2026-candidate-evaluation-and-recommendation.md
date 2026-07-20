# Embedding Models for `bsearch`: 2025–2026 Candidate Evaluation and Recommendation

## TL;DR
- **Default pick: `google/embeddinggemma-300m`.** It is the strongest small, resident, battery-friendly on-device option for bsearch: 308M params (~150–330MB as a typical GGUF quant), 768 native dims with MRL truncation to 512/256/128, a 2,048-token context, and — critically — it is a **first-class embedding model on LM Studio's `/v1/embeddings` GGUF endpoint** (Google and the LM Studio team shipped explicit support). It posts an MTEB English-v2 mean of 69.67 with a retrieval subscore of ~60.9.
- **Co-defaults for the bake-off: `Qwen/Qwen3-Embedding-0.6B`** (Apache 2.0; 0.6B; 1024 dims selectable 32–1024 via MRL; 32K context; **MTEB-R English-v2 retrieval 61.82 — the highest retrieval quality of any sub-1B model reviewed**) and, as a pure-encoder control, `Snowflake/snowflake-arctic-embed-l-v2.0` (Apache 2.0; 568M; 1024 dims; 8K context; best-documented MRL + binary-compression story, 55.6 nDCG@10 on BEIR).
- **Quality-if-you-can-afford-it: `Qwen3-Embedding-4B/8B`** (MTEB-eng-v2 retrieval 68.46 / 69.44) — but these are 1B+ and must be flagged as heavier on battery/residency; keep them out of the always-resident interactive path. **One serving fact overrides everything else on the Mac: LM Studio's MLX engine does NOT serve embeddings — only its GGUF/llama.cpp backend does — so standardize bsearch on GGUF regardless of the M5's usual MLX affinity.**

## Key Findings

1. **Serving reality on the Mac is the binding constraint.** LM Studio's MLX runtime (built on mlx-lm / mlx-vlm) does not implement embedding pooling; only the GGUF/llama.cpp backend serves `/v1/embeddings`. The bug **"Qwen3 Models not Recognized as Embedding Types for MLX Format" (LM Studio bug tracker issue #808, opened Jul 29 2025 by user aperepel) remains open**, with the verbatim report: *"Qwen3 models not recognized as embedding models in MLX format (but same models are ok in GGUF)."* MLX-format embedding builds exist on Hugging Face (`mlx-community/*` for all three top candidates), but LM Studio cannot serve them as embeddings; hitting `/v1/embeddings` with an MLX embedding model returns *"Model is not embedding."* **Conclusion: standardize on GGUF quantizations for bsearch.** All top candidates ship GGUF. (If you later want MLX-accelerated embeddings, you must run a separate server such as `mlx-embeddings`/`mlx-serve-embeddings` — not LM Studio.)

2. **EmbeddingGemma is purpose-built for exactly bsearch's use case** — on-device, small, resident, battery-constrained. Google and LM Studio shipped first-class `/v1/embeddings` GGUF support (`lmstudio-community/embeddinggemma-300m-qat-GGUF`), and Ollama supports it (requires Ollama v0.11.10+; default pull ~622MB). It is described in Google's launch materials as the top-performing multilingual text-only embedding model under 500M parameters at release.

3. **Qwen3-Embedding-0.6B has the best small-model retrieval quality** but carries two operational wrinkles: as a decoder model llama.cpp sometimes mislabels it as an LLM (fix with LM Studio's "Override Domain Type"; tracked as issue #696), and there is a SEP-token warning in llama.cpp that degrades embeddings unless `add_eos_token` is set true in the GGUF. Use a GGUF known to set the flag (e.g. official `Qwen/Qwen3-Embedding-0.6B-GGUF` or a corrected community build).

4. **Dimensionality is a tunable cost, not a hard cap.** At 1024-dim float32, ~1M chunks ≈ 4.0GB; 768-dim ≈ 3.0GB; 512 ≈ 2.0GB; 256 ≈ 1.0GB (scales linearly). MRL lets a higher-dim model be tuned down cheaply: Snowflake's Arctic-L card states *"Leverage vector truncation via MRL to decrease vector size by 4x with less than 3% degredation in quality… retrieval in 128 bytes per doc"* when combined with int4; EmbeddingGemma's docs report truncating 768→256 dims costs only −2.4% overall score. Layering bsearch's planned binary quantization + full-precision rescore on top yields a further ~32x storage reduction while retaining ~95% recall.

5. **English retrieval ranking among the small (≤600M) candidates** (MTEB v2 retrieval subscore / BEIR nDCG@10, not overall averages): **Qwen3-0.6B (61.82) > EmbeddingGemma 768d (~60.9) > Granite-english-r2 149M (56.4 MTEB-v2 retrieval) ≈ Arctic-L v2.0 (55.6 BEIR / 0.556 MTEB-R).** The larger Qwen3-4B/8B reach 68.46 / 69.44 but break the resident/battery budget.

## Details

### Candidate comparison

**1. EmbeddingGemma-300m — Google, September 2025 — *default pick***
- **Params:** 308M. **Native dims:** 768. **MRL:** 512/256/128 (per Google docs, truncating 768→256 costs −2.4% overall; retrieval subscore 60.9 at 768d → 59.3 at 256d in the paper).
- **Max input:** 2,048 tokens (comfortably above bsearch's ~1024-token chunks).
- **Symmetry:** Asymmetric with task prompts. Query = `task: search result | query: {q}`; document = `title: none | text: {doc}` (or the actual title if available). These are the exact prefixes bsearch should store.
- **Availability:** First-class GGUF in LM Studio (`lmstudio-community/embeddinggemma-300m-qat-GGUF`, quantized by the LM Studio team); GGUF quants range ~212MB (Q2_K) to 612MB (F16), with Q4/Q8 in the 236–329MB range. MLX builds exist (`mlx-community/embeddinggemma-300m-{4,5,8bit,bf16}`) but **cannot be served as embeddings in LM Studio.** Ollama v0.11.10+ (~622MB). **Caveat: activations do not support float16** — use bf16/f32/int quant.
- **Retrieval:** MTEB English v2 mean **69.67**, retrieval subscore **~60.9** (768d); Multilingual v2 **61.15**; Code v1 **68.76**.
- **Quantization/MRL:** ships a native QAT (quantization-aware trained) release; MRL tradeoffs published.
- **License:** Gemma Terms of Use — permissive (commercial use and redistribution allowed) but **not OSI-approved** and carries a use policy.
- **Footprint:** ~150–330MB resident at typical quant. Best-in-class battery fit.

**2. Qwen3-Embedding-0.6B — Alibaba, June 2025 — *quality challenger***
- **Params:** ~0.6B. **Native dims:** 1024 (user-selectable **32–1024** via MRL). **Max input:** 32,768 tokens.
- **Symmetry:** Asymmetric / instruction-aware. Query = `Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery: {q}`; document = raw text. Instructions typically add 1–5% on downstream tasks (write them in English).
- **Availability:** Official GGUF (`Qwen/Qwen3-Embedding-0.6B-GGUF`); serves on LM Studio `/v1/embeddings` (0.6B works; may need Override Domain Type per issue #696; larger 4B/8B GGUFs have shown "Failed to decode batch!" errors per issue #1647). SEP-token warning in llama.cpp — confirm `add_eos_token`. MLX exists but not servable as embeddings in LM Studio (#808). In Ollama library.
- **Retrieval:** MTEB(eng, v2) mean **70.70**, retrieval subscore **61.82**; CMTEB-R 71.02; MMTEB-R **64.64**; multilingual leaderboard mean 64.33. Best sub-1B retrieval in this set. (The 8B sibling ranked #1 on the MTEB multilingual leaderboard as of June 5, 2025 at 70.58.)
- **License:** **Apache 2.0.**
- **Footprint:** ~0.6–1.2GB quantized resident. Fine on the M5, but a few×heavier than Gemma.

**3. Snowflake arctic-embed-l-v2.0 — December 2024 — *pure-encoder control / best compression story***
- **Params:** 568M (303M non-embedding). **Native dims:** 1024. **MRL:** to 256 (card: 4x smaller, <3% loss). **Max input:** 8,192 (RoPE). Built on `bge-m3-retromae` (XLM-RoBERTa family).
- **Symmetry:** Asymmetric. v2 uses E5-style `query: ` prefix on queries; documents raw; **CLS pooling**.
- **Availability:** GGUF community builds; BERT/XLM-R architecture → cleaner llama.cpp embedding classification than decoder models; Ollama `snowflake-arctic-embed2` (~568M, 1024d, 8K ctx, Apache 2.0). MLX 8bit exists but not servable in LM Studio.
- **Retrieval:** MTEB-R (English) nDCG@10 **0.556**; BEIR **55.6**, MIRACL 55.8, CLEF 54.3; 58.56 mean on the English retrieval leaderboard cited in third-party evaluations.
- **Quantization/MRL:** explicitly quantization-aware; 128 bytes/vector with MRL + int4.
- **License:** **Apache 2.0.** **Footprint:** ~0.3–0.6GB quantized.

**4. arctic-embed-m-v2.0** — 305M, 768 dims, 8K context, MTEB-R 0.554, Apache 2.0, GTE-multilingual-base backbone. A lighter sibling of #3 with nearly identical English retrieval and smaller footprint.

**5. Nomic Embed Text v1.5 — Feb 2024** — 137M, 768 dims (MRL 64–768; per the card, 768→256 loses only 0.24–1.24 MTEB points), 8192 native context (**llama.cpp defaults to 2048**; needs YaRN/rope-scaling for the full window), Apache 2.0, asymmetric `search_query:` / `search_document:`. MTEB ~62.28. **This is LM Studio's default RAG embedder**, so it is the most battle-tested GGUF embedding path. Very battery-friendly, but English retrieval trails Gemma/Qwen3.

**6. Nomic Embed Text v2 MoE — Feb 2025** — 475M total / 305M active (MoE, 8 experts top-2), 768 dims (MRL to 256), Apache 2.0, `search_query:` / `search_document:`. **Max context only 512 tokens** — a real constraint against bsearch's up-to-1024-token chunks. BEIR 52.86; multilingual focus. Not recommended as default due to the context ceiling.

**7. IBM Granite Embedding English R2 — Aug 2025** — 149M, 768 dims, **8192 context**, ModernBERT, Apache 2.0, **symmetric (no prefix needed)**. MTEB-v2 retrieval **56.4**; the 47M small-english-r2 (384d) scores 53.9. Trained only on permissively-licensed data. Symmetric operation = the simplest to wire into bsearch (no query/passage templates), and ONNX builds exist. A strong "boring/reliable" control candidate.

**8. mxbai-embed-large-v1 — 2024** — 335M, 1024 dims (MRL to 512/256), **512-token context (limiting)**, Apache 2.0, asymmetric query prefix "Represent this sentence for searching relevant passages:". MTEB 64.68 (2024 SOTA for BERT-large size). GGUF + LM Studio supported. Aging and short-context; skip for bsearch.

**9. BGE-M3 — Jan 2024** — 568M, 1024 dims (**no MRL**), 8192 context, **MIT**, symmetric, uniquely emits dense + sparse + ColBERT vectors from one model. Strong multilingual; Ollama + GGUF. No single comparable MTEB retrieval number (it mixes modes). Its MIT license is the most permissive of the set, and its built-in sparse output could complement bsearch's BM25/FTS5 side — worth a look if you want one model feeding both halves of hybrid search.

**10. Qwen3-Embedding-4B / 8B — *quality ceiling, non-resident only*** — MTEB(eng,v2) retrieval **68.46 (4B) / 69.44 (8B)**; Apache 2.0; 1024–4096 dims with MRL; 32K context. But 4B ≈ 2.5–5GB and 8B ≈ 5GB+ quantized — too heavy to keep resident on battery for sub-500ms interactive queries. Use only for background batch embedding if small-model quality proves insufficient.

### Dimensionality & storage math for bsearch (~1M chunks)
- 1024-dim fp32 ≈ **4.0GB**; 768-dim ≈ **3.0GB**; 512 ≈ **2.0GB**; 256 ≈ **1.0GB** (linear in dimension).
- MRL brings a high-dim model down cheaply: Qwen3-0.6B / Arctic-L (1024→256) or Gemma (768→256) shrink ~3–4x for <3% (Arctic) to −2.4% (Gemma) quality loss.
- Binary quantization + fp rescore (bsearch's plan) yields a further ~32x reduction while retaining ~95% recall. All four top candidates are documented to quantize well; Arctic explicitly targets **128 bytes/vector** (MRL-256 + int4). Net: even at 1M chunks the vector index can sit comfortably in the low hundreds of MB.

## Recommendations

**Stage 1 — Bake-off defaults.** Run these head-to-head on your own Obsidian/PDF/office corpus with the hybrid RRF pipeline, measuring recall@10 / nDCG@10 on a hand-labeled query set, plus p95 warm latency and index size at 1M-chunk scale:

1. **EmbeddingGemma-300m (GGUF) — the default.** Smallest resident footprint, best battery profile, first-class LM Studio `/v1/embeddings` support, solid English retrieval (60.9), MRL for cost tuning, 2K context covers your chunks. Start at 768-dim; test 256-dim MRL for the 1M-chunk index. Store its `task:`/`title:` prefixes. Avoid float16.
2. **Qwen3-Embedding-0.6B (GGUF) — the quality challenger.** Highest small-model retrieval (61.82) and huge 32K context headroom. Accept the instruction-prefix + Override-Domain-Type wrinkle and verify `add_eos_token`/SEP handling in the GGUF. Test at 1024 dims and MRL-256.
3. **arctic-embed-l-v2.0 (GGUF)** *or* **Granite-english-r2 (149M, GGUF/ONNX)** as the pure-encoder control — Arctic for the best compression story (128 bytes/vector, 8K context), Granite for the simplest operation (symmetric, no prefixes, MIT-clean permissive lineage, 8K context).

**Stage 2 — Escalate only if small-model quality is insufficient after MRL/quant tuning:** trial **Qwen3-Embedding-4B (GGUF, Q4)** as a **background-batch-only** embedder (explicitly flagged battery-heavy, not resident), while keeping a 300–600M model for interactive query embedding. Because these are asymmetric with prefixes, query- and index-time models must match, so a 4B-index / small-query split is not viable — commit the whole index to one model.

**Decision thresholds that change the recommendation:**
- If p95 warm latency exceeds **500ms** with a 0.6B model, drop to EmbeddingGemma-300m or Granite-149M.
- If storage at 1M chunks exceeds budget, apply **MRL-256 → binary quantization + rescore** *before* switching models (cheaper than changing embedders).
- If recall falls below **~0.80** on your eval set with every small model, escalate to Qwen3-4B for batch embedding only.
- If you want a single model to also feed the lexical side of hybrid search, evaluate **BGE-M3** (dense + sparse from one pass, MIT).

**Strongest small on-device option:** EmbeddingGemma-300m. **Strongest quality-if-you-can-afford-it:** Qwen3-Embedding-8B (or 4B as a lighter compromise) — via GGUF, batch-only, non-resident.

## Caveats
- **MLX cannot serve embeddings in LM Studio** (bug #808, open since Jul 29 2025); the fix would require LM Studio to integrate a package like `mlx-embeddings`, which it has not. This overrides the usual "prefer MLX on Apple Silicon" advice — use GGUF for bsearch.
- Many MTEB numbers here come from vendor model cards, the Qwen3 paper (arXiv:2506.05176), the EmbeddingGemma paper/blog, and third-party aggregators (e.g. morphllm, ailog, awesomeagents). **MTEB v1 vs v2, and English vs multilingual (MMTEB) boards, are not directly comparable** — treat all published scores as a shortlist signal and re-benchmark on your own corpus.
- Some third-party "2026 leaderboard" pages cite very new models (Jina v5-text, Microsoft Harrier-OSS, Tencent KaLM-Gemma3-12B, Gemini Embedding 2, Voyage 4). These are either large/heavy, API-only, or lack verified small-footprint GGUF embedding-serving paths as of this writing, so they are out of scope for a resident, battery-constrained, GGUF-served local tool — but worth periodically re-checking on the live MTEB leaderboard.
- **EmbeddingGemma's Gemma license is permissive but not OSI Apache/MIT.** If strict OSI licensing matters for the MIT-licensed bsearch project, prefer Qwen3-0.6B (Apache 2.0), Arctic-L (Apache 2.0), Granite-R2 (Apache 2.0), or BGE-M3 (MIT).
- **Context ceilings:** Nomic v2 MoE and mxbai-embed-large cap at 512 tokens — tight for bsearch's up-to-1024-token chunks. Nomic v1.5's full 8192 context needs explicit rope-scaling in llama.cpp (defaults to 2048).
- **Qwen3 decoder-based embedders need correct EOS/SEP handling in the GGUF** or retrieval quality silently suffers — validate on a known query/passage pair before indexing 1M chunks.
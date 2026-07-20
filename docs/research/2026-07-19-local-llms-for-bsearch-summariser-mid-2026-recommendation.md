# Local LLMs for the bsearch Summariser: A Mid-2026 Recommendation

## TL;DR
- **For faithful pyramid summaries on a battery-constrained M5 Max, the strongest fits are IBM Granite 4.1 8B, Qwen3.5/3.6-35B-A3B (run in non-thinking mode), and Google Gemma 3 12B-IT** — all open-weight, permissively licensed, available as both GGUF *and* Apple MLX builds for LM Studio/Ollama, and among the best small models on summarization faithfulness.
- **Prefer non-reasoning / thinking-disabled models.** The single most important 2026 finding for this use case is the "reasoning tax": on Vectara's HHEM summarization-faithfulness leaderboard, reasoning/thinking modes consistently *increase* hallucination roughly 2–3× (e.g., DeepSeek V3.2→R1 rose 5.3%→11.3%, and Grok 3→Grok 4 Fast rose 5.8%→20.2%). Granite 4.1 (no chain-of-thought), Gemma 3, and thinking-disabled Qwen are therefore safer for index-time summaries than reasoning-default models.
- **MoE with a small active-parameter count (A3B/A4B) gives the best tokens-per-watt** for background indexing. Qwen3.5/3.6-35B-A3B (~3B active) and Gemma 4 26B-A4B (~3.8B active) deliver 35B-class quality at roughly 8B-class compute cost — the right tradeoff for on-battery runs. Avoid dense 70B+ models even though 128 GB could hold them.

## Key Findings

1. **The summariser should default to non-thinking mode regardless of model.** Vectara's HHEM leaderboard (last updated May 11, 2026, using HHEM-2.3 on a dataset expanded from ~1,000 to 7,700+ articles spanning law, medicine, finance, technology, and education, with documents up to ~32K tokens) shows frontier reasoning models exceeding 10% hallucination on grounded summarization, while small non-reasoning models sit at 4–6%. CodingFleet's May 29, 2026 analysis of the HHEM data found thinking/reasoning modes "amplify hallucination 2–3× across all model families." A reasoning model "thinks up" inferences and connections not present in the source — precisely what a faithful summary must avoid.

2. **Best measured faithfulness among small open-weight models (Vectara HHEM, May 11 2026 snapshot):**
   - **Phi-4 (14B):** 3.7% hallucination / 96.3% factual consistency — *but only an 80.7% answer rate* (it refuses/declines on ~19% of documents), which flatters the score and is a liability for a pipeline that must summarize every document.
   - **Gemma-3-12B-IT:** 4.4% / 95.6%, 97.4% answer rate — the best "clean" small performer (it significantly outperforms the 4B at 6.4% and the 27B at 7.4%).
   - **Qwen3-8B:** 4.8% / 95.2%, 99.9% answer rate.
   - **Mistral-Small-2501:** 5.1% / 94.9%.
   - **Granite-4.0-H-Small** and **Gemma-4-26B-A4B:** both 5.2% / 94.8%.
   - **Qwen3-4B:** 5.7%; **Qwen3-14B:** 5.4%; **Qwen3-32B:** 5.9%.
   - **Notable regression:** the newer Qwen3.5 family hallucinates *more* (Qwen3.5-35B-A3B: 10.5%, Qwen3.5-27B: 12.1%) and the newest Ministral-3 series is worse still (Ministral-3-8B-2512: 21.7%; Ministral-3-3B-2512: 24.2%) — the reasoning-default/thinking tax again.
   - **Lowest on the entire board:** Ant Group's finix_s1_32b at 1.8% hallucination / 98.2% factual consistency / 99.5% answer rate — but it is 32B and outside the small tier.

3. **Meta Llama is effectively out.** There is no 2026 open Llama release; Scout (17B active / 109B total) and Maverick (17B active / 400B total) shipped April 5, 2025 under the Llama 4 Community License with its 700-million-monthly-active-user threshold that requires a separate license from Meta, and in April 2026 Meta pivoted to the *closed-weight* Muse Spark (which replaced Llama 4 as the engine behind Meta AI). Both Llama 4 models are also too large for battery-friendly background work.

4. **Cloud-only frontier models are correctly excluded** (hosted GPT/Claude/Gemini). OpenAI's gpt-oss-20b/120b are the only OpenAI open weights and are viable locally, but the 20b is a reasoning-tuned MoE, so it inherits the reasoning-tax risk on leaf summaries.

## Details (candidate-by-candidate)

### IBM Granite 4.1 (3B / 8B / 30B dense) — released April 29, 2026 ★ top pick
- **Architecture:** Dense decoder-only, non-reasoning (no chain-of-thought toggles) — deliberately built for "predictable latency, stable token usage." Per IBM Research, the 8B instruct model "consistently matches or outperforms" the prior Granite 4.0 32B MoE (32B total / 9B active) — a 4× parameter reduction with no performance loss. Trained on ~15 trillion tokens.
- **Context:** 128K production context, extended to **512K** on the 8B/30B (3B caps at 128K) — the largest in this shortlist, ideal for reducing how often map-reduce is needed.
- **License:** Apache 2.0, cryptographically signed, ISO 42001 certified — clean for an MIT project.
- **Builds:** Official/community GGUF (IBM, Unsloth) and MLX; listed in the LM Studio catalog and Ollama, with IBM publishing explicit LM Studio run docs. Native support in llama.cpp, vLLM, MLX.
- **Quality signals:** Explicitly tuned for instruction-following, tool-calling, RAG, and summarization (the model card names summarization as a prime use case). Granite-4.0-H-Small scored 5.2% hallucination on Vectara; IBM markets 4.1 as especially reliable at structured output (e.g., messy document → clean JSON), useful for emitting and validating the word-count tiers.
- **Speed on Apple Silicon:** A dense 3B/8B model is very light for background indexing; dense 8B at Q4 typically runs ~40–60 tok/s on M4-class chips, and the M5 Max should exceed this. The Mamba-2/transformer hybrid design in the Granite 4 line reduces memory ~70–80% vs. transformer-only, aiding co-residency with other apps.

### Qwen3.5 small + 35B-A3B / Qwen3.6-35B-A3B — Feb–April 2026 ★ throughput pick
- **Architecture & timeline:** Qwen3.5 released Feb 16 2026 (flagship 397B-A17B MoE), with 122B-A10B / 35B-A3B / 27B on Feb 24 2026 and small dense models (0.8B, 2B, 4B, 9B) on March 2 2026. Qwen3.6-35B-A3B (MoE, 35B total / ~3B active) landed April 16 2026; a dense Qwen3.6-27B on April 22 2026. **Thinking mode is on by default and must be disabled for faithful summaries.**
- **Context:** 262,144 tokens native (extensible to ~1M with YaRN) — best-in-class here for long documents.
- **License:** Apache 2.0.
- **Builds:** GGUF (Unsloth, LM Studio community) and MLX (mlx-community) widely available; LM Studio's Discover tab lists them with an MLX filter. Caveat: some Qwen3.5 GGUFs did not initially work in Ollama due to separate vision mmproj files — use llama.cpp/LM Studio, or verify current Ollama support.
- **Quality signals:** Strong instruction-following and very broad multilingual coverage (201 languages). But faithfulness regressed vs. Qwen3 (Qwen3.5-35B-A3B 10.5% vs. Qwen3-8B 4.8%), largely due to thinking-by-default — **disable thinking** for summarization, or consider staying on Qwen3-8B/14B for leaf summaries.
- **Speed on Apple Silicon:** 35B-A3B is the standout for battery: only ~3B active params. On M4 Max, an independent engine benchmark reports MLX reaching ~130 tok/s native (100+ via HTTP); other guides report ~55–70 tok/s depending on quant/context, peaking ~19.5 GB unified memory at MLX 4-bit. The A3B design is the best throughput-per-watt option in this shortlist.

### Google Gemma 3 (1B/4B/12B/27B) and Gemma 4 (E2B/E4B/12B/26B-A4B/31B) ★ faithfulness pick
- **Gemma 3** (March 2025): dense, 128K context (32K for 1B), multimodal on 4B+, strong instruction-following and summarization. **Gemma-3-12B-IT is the best "clean" small faithfulness result (4.4% hallucination, 97.4% answer rate).**
- **Gemma 4** (April 2 2026): E2B/E4B small (2.3B/4.5B effective), 26B-A4B MoE (~3.8B active), 31B dense (256K context), plus a 12B unified encoder-free model (June 3 2026). Configurable thinking modes; multi-token-prediction draft models for speculative decoding. Gemma-4-26B-A4B scored 5.2% hallucination; Gemma-4-31B 7.4%.
- **License:** Gemma 3 shipped under the custom "Gemma Terms of Use"; 2026 coverage widely reports Gemma 4 as Apache 2.0. *Sources conflict — verify against the official Gemma 4 model card before shipping.* Both permit personal/self-hosted use.
- **Builds:** Extensive GGUF and MLX (e.g., unsloth/gemma-4-31B-it-GGUF, mlx-community/gemma-4-31B-it-MLX); available via LM Studio, Ollama, Kaggle, Hugging Face, with official QAT INT4 checkpoints for lower memory.
- **Speed on Apple Silicon:** One July 2026 index (LLMCheck) reports Gemma 4 E2B at ~158 tok/s on M5 Max via MLX — the fastest entry measured — and the 26B-A4B MoE is fast for its quality due to 3.8B active params.

### Microsoft Phi-4 family — MIT-licensed, faithful but selective
- **Phi-4 (14B dense):** best small-model faithfulness on Vectara (3.7%) but with only an 80.7% answer rate — its tendency to refuse hurts a pipeline that must summarize *every* document. **Phi-4-reasoning-vision-15B** (March 4 2026) is multimodal but has only a 16,384-token context (too small for comfortable map-reduce) and is reasoning-tuned. **Phi-5 is not officially released** — only pre-release/speculative blog coverage exists; treat as unconfirmed.
- **License:** MIT (most permissive), a genuine plus for an MIT project.
- **Context:** Phi-4 ~16K–64K depending on variant — modest.

### Mistral Small 4 / Ministral 3
- **Mistral Small 4** (March 16 2026): 119B-total MoE, ~6B active, 256K context, Apache 2.0, unifies reasoning/vision/coding. Quality is high, but 119B total weights (~242 GB BF16) are heavy to load and keep resident — better suited to a dedicated inference box than laptop background runs.
- **Ministral 3** (3B/8B/14B, Dec 2 2025): Apache 2.0, edge-oriented, 128K–256K context — but the 2512 series regressed badly on summarization faithfulness (8B: 21.7%). The older mistral-small-2501 (5.1%) is actually the safer Mistral choice for faithful summaries.

### gpt-oss-20b (OpenAI, Apache 2.0)
- MoE 21B total / 3.6B active, 128K context, GGUF + MLX, runs in ~16 GB, fast (~150–170 tok/s on M4 Pro-class hardware). But it is reasoning-tuned (o3-mini class), so leaf-summary faithfulness carries the reasoning-tax risk. Best reserved for the "reduce/reasoning" step of map-reduce rather than faithful leaf summaries.

## Recommendations

**Stage 1 — Start here (best overall fit).** Deploy **IBM Granite 4.1 8B** as the default bsearch summariser. It is non-reasoning (predictable, low-hallucination by design), explicitly tuned for instruction-following and summarization, Apache 2.0, has the largest context in the shortlist (512K), ships as GGUF and MLX for LM Studio/Ollama, and is light enough (dense 8B) for on-battery background indexing. Use its strong structured-output ability to emit the 4/16/64-word tiers, then validate/trim to exact counts in your pipeline. For the map-reduce *reduce* step over intermediate summaries, the same model works well; if you want extra reduction quality, a larger Granite 4.1 30B can be JIT-loaded only for the reduce pass.

**Stage 2 — If you need fewer map-reduce passes or more throughput-per-watt.** Add **Qwen3.5-35B-A3B (or Qwen3.6-35B-A3B) with thinking mode DISABLED** (`enable_thinking:false` / suppress `<think>`). The ~3B-active MoE is the best tokens-per-watt option here, and its 262K context minimizes chunking on long PDFs/office docs. Turning off thinking is mandatory — otherwise faithfulness degrades to ~10%.

**Stage 3 — If faithfulness is the top priority and battery allows a dense mid-size model.** Use **Gemma 3 12B-IT** (best clean small faithfulness, 4.4%) or **Gemma 4 26B-A4B** in non-thinking mode (5.2%, MoE-fast). Gemma's instruction-following is strong for hitting tight word counts.

**Tiny/edge fallback:** **Qwen3-8B** (non-thinking) gives excellent faithfulness (4.8%) at minimal footprint for the shortest tiers or lowest-power runs; **Granite 4.1 3B** for the most aggressive battery savings on short documents.

**Benchmarks/thresholds that would change these picks:**
- Build a small internal HHEM-style eval on *your* real mix (PDFs, office docs, markdown notes, emails). If a candidate exceeds ~6–7% hallucination on it, drop it or force non-thinking mode.
- If cold-start/load time dominates JIT-load runs, prefer smaller dense models (Granite 3B/8B, Gemma 3 4B/12B) over MoE — an MoE must load *all* experts into memory even though only a few activate per token.
- If a genuine Qwen3.6 dense small variant, a Gemma 4 QAT small model, or a new non-reasoning entrant ships with published low HHEM scores, re-benchmark — this field moves monthly.
- Re-verify the Gemma 4 license (Apache 2.0 vs. Gemma Terms of Use is unresolved across sources) before finalizing.

## Caveats
- **Faithfulness numbers are from Vectara HHEM's summarization benchmark**, which measures grounding (does the summary invent facts), not summary *quality* or word-count adherence. Validate length/word-count compliance separately in your pipeline.
- **The "reasoning tax" is the dominant design lever.** Many 2026 flagships default to thinking mode; for index-time faithful summaries you almost always want it off.
- **M5 Max throughput data is sparse** (the chip is very new). Most cited numbers are M3/M4 Max MLX/llama.cpp proxies plus one early M5 Max index (LLMCheck); expect the M5 Max to be faster, but benchmark on your own hardware.
- **MoE loads all experts into unified memory** even though only a few activate per token — a 35B-A3B still needs ~20 GB resident. Fine on 128 GB, but it affects cold-start and co-residency with other apps.
- **Some cited figures come from vendor announcements and community blogs**, which can be optimistic; treat headline generational jumps (e.g., Gemma 4's very large reported gains) as vendor-reported until independently confirmed.
- **Rapidly moving target:** Alibaba was previewing further releases (e.g., a 2.4T-parameter Qwen3.8, weights promised but not yet shipped as of July 19, 2026) and IBM/Google iterate frequently; re-check for newer small non-reasoning models before finalizing your default.
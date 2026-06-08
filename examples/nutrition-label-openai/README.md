# OpenAI Agents SDK — SG Nutrition Investigator (image input)

Idiomatic [OpenAI Agents SDK](https://openai.github.io/openai-agents-python/) implementation of the agent in [`../specs.md`](../specs.md). The agent is given a **photo of a nutrition label**; the model's own vision reads the product name, additives, and sugar/saturated-fat values, then runs the SFA additive, HCS, and Nutri-Grade tools. There is no Open Food Facts lookup — the image is the sole source of truth.

## Run

```bash
cp .env.example .env        # then fill in your LiteLLM proxy key
uv sync
uv run python main.py                  # defaults to the bundled milo.jpeg
uv run python main.py path/to/label.jpeg
```

Pass any label photo path; defaults to the bundled `milo.jpeg`. Reads proxy + model
config (`OPENAI_API_KEY`, `OPENAI_BASE_URL`, `OPENAI_MODEL`) from a local `.env`
in this directory (gitignored; copy from `.env.example`). The local `.env` is
loaded with `override=True`, so it wins over any stray `OPENAI_*` in your shell.

## What to notice (SDK characteristics)

- **Image input is a content-list item.** The run `input` is a message list with a `content` array mixing `{"type": "input_text", ...}` and `{"type": "input_image", "image_url": "data:image/jpeg;base64,..."}`. A base64 data URL works directly — clean and explicit.
- **Tools = `@function_tool`, zero schema authoring.** The JSON schema is inferred from the function's type hints and docstring — the least-friction tool definition of the three SDKs.
- **Typed structured output.** `output_type=NutritionVerdict` (a Pydantic model) means the run returns a validated Python object — no parsing. `result.final_output` is a `NutritionVerdict`. The standout feature versus the Claude demo's free text. The model's `reasoning` is declared as the **first** field, so structured output doesn't cost you the rationale — the model writes its working-out before committing to the findings (and that field doubles as a scratchpad). All three demos surface reasoning; here it's a typed field rather than a prose section.
- **Parallel tool calls by default.** The model often dispatches the per-additive `check_sfa_additive` calls concurrently. Our tools are async + `httpx`, so that's safe — but tools that mutate shared state would need care.
- **Pointing at a non-OpenAI endpoint.** We build an `AsyncOpenAI(base_url=..., api_key=...)` against the LiteLLM proxy and wrap it in `OpenAIChatCompletionsModel`. Use the Chat Completions model (not the Responses API) for third-party proxies.
- **Tracing gotcha.** The SDK's built-in tracing tries to export to OpenAI's backend; with a proxy-issued key that fails, so we call `set_tracing_disabled(True)`. On real `api.openai.com` you'd get a full trace graph in the dashboard for free — a strength you lose behind a proxy.

## Additive data

`check_sfa_additive` resolves against the **full SFA permitted-additives list** (~540 entries, ~270 with E-numbers) in `sfa_additives.json`, parsed from the official SFA PDF by [`../build_additives.py`](../build_additives.py). Lookups work by E-number, INS number, or name. At load time the demo builds a normalised alias index (dropping parentheticals and stereo markers, so "monosodium glutamate" matches the PDF's "Monosodium L- glutamate") and indexes both E- and INS-numbers (incl. base numbers, so "500" finds "500(ii)") — making ~110 number-only entries like the phosphates findable. A tiny map covers true colloquialisms absent from the PDF (MSG, vitamin C, baking soda). The prompt also nudges the vision model to pass an E-number when the label only prints a name, since number lookups are most reliable. A handful of consumer-relevant warnings (benzene/Vitamin C interaction, PKU/phenylalanine, etc.) are overlaid on top, since the PDF's own "Notes" column only records "GMP"/"*". Additives not in the list return an honest "not found — worth noting" rather than assuming permitted.

To regenerate: `uv run --with pdfplumber python ../build_additives.py`.

## Memory & learning (across runs)

The agent persists to `agent_memory.json` (git-ignored, created on first run) and improves with use:

- **Learns additive aliases.** When the label prints a name the table doesn't know but the model supplies its E/INS number as `e_number_hint`, the `name → number` mapping is saved. Next run the same name resolves deterministically — no model round-trip. (e.g. after one Milo run, `disodium phosphate → 339` is remembered.)
- **Remembers products.** Each verdict is saved by product name; a `recall_product` tool (called first) surfaces the prior verdict so a re-run cross-checks against it instead of starting blind.

This is system-level learning — the model's weights never change, but the agent's data improves, and it **compounds the more labels you run**: each read can teach a new alias and add a recallable product, so over time it resolves more deterministically and calls the model less. For production memory, the OpenAI Agents SDK ships `SQLiteSession` / `Session`; this JSON file keeps the demo portable and inspectable.

## Observed behaviour

Reads both sample labels accurately (e.g. Marigold HL: 4.5 g sugar / 0.3 g sat fat per 100ml → Nutri-Grade B), runs the additive loop once per identified additive, and returns a clean typed verdict every time.

## Run under `runtimed` (hosted on the platform)

The same agent can run as a first-class Runtime agent, hosted by the control
plane through the Python contract shim (`../../contrib/shims/python`, the
reusable `runtime_contract` library). `runtimed` execs `uv run python serve.py`
here as a supervised subprocess speaking the HTTP/SSE agent contract; the typed
`NutritionVerdict` is rendered to the same prose block the CLI prints and
streamed as a `text` event.

```bash
cp .env.example .env          # fill in your LiteLLM proxy key
make run                      # builds binaries, uv sync, runs the control plane
# in a second shell:
make conformance              # contract acceptance gate (same suite as Go agents)
make demo-image IMAGE=milo.jpeg   # base64 the photo → POST → stream the verdict
make demo-text                # investigate a pasted label
make sessions                 # list this agent's sessions
```

Requires Postgres for the control plane (`make -C ../.. pg-up`, or Postgres.app).
Durability is Level 1 (sessions/events persist in `shim.db`, replayable via
`?since=N`; conversation memory via `SQLiteSession`); plus the agent's own
`agent_memory.json` learned aliases + product verdicts. Level 2 (in-flight crash
resume) is out of scope — see the repo `ROADMAP.md` §C1.

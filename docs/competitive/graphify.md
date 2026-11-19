# Graphify vs repath

What the leading tool in this category does, where it beats us, where we beat it,
and what is worth taking. Everything here is either read from Graphify's docs and
source or measured on this machine on 2026-11-19 with Graphify CLI **0.9.112**
(PyPI `graphifyy`) against repath **b61b260** — 12 vCPU, 7 GB RAM, another agent
session running concurrently, so treat wall-clock numbers as ±30 %.

Graphify is two products wearing one name:

| Surface | What it is |
|---|---|
| [Graphify-Labs/graphify](https://github.com/Graphify-Labs/graphify) | Apache-2.0 Python CLI + agent skill + MCP server, runs on your machine. 150,476 stars, 14,470 forks, 1,688 open issues, default branch `v8` at the time of writing. |
| [graphify.com](https://graphify.com) | Hosted SaaS: the same graph, plus wiki, memory, PR review, "formal verification", cross-repo workspaces. Free for 3 repos. |

The comparison below is against the open-source CLI, because that is the only
part that competes with what we ship. The hosted tier is a different business
(accounts, OAuth, servers), not a feature gap we should chase.

## 1. What it actually builds

`detect → extract (tree-sitter) → resolve → cluster (Leiden) → analyze → report
→ export`, one Python module per stage, communicating through plain dicts and a
NetworkX graph. The output is a **`graphify-out/graph.json`** file (plus
`graph.html`, `GRAPH_REPORT.md`, `cache/`, `manifest.json`), rewritten by every
build and loaded whole into memory to answer a question.

The graph is a *knowledge* graph, not a symbol graph. On `django` (2,943 `.py`
files) it produced 54,873 nodes of three kinds:

| Node kind | Count | What it is |
|---|---|---|
| `code` | 46,006 | files, modules, classes, functions, methods |
| `rationale` | 8,510 | one node per docstring paragraph, with `rationale_for` edges to the code it documents |
| `concept` | 357 | ecosystem concepts minted from manifests (`package.json` dependencies and friends), not just prose |

and 132,637 edges across 14 relations — `calls` 37,640, `method` 29,435,
`imports` 13,308, `contains` 12,505, `imports_from` 11,108, `rationale_for` 8,940,
`uses` 6,336, `references` 6,108, `inherits` 5,115, and long tails including
`dynamic_import`, `re_exports`, `case_of`, `declares`, `defines`.

So: more things are nodes, imports are edges, and docstrings are entities. That
is a deliberate product choice — and the opposite of our decision 8 (symbols are
the only nodes; files and modules are dimensions).

## 2. Design, side by side

| | Graphify 0.9.112 | repath b61b260 |
|---|---|---|
| Product shape | knowledge graph over any folder, for agents + humans | structural answer engine over one repo, for agents |
| Inputs | code (40 languages: 33 tree-sitter grammars + 7 built-in parsers), docs, PDFs, images, SQL, Terraform, Postgres, video transcripts | `.ts/.mts/.cts`, `.tsx/.jsx`, `.js/.mjs/.cjs`, `.py`, `.rs` |
| LLM in the pipeline | no for code, yes for docs/images/community names/`--dedup-llm` | never |
| Node identity | generated id from file path + label; display labels like `.request()`, `Session`; cross-file fuzzy merge | `(qname, kind, arity)`, scoped, stable across re-index |
| Dedup | MinHash/LSH blocking + Jaro-Winkler + union-find; 12 merges on `requests`, 456 on `django` (166 fuzzy) | none needed — identity is exact |
| Edge direction | `graph.json` is `directed: false`; direction lives in edge ordering and relation semantics; same-endpoint collapse is a known diagnostic (`graphify diagnose multigraph`) | edge PK is `(src, dst, kind)` — mutual calls are two rows |
| Confidence | three labels (`EXTRACTED`/`INFERRED`/`AMBIGUOUS`) plus a coarse `confidence_score` (1.0 / 0.95 / 0.85 observed) | six-rung ladder (1.00 → 0.90 → 0.85 → 0.60 → 0.40 → dropped), per-language rules at 0.70–0.99, and a stable `rule` id on every edge |
| Unresolved references | dropped silently; no count anywhere | `pending` table, `status` reports count + rate + four reason buckets |
| Ambiguity | dropped, or written as `AMBIGUOUS` — zero such edges in either corpus I built | 0.40 group edges with every candidate + rule id visible; `--min-confidence` chooses |
| Storage | one JSON file, whole graph in RAM (NetworkX) | SQLite + recursive CTEs, schema v6, 25 MB for `django` |
| Incremental | SHA-256 AST cache + `manifest.json`; `update`, `watch`, git hooks + a `graph.json` merge driver | git-aware discovery (`git status -uno`, `ls-files -t`), stat/hash skip; cold/incremental/`--full` row-set parity is a test |
| Freshness model | something must run: hook, watch, `update`, or CI | lazy auto-index on first query; no daemon |
| Agent surface | `/graphify` skill installed for ~25 platforms; MCP server with **10** tools (`query_graph`, `get_node`, `get_neighbors`, `get_community`, `god_nodes`, `graph_stats`, `shortest_path`, `list_prs`, `get_pr_impact`, `triage_prs`) | `skills/repath/SKILL.md` + CLI; MCP with **6** answer-shaped tools, one envelope with `confidence`/`truncated`/`provenance`, dual protocol eras |
| Human outputs | `graph.html`, `GRAPH_REPORT.md` (god nodes, surprising connections, suggested questions), Obsidian vault, wiki, Mermaid callflow, SVG, GraphML, Cypher for Neo4j/FalkorDB | text + `--json` on stdout, read-only HTTP `serve`, commit-keyed artifacts |
| CI | `extract` in GitHub Actions + a PR-impact-comment cookbook, `graphify prs` (local PR dashboard), hosted reviews; no local static-diagnostics command | `check` with cycles/dead code/boundaries/hotspots, `--sarif`, `0/1/2/3/4` + `5` exit-code contract |
| Distribution | `pip install graphifyy` / `uv tool install`; Python 3.10+ | one cgo-built Go binary per platform via GoReleaser |

## 3. Measured head-to-head

Same corpora, same machine, cold builds, `--code-only` for Graphify (no LLM).
repath numbers come from `repath index --full` on a clean `.repath/`.

### `psf/requests` @ `3c7baaa` — 37 Python files

| | Graphify | repath |
|---|---|---|
| Wall clock | 2.79 s (4.12 s first run) | 0.22 s |
| Peak RSS | 77.5 MB | 32.1 MB |
| Nodes | 1,517 (1,151 code, 299 rationale, 67 concept) | 787 symbols |
| Edges | 2,894 (`calls` 574, `references` 370, `extends`/`inherits` 53) | 1,044 (`CALLS` 720, `REFERENCES` 283, `EXTENDS` 41) |
| Unresolved refs | not reported | 544 (reported, bucketed) |
| Ambiguous | 0 edges | 25 (2.4 %) |
| Output size | 1.2 MB | 480 KB |

### `django/django` @ `f24f38c` — 2,943 Python files (repath indexes 3,051 paths)

| | Graphify | repath |
|---|---|---|
| Wall clock | **112.9 s** | **3.9 s** (index phase 3.6 s) |
| Peak RSS | 385.9 MB | 336.4 MB |
| Nodes | 54,873 | 44,426 symbols |
| Edges | 132,637 | 56,800 (`CALLS` 28,208, `EXTENDS` 8,690, `REFERENCES` 19,902) |
| Unresolved refs | not reported | 92,533 (62.0 %, bucketed) |
| Ambiguous | 0 edges | 1,570 (2.8 %) |
| Parse failures | 1 file warned | 3 files counted |
| Output size | 57.8 MB `graph.json` | 25 MB `index.db` |

**29× faster cold index at comparable RSS.** Graphify's cost is Python
process-per-file extraction plus building the NetworkX graph and running Leiden.

### Kibana `x-pack/solutions/security/packages` — 2,867 TypeScript files

The subtree copied out of the monorepo so both tools see the same files (a
detached tree has no root `package.json` workspaces, which is why `@kbn/*` stays
unresolved for us — that is the documented gap, not a test artifact).

| | Graphify | repath |
|---|---|---|
| Wall clock | **62.2 s** | **1.3 s** |
| Peak RSS | 188.8 MB | 111.8 MB |
| Nodes | 17,220 (16,431 code, 730 concept, 59 rationale) | 7,538 symbols |
| Edges | 42,769 — of which call-shaped 5,295 (`calls` 4,111 + `indirect_call` 444 + `references` 740) | 3,592 (`CALLS` 3,559 + `REFERENCES` 32 + `EXTENDS` 1) |
| Unresolved refs | not reported | 9,461 (72.5 %) |
| Ambiguous | 0 edges | 90 (2.5 %) |
| Parse failures | 2 sources incomplete (JSON-shaped) | 22 counted |

### Kibana `x-pack/solutions` @ `0551196` — 38,548 TypeScript files

| | Graphify | repath |
|---|---|---|
| Wall clock | **749.8 s** (12.5 min) | **24.9 s** |
| Peak RSS | **1.78 GB** | **1.03 GB** |
| Nodes | 183,061 (180,282 code, 2,014 concept, 765 rationale) | 87,478 symbols |
| Edges | 621,299 — `imports_from` 192,590, `imports` 178,760, `contains` 129,423, `calls` 76,439, `references` 13,018, `re_exports` 12,624, `indirect_call` 5,713 | 62,489 (`CALLS` 60,571, `REFERENCES` 1,643, `EXTENDS` 214, `IMPLEMENTS` 61) |
| Unresolved refs | not reported | 157,205 (71.6 %) |
| Ambiguous | 0 edges | 5,138 (8.2 %) |
| Parse failures | 2 sources flagged incomplete | 158 counted |
| Output size | **367 MB** `graph.json` | **25 MB** `index.db` |

Both tools finish, so the honest reading is not "Graphify cannot scale to Kibana"
— it can, in 12.5 minutes and 1.8 GB — but that our §10 budget (<3 min, <1 GB)
is met with 30× headroom on the same tree, and the artifact is 15× smaller. The
367 MB JSON is rewritten whole on every build and is what the git merge driver and
`watch` mode have to move around; our 25 MB SQLite file is patched in place.

Graphify does find more call edges on the full tree (76,439 `calls` vs our 60,571
`CALLS`) and many more import-shaped ones, because it keeps imports, containment
and dynamic imports as edges while we keep imports as a resolution input. That is
a real recall advantage on TypeScript and the one place where copying a resolver
pass would move a number.

**Reading the node/edge counts.** They are not comparable across tools. Graphify
counts files, modules, docstring paragraphs and concepts as nodes, and models
imports and containment as edges; we count symbols and deliberately store imports
as resolution inputs instead of edges (decision 8 and the `imports` table). The
comparable line is call-shaped edges: on the same 2,867 TS files Graphify kept
5,295 call-ish edges, we kept 3,592 — Graphify resolves more call sites there
(it also silently keeps whatever it resolves and reports nothing about the rest),
while we resolve fewer and account for the 9,461 we did not. On Python it is the
opposite: 1,044 vs 2,894 edges on `requests`, but our `CALLS` count (720) exceeds
Graphify's `calls` (574).

## 4. Where Graphify is better

1. **Breadth of inputs.** 40 languages (including Go, Java, C#, Ruby, PHP, C/C++,
   Kotlin, Swift) plus docs, PDFs, images, SQL, Terraform, live Postgres and video
   transcripts. We index three languages. For most working developers, Go or Java
   support is the first thing that decides whether they can try us at all.
2. **Adoption surface.** `graphify install` writes the skill into ~25 agent
   platforms (Claude Code, Codex, Cursor, Gemini CLI, VS Code, …) and adds
   always-on hooks; `/graphify` is a one-word on-ramp. We ship one `SKILL.md` and
   expect a shell.
3. **Always-on freshness.** `watch` (instant rebuild on code save), git
   post-commit/post-checkout/post-merge hooks, and a `graph.json` merge driver so
   the graph survives branches. Our answer is lazy auto-index plus per-worktree
   caches, which is excellent for one-shot questions and worse for an agent that
   wants a graph that is already warm and shared.
4. **Human-facing artifacts.** `graph.html`, `GRAPH_REPORT.md` with god nodes and
   "surprising connections", an Obsidian vault export, a generated wiki, Mermaid
   call-flow pages, GraphML/Cypher exports. All of it is generated from the same
   graph; none of it is a UI we have to serve.
5. **Cross-repo.** `merge-graphs`, `global add/list`, and two deterministic
   cross-repo passes (`calls` when a receiver type resolves in another repo,
   `same_type_as` when two repos declare the same type). Our artifact is one repo
   per snapshot; multi-repo is explicitly deferred (§16).
6. **PR-shaped workflow.** `graphify prs`, `list_prs`, `get_pr_impact`,
   `triage_prs`, plus hosted review comments; the local CLI already ranks open PRs
   by blast radius and flags collisions.
7. **A token-savings number.** `graphify benchmark` puts a reduction ratio on a
   corpus (83×–137× in their docs) and prints it after every run. It is a crude
   measurement — naive cost is estimated from word count, answers are `chars/4`,
   five canned questions — and their own docs call it "not a quality benchmark".
   But it is a number a reader can repeat, and we have no equivalent.
8. **Knowledge-graph extras.** Rationale/concept nodes, community detection,
   `explain`, work memory (`save-result`, `reflect` with half-life and
   corroboration). Cheap ways to make a graph feel like it knows the project.

## 5. Where repath is better

1. **Answer identity.** `graphify explain ".request()"` reports *three* nodes with
   that label across `models.py`, `sessions.py` and `api.py`, and asks the user to
   disambiguate by path; `graphify affected ".request()"` accepts the same string
   and answers from a single node, printing `Affected nodes for .request()` with no
   file, line or qname for the seed it chose. `repath callers Session.request`
   resolves by scoped qname, states the definition site, and returns the 10 real
   callers with per-edge confidence (0.90/0.70) and the `calls` kind. The label is
   not the answer's problem; not saying which node answered is.
2. **Nothing is thrown away silently.** `status` on the same 2,867-file Kibana
   subtree reports 9,461 unresolved references with reason buckets (`member-scope` 1,429,
   `missing-definition` 7,982, `parse-failure` 14, `scope-gap` 36). Graphify's
   only "unresolved" concept is a query seed that matched no label — there is no
   unresolved-reference count anywhere in `graph_stats`, the report, or the MCP
   tools. A user cannot tell how complete the graph is.
3. **Ambiguity is preserved.** We emit 0.40 groups with all candidates and the
   rule id (5,138 groups on Kibana), so `--min-confidence` is a completeness dial
   and the SCIP/LSP adapters have something to disambiguate. Graphify drops
   non-unique resolutions; every graph I built had **zero** `AMBIGUOUS` edges.
4. **Directional edges with rule provenance.** Every repath edge carries `kind`,
   `confidence`, `rule`, `line`. Graphify's `graph.json` is `directed: false`,
   with `confidence_score` collapsed into three values and a `_origin: ast` tag
   on everything.
5. **Determinism and parity.** Same input → same rows; cold, incremental and
   `--full` builds are asserted to produce identical edge/pending row sets. That
   is what lets an agent trust an answer, and it is what the SCIP adapter's
   "never overwrite a resolved edge" rule protects.
6. **Speed and scale.** 29× faster on `django`, 48× faster on the TS subtree,
   30× faster on the full 38,548-file Kibana tree — and there we stay inside the
   §10 budget (24.9 s, 1.03 GB) while Graphify needs 12.5 minutes and 1.78 GB and
   rewrites a 367 MB `graph.json` on every build.
7. **Multi-agent filesystem story.** `.repath/` per worktree, seeded from an
   immutable commit-keyed cache, plus commit-keyed artifacts for CI and teammates.
   Graphify writes `graphify-out/graph.json` into the working tree and needs a git
   merge driver to survive parallel branches.
8. **CI diagnostics nobody else has.** `check` (cycles, dead code, boundaries,
   hotspots, unresolved rate) with a stable exit-code contract, opt-in SARIF
   attachment, `tests --diff`, `flows`, `trace` (V8 profile evidence), cross-
   language HTTP links, and the SCIP/LSP adapter seam.
9. **Agent surface discipline.** Six answer-shaped tools, one envelope with
   `confidence`, `truncated`, `provenance`, and both MCP protocol eras — against
   ten mixed-shape tools that return formatted text.

## 6. What to take

Ranked by (value to us) ÷ (cost), and noting what each one touches in the contract.

| # | Idea | Why it lands here | Cost | Contract |
|---|---|---|---|---|
| 1 | **`repath install --skill <agent>`** — copy `skills/repath/SKILL.md` into Claude Code / Codex / Cursor / Gemini / VS Code config the way `graphify install` does (with `uninstall` and a `--purge` for `.repath/`) | Removes the only step between "found the repo" and "agent uses it". Pure file copy, no new logic. | ~150 LOC + docs | none (new command) |
| 2 | **A token-savings measurement in the eval harness** — for a corpus, compare tokens of `map --max-tokens`, `impact`, `find`, `path` answers against reading the files those answers point at; print the ratio per question | Makes our "bounded answers" claim repeatable, the way `graphify benchmark` makes theirs. Belongs in `internal/eval` / `make bench-*`, never in `make check`. | ~250 LOC, test-only | none (§13 harness) |
| 3 | **`repath impact --diff [base]`** — reuse `index.ChangedFiles` + the per-file impact walker to answer "what does this PR reach", in text and `--json`, with the existing exit codes | We already have `tests --diff`; impact is the question reviewers actually ask. It is the local half of Graphify's PR tooling with no GitHub dependency. | ~200 LOC + tests | §6 CLI contract (additive flag) |
| 4 | **`repath map --md`** — write the existing module/hub/cycle rollup as committable markdown (one file per module, `index.md` entry) | Gives agents a file they can read instead of a DB they cannot, and humans a diffable artifact. No server, no HTML, no decision 18 conflict. | ~350 LOC | §6 + a note that markdown export is not a UI |
| 5 | **`--scope <subtree>` on `index`** — index a slice of a monorepo without walking to the git root | Running inside a subdirectory walks up to the git root, so a 2,867-file package turns into a 38,548-file build; Graphify indexes what you point at. | ~200 LOC, module-detection risk | §8 discovery |
| 6 | **Cross-repo merge** — `repath merge a.db b.db --out c.db` with `same_type_as`-style links, built on the existing artifact manifest | graphify's `merge-graphs`/`global` show the demand; our artifact is already commit-keyed and schema-gated, so the units exist. | large (fake repos, cross-repo resolution) | §3 decision + §16 |
| 7 | **`--graphml` / `--dot` export** from an artifact | Two hours of `encoding/xml` gives every graph viewer (Gephi, Obsidian, yEd) a path to our data without us owning a renderer. | ~120 LOC | §6 (additive) |
| 8 | **Resolver passes Graphify has and we do not** — interface/typed dispatch beyond annotations, dynamic-`import()` chains, cross-module re-export heuristics | The TS subtree is where they beat us on call edges (5,295 vs 3,592). Some of that is their `interface_dispatch.py` / rescue passes. | medium, per-language | §5 ladder (new rule ids) |

Not on the list: `explain` (our `find` card is the same thing), `god-nodes`
(`map` ranks already), Obsidian vault export (`map --md` plus a script covers it).

**The one that is not a copy: Go.** It is the single largest adoption blocker
relative to Graphify's 40 languages, and it is a repeat of the "Adding a language"
playbook we already have. It needs no contract change — a grammar, an extractor,
module rules (`go.mod` `replace`/`require`, no workspace drama), fixtures, and
ladder rows.

## 7. What not to take

- **An LLM anywhere in extraction or query.** It buys docstring concepts and
  semantic edges, and it costs determinism, offline operation, and the "same input,
  same rows" property our adapters and parity tests depend on (decisions 2, 11, 18).
- **Label-based identity with fuzzy dedup.** Union-find plus Jaro-Winkler merging
  `.send()` in two classes, or `TestX.test_foo` and `test_foo`, is a precision bug
  we would inherit. Our `(qname, arity)` identity is the thing to keep.
- **Undirected storage.** `directed: false` plus edge ordering plus a collapse
  diagnostic is a tax we do not need to pay: `(src, dst, kind)` already keeps
  reciprocal calls apart.
- **Docs/PDF/image ingestion and the hosted control plane.** Different product,
  different threat model, and it would put a model or a server in the middle of a
  tool whose selling point is "one binary, no daemon, no LLM".
- **HTML dashboards and wikis as first-class outputs.** `map --md` gets humans the
  readable artifact; owning a renderer does not.
- **The star-count arms race.** Graphify has 150k stars and an issue backlog of
  1,688; the moat is answer quality and trust, which is exactly our §2 line.

## 8. Three PRs I would open next

1. `feat(cli): install the skill into agent config dirs` — item 1, no contract
   change, immediate adoption win.
2. `test(eval): measure answer tokens against reading the files` — item 2, gives
   us a Graphify-comparable number with honest method notes.
3. `feat(cli): impact --diff` — item 3, the reviewer-facing question, reusing the
   existing changed-file discovery and impact walker.

Then `docs/benchmarks.md` gains a competitive section with the table from §3, and
`PLAN.md` §2 gains the row this document was written for.

## Method and what I did not verify

- Graphify: `graphify extract <dir> --code-only [--max-workers N]`, defaults
  otherwise; repath: `repath index --full` on a clean `.repath/`, current `main`
  binary. Peak RSS from `/usr/bin/time -v`/`-f %M`. One run per corpus except
  `requests` (two), so these are not medians; a concurrent session shares the CPU.
- Kibana was measured two ways: the real repo root (repath, 38,548 files) and the
  `security/packages` subtree copied out so both tools see identical inputs.
- **Not verified:** precision/recall against ground truth for either tool (no
  sampled edge audit was run); the hosted Graphify product (needs an account); the
  correctness of Graphify's token ratio (their method is estimated, mine was not
  attempted); multi-run medians or memory under identical load; any language
  outside Python and TypeScript.

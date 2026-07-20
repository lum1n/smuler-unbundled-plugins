# Drift Detection: Future Improvements

The current drift detector uses simple heuristics (keyword/phrase matching and a small contradiction lexicon). This is enough to catch obvious mismatches, but several improvements would make it much more useful.

## Near-term improvements

### 1. Richer contradiction lexicon
- Load pairs from a config file (`~/.config/smuler/contradictions.json`) so users can add domain-specific opposites.
- Include common software pairs: `interface` vs `type`, `class` vs `function`, `sync` vs `async`, `REST` vs `GraphQL`, etc.

### 2. Rule file content matching
- Currently only exact phrase matches are skipped when suggesting missing rules.
- Add fuzzy/semantic matching so a rule like "preserve the public API" also covers "don't break backwards compatibility".

### 3. Per-repo rule importance
- Some rules are more important than others. AGENTS.md should probably outweigh a single Cursor rule.
- Weight suggestions by rule source (project > agent-specific) and recency.

### 4. Suggestion confidence scores
- Show a confidence percentage based on how many times a pattern appeared and how strongly it conflicts.
- Let users set a minimum confidence threshold.

## Medium-term improvements

### 5. Embedding-based similarity
- Embed agent tasks and rule sentences with a small local model (e.g., `sentence-transformers/all-MiniLM-L6-v2`).
- Detect near-misses where a task violates the spirit but not the literal text of a rule.
- Much better false-positive rate than keyword matching.

### 6. LLM-based reasoning
- For high-confidence cases, call a local or hosted LLM to:
  - Explain why the task conflicts with the rule.
  - Generate a better proposed patch.
  - Summarize multiple related violations into one suggestion.
- Keep this optional and behind a setting because it sends data to a model.

### 7. Historical trend analysis
- Track how often each suggestion category appears over time.
- Surface "this rule is ignored in 80% of sessions" or "agents keep asking about X — consider adding a skill".

### 8. Cross-repo learning
- If you work on many repos, learn which rules you add most often.
- Propose adding those rules to new repos that lack them.

## Long-term improvements

### 9. Intent harvesting
- Extract intent/decisions from agent conversations, not just the initial task.
- Detect when an agent explicitly overrides a rule and the user accepts it.
- Propose updating the rule rather than fighting the same battle again.

### 10. Automatic skill/rule generation
- When a new pattern is confirmed multiple times, auto-generate a `.claude/skill` or `.cursor/rules` entry.
- Show a diff and require user approval before writing.

### 11. Team-wide rule sync
- Compare rules across a team's repos.
- Surface inconsistencies like "repo A uses 2-space indent, repo B uses 4".
- Propose a shared team ruleset.

## Implementation notes

- Keep the heuristic detector as a fast first pass.
- Run embedding/LLM analysis asynchronously or on demand to avoid slowing down `GetStatus`.
- Persist all suggestions with evidence so users can audit why a rule was proposed.
- Always require explicit approval before writing files, even for "auto-apply" modes.

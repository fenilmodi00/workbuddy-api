# DSH History Replay and GUI Environment-Sentence Compatibility

> **Historical implementation note:** The text below describes early DSH-specific handling. Since v1.14.43, the identity sentence and the full GUI opening have moved into `identity.replacements`, with no dedicated hardcoded rewrites; ordinary entries run as case-sensitive literal replacements, and code regions follow the same rule. For current config and the restricted leading-wildcard boundaries, see the [user guide](user-guide.md#configuring-text-replacement-and-the-restricted-leading-wildcard).

Related: [Issue #24](https://github.com/CangShui/workbuddy-gateway/issues/24)
and [Issue #21](https://github.com/CangShui/workbuddy-gateway/issues/21).

The existing rules only processed the first system text block, and ran before config prompt injection.
So assistant history, later text blocks, and `systemPrompt.fallback/force` could still carry the
same known identity sentence; Anthropic's text-block merging also loses the position of later block starts.
Handling only the identity prefix also left the DSH GUI environment sentence uncovered.

This patch only closes the following gaps:

- Known identity prefixes of system / assistant strings and every standard `type: "text"` block,
  reusing the existing neutral replacement and case-sensitive matching rules; leading whitespace and mid-body occurrences do not participate in prefix matching.
- Exact matching on the two known GUI opening sentences, changing only
  `the DeepSeek Harness Web GUI` in the first sentence to `a Web GUI`.
  Later page-reference rules and feature descriptions stay verbatim, without relying on a version or summary of the whole paragraph.
- The GUI rules preserve fenced, inline, and indented code; backtick-delimiter state is carried across lines.
- The three protocols run the shared handling after fallback / force injection; Messages additionally runs the same idempotent pass
  before system / assistant text-block merging.

The known GUI opening comes from DeepSeek Harness's public package
[`@deepseek-ai/dsh-harness-dev-context`](https://github.com/deepseek-ai/deepseek-harness/tree/main/packages/context/harness-dev-context).
The implementation also referenced
[dsh-connect-workbuddy v4.15.1 release notes](https://github.com/dingminhua/dsh-connect-workbuddy/releases/tag/v4.15.1).

User body text, tool results, tool arguments, reasoning, and image blocks never participate in the new rules.
Logs record only fixed-rule names and hit counts, with no new request-body logging.
The existing full-request diagnostic switch, account scheduling, SSE conversion, and billing logic are unchanged.
This patch does not change headers, client signatures, or authorization identity, and adds no billed retries.
If the upstream still returns 11128, the error is returned as-is; this is not a universal solution to all channel restrictions.

## Validation Scope

On top of upstream main `ceb2ac8`, HTTP regression tests were added first, and 24 config-injection cases plus
12 history-replay cases failed against the original code. After the fix, streaming and aggregated requests of all
three protocols were checked, covering code boundaries, tool argument and result association, log content, repeated processing, and no-retry on continued rejection.

The related compatibility logic was already verified and deployed in the fork; this upstream port uses only synthetic requests and a local
`httptest` upstream, reads no production credentials, replays no private sessions, and adds no real billed calls.
This does not mean full long-running sessions or later upstream policy changes have all been validated.

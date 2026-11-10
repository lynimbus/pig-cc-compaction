// plugin/compaction.js — the policy core of pig-cc-compaction.
//
// Two pure functions, JSON in and JSON out: no imports, no host objects, no
// Node API, so the very same source runs under moejs in the Go host and in a
// plain test.
//
//   planCompact(input)  -> {action: "pass", why}
//                        | {action: "takeover", request, payload, promptChars}
//   parseSummary(input) -> {summary} | {retry: true, why} | {none: true, why}
//
// planCompact reproduces what Claude Code does when it compacts: the provider
// request the session last sent is resent unchanged with one user turn
// appended, which is the only way the provider's prompt cache still hits.
// parseSummary reads the reply back: the <analysis> block goes first (models
// quote it, so a <summary> inside it is not the summary), then the first
// <summary> block is the summary.
//
// CC_PROMPT is Claude Code's compaction prompt, embedded verbatim so that a
// wording change is an edit here and never a Go rebuild.

export const CC_PROMPT = `Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions.
This summary should be thorough in capturing technical details, code patterns, and architectural decisions that would be essential for continuing development work without losing context.

Before providing your final summary, wrap your analysis in <analysis> tags to organize your thoughts and ensure you have covered all necessary points. In your analysis process:

1. Chronologically analyze each message and section of the conversation. For each section thoroughly identify:
   - The user's explicit requests and intents
   - Your approach to addressing the user's requests
   - Key technical concepts
   - Specific code sections, file paths, function signatures
   - Errors and how they were fixed
   - Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
2. Double-check for technical accuracy and completeness, addressing each required element thoroughly.

Your summary should include the following sections:

1. Primary Request and Intent: Capture all of the user's explicit requests and intents in detail
2. Key Technical Concepts: List all important technical concepts, technologies, and frameworks discussed.
3. Files and Code Sections: Enumerate specific files and code sections examined, modified, or created. Pay special attention to the most recent messages and include full code snippets where applicable and include a summary of why this file read or edit is important.
4. Errors and fixes: List all errors that you encountered and how you fixed them. Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
5. Problem Solving: Document problems solved and any ongoing troubleshooting efforts.
6. All user messages: List ALL user messages that are not tool results. These are critical for understanding the users' feedback and changing intent.
7. Pending Tasks: Outline any pending tasks that you have explicitly been asked to work on.
8. Current Work: Describe in detail precisely what was being worked on immediately before this summary request, paying special attention to the most recent messages from both user and assistant. Include file names and code snippets where applicable.
9. Optional Next Step: List the next step that you will take that is related to the most recent work you were doing. IMPORTANT: ensure that this step is DIRECTLY in line with the user's most recent explicit requests, and the task you were working on immediately before this summary request. If your last task was concluded, then only list next steps if they are explicitly in line with the users request. Do not start on tangential requests or really old requests that were already completed without confirming with the user first.
   If there is a next step, include direct quotes from the most recent conversation showing exactly what task you were working on and where you left off. This should be verbatim - it should be an accurate depiction of what was happening and where the user left off.

Additional Instructions: <the /compact argument when the caller passed one; omit this whole block when empty>

Your response must contain only the <analysis> and <summary> blocks.`;

// CUSTOM_BLOCK is the line of CC_PROMPT the /compact argument replaces. With
// no argument the whole block, blank lines included, is dropped.
const CUSTOM_BLOCK =
  'Additional Instructions: <the /compact argument when the caller passed one; omit this whole block when empty>';

// RETRY_PREAMBLE is what makes a second attempt read differently: it says what
// the last reply got wrong and asks for the two blocks and nothing else. The
// heading requirement is spelled out because parseSummary checks it.
const RETRY_PREAMBLE =
  'Your previous reply did not contain a usable <summary> block, so this is a retry. ' +
  'Reply with exactly two blocks and nothing else: first <analysis>...</analysis>, ' +
  'then <summary>...</summary>. The summary must contain all nine sections the ' +
  'instructions list, each section name alone on its own line, and it must finish. ' +
  'Do not call tools and do not write anything before, between or after the two ' +
  'blocks.\n\n';

const ANALYSIS_OPEN = '<analysis>';
const ANALYSIS_CLOSE = '</analysis>';
const SUMMARY_OPEN = '<summary>';
const SUMMARY_CLOSE = '</summary>';

// SECTIONS are the nine section names the prompt asks for, in the prompt's own
// order. They serve two purposes: they tell a real summary from a quotation of
// the instructions, and they are the completeness check. A block that names only
// a few of them is a reply that stopped early, not a summary worth storing.
const SECTIONS = [
  'Primary Request and Intent',
  'Key Technical Concepts',
  'Files and Code Sections',
  'Errors and fixes',
  'Problem Solving',
  'All user messages',
  'Pending Tasks',
  'Current Work',
  'Optional Next Step',
];

// MIN_SECTIONS is how many headings a summary must carry to be accepted. It sits
// below nine on purpose: a model that merges two sections still produces a
// usable summary, while a reply cut off mid-way lands far below this line.
const MIN_SECTIONS = 5;

// TRUNCATED_STOP_REASONS are the stop reasons that mean the provider cut the
// reply at the output limit, in the canonical value Pi reports ("length") plus
// the raw ones providers send when a proxy passes them through.
const TRUNCATED_STOP_REASONS = {
  length: true,
  max_tokens: true,
  maxTokens: true,
  MAX_TOKENS: true,
};

function isObject(value) {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// count reads a non-negative integer: a missing, negative or fractional field
// is zero, which is what both functions mean by "not set".
function count(value) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return 0;
  return value > 0 ? Math.floor(value) : 0;
}

// pass is the answer that hands compaction back to the host, with the reason
// spelled out for the trace log.
function pass(why) {
  return { action: 'pass', why };
}

// buildInstruction is CC_PROMPT with the custom instructions spliced in, and
// with the retry preamble in front from the second attempt on.
function buildInstruction(attempt, customInstructions) {
  const custom = typeof customInstructions === 'string' ? customInstructions.trim() : '';
  const base = applyCustomInstructions(CC_PROMPT, custom);
  return count(attempt) >= 1 ? RETRY_PREAMBLE + base : base;
}

function applyCustomInstructions(prompt, custom) {
  const placeholder = prompt.indexOf(CUSTOM_BLOCK);
  if (custom === '') {
    if (placeholder < 0) return prompt;
    // Drop the line and the blank line above it, keeping one blank line
    // between section 9 and the closing instruction.
    return prompt.split('\n\n' + CUSTOM_BLOCK).join('');
  }
  if (placeholder < 0) {
    // The prompt was edited without the placeholder: keep the argument rather
    // than silently dropping it.
    return prompt + '\n\nAdditional Instructions: ' + custom;
  }
  return prompt.split(CUSTOM_BLOCK).join('Additional Instructions: ' + custom);
}

// normalizeHeading reduces a line to the section name it might be: bullets,
// numbering, emphasis and a trailing colon are decoration, and any text after
// the name means the line is prose rather than a heading.
function normalizeHeading(line) {
  return line
    .replace(/^\s*[-*+]\s+/, '')
    .replace(/^\s*\d+[.)]\s+/, '')
    .replace(/^[#>\s]+/, '')
    .replace(/[*_`]/g, '')
    .replace(/[:：\s]+$/, '')
    .trim()
    .toLowerCase();
}

// headingNames is the lookup headingCount uses, built once from SECTIONS.
const headingNames = {};
for (const name of SECTIONS) headingNames[normalizeHeading(name)] = true;

// headingCount counts the section names that stand alone on a line. This is what
// separates a summary from a quotation of the instructions: a model quoting the
// prompt writes "Primary Request and Intent: ..." inside a sentence, while a
// real summary labels each section on its own line.
function headingCount(block) {
  let n = 0;
  for (const line of block.split('\n')) {
    if (headingNames[normalizeHeading(line)] === true) n += 1;
  }
  return n;
}

// pickSummary returns the best <summary> candidate and how many headings it
// carries. Blocks are paired open-to-next-close, so a quotation of the
// instructions is its own candidate and loses to the summary that follows it;
// picking the best candidate is what keeps a quoted </summary> from ending the
// block mid-sentence (measured twice before this rule, at 642 and 435
// characters, both cut off inside the quoted tag).
function pickSummary(text) {
  const opens = [];
  for (let i = text.indexOf(SUMMARY_OPEN); i >= 0; i = text.indexOf(SUMMARY_OPEN, i + 1)) opens.push(i);
  const closes = [];
  for (let i = text.indexOf(SUMMARY_CLOSE); i >= 0; i = text.indexOf(SUMMARY_CLOSE, i + 1)) closes.push(i);

  let best = '';
  let bestScore = 0;
  // Every open pairs with every close after it. A quotation adds one open and
  // one close, so the real summary survives as the (open, close) pair that spans
  // both; the score decides, and the longer block wins a tie because the shorter
  // one is the version cut at the quoted tag.
  for (const open of opens) {
    for (const close of closes) {
      if (close < open) continue;
      const block = text.slice(open + SUMMARY_OPEN.length, close).trim();
      const score = headingCount(block);
      if (score > bestScore || (score === bestScore && block.length > best.length)) {
        best = block;
        bestScore = score;
      }
    }
  }
  return { block: best, score: bestScore };
}

// stripLeadingAnalysis removes the analysis blocks a reply opens with, and only
// those. Stripping every analysis block anywhere also ate the ones a summary
// quotes while describing the instruction, so the model's own words came back
// mangled ("format \u0060\u0060"). An analysis block that does not open the reply is
// part of the summary text and stays.
function stripLeadingAnalysis(text) {
  let rest = text;
  for (;;) {
    const open = rest.length - rest.trimStart().length;
    if (!rest.startsWith(ANALYSIS_OPEN, open)) return rest;
    const close = rest.indexOf(ANALYSIS_CLOSE, open + ANALYSIS_OPEN.length);
    if (close < 0) {
      // A truncated analysis block: whatever it wraps is analysis, not a
      // summary, so nothing after its opening tag can be trusted.
      return rest.slice(0, open);
    }
    rest = rest.slice(0, open) + rest.slice(close + ANALYSIS_CLOSE.length);
  }
}

// planCompact decides whether this compaction resends the captured request.
//
// The pass rules are in priority order: an overflowed request cannot be resent
// at all, a session with no capture has no prefix, an open breaker has given up
// on AUTOMATIC compaction, and a payload without messages is no request.
//
// The breaker gates automatic compaction only: an explicit /compact (reason
// "manual") still takes over, because that is the session's only way back once
// the breaker tripped (pig-cc-compaction.md §4.6: 连续 3 次失败 → 本会话停用
// 自动接管，/compact 仍可用). Nothing else is special about "manual": an
// overflowed or uncaptured request still passes.
export function planCompact(input) {
  const event = isObject(input) ? input : {};
  const reason = typeof event.reason === 'string' ? event.reason : '';
  const capture = isObject(event.capture) ? event.capture : null;
  const breaker = isObject(event.breaker) ? event.breaker : {};
  const payload = capture === null ? null : capture.payload;

  if (reason === 'overflow') {
    return pass('overflow: the request that was captured is itself past the context limit, so resending it would overflow again');
  }
  if (capture === null) {
    return pass('no captured request: no provider payload was recorded for this session, so there is no prefix to resend');
  }
  if (breaker.disabled === true && reason !== 'manual') {
    return pass('circuit breaker open: automatic compaction failed or thrashed too often for this session; an explicit /compact still takes over');
  }
  if (!isObject(payload) || !Array.isArray(payload.messages) || payload.messages.length === 0) {
    return pass('captured payload has no messages: there is no request prefix to resend');
  }

  // Copy, never mutate: the payload the session actually sent stays untouched,
  // and so does everything but messages in the copy below.
  const messages = payload.messages.slice();
  const instruction = buildInstruction(event.attempt, event.customInstructions);
  messages.push({ role: 'user', content: [{ type: 'text', text: instruction }] });

  return {
    action: 'takeover',
    why: 'resending the captured request verbatim with the summary instruction appended',
    // The host validates request.messages against Pi's roles (system, user,
    // assistant, tool) and rejects the provider-format "developer" turn the
    // captured body starts with, so the vehicle request carries the instruction
    // alone. It never reaches the provider: onPayload replaces the body with
    // payload below, which keeps the captured prefix byte for byte.
    request: { messages: [{ role: 'user', content: [{ type: 'text', text: instruction }] }] },
    // Every field but messages comes from the captured payload as it was sent:
    // model string, system prompt, tools and cache_control markers included.
    // That is what keeps the provider's prompt cache warm.
    payload: Object.assign({}, payload, { messages }),
    promptChars: instruction.length,
  };
}

// parseSummary reads the summary out of the model's reply.
export function parseSummary(input) {
  const result = isObject(input) ? input : {};
  const text = typeof result.text === 'string' ? result.text : '';
  const attempt = count(result.attempt);
  const stopReason =
    typeof result.stopReason === 'string' && result.stopReason !== '' ? result.stopReason : 'unknown';
  const outputTokens = count(result.outputTokens);

  // Every complete <analysis> block the reply opens with is dropped first: a
  // <summary> quoted inside the analysis is not the summary.
  const picked = pickSummary(stripLeadingAnalysis(text));
  const detail = 'stopReason=' + stopReason + ' outputTokens=' + outputTokens;

  // Accepted only when the block names enough of the nine sections and the
  // provider did not stop it at the output limit. A length stop can leave a
  // closed block that is still incomplete, and storing that would drop history
  // with nothing in the trace to show for it.
  const truncated = TRUNCATED_STOP_REASONS[stopReason] === true;
  if (picked.block !== '' && picked.score >= MIN_SECTIONS && !truncated) {
    return { summary: picked.block };
  }

  let why;
  if (picked.block === '') {
    why = 'the reply has no <summary> block with content';
  } else if (truncated) {
    why = 'the reply stopped at the output limit with ' + picked.score + ' of the nine sections';
  } else {
    why = 'the summary names ' + picked.score + ' of the nine sections, so the reply is incomplete';
  }
  if (attempt === 0) {
    return {
      retry: true,
      why: why + ' (' + detail + '), asking again with a stricter instruction',
    };
  }
  return {
    none: true,
    why: why + ' after a retry (' + detail + '), leaving compaction to the host',
  };
}

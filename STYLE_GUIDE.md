# Writing style

A guide for writing docs (`docs/`) so they read as if a person wrote them:
varied, plain where it can be, with no filler.

**Scope.** This file governs rhetorical style: em-dash/italics/
antithesis/callout/aphorism budgets, filler vocabulary, and quality
self-labels. It is not a completeness check.

## The one principle

Docs start to read machine-written when every sentence does the same rhetorical
work: claim → contrast → em-dash aside → tidy one-liner. The fix is **restraint
and variation**, not more polish. Dial the machinery down.

- **Vary.** Mix short sentences with long. Let some paragraphs end plainly, with no mic-drop.
- **Show, don't announce.** Don't tell the reader something is "simple," "robust," or
  "battle-tested". A code reference, a linked test suite, or a status condition name
  demonstrates it instead.
- **Cut the scaffolding.** A sentence that only tells the reader what the next one will do is filler.

## Budget (per ~1,000 words)

Checkable limits:

| Device | Limit |
|---|---|
| Em-dash (—) | ≤ 2 |
| Italic emphasis (`*word*`) | ≤ 4 |
| Antithesis ("not X, but Y" / "rather than") | ≤ 1 per page |
| `:::` callout boxes | ≤ 1–2 per page |
| Aphoristic one-liner close | ≤ 1 per page |

## Do / Don't

### 1. Antithesis — keep one, drop the rest
The "not X, but Y" chiasmus makes a point land once per page. Reused every paragraph, it's a tell.

- **Don't:** "The webhook doesn't just validate the CR — it doesn't let bad state through, it stops it at the door, not after it lands. And the operator doesn't just reconcile once — it keeps going, forever chasing drift."
- **Do:** "The webhook rejects an invalid CR before it's persisted, not after. The operator then reconciles continuously, correcting drift as it appears."

### 2. Em-dash — reserve it for the real pivot
- **Don't:** two or three em-dashes carrying one sentence's structure.
- **Do:** replace most of them with a period, comma, colon, or parentheses. At most one em-dash per paragraph, saved for the actual turn.

### 3. Italics — only terms and true contrasts
- **Don't:** "The *reconciler* watches the *CR* and drives it toward the *desired* state."
- **Do:** italicize a term once, on first definition (a *sub-reconciler* owns one condition type) and leave it upright everywhere after. When in doubt, leave it upright.

### 4. Aphorisms and callouts — ration them
- **Don't:** end every paragraph on a slogan ("Idempotency is the whole point.") or restate the same point again in a `:::` box.
- **Do:** one memorable line per page, earned, tied to something concrete. Let other paragraphs stop when the point is made. Keep `:::` boxes for one genuine takeaway per page; fold the rest into prose.

### 5. Don't announce quality — demonstrate it
- **Don't:** "this is the robust way to do it," "a clean, correct implementation," "this is not just a workaround."
- **Do:** let the specifics carry it — a linked test suite, a condition name, an exact command. Delete the self-assessment; state the fact it was standing in for.

### 6. Tricolons — break the symmetry
Three parallel items in a row, again and again, reads mechanical.
- **Do:** sometimes use two, sometimes four; sometimes split into separate sentences. Variation over symmetry.

### 7. Cut the meta-signposting
- **Don't:** "The previous section explained the CRD fields; this one shows how they're used." "The above gives the detail, this is the same thing at a glance."
- **Do:** delete it. The page's opening paragraph and its heading already orient the reader.

### 8. Retire the filler vocabulary
Recurring abstractions that turn from style into template by their sixth use:
`load-bearing`, `by construction`, `structural rather than aspirational`, `first-class`,
`precisely`, `exactly`, `deliberately`. Use concrete wording instead, and delete
`precisely`/`exactly` outright — they rarely strengthen anything.

### 9. Sentences you can read in one breath
- **Don't:** 3–5 subordinate clauses and asides packed into one sentence (overview paragraphs and bullet lists are the worst offenders).
- **Do:** split at the colon or semicolon. If you run out of breath reading it aloud, break it.

## Keep — don't sand these off

- **Frontmatter** (`title`, `quadrant`) and the page's structure.
- **Tables, diagrams, and code blocks.** Structure is fine unless you are explicitly tasked to restructure the docs.
- **Cross-links between pages.** They do real orienting work; don't replace them with prose recaps.

## Pre-commit check

Before committing edited docs, scan for:

1. More than ~2 em-dashes or ~4 italic spans per 1,000 words? Cut.
2. A second "not X, but Y" on the same page? Rewrite it as a plain statement.
3. A quality self-label ("robust," "clean," "battle-tested") with nothing concrete backing it? Replace or delete.
4. A paragraph that ends on a slogan *and* a `:::` box that repeats it? Keep one.
5. A sentence you can't read aloud in one breath? Split it.
6. A sentence that only previews the next one? Delete it.

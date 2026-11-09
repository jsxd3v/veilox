# Detectors

Detection is deterministic: the same text and the same config always give the same placeholders. Every
rule below is covered by table tests in `internal/detect`, with the examples shown here.

Try any text with `veilox scan file.txt` (or `-` for stdin). It runs locally and sends nothing.

## Built-in detectors

### `EMAIL`

`local@domain.tld`, where the local part uses letters, digits and `._%+-`, and the top-level domain has
at least two letters.

| Masked | Not masked |
|---|---|
| `maria.santos@example.com`, `maria+tag@mail.example.co.uk` | `a@b` (no TLD), `a@b.c` (one-letter TLD), `@maria`, `pkg@1.2.3` |

`MARIA@EXAMPLE.COM` and `maria@example.com` get the same placeholder.

### `CREDIT_CARD`

13 to 19 digits, written together or in groups split by single spaces or dashes, that **pass the Luhn
checksum**. A digit run glued to letters is ignored.

| Masked | Not masked |
|---|---|
| `4111 1111 1111 1111`, `4111-1111-1111-1111`, `3782 822463 10005` | `4111 1111 1111 1112` (fails Luhn), `0000 0000 0000 0000`, `x4111111111111111` |

In `order 12 4111 1111 1111 1111`, only the card is masked: veilox picks the longest valid stretch of
digit groups.

### `IBAN`

Country code, two check digits, then the account number, written together or in groups of four. It
**must pass the ISO 13616 mod-97 check** and have the right length for its country. Only countries in
the IBAN registry are recognised.

| Masked | Not masked |
|---|---|
| `DE89 3704 0044 0532 0130 00`, `GB82WEST12345698765432`, `pt50000201231234567890154` | `DE88 3704 …` (bad check digits), `AB12 is not an iban` |

### `PHONE`

9 to 15 digits in a phone-like layout:

- starting with `+`, with or without separators: `+351 912 345 678`, `+351912345678`, `+1 (555) 010-4477`;
- or split by spaces, dashes, dots or brackets: `(555) 010-4477`, `06 12 34 56 78`, `912 345 678`,
  `030.1234.5678`. A first group of four or five digits must start with `0` (an area code).

Plain digit runs without `+` are **not** phones, so order numbers, IDs and timestamps stay as they are.

| Not a phone | Why |
|---|---|
| `123456789` | no `+` and no separators |
| `2026-1105-77` | `2026` is not an area code |
| `2026-11-09 14:30`, `09.11.2026 10 00` | dates |
| `192.168.100.200` | a dotted quad is an IP address (masked as `IPV4`) |
| `555-0101` | fewer than 9 digits |
| `4111 1111 1111 1112` | more than 15 digits: a failed card number is never cut down into a phone |

A number written like `1 000 000 000` has a phone-like layout and *is* masked. If that matters for your
data, turn the detector off and add a custom rule for your country's format.

### `IPV4`

Four numbers from 0 to 255, no leading zeros, not part of a longer dotted sequence.

| Masked | Not masked |
|---|---|
| `203.0.113.7`, `10.0.0.1`, `0.0.0.0` | `1.2.3.4.5` and `v1.2.3.4` (versions), `256.1.1.1`, `01.2.3.4` |

`masking.ipv4.skip_private: true` leaves private and loopback addresses unmasked.

### `NAME`

Only the names you list in `masking.names` or `masking.names_file`. veilox has no name recognition of
its own (see the README's "What veilox does NOT protect against").

- Case-insensitive, with any whitespace between words: `Maria  Santos` matches `Maria Santos`.
- Whole words only, Unicode-aware: `Maria` doesn't match inside `Mariana`, and `José Álvarez` works.
- The longest listed name wins: with both `Maria Santos` and `Maria` listed, `Maria Santos` is one
  placeholder.
- Names must start with a letter or a digit.

A list of tens of thousands of names is fine: names are indexed by their first word.

## Custom rules

```yaml
masking:
  rules:
    - name: CUSTOMER_ID
      pattern: 'CUS-[0-9]{6}'
    - name: POLICY_NO
      pattern: 'policy no\. ([A-Z]{2}[0-9]{8})'
      group: 1
```

- The pattern uses [RE2 syntax](https://github.com/google/re2/wiki/Syntax): linear time, no
  lookarounds or backreferences.
- `group` masks one capture group instead of the whole match (`policy no. <POLICY_NO_1>`).
- The name becomes the placeholder type. It must be upper case and can't reuse a built-in name.

## When matches overlap

All detectors run on the original text. Overlaps are resolved in this order:

1. **The longer match wins.** `maria.santos@example.com` is one `EMAIL`, not a `NAME` inside an email.
2. **Same length:** `CREDIT_CARD` > `IBAN` > `EMAIL` > `IPV4` > `PHONE` > custom rules (in config order) >
   `NAME`.
3. **Still tied:** the earlier match wins.

## Placeholders

- Format: `<TYPE_N>`, numbered per type in order of first appearance: the system prompt first, then each
  message in order.
- **Same value, same placeholder** within one request, across all messages. The value given back is the
  first spelling seen.
- If the prompt already contains text shaped like a placeholder (say, documentation that mentions
  `<EMAIL_1>`), veilox skips that number, and that text comes back exactly as it was.
- Placeholders are never reused across requests. The mapping lives in memory for one request only.

## What is masked in a request

| API | Masked | Left as is |
|---|---|---|
| OpenAI | message `content` (string or `text` parts), `refusal`, tool-call `arguments` | `name`, tool definitions, images/audio/files (see `unsupported_content`) |
| Anthropic | `system`, `text` blocks, `tool_use` input values, `tool_result` content | `thinking` blocks (signed by Anthropic: changing them would break the request), images/documents |

# Indexer-Hardening Guide: outline (draft)

Target: a page ready to publish on developers.stellar.org, reviewed by a stellar-core or platform engineer before publication (RFP section 1, "Guide"). Every claim about what stellar-core validates will cite the stellar-core source file and line it comes from, and the field-by-field table is generated from the same data as the corpus manifest so the two cannot disagree.

## 1. Two rules for anyone who reads the ledger

1. Fields that stellar-core does not validate are attacker-controlled input. Anyone who submits a transaction chooses them.
2. Who a transaction affected comes from the ledger meta (what core changed), never from the envelope (what the submitter claimed).

## 2. Validated vs. unvalidated fields (field-by-field table)

One row per transaction and operation field: validated by core (yes / partially / no), what the check is, where in stellar-core it happens, and the corpus entry that exercises it.
- Known starting points from the RFP: validated: signatures, sequence numbers, fees, time bounds. Passed through untouched: memos, Soroban authorization trees, muxed account IDs.
- Each row links to the corpus entry IDs whose manifest `shapes` cover it.

## 3. Depth and size caps

- Recursive structures: ScVal (vec, map), SorobanAuthorizedInvocation sub-invocations.
- Recommended caps per structure, with the limits core and the host already enforce cited from source, and the decoder limits each SDK exposes (rs-stellar-xdr `Limits`, go-stellar-sdk decoder options, js-xdr).
- Memory: decode with a byte budget; never pre-allocate from an untrusted length prefix (rs-stellar-xdr#581, #598).
- Linter rule XDRL002 enforces the recursion bound in Go code.
- Corpus entries: `cap46-nested-authorization-contract-calls-sac` (an authorization tree with a sub-invocation) and `cap21-vectors-at-max-length` (var-arrays at their bound: 100 operations, two extra signers, 64-byte names and values, a 28-byte memo).

## 4. Participants from meta

- Why the envelope is the wrong source: muxed IDs, sponsorships, SAC operations that change trustlines with no ChangeTrust operation (CAP-0073, wallet-backend#731).
- How to derive participants from `TransactionMeta` v4 ledger-entry changes, operation by operation.
- Corpus entry: `cap73-sac-created-trustline`.

## 5. Muxed and signed-payload addresses

- SEP-0023 strkey forms: G, M (muxed, CAP-0027), P (signed payload, CAP-0040), C, and their binary layouts.
- Storage: never truncate to the 32-byte account key; store the full strkey or its full raw form (wallet-backend#716).
- Muxed IDs: full u64 range, including 0 and 18446744073709551615 (corpus entry `cap27-muxed-source-and-destination-id-extremes`).
- Signed payloads: payload length 1 to 64 bytes, padding rules (corpus entry `cap40-signed-payload-signer-max-payload`).

## 6. Checklist for indexers in any language

The same three checks the linter enforces in Go, written so a reviewer can apply them by hand:
1. Every field you store or act on: is it validated by core? If not, treat it as untrusted.
2. Every recursive walk: does it take and enforce a depth bound and a size budget?
3. Every participant list: is it built from ledger meta?

## 7. How to test your indexer with the corpus

Point the replay harness at your pipeline through an adapter (mock RPC `getLedgers` first), run the corpus, read the pass / mishandled / dropped report.

# IU-WS3-01: explicit v3 audio ingress

This slice connects an explicitly configured private media participant to the
existing Encoder-Recorder archive layout, append-safe file opener, bridge packet
channel, and Manager statistics. Existing v2 HTTP and Add behavior is preserved.
Ordinary startup registers the v3 endpoints with a nil participant and refuses
them before archive, dedupe, queue, or statistics mutation.

## Transport and trusted inputs

This is an internal synthetic integration transport, not an adopted signed
production protocol.

| Input | Meaning |
| --- | --- |
| POST /v3/streams/{id}/audio/opus | Existing bounded IngestRequest packet body |
| X-Autostream-Lifecycle | Exactly one base64url-without-padding shared CC02 Record, encoded size at most 32768 bytes |
| POST /v3/streams/{id}/audio/opus/inspect | Same original CC02 metadata; empty body; no reappend/requeue |
| HTTP 202 body | Direct shared CC08 Record, never evidence of a remote terminal result |
| Error body | Shared lifecycle.Result with refusal, stale, conflict, or uncertainty |

OwnerSnapshot is explicit trusted dependency injection. It includes validated
CC01 membership, immutable original CC03 admission, complete ingress and closure
profiles/vectors, admission model/CAS, exact ingress and owner producers, namespace,
source frame, and positive raw byte budget. No missing axis, profile, limit,
generation, incarnation, service identity, or runtime signing key is filled from
a request, environment variable, current value, zero, or unlimited default.
The metadata decoder is the pinned Contracts decoder; this repository contains
no duplicate common DTO, enum, validator, or lifecycle reducer.

NewSyntheticParticipant requires the literal ISOLATED_TEST_ONLY marker and an
absolute isolated journal directory. No ordinary server or environment option
selects it. ProductionQualified remains false. NewSyntheticQueueFixture uses the
actual bridge packet channel without starting a codec, RTP sender, or media process.

The explicit handler verifier uses only its supplied key/token. It does not
discover runtime node credentials through the existing v2 environment fallback.
The synthetic service-token literals in tests are deliberate test fixtures and
are not deployment credentials.

## Guard, persistence, and outcomes

The participant validates current membership, producer, complete resource-bound
vector and original tuple, then registers parent and queue child through the
shared ReduceAdmission model. A start record is exclusively created and file
Sync/Close observed before the product callback can create an archive directory,
touch dedupe, append raw JSONL, or enqueue a packet. An exclusive process-shared
owner guard plus persisted owner-state comparison prevents independent stores
from assigning overlapping sequence numbers. A crashed guard stays held.
OwnerRevision is a required trusted generation CAS. Generation snapshots are
persisted under the same process guard; another store with a cached old Run or
producer vector is refused before product effects. No missing owner revision is
defaulted or inferred from the caller's metadata.
There is no automatic reissue, retry, requeue, reserve release, hold clearance,
forced stop, or READY path.

Raw JSONL contains the existing product packet payload. The lifecycle journal,
CC02/CC03/CC04/CC08, audit metadata, and responses contain no raw bytes, base64
packet content, credential, user payload, or secret-derived commitment.

The response separates these observed boundaries:

| Boundary | Evidence and limitations |
| --- | --- |
| RAW_WRITE | Exact requested/written extent and offset, including short writes |
| DURABILITY | File Sync and Close observed; this is not filesystem/power-loss qualification |
| FD release | Close observed or explicitly UNKNOWN independently of write/sync |
| QUEUE_ADMISSION | Actual bounded bridge enqueue; v3 does not drop earlier queued work to claim admission |
| QUEUE_TERMINAL | Pending/UNKNOWN until a fresh owner callback with exact generation and receipt CAS |
| Provider / codec / mux / remote delivery | Not exercised or certified by this slice |

Partial write, sync failure, close ACK loss, missing/full/closed queue, old seen
marker, and partial queue admission keep distinct typed outcomes. A v2 seen marker
left before a failed write never becomes a successful duplicate receipt.
After dispatch uncertainty, only the same Attempt can be inspected.
Result-journal write uncertainty leaves the initial unresolved dispatch record.

Restart requires the owner-supplied current admission model to match the complete
journal. A missing genesis, stale owner state, missing closed-cut marker against
a closed owner snapshot, malformed journal, or crashed owner guard does not
reopen admission. Inspect reads the original tuple without invoking product effects.
Selective local restore cannot be qualified against a separately stale external
owner: independent restore freshness, storage qualification and D4 remain unresolved.

Packet dedupe is Manager-local. The persistent participant prevents same-Attempt
replay after restart. This slice does not claim exactly-once packet admission
across a manually different Attempt after a Manager restart.

## Cut and completion export

CloseAdmission acquires the same guard as append/child registration and closes
registration. It does not stop playback or infer completion from an empty queue.

ClosureAdmission returns a distinct immutable shared CC03 observation of the
actual cut. The original admission record and accepted WorkRef admission IDs stay
unchanged. Their association is owned by the private journal: same owner,
membership, admission epoch, reservation tuple, namespace, and cut registration.
The frozen shared schema has no explicit original-admission-parent field.
A cross-repository synthetic verifier must check this owner association; public
IDs alone do not prove it. Production composition remains unqualified.

CompletionSnapshot exports a direct shared CC04 and ObservationSet using the
actual cut, final accepted frontier, contiguous terminal frontier, holes, physical
obligations, and lineage closure. Its content-specific ID/proof reference hashes
only nonsecret typed local metadata, in deterministic order. These identifiers
are not D4 commitments or durable independent witnesses.
Repeated identical content has the same ID; pending and terminal contents have
different IDs. Every receipt hold stays HELD. No CC05 seal is constructed.
Queue dequeue without a terminal callback leaves completion pending/UNKNOWN.

Parent and child IDs are owner-local nonsecret metadata identifiers:
sha256 of JSON encoding the shared Correlation fields in their declared order;
ingress-{hex} and queue-{hex}, with the latter parent bound to the former.
Consumers need an explicit trusted expectation; this is not an authority token.

## Required evidence

| Scenario | Required roots |
| --- | --- |
| SC-CC-02 / 03 | TestIUWS3ActualHTTPIngress; TestIUWS3CurrentGenerationAndCallback |
| SC-CC-04 | TestIUWS3AdmissionCutAndRestart; TestIUWS3QueueTerminalCAS |
| SC-CC-07 | TestIUWS3RawFaultBoundaries |
| SC-CC-08 | TestIUWS3IndependentProcess; TestIUWS3ProcessSharedOwnerGuard |
| Malformed metadata / cancellation | TestIUWS3CancellationAndMalformedMetadata |

Tests use fixed independent literal inputs in testdata/owner.json and
testdata/request.json, isolated temporary raw files, real loopback HTTP servers,
explicit barriers, short-write/Sync/Close faults, a native child test process,
and process-shared owner objects. No real audio, codec, mux, FFmpeg, provider,
VPS, credential, live DB, or deployment is used.

The initial RED compile was observed before participant implementation. The six
participant roots executed natively on Windows; final candidate results and
all raw/HTTP/Race/cross-build evidence must be read from the required CI records.
A Windows full-package attempt was blocked during existing Google API module
extraction with Access denied before audioingest/httpapi tests could execute.
It is not a product-test PASS. Directory Sync, power-loss durability, real mixer
terminal integration, signed production transport, independent owner/trust
qualification, provider effects, full media acceptance and D4 are not certified.

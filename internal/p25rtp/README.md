# FullRate RTP wire codec

This package implements the wire format of a private, current-only Phase 1
FullRate group profile. It is not a complete ISSI implementation or an
interoperability claim. The profile uses RTP v2, PT100, 8000 Hz and group role
SSRC1 (Home) or SSRC2 (Serving). The existing Pion RTP dependency serializes the
RTP header. There are no SIP, socket, session, authorization, timer, vocoder,
FEC, crypto, media adapter or native DVM transport dependencies here.

`Marshal` and `Parse` handle one complete datagram. Sequence16 and timestamp32
are supplied unchanged by the owner. The owner must initialize and advance the
sequence across all packets, assign proper 125 microsecond timestamps, bind the
authenticated peer/role, own TSNs and parity, and handle loss, replay,
reordering, spurt state and cross-packet SF continuity. Duplicated or reordered
datagrams are not rejected by a stateless wire decoder. HeartbeatQuery uses
TSN0; Heartbeat can also carry a nonzero muted-spurt TSN. Other packet types
require nonzero TSN. No sequential allocation or 63-TSN limit is assumed.

## Deliberate private restrictions

- One voice block per packet (VBB1) on both send and receive. BACA also permits
  VBB2/3; this profile rejects them rather than retaining unused bundling code.
- Fixed PT100. BACA permits a different SDP-negotiated payload type.
- Only PacketType1, RFControl11, ConsoleControl15, HeaderWord5 and FullRateVoice0
  blocks. Alias, vendor, HalfRate, Native and vocoder-mode blocks are rejected.
- At most four blocks, with PacketType first, one RF/Console control word second
  for Request/Start/Progress, then optional HeaderWord and one voice block.
  Header/voice appear only in Request/Progress. This profile admits HeaderWord
  before voice only; it does not infer cross-packet arrival order.
- Transmit zero and reject nonzero reserved bits in quality, HeaderWord, LSD
  and ES. This is explicit private policy, not a universal rule inferred from
  a diagram's reserved label.
- Console descriptor0 only. Descriptor1 is reserved for future conventional
  complexity1 use in the cited edition's note. ConsoleTXPriority retains all
  256 byte values. RF/console priority types0..2 are represented.

The largest accepted profile packet is 93 bytes: fixed RTP12 + control1 + four
block headers16 + PacketType4 + ConsoleControl24 + HeaderWord18 + ES voice18.
RF-control packets are smaller. Parse bounds the whole datagram and every
header/length before indexing, rejects trailing bytes, and owns copied arrays.

## Voice and public metadata

Each voice block represents 20 ms and carries exactly 88 bits: U0..U3 are 12
bits, U4..U6 are 11 bits, U7 is 7 bits, concatenated MSB first. FEC is already
removed. The package retains that 11-byte string exactly; matching an existing
native codec's bit representation is a separate adapter qualification.

Quality values are supplied receiver reports. The codec packs them into their
field widths, without estimating errors or changing mute/lost flags. FR9/18
carry two decoded low-speed words and two statuses. FR12..17 carry four raw
six-bit, Hamming-decoded but not Reed-Solomon-decoded encryption-sync symbols
and the raw ternary-weighted status. Values81..127 cannot represent four valid
status trits and are rejected. No ES symbols/status, LSD, quality or FEC are
synthesized from MI, algorithm, key ID or voice bytes.

HeaderWord retains MI72, ALGID8, KID16, MFID8, group16 and NID16. It neither
interprets algorithm support nor supplies traffic keys. The header SF is
initial0 and VBB is1. ServiceOptions, PacketType mute/losing, voice quality mute/
lost and unrelated RTP marker/block E bits remain distinct fields. The codec
does not derive emergency, clear encrypted voice or rewrite vendor identities.

## Exact sources

The coherent source is TIA-102.BACA-B November2012, with applicable
TIA-102.BACA-B-1 July2013 and BACA-B-2 November2016 corrections. B1 does not
change these wire diagrams. B2 replaces the indicated clauses below; its
default unknown ServiceOptions4 supersedes the base's0. Supplying unknown
service options is a caller responsibility. B3 IWF/MCPTT is out of scope.
Missing BACD supplementary-service documents remain missing; this package does
not supply supplementary or emergency-cancellation messages.

Document A: `p25_2ba752917c605931`, SHA256
`2ba752917c6059313086fb0f2defb1a086c1b232363e6dd3c0727143ee813230`,
`documents/tia-library/819 TIA 102BACA B Final Published Document 2012 11 19.pdf`.

Document B: `p25_c936752dd3e10621`, SHA256
`c936752dd3e10621c8d5b80cdb502b385c54531185291ce08bc9e4257208cd43`,
`documents/tia-library/8.19_TIA-102.BACA-B-2_Final Published Document_2016_11_03.pdf`.

The original PDF figures were visually checked. Page numbers below are 1-based PDF positions.

| Implemented definition | Exact citation |
| --- | --- |
| MSB/network transmission order | A §2.3, Figures25–26; PDF89–90, printed65–66 |
| RTP fixed header, PT, 8k clock, group SSRC | A §4.1, Figure40/Table36; PDF228–230, printed204–206 |
| S/C/count and verbose-only headers | A §4.2.1, Figure42; PDF231–232, printed207–208 |
| Block E/type7/offset14/length10 and types | B §4.2.2, Figure43/Table37; PDF124–126, printed108–110 |
| 20 ms/88-bit voice, quality, frame types/length | B §4.3.1, Figure44/Table38; PDF127–129, printed111–113 |
| LSD words/status, four ES symbols/status equation | A §4.3.1.1–.3, Figures45–46; PDF236–238, printed212–214 (unchanged by B) |
| PacketType/M/service/TSN/L/interval | B §4.3.2, Figure47; PDF130–132, printed114–116 |
| HeaderWord fields, SF0 and VBB | A §4.3.3, Figure48; PDF240–241, printed216–217 |
| RFControl SUID and transmit priority | B §4.3.4, Figure49; PDF133–134, printed117–118 |
| ConsoleControl layout and descriptor/priority | B §4.3.5, Figure50; PDF134–137, printed118–121 |
| Required block ordering and packet combinations | B §4.4.1–.2, Figure57; PDF144–146, printed128–130 |
| S-bit table, sequence and TSN semantics | A §7.3–.4, Table49; PDF403–406, printed379–382 |

## Independent structural tests

The test literals were derived manually from the original figures and checked
by a reviewer who did not use this encoder. Existing corpus fixture candidates
Request/Grant/End/Heartbeat/HeaderWord were separately checked. The 73-byte
Request adds FR1 with U0..7 values123/456/789/ABC/5A3/456/321/55 and nonzero
quality AE A4. FR9 and FR12 literals separately exercise raw LSD/status and
raw ES/status. ConsoleStart independently checks three 56-bit SUIDs.

These use synthetic fields, not live credentials, operational randomness,
valid encrypted speech or valid RF FEC. They establish structural packing,
not voice conversion, RF timing, audio quality or equipment interoperability.
Captured/independent codec vectors remain necessary for adapters and end-to-end
CAI reconstruction. Pure parser fuzzing and malformed tests do not fill that gap.

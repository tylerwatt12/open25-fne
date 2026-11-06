# Private group SIP/TCP component

This is internal Zone–Site call control. It is not an ISSI/CSSI implementation, registrar, PBX, proxy or radio qualification. Four service boundaries remain unchanged. Only current group FullRate, PT100/8000, VBB1, grant-first operation is in scope. REGISTER, OPTIONS, unit calls, legacy FullRate, re-INVITE, supplementary services, IWF and forking are absent. CAI registration is separate.

```go
node, err := sipvoice.New(sipvoice.Options{
    PeerID: enrolledLocalServiceID,
    BearerToken: existingLocalServiceToken,
    Authenticate: validateEnrolledPeerAndCurrentCallProof,
    OnInvite: allocateAuthorizedSegment,
    OnEnd: recordSegmentEnd,
})
// ServeTCP owns a bound *net.TCPListener. The deployment VPN protects transport.
go node.ServeTCP(listener)
session, err := node.Invite(ctx, sipvoice.Peer{ID: remoteID, URI: remoteSIPTCPURI}, call, localEndpoints)
// Decode and validate the accepted Grant.ID/Talkgroups before starting media.
acceptedContext := session.ResponseContext()
local, remote := session.Media()
// Pass these endpoints to the separately owned plain UDP RTP engine.
```

`Call.ID` identifies one framework allocation/leg; any logical-call identifier and full user/automation authorization proof remain in its bounded `Authorization` JSON object. `SourceRF` and `SourceConsole` are explicit. The framework validates them, endpoints, enrollment, inhibition, grant/recipient ownership, proof expiry and one-use semantics before allocation. SIP dialog identity is separate. The Node owns transaction/dialog retransmission idempotency, not another authorization ledger. It does not derive authority from an SUID or a management report. It does not manufacture an ISSI SIP Priority header; authorized priority/emergency/protection remain framework context and RTP payload responsibilities.

`OnInvite(ctx,*Session)` receives setup context and returns `Answer{Endpoints,ResponseContext,Release}`. The response context is an object containing the **actual accepted grant**, never FNE traffic keys. It is carried beside SDP in a bounded MIME part, wrapped with immutable `Call.ID` and accepted only on the matching SIP transaction/dialog. `ResponseContext()` clones it. The framework must decode/check the accepted grant ID and expanded talkgroups before using it; a valid JSON object alone is not authorization. There is no HTTP fallback.

`Release` owns that incoming allocation and runs once, including when cancellation or local End wins while the callback is returning. It must use a service-owned bounded cleanup operation, not retain the setup context, which ends after INVITE/ACK. `OnEnd` is once-only notification after allocation cleanup. Callbacks must honor their context, be bounded, and must not call `Node.Close` recursively. Outgoing endpoint ownership remains with the caller until success; use the caller's existing idempotent release operation on Invite error and OnEnd.

Wait for `Session.Ready()` and also honor `Session.Done()` before media. An incoming accepted SIP segment never authorizes a talk spurt on its own. The RTP engine separately enforces endpoint/SSRC/epoch ownership, PTT grants, sequence/replay rules, recipient leases and native RF cleanup. A valid SDP or an ACK cannot bypass those checks.

This private deployment uses plain TCP for SIP and plain UDP for RTP inside the user-selected VPN. It has no TLS, SDES, SRTP or media-key API. The existing enrolled service ID/Bearer credential and full per-call proof remain mandatory; the VPN does not replace call authorization. Enrolled peer URIs and dialog contacts must be explicit `sip:service@host:port;transport=tcp`. This package exposes only `ServeTCP(*net.TCPListener)` and pins every outgoing request to TCP and the configured destination. SIPgo internally supports other transports, but the package does not listen on or select them, accept their URI parameters, or provide fallback.

Only one RTP/AVP media description with current FullRate100/8000 is accepted, with even RTP2–65532 and the following RTCP port at the same unicast IPv4 address; the limited broadcast address is rejected before allocation. SAVP, SDES/crypto and legacy key lines, mux, legacy codec, extra media and unsupported SDP attributes are rejected. VBB1 is the fixed payload contract, not a private SDP fmtp extension. The media engine remains responsible for fixed endpoint/SSRC ownership, sequence policy and PTT state.

Session/authorization formatting is redacted; errors never quote SDP or credentials. SIPgo stream-parser errors can otherwise log raw packet bytes, so this component disables SIPgo's dependency-wide default logger at package initialization and uses discard loggers. Keep `sip.SIPDebug` and raw tracers disabled. Use typed call/lifecycle metadata for diagnostics, never raw SIP, SDP or authorization context.

SIPgo provides parsing, message construction, transport and transactions. The small transaction-user state implements the required missing pieces: absolute64*T1 ACK wait, bounded CANCEL/late200 ACK+BYE, and reliable-transport duplicate-response tombstones. Successful duplicates re-ACK without allocating; repeated BYE cannot repeat Release/OnEnd. BYE writes/transactions are capped at2s. Callee End before ACK releases local resources and defers BYE until ACK or the RFC absolute timeout. `End` success is not evidence of an RF terminator, subscriber reception or an air-interface tail. Node.Close cancels setup, joins owned work, and closes its listener/transports. `MaxSessions` also bounds retained transaction-user tombstones (64*T1); it is not a radio-channel capacity setting.

## Sources and private-profile boundary

- SIPgo official v1.6.0 tag `e8676d2dd709405487a458e645be3f53dabe7cd1`, BSD2-Clause. Keep `third_party/sipgo-LICENSE` and `third_party/sipgo-dependency-LICENSES` with source and binary notices. Official source: https://github.com/emiago/sipgo/tree/v1.6.0 . The pinned library's high-level cancellation/answer helpers are not used for their unbounded/racing paths; no library fork or handwritten SIP parser is added.
- RFC3261 sections9.1–9.2 (CANCEL),12(dialogs),13.2.2.4/13.3.1.4 (2xx/ACK),15(BYE),18.1.1/18.2.1(reliable TCP transport); RFC6026 transaction retention. https://www.rfc-editor.org/rfc/rfc3261.html ; https://www.rfc-editor.org/rfc/rfc6026.html .
- RFC3551 section6 (RTP/AVP) and RFC4566 sections5.7/5.14 (connection/media SDP): https://www.rfc-editor.org/rfc/rfc3551.html ; https://www.rfc-editor.org/rfc/rfc4566.html .
- TIA-102.BACA-B November2012 sections3.7.1/4.1, Table36: PDF139–142/227–230, printed115–118/203–206, document `p25_2ba752917c605931`, `documents/tia-library/819 TIA 102BACA B Final Published Document 2012 11 19.pdf`.
- TIA-102.BACA-B-2 November2016 section3.7.2/Table26 PDF61–64, printed45–48; section4.4.2/Figure57 PDF145–146, printed129–130, document `p25_c936752dd3e10621`, `documents/tia-library/8.19_TIA-102.BACA-B-2_Final Published Document_2016_11_03.pdf`.
- TCP-only SIP, VPN-protected plain RTP/AVP and the authorization/answer MIME objects are explicit project deployment/control policy layered on the standard P25 RTP payload; they do not establish full BACA conformance. The RTP adapter owns payload layouts and VBB1 validation. See the separate sealed corpus review for omitted mobility, emergency-cancellation/BACD gaps and role obligations.

The prior qualified TLS/SDES candidate and its receipts remain preserved in the separate `sip-rtp-transport-20261101-01/candidate` evidence directory. This is a clean current-profile rewrite, with no secure/legacy compatibility branch. Local TCP tests are software qualification, not Pi, RF, subscriber or ISSI interoperability proof.

# Private FullRate RTP transport

Plain RTP/UDP over the deployment VPN, as requested by the user. No TLS, SRTP, SDES, transport keys or encrypted fallback. This does not change P25 radio encryption. Web applications retain browser transport protection.

Listen reserves an even RTP port and adjacent RTCP port. New transfers the pair to one immutable endpoint/role-bound session. Send owns the randomized 16-bit RTP sequence. Receive(ctx) validates endpoint, header, FullRate payload and a 128-packet wrapped sequence window using the existing Pion replay detector. Sequence checks reject duplicates/old packets; they are not cryptographic authentication. Reordering remains visible to the caller's bounded media/spurt engine. Shutdown interrupts and joins reads/writes. There are no background readers or packet queues here. RTCP is reserved and may be discarded under the selected BACA profile.

Use a fresh pair/session per admitted SIP media connection. The caller owns enrolled peer authorization, spurt/TSN ownership, timestamps, pacing, reordering/loss treatment and the actual native grant. A valid packet cannot allocate a call or change endpoints. No endpoint learning, SRTP compatibility or sequence-65535 End sentinel exists. Maximum profile datagram is 93 bytes; writes have at most one frame (20ms) of local socket budget. These private limits do not prove end-to-end timing or radio qualification.

Sources: TIA-102.BACA-B November2012 and B-2 November2016 exact citations in the corpus review; RFC3550; pinned github.com/pion/transport/v5 v5.1.1 replaydetector. Earlier SRTP receipts remain historical and are superseded by the user policy.

An admitted SIP allocation may contain successive actual speech spurts. Its caller owns and finally closes Session; each Engine borrows it until Done, retaining the negotiated endpoint and replay window across epochs. Engine.End/Close stops and joins only that spurt's reader. Start a subsequent Engine only after the prior Done, with a freshly admitted parity-correct TSN. A spurt End never releases the native allocation by itself.

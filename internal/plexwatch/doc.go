// Package plexwatch delivers one viewer's watched state, and whole-star
// ratings, to that viewer's own Plex account.
//
// In this release the integration is dormant: AccountSyncSupported is false
// because scrobbles.plex.tv accepts no credential a non-Plex server can hold.
// Nothing below runs against Plex until that constant is turned on.
//
// Local watch rows stay authoritative. A watch mark and a rating each have
// their own timestamp, so one cannot make the other look newer. Intents are
// rows in state_sync_journal, written in the same transaction as the watch
// change. This package does not subscribe to in-memory state events.
//
// Provenance of the upstream contract, none of it a live call from this
// repository except the November 2026 interoperability probe:
//
//   - POST https://scrobbles.plex.tv/state returned 201 for items with
//     guid, viewedAt (unix milliseconds), viewState "complete" or "unplayed",
//     optional isBulk, and optional ratedAt plus an integer rating.
//     The user field is the numeric Plex account id. The GET echo is
//     GET https://scrobbles.plex.tv/state/{accountId}?after={cursor}&count=100
//     with items and pageInfo.endCursor / hasNextPage. isBulk and a show-level
//     guid in the POST were omitted from that echo.
//     Source: PMS verbose log posted by doug316, 2024-06-07,
//     https://forums.plex.tv/t/mark-as-unwatched-does-not-work/879522
//
//   - viewState "complete" for a watched movie, a rating-only item
//     {"guid","ratedAt","rating"}, and HTTP 422 when viewedAt is in the
//     future (the whole POST fails). Plex employee gbooker02 identified the
//     future timestamp. Source: Havohej, 2024-01-24,
//     https://forums.plex.tv/t/status-sync-for-old-movies-and-series/867157
//
//   - A custom-agent GUID received HTTP 400 and PMS stopped that sync.
//     Plex employee BigWheel attributed it to the custom agent.
//     Source: ItsThatJA, 2026-03-19,
//     https://forums.plex.tv/t/can-these-reviews-for-my-pfofile-be-deleted-by-a-plex-dev/937027
//
//   - PMS decides whether to sync with
//     GET https://plex.tv/api/v2/server/users/{accountId}/view_state_sync
//     (200 continue, 403 stop). It obtains per-user tokens from
//     GET https://plex.tv/api/v2/server/users/services?auth_token=<server token>.
//     The services route is authenticated with Ether's claimed server token.
//     Service credentials are selected by the connected viewer's account ID.
//     Sources: https://forums.plex.tv/t/unable-to-install-new-plex-server-on-new-w-11-computer/934457
//     and https://forums.plex.tv/t/403-response-from-get-https-plex-tv-api-v2-server-users-xxx-view-state-sync-reused/883818
//
//   - Official scope: movies and episodes only, no in-progress offsets, no
//     music, no movie editions. Activity and profile history depend on the
//     account setting "Sync My Watch State and Ratings" and on privacy
//     settings. A POST does not by itself guarantee a Discover Activity card.
//     https://support.plex.tv/articles/sync-watch-state-and-ratings/ (modified 2025-03-24)
//
//   - A Plex engineer said metadata.provider.plex.tv scrobble handlers are
//     deprecated. This package does not call them.
//     https://github.com/Taxel/PlexTraktSync/discussions/1842
//
// The token stored here is the authToken from POST/GET /api/v2/pins, the same
// credential Ether already uses for GET /api/v2/user and the account consent
// endpoint. It is not sent to scrobbles.plex.tv. The claimed Ether server token
// obtains the matching account's scrobbling service envelope; the decrypted JWT
// is sent as Authorization: Bearer. A read-only probe using Ether's own claim
// returned HTTP 200 with items/pageInfo on 2026-11-04. PMS 1.43.4/1.43.5 loader
// inspection established the base64/AES-CBC envelope and length-byte padding.
// Service tokens remain in memory for at most five minutes, bounded by expiry,
// and are discarded when the claim changes or the service refuses a token.
// Consent checks never modify Plex settings. Their result is stored only for
// the generation checked, so late responses cannot restore a revoked account.
//
// Permit admission and consent changes (enable, disable, reconnect, revoke)
// take the same fence-row lock. Admission inserts a permit only when that
// locked generation matches the intent's stored consent generation and this
// attempt still owns the live journal row. A database error reading that
// generation leaves the intent retryable. A confirmed missing or stale
// generation does not send.
//
// Admission also commits an immutable plexwatch_attempt containing the exact
// account id, consent generation, resolved GUID, aspect, original timestamp
// and wire payload. It contains no plaintext credential. Failure or uncertainty
// committing admission means no network send. Once admission is durable, even
// a crash before transmission leaves an unknown-effect hold. Expiring worker
// claims or permits never expires that hold. The remote account/GUID/aspect
// conflict index and a local user/key/aspect check prevent aliases and remaps
// from bypassing it. Unrelated known targets and other aspects remain usable.
//
// A successful response commits the attempt's receipt, import echo and original
// journal acknowledgement together. A failed or uncertain local commit cannot
// reopen the network path. Transport/read errors, server failures, ambiguous
// statuses and Retry-After do not prove no effect; delivery stops until the
// hold is reconciled against GET /state/{accountId}. Definitive refusals (HTTP
// 400, 401, 403, 422) clear the attempt fence so later changes to that title
// are not blocked.
//
// Reconciliation reads GET /state/{accountId} on a backoff schedule, on
// reconnect/consent/remap, and on explicit viewer Resolve. If Plex already
// reflects the admitted payload, the attempt is receipted without re-POSTing.
// If Plex does not reflect it and no permit for that attempt is still in
// flight, the hold is released and one fresh admission of the current intent
// is allowed.
//
// A permit already inserted is in flight. Turning sync off does not delete
// it, and a request already handed to Plex cannot be recalled. An intent
// that has not been admitted is not sent after the fence moves, including
// when lookup began under the previous generation. Releasing one permit
// does not change another. A journal row records the fence generation
// observed when it was written; a row that commits after a disable keeps
// that older generation and is not sent when sync is turned on again.
package plexwatch

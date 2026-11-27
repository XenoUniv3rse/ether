# Vela title preferences

Vela advertises title feedback in `features.notInterested` in its capabilities
document, including the copy served in bootstrap.

```json
{"features":{"notInterested":{"available":true,"thumbsUp":true}}}
```

`available` enables Not interested and Undo. `thumbsUp` enables the like action
and its removal.

Send a partial update to `POST /vela/v0/titles/{id}/watch` with the usual
idempotency key. Omitted fields are left alone except when setting the opposite
preference. `notInterested: true` adds the title to the viewer's shared Not
interested list, feeds the dislike to taste, and clears any thumbs-up.
`notInterested: false` removes the title from the list. `thumbsUp: true` records a
like and removes the title from the list. `thumbsUp: false` clears the like.
Sending both fields as `true` returns HTTP 400 with `invalid_request`.

This request marks a title.

```json
{"idempotencyKey":"example-mark-title","notInterested":true}
```

The response is HTTP 200 with the updated viewer data.

```json
{
  "accepted": true,
  "watch": {
    "positionSeconds": 0,
    "played": false,
    "favorite": false,
    "watchlisted": false,
    "notInterested": true,
    "thumbsUp": false
  }
}
```

Title objects that carry viewer data, including detail, rows, search and children,
always include both booleans in their `watch` object. A marked title is excluded
on the next Home or library row fetch. To undo the mark, send
`{"idempotencyKey":"example-undo-title","notInterested":false}`. The title is
eligible for rows again. Bootstrap revalidation uses the viewer preference
revision, and a stale row cursor can be refreshed by fetching a new row list.

package httpx

// Page is the generic cursor-paginated collection envelope used by all list endpoints.  It matches the Omnilium HTTP
// house guide:
//
//   - The items field carries the page of results (typed).
//   - The pagination field carries the cursor and hasMore flag.
//   - nextCursor is an opaque base64-encoded string; null when there is no next page (hasMore=false).  Callers pass it
//     back as ?cursor= to fetch the next page.  A null nextCursor and hasMore=false are equivalent signals; both are
//     present for client convenience per the guide.
//   - hasMore is true when a next page exists.
//   - Total is deliberately absent: the guide prohibits an eager total on cursor-paginated lists.
//
// JSON response shape (mid-stream page):
//
//	{
//	  "data": [...],
//	  "pagination": { "nextCursor": "opaque-token", "hasMore": true }
//	}
//
// JSON response shape (last page):
//
//	{
//	  "data": [...],
//	  "pagination": { "nextCursor": null, "hasMore": false }
//	}
type Page[T any] struct {
	// Data is the slice of items in this page.
	Data []T `json:"data"`
	// Pagination carries the cursor fields for the next page.
	Pagination PagePagination `json:"pagination"`
}

// PagePagination is the pagination sub-object embedded in Page[T].
type PagePagination struct {
	// NextCursor is the opaque cursor for the next page.  Null (nil pointer) when HasMore is false; a non-empty string
	// when HasMore is true. Per the HTTP house guide, last-page responses must serialize as JSON null, not an empty
	// string, so clients can distinguish "no cursor" from a zero-length token.
	NextCursor *string `json:"nextCursor"`
	// HasMore is true when there is at least one more page after this one.
	HasMore bool `json:"hasMore"`
}

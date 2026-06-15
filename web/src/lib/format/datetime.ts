// Shared date/time formatting helpers.
//
// formatDueAt formats a dueAt ISO string into a human-friendly, locale-aware
// string (e.g. "Fri, Jan 15, 9:00 AM"). Falls back to the raw string when the
// value is empty, null, undefined, or unparseable — never throws, never returns
// "NaN" or "Invalid Date".

/**
 * Format a dueAt ISO 8601 / RFC 3339 string into a human-readable date+time
 * using the browser's locale and timezone.
 *
 * Returns the raw string on any parse failure so callers always get something
 * displayable.
 */
export function formatDueAt(dueAt: string | null | undefined): string {
  if (!dueAt) return "";
  try {
    const d = new Date(dueAt);
    // new Date() with an unparseable string yields NaN for getTime(); guard it.
    if (Number.isNaN(d.getTime())) return dueAt;
    return new Intl.DateTimeFormat(undefined, {
      weekday: "short",
      month: "short",
      day: "numeric",
      hour: "numeric",
      minute: "numeric",
    }).format(d);
  } catch {
    return dueAt;
  }
}

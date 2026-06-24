// Universal load for the home (chat) route.
//
// CSR-only (ssr=false inherited from +layout.ts) — runs client-side only.
//
// Behaviour:
//   - Reads the active conversation id from the store (set by a previous visit or already present after a reload that
//     happened mid-turn).
//   - When an active conversation id is found, calls GET /api/v1/conversations/{id} to fetch the history and seeds
//     the store. A 404 (conversation doesn't exist yet on the server) is silently swallowed: the conversation will be
//     created implicitly by the first POST.
//   - When no active conversation id is found, mints a fresh client-supplied ULID and seeds the store with an empty
//     history. The server creates the conversation implicitly on the first POST /api/v1/conversations/{id}/messages.
//
// Reload-resilience (AC #4): if the user reloads mid-turn, this load function re-fetches the history (including any
// already-completed assistant messages) and the continuing SSE stream is still open, so new deltas resume into the
// live slot.
//
// The load function exports `conversationId` so the page can read it; the store is the reactive authority for
// rendering.

import { ulid } from "ulid";
import { Api } from "$lib/api/index.js";
import { getActiveConversationId, setActiveConversation } from "$lib/stores/conversation.svelte.js";
import type { PageLoad } from "./$types.js";

export const load: PageLoad = async () => {
  let conversationId = getActiveConversationId();

  if (conversationId === null) {
    // No conversation open yet: mint a fresh client-supplied ULID. The server will create the conversation record
    // implicitly on the first POST.
    conversationId = ulid();
    setActiveConversation(conversationId, []);
    return { conversationId };
  }

  // Existing conversation: fetch history to seed the store (reload-resilience). 404 is not an error here — the
  // conversation may not yet exist on the server (fresh ULID not yet POSTed). Swallow it silently.
  const { data } = await Api.GET("/conversations/{id}", {
    params: { path: { id: conversationId } },
  });

  if (data?.messages !== undefined) {
    setActiveConversation(conversationId, data.messages);
  }

  return { conversationId };
};

<script lang="ts">
  // Home page — chat interface.
  //
  // Renders the active conversation history (from the conversation store) and
  // provides a composer to send new messages.
  //
  // Message flow:
  //   1. User types and presses Enter / clicks Send.
  //   2. POST /api/v1/conversations/{id}/messages — 202 with persisted user message.
  //   3. The persisted user message is appended to the history store.
  //   4. The assistant reply streams in via SSE (message.delta → applyStreamingDelta,
  //      message.completed → finalizeStreamingMessage) — no polling here.
  //   5. Tool calls emit tool.started → chip; tool.completed → entity card;
  //      tool.failed → soft error. Chips render inline below the streaming slot.
  //   6. On turn.failed, the store's getTurnError() becomes non-null and the UI
  //      shows a calm generic error line.
  //
  // Security: ALL assistant/tool text flows through renderSafeMarkdown()
  // (marked → DOMPurify) before {@html}. User messages are rendered as escaped
  // text (plain {content}), never {@html}.
  //
  // Tool-call / reload-dedup note: tool chips are live-only (SSE events). On a
  // page reload the history returns with role:"tool" messages and a toolCalls
  // field on assistant messages — those are NOT yet rendered as chips (the
  // getToolCalls() store is empty after a reload). Dedup of live vs loaded tool
  // calls is a follow-up; the live path is clean and keyed by callId.

  import { Api } from "$lib/api/index.js";
  import {
    getConversationHistory,
    getActiveConversationId,
    getTurnError,
    clearTurnError,
    appendMessage,
  } from "$lib/stores/conversation.svelte.js";
  import { getToolCallsForMessage } from "$lib/stores/tool-calls.svelte.js";
  import { renderSafeMarkdown } from "$lib/markdown/sanitize.js";
  import { chat_composer_placeholder, chat_send, chat_turn_failed } from "$lib/paraglide/messages.js";
  import ToolCallChip from "$lib/components/ToolCallChip.svelte";
  import type { PageData } from "./$types.js";

  interface Props {
    data: PageData;
  }

  const { data }: Props = $props();

  // ---------------------------------------------------------------------------
  // Reactive history and tool calls — derived from stores.
  // $derived reads the stores reactively; no $effect needed.
  // ---------------------------------------------------------------------------
  const history = $derived(getConversationHistory());
  const turnError = $derived(getTurnError());

  // ---------------------------------------------------------------------------
  // Composer state
  // ---------------------------------------------------------------------------
  let composerText = $state("");
  let sending = $state(false);

  // ---------------------------------------------------------------------------
  // Send message
  // ---------------------------------------------------------------------------

  async function sendMessage(): Promise<void> {
    const text = composerText.trim();
    if (text === "" || sending) return;

    const conversationId = getActiveConversationId() ?? data.conversationId;

    // Clear any previous turn error when sending a new message.
    clearTurnError();

    sending = true;
    composerText = "";

    try {
      const { data: msgData } = await Api.POST("/conversations/{id}/messages", {
        params: { path: { id: conversationId } },
        body: { content: text },
      });

      // 202: append the persisted user message to the history.
      // appendMessage() splices before any open streaming slot so the user bubble
      // always precedes the assistant reply, even when a message.delta arrived
      // over the SSE connection while the POST was still in-flight.
      if (msgData !== undefined) {
        appendMessage({
          id: msgData.id,
          role: msgData.role,
          content: msgData.content,
          createdAt: msgData.createdAt,
          abandoned: false,
        });
      }
    } catch {
      // Network error — restore the text so the user can retry.
      composerText = text;
    } finally {
      sending = false;
    }
  }

  function handleKeydown(event: KeyboardEvent): void {
    // Send on Enter (without Shift — Shift+Enter inserts a newline).
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      void sendMessage();
    }
  }
</script>

<div class="flex h-full flex-col">
  <!-- Message thread -->
  <div class="flex-1 overflow-y-auto px-4 py-4">
    {#if history.length === 0 && turnError === null}
      <!-- Empty state: no messages yet -->
      <p class="text-text-subtle text-sm">Start a conversation below.</p>
    {:else}
      <ul class="flex flex-col gap-4" role="list">
        {#each history as message (message.id)}
          <li class="flex {message.role === 'user' ? 'justify-end' : 'justify-start'}">
            {#if message.role === "user"}
              <!--
                User messages: plain escaped text — never {@html}.
                User input is trusted only in the sense that it belongs to the
                authenticated user, but XSS prevention still requires escaping.
              -->
              <div class="bg-surface-raised text-text max-w-[80%] rounded-xl px-4 py-2 text-sm">
                {message.content}
              </div>
            {:else if message.role === "assistant"}
              <!--
                Assistant messages: sanitized Markdown via renderSafeMarkdown()
                (marked → DOMPurify). Safe to use {@html} here.
                The streaming slot has `streaming: true` while the turn is in
                progress, allowing the in-flight bubble to animate naturally.
              -->
              <div
                class="text-text max-w-[80%] rounded-xl px-4 py-2 text-sm {'streaming' in message && message.streaming
                  ? 'opacity-80'
                  : ''}"
              >
                <!-- eslint-disable-next-line svelte/no-at-html-tags -->
                {@html renderSafeMarkdown(message.content)}
                {#if "streaming" in message && message.streaming}
                  <span class="ml-1 inline-block h-2 w-2 animate-pulse rounded-full bg-current" aria-hidden="true"
                  ></span>
                {/if}

                <!--
                  Tool-call chips: render inline below the assistant message text.
                  Keyed by callId; transition: started → entity card / error.
                  During streaming, message.id is the sentinel "__streaming__" and
                  getToolCallsForMessage returns the in-flight entries. After the turn
                  finalizes, message.id becomes the real id and the retagged entries
                  remain visible — cards persist after streaming ends.
                  Quiet reads (list_reminders) render as nothing inside ToolCallChip.
                -->
                {#if getToolCallsForMessage(message.id).length > 0}
                  <ul class="mt-2 flex flex-col gap-1" role="list" aria-label="Tool calls">
                    {#each getToolCallsForMessage(message.id) as entry (entry.callId)}
                      <li>
                        <ToolCallChip {entry} />
                      </li>
                    {/each}
                  </ul>
                {/if}
              </div>
            {/if}
          </li>
        {/each}

        <!-- turn.failed error line -->
        {#if turnError !== null}
          <li class="flex justify-start" role="alert">
            <p class="text-text-error text-sm">{chat_turn_failed()}</p>
          </li>
        {/if}
      </ul>
    {/if}
  </div>

  <!-- Composer -->
  <div class="border-border border-t px-4 py-3">
    <div class="flex items-end gap-2">
      <textarea
        rows={1}
        placeholder={chat_composer_placeholder()}
        disabled={sending}
        class="border-border bg-surface text-text focus:ring-border-focus flex-1 resize-none rounded-lg border px-3 py-2 text-sm
               focus:outline-none focus:ring-2 disabled:opacity-50"
        bind:value={composerText}
        onkeydown={handleKeydown}
        aria-label={chat_composer_placeholder()}
      ></textarea>
      <button
        type="button"
        disabled={sending || composerText.trim() === ""}
        onclick={() => void sendMessage()}
        class="bg-primary text-on-primary rounded-lg px-4 py-2 text-sm font-medium
               disabled:cursor-not-allowed disabled:opacity-50
               hover:enabled:opacity-90 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-current"
      >
        {chat_send()}
      </button>
    </div>
  </div>
</div>

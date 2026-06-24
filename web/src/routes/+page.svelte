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
    getActiveConversationId,
    clearTurnError,
    sendChatMessage,
    type PostMessageFn,
  } from "$lib/stores/conversation.svelte.js";
  import { chat_composer_placeholder, chat_send, conv_switcher_open } from "$lib/paraglide/messages.js";
  import ChatThread from "$lib/components/ChatThread.svelte";
  import ConversationSwitcher from "$lib/components/ConversationSwitcher.svelte";
  import type { PageData } from "./$types.js";

  interface Props {
    data: PageData;
  }

  const { data }: Props = $props();

  // ---------------------------------------------------------------------------
  // Composer ref — used to focus the textarea after "New conversation"
  // ---------------------------------------------------------------------------
  let composerEl = $state<HTMLTextAreaElement | null>(null);

  function focusComposer(): void {
    composerEl?.focus();
  }

  // ---------------------------------------------------------------------------
  // Conversation switcher panel state
  // ---------------------------------------------------------------------------
  let switcherOpen = $state(false);

  // ---------------------------------------------------------------------------
  // Composer state
  // ---------------------------------------------------------------------------
  let composerText = $state("");
  let sending = $state(false);

  // ---------------------------------------------------------------------------
  // Send message
  // ---------------------------------------------------------------------------

  // Narrow postFn adapter — translates the full Api.POST signature for this
  // endpoint to the PostMessageFn shape expected by sendChatMessage.
  const postMessageFn: PostMessageFn = async (conversationId, text) =>
    Api.POST("/conversations/{id}/messages", {
      params: { path: { id: conversationId } },
      body: { content: text },
    });

  async function sendMessage(): Promise<void> {
    const text = composerText.trim();
    if (text === "" || sending) {
      return;
    }

    const conversationId = getActiveConversationId() ?? data.conversationId;

    // Clear any previous turn error when sending a new message.
    clearTurnError();

    sending = true;
    composerText = "";

    // sendChatMessage handles POST + success (append) + error (setTurnFailed).
    // Returns the text to restore on any error path, null on success.
    const restoreText = await sendChatMessage(postMessageFn, conversationId, text);
    if (restoreText !== null) {
      composerText = restoreText;
    }

    sending = false;
  }

  function handleKeydown(event: KeyboardEvent): void {
    // Send on Enter (without Shift — Shift+Enter inserts a newline).
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      void sendMessage();
    }
  }
</script>

<ConversationSwitcher bind:open={switcherOpen} {focusComposer} />

<div class="flex h-full flex-col">
  <!-- Chat header: conversation switcher trigger -->
  <div class="border-border flex shrink-0 items-center justify-between border-b px-4 py-2">
    <button
      type="button"
      class="text-fg-muted hover:text-fg flex items-center gap-1.5 rounded px-2 py-1.5 text-sm
             focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-current"
      onclick={() => {
        switcherOpen = true;
      }}
      aria-label={conv_switcher_open()}
    >
      <!-- Chat bubbles icon (X icon from phosphor; deep-import to avoid barrel-import cost) -->
      <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 256 256" aria-hidden="true">
        <path
          fill="currentColor"
          d="M216 48H40a16 16 0 0 0-16 16v160a15.85 15.85 0 0 0 9.24 14.5A16.05 16.05 0 0 0 40 240a15.89 15.89 0 0 0 10.25-3.78l.09-.07 34.33-30.08A8 8 0 0 1 88 200h128a16 16 0 0 0 16-16V64a16 16 0 0 0-16-16Zm0 136H88a24.07 24.07 0 0 0-15.29 5.47L40 220.32V64h176Z"
        />
      </svg>
      {conv_switcher_open()}
    </button>
  </div>

  <!-- Message thread — single source of truth in ChatThread.svelte -->
  <div class="flex-1 overflow-y-auto px-4 py-4">
    <ChatThread />
  </div>

  <!-- Composer -->
  <div class="border-border border-t px-4 py-3">
    <div class="flex items-end gap-2">
      <textarea
        bind:this={composerEl}
        rows={1}
        placeholder={chat_composer_placeholder()}
        disabled={sending}
        class="border-border bg-surface text-fg focus:ring-border-focus flex-1 resize-none rounded-lg border px-3 py-2 text-sm
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

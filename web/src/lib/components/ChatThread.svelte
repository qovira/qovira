<script lang="ts">
  // ChatThread — the single source of truth for the message thread markup.
  //
  // Rendered by both +page.svelte (the live chat route) and Storybook stories. Reads directly from the
  // conversation, tool-calls, and confirmations stores so stories exercise the actual rendering paths without
  // bootstrapping a full SvelteKit page.
  //
  // Security: assistant text flows through renderSafeMarkdown() (marked → DOMPurify) before {@html}. User
  // messages are rendered as escaped {content} — never {@html}. This is the ONLY place the {@html} rendering
  // path exists; +page.svelte delegates here entirely.

  import { getConversationHistory, getTurnError } from "$lib/stores/conversation.svelte.js";
  import { getToolCallsForMessage } from "$lib/stores/tool-calls.svelte.js";
  import { getConfirmationsForMessage } from "$lib/stores/confirmations.svelte.js";
  import { renderSafeMarkdown } from "$lib/markdown/sanitize.js";
  import {
    chat_turn_failed,
    chat_empty_prompt,
    chat_tool_calls_label,
    chat_confirmations_label,
  } from "$lib/paraglide/messages.js";
  import ToolCallChip from "$lib/components/ToolCallChip.svelte";
  import ConfirmationCard from "$lib/components/ConfirmationCard.svelte";

  const history = $derived(getConversationHistory());
  const turnError = $derived(getTurnError());
</script>

{#if history.length === 0 && turnError === null}
  <p class="text-fg-muted text-sm">{chat_empty_prompt()}</p>
{:else}
  <ul class="flex flex-col gap-4" role="list">
    {#each history as message (message.id)}
      <li class="flex {message.role === 'user' ? 'justify-end' : 'justify-start'}">
        {#if message.role === "user"}
          <!--
            User messages: plain escaped text — never {@html}. User input is trusted only in the sense that it
            belongs to the authenticated user, but XSS prevention still requires escaping.
          -->
          <div class="bg-surface-raised text-fg max-w-[80%] rounded-xl px-4 py-2 text-sm">
            {message.content}
          </div>
        {:else if message.role === "assistant"}
          <!--
            Assistant messages: sanitized Markdown via renderSafeMarkdown() (marked → DOMPurify). Safe to use
            {@html} here. The streaming slot has `streaming: true` while the turn is in progress, allowing the
            in-flight bubble to animate naturally.
          -->
          <div
            class="text-fg max-w-[80%] rounded-xl px-4 py-2 text-sm {'streaming' in message && message.streaming
              ? 'opacity-80'
              : ''}"
          >
            <!-- eslint-disable-next-line svelte/no-at-html-tags -->
            {@html renderSafeMarkdown(message.content)}
            {#if "streaming" in message && message.streaming}
              <span class="ml-1 inline-block h-2 w-2 animate-pulse rounded-full bg-current" aria-hidden="true"></span>
            {/if}

            <!--
              Tool-call chips: render inline below the assistant message text. Keyed by callId; transition:
              started → entity card / error. During streaming, message.id is the sentinel "__streaming__" and
              getToolCallsForMessage returns the in-flight entries. After the turn finalizes, message.id becomes
              the real id and the retagged entries remain visible — cards persist after streaming ends. Quiet
              reads (list_reminders) render as nothing inside ToolCallChip.
            -->
            {#if getToolCallsForMessage(message.id).length > 0}
              <ul class="mt-2 flex flex-col gap-1" role="list" aria-label={chat_tool_calls_label()}>
                {#each getToolCallsForMessage(message.id) as entry (entry.callId)}
                  <li>
                    <ToolCallChip {entry} />
                  </li>
                {/each}
              </ul>
            {/if}

            <!--
              Confirmation cards: inline approve/deny cards for risk-tier actions. Keyed by callId; keyed
              independently from tool chips. During the suspension window, message.id is the confirmation
              sentinel and getConfirmationsForMessage returns the in-flight entries. After message.completed
              fires, the sentinel is retagged to the real messageId so the cards persist inline under the
              finalized turn — same pattern as tool-call chips.
            -->
            {#if getConfirmationsForMessage(message.id).length > 0}
              <ul class="mt-2 flex flex-col gap-2" role="list" aria-label={chat_confirmations_label()}>
                {#each getConfirmationsForMessage(message.id) as entry (entry.callId)}
                  <li>
                    <ConfirmationCard {entry} />
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
      <li class="flex justify-start">
        <p class="text-fg-error text-sm" role="alert">{chat_turn_failed()}</p>
      </li>
    {/if}
  </ul>
{/if}

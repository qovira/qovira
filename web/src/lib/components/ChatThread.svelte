<script lang="ts">
  // ChatThread — thin presentational wrapper around the message thread portion
  // of the chat page. Reads from the real conversation store (getConversationHistory,
  // getTurnError) so stories exercise the actual rendering paths used in +page.svelte.
  //
  // This is NOT a replacement for +page.svelte — it is extracted purely to make
  // the thread fragment storiable without bootstrapping a full SvelteKit page.
  // The markup here mirrors +page.svelte's message list section exactly.
  //
  // Security: same rules as +page.svelte — assistant text through renderSafeMarkdown(),
  // user text through plain {content} interpolation.

  import { getConversationHistory, getTurnError } from "$lib/stores/conversation.svelte.js";
  import { getToolCallsForMessage } from "$lib/stores/tool-calls.svelte.js";
  import { getConfirmationsForMessage } from "$lib/stores/confirmations.svelte.js";
  import { renderSafeMarkdown } from "$lib/markdown/sanitize.js";
  import { chat_turn_failed } from "$lib/paraglide/messages.js";
  import ToolCallChip from "$lib/components/ToolCallChip.svelte";
  import ConfirmationCard from "$lib/components/ConfirmationCard.svelte";

  const history = $derived(getConversationHistory());
  const turnError = $derived(getTurnError());
</script>

<div class="flex flex-col gap-4 p-4" style="min-height: 200px; max-width: 640px;">
  {#if history.length === 0 && turnError === null}
    <p class="text-text-muted text-sm">Start a conversation below.</p>
  {:else}
    <ul class="flex flex-col gap-4" role="list">
      {#each history as message (message.id)}
        <li class="flex {message.role === 'user' ? 'justify-end' : 'justify-start'}">
          {#if message.role === "user"}
            <div class="bg-surface-raised text-text max-w-[80%] rounded-xl px-4 py-2 text-sm">
              {message.content}
            </div>
          {:else if message.role === "assistant"}
            <div
              class="text-text max-w-[80%] rounded-xl px-4 py-2 text-sm {'streaming' in message && message.streaming
                ? 'opacity-80'
                : ''}"
            >
              <!-- eslint-disable-next-line svelte/no-at-html-tags -->
              {@html renderSafeMarkdown(message.content)}
              {#if "streaming" in message && message.streaming}
                <span class="ml-1 inline-block h-2 w-2 animate-pulse rounded-full bg-current" aria-hidden="true"></span>
              {/if}

              {#if getToolCallsForMessage(message.id).length > 0}
                <ul class="mt-2 flex flex-col gap-1" role="list" aria-label="Tool calls">
                  {#each getToolCallsForMessage(message.id) as entry (entry.callId)}
                    <li>
                      <ToolCallChip {entry} />
                    </li>
                  {/each}
                </ul>
              {/if}

              {#if getConfirmationsForMessage(message.id).length > 0}
                <ul class="mt-2 flex flex-col gap-2" role="list" aria-label="Pending confirmations">
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

      {#if turnError !== null}
        <li class="flex justify-start">
          <p class="text-error-text text-sm" role="alert">{chat_turn_failed()}</p>
        </li>
      {/if}
    </ul>
  {/if}
</div>
